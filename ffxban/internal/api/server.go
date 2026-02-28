package api

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"ffxban/internal/config"
	"ffxban/internal/models"
	"ffxban/internal/processor"
	"ffxban/internal/services/alerter"
	"ffxban/internal/services/blocker"
	"ffxban/internal/services/nodessh"
	"ffxban/internal/services/panel"
	"ffxban/internal/services/storage"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/bcrypt"
)

//go:embed panel.html
var panelHTML []byte

//go:embed panel.css
var panelCSS []byte

var durationPattern = regexp.MustCompile(`^(\d+[smhd]|permanent)$`)
var onlinePresenceWindow = 3 * time.Minute

const (
	panelLoginMaxFailedAttempts = 5
	panelLoginLockoutDuration   = 60 * time.Second
)

type panelOnDemandRefresher interface {
	RefreshIfStale(ctx context.Context, maxAge time.Duration) bool
}

type panelSession struct {
	ExpiresAt time.Time
}

type panelLoginAttempt struct {
	FailedCount int
	LockedUntil time.Time
	LastAttempt time.Time
}

type runtimeConfigOverrides struct {
	MaxIPsPerUser               *int
	BlockDuration               *string
	UserIPTTLSeconds            *int
	ClearIPsDelaySeconds        *int
	SharingDetectionEnabled     *bool
	TriggerCount                *int
	TriggerPeriodSeconds        *int
	TriggerMinIntervalSeconds   *int
	SharingSustainSeconds       *int
	BanlistThresholdSeconds     *int
	SharingSourceWindowSeconds  *int
	SubnetGrouping              *bool
	SharingBlockOnBanlistOnly   *bool
	SharingBlockOnViolatorOnly  *bool
	SharingHardwareGuard        *bool
	SharingPermanentBanEnabled  *bool
	SharingPermanentBanDuration *string
	BanEscalationEnabled        *bool
	BanEscalationDurations      *[]string
	GeoSharingEnabled           *bool
	GeoSharingMinCountries      *int
	NetworkPolicyEnabled        *bool
	NetworkMobileGraceIPs       *int
	NetworkForceBlockExcessIPs  *int
	HeuristicsEnabled           *bool
	HeuristicsWindowSeconds     *int
	NodeHeartbeatTimeoutSeconds *int
	PrometheusEnabled           *bool
}

type serverMetrics struct {
	registry           *prometheus.Registry
	handler            http.Handler
	sharingViolators   prometheus.Gauge
	sharingBanlist     prometheus.Gauge
	sharingPermanent   prometheus.Gauge
	geoMismatches      prometheus.Gauge
	activeBans         prometheus.Gauge
	nodesTotal         prometheus.Gauge
	nodesOnline        prometheus.Gauge
	logQueueLen        prometheus.Gauge
	sideEffectQueueLen prometheus.Gauge
	activeSessions     prometheus.Gauge
	uptimeSeconds      prometheus.Gauge
}

type Server struct {
	router        *gin.Engine
	processor     *processor.LogProcessor
	storage       storage.IPStorage
	blocker       blocker.Blocker
	notifier      alerter.Notifier
	limitProvider panel.UserLimitProvider
	networkClass  networkClassifier
	cfg           *config.Config
	baseConfig    config.Config
	port          string
	wsHub         *wsHub
	nodeSSH       *nodessh.Executor
	startedAt     time.Time
	buildVersion  string
	metrics       *serverMetrics

	sessionMu sync.Mutex
	sessions  map[string]panelSession

	loginMu       sync.Mutex
	loginAttempts map[string]panelLoginAttempt

	configMu        sync.RWMutex
	configOverrides runtimeConfigOverrides
}

type networkClassifier interface {
	Classify(ctx context.Context, ip string) models.NetworkInfo
}

type ipNodeCandidatesFinder interface {
	FindNodeCandidatesByIP(ctx context.Context, ip string, limit int) ([]string, error)
}

type ipNodeSubnetCandidatesFinder interface {
	FindNodeCandidatesBySubnet24(ctx context.Context, subnetPrefix string, limit int) ([]string, error)
}

func NewServer(
	cfg *config.Config,
	proc *processor.LogProcessor,
	storage storage.IPStorage,
	blk blocker.Blocker,
	limitProvider panel.UserLimitProvider,
	networkClass networkClassifier,
	buildVersion string,
) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	hub := newWsHub()
	go hub.Run()

	nodeSSHExec, sshErr := nodessh.New(nodessh.Config{
		Enabled:        cfg.NodeSSHEnabled,
		User:           cfg.NodeSSHUser,
		Password:       cfg.NodeSSHPassword,
		PrivateKey:     cfg.NodeSSHPrivateKey,
		DefaultPort:    cfg.NodeSSHDefaultPort,
		ConnectTimeout: cfg.NodeSSHConnectTimeout,
		MaxOutputBytes: cfg.NodeSSHMaxOutputBytes,
	})
	if sshErr != nil {
		log.Printf("Warning: SSH executor disabled: %v", sshErr)
	}

	s := &Server{
		router:        router,
		processor:     proc,
		storage:       storage,
		blocker:       blk,
		notifier:      alerter.NewWebhookAlerter(cfg.AlertWebhookURL, cfg.AlertWebhookMode, cfg.AlertWebhookToken, cfg.AlertWebhookAuthHeader, cfg.AlertWebhookUsernamePrefix),
		limitProvider: limitProvider,
		networkClass:  networkClass,
		cfg:           cfg,
		baseConfig:    *cfg,
		port:          cfg.Port,
		sessions:      make(map[string]panelSession),
		wsHub:         hub,
		nodeSSH:       nodeSSHExec,
		startedAt:     time.Now(),
		buildVersion:  strings.TrimSpace(buildVersion),
		loginAttempts: make(map[string]panelLoginAttempt),
		metrics:       newServerMetrics(),
	}
	if s.buildVersion == "" {
		s.buildVersion = "dev"
	}

	// Подписываем hub на события из processor'а
	proc.OnBatchProcessed = hub.Notify

	if err := s.ensurePanelPasswordHash(); err != nil {
		log.Printf("Warning: не удалось инициализировать пароль панели: %v", err)
	}
	if err := s.loadRuntimeConfigOverrides(); err != nil {
		log.Printf("Warning: не удалось загрузить runtime-overrides: %v", err)
	}

	s.setupRoutes()
	return s
}

func (s *Server) GetRouter() *gin.Engine {
	return s.router
}

func (s *Server) setupRoutes() {
	s.router.POST("/log-entry", s.handleProcessLogEntries)
	s.router.POST("/device-report", s.handleDeviceReport)
	s.router.POST("/node-heartbeat", s.handleNodeHeartbeat)
	s.router.GET("/health", s.handleHealthCheck)
	s.router.GET("/metrics", s.handleMetrics)
	s.router.GET("/provision", s.handleNodeProvision)
	s.router.GET("/panel", s.handlePanel)
	s.router.GET("/panel/", s.handlePanel)
	s.router.GET("/panel.css", s.handlePanelCSS)

	panelGroup := s.router.Group("/api/panel")
	panelGroup.POST("/login", s.handlePanelLogin)
	panelGroup.GET("/me", s.apiAuthMiddleware(), s.handlePanelMe)
	panelGroup.POST("/logout", s.apiAuthMiddleware(), s.handlePanelLogout)
	panelGroup.POST("/password", s.apiAuthMiddleware(), s.handlePanelPasswordUpdate)

	internal := s.router.Group("/api")
	internal.Use(s.apiAuthMiddleware())
	internal.GET("/stats", s.handleInternalStats)
	internal.GET("/sysinfo", s.handleSysInfo)
	internal.GET("/users", s.handleInternalUsers)
	internal.GET("/traffic/users", s.handleInternalTrafficUsers)
	internal.GET("/users/:id", s.handleInternalUserDetails)
	internal.GET("/logs", s.handleInternalLogs)
	internal.GET("/bans/active", s.handleInternalActiveBans)
	internal.GET("/banlist/users", s.handleInternalSharingBanlist)
	internal.GET("/sharing/permanent", s.handleInternalSharingPermanent)
	internal.GET("/networks/types", s.handleInternalNetworkTypes)
	internal.GET("/nodes/health", s.handleInternalNodeHealth)
	internal.GET("/nodes", s.handleInternalNodes)
	internal.POST("/nodes/ssh/exec", s.handleNodeSSHExec)
	internal.GET("/nodes/ssh/creds", s.handleNodeSSHListCreds)
	internal.POST("/nodes/ssh/creds", s.handleNodeSSHSaveCreds)
	internal.DELETE("/nodes/ssh/creds", s.handleNodeSSHDeleteCreds)
	internal.GET("/nodes/ssh/terminal", s.handleSSHTerminal)
	internal.GET("/nodes/ssh/terminal/nodes", s.handleSSHTerminalNodes)
	internal.POST("/actions/block", s.handleManualBlock)
	internal.POST("/actions/unblock", s.handleManualUnblock)
	internal.POST("/actions/clear", s.handleManualClear)
	internal.POST("/actions/reset_sharing", s.handleResetSharing)
	internal.GET("/config", s.handleConfig)
	internal.POST("/config/overrides", s.handleConfigOverridesUpdate)
	internal.DELETE("/config/overrides", s.handleConfigOverridesReset)
	internal.GET("/ws", s.handleWebSocket)
}

func (s *Server) Run() error {
	return s.router.Run(":" + s.port)
}

func (s *Server) handleProcessLogEntries(c *gin.Context) {
	var entries []models.LogEntry
	if err := c.ShouldBindJSON(&entries); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := s.processor.EnqueueEntries(entries); err != nil {
		log.Printf("Warning: log queue is full. Rejecting request for %d entries. Error: %v", len(entries), err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Service is temporarily overloaded. Please try again later.",
		})
		return
	}

	// Если в записи есть user_agent — сохраняем как отчёт об устройстве.
	now := time.Now()
	for _, e := range entries {
		if e.UserAgent != "" {
			_ = s.storage.SaveDeviceReport(c.Request.Context(), e.UserEmail, e.UserAgent, now)
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":            "accepted",
		"processed_entries": len(entries),
	})
}

func (s *Server) handleNodeHeartbeat(c *gin.Context) {
	token := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if token == "" {
		authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			token = strings.TrimSpace(authHeader[7:])
		}
	}
	if token == "" || token != strings.TrimSpace(s.cfg.InternalAPIToken) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req struct {
		NodeName  string `json:"node_name"`
		Timestamp string `json:"timestamp,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	nodeName := strings.TrimSpace(req.NodeName)
	if nodeName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_name is required"})
		return
	}

	seenAt := time.Now()
	if raw := strings.TrimSpace(req.Timestamp); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			seenAt = parsed
		}
	}

	s.processor.RecordNodeHeartbeat(nodeName, seenAt)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handleHealthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := http.StatusOK
	activeSessions := s.processor.CountActiveSessions(onlinePresenceWindow)
	uptime := time.Since(s.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	response := gin.H{
		"redis_connection":    "ok",
		"rabbitmq_connection": "ok",
		"active_sessions":     activeSessions,
		"uptime_seconds":      int64(uptime.Seconds()),
		"started_at":          s.startedAt.UTC().Format(time.RFC3339),
		"version":             s.buildVersion,
	}

	if err := s.storage.Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		response["redis_connection"] = "failed"
	}

	if err := s.blocker.Ping(); err != nil {
		status = http.StatusServiceUnavailable
		response["rabbitmq_connection"] = "failed"
	}

	if status == http.StatusOK {
		response["status"] = "ok"
	} else {
		response["status"] = "error"
	}

	c.JSON(status, response)
}

func (s *Server) handlePanel(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", panelHTML)
}

func (s *Server) handlePanelCSS(c *gin.Context) {
	c.Data(http.StatusOK, "text/css; charset=utf-8", panelCSS)
}

func (s *Server) apiAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(c.GetHeader("X-API-Key"))
		if token == "" {
			authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
			if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				token = strings.TrimSpace(authHeader[7:])
			}
		}

		if token != "" && token == strings.TrimSpace(s.cfg.InternalAPIToken) {
			c.Next()
			return
		}

		if s.hasValidPanelSession(c) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	}
}

func (s *Server) hasValidPanelSession(c *gin.Context) bool {
	raw, err := c.Cookie("ffxban_panel_session")
	if err != nil || raw == "" {
		return false
	}

	now := time.Now()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	for token, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, token)
		}
	}

	session, ok := s.sessions[raw]
	return ok && now.Before(session.ExpiresAt)
}

func (s *Server) createPanelSession(c *gin.Context) error {
	tokenRaw := make([]byte, 32)
	if _, err := rand.Read(tokenRaw); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenRaw)
	expires := time.Now().Add(s.cfg.PanelSessionTTL)

	s.sessionMu.Lock()
	s.sessions[token] = panelSession{ExpiresAt: expires}
	s.sessionMu.Unlock()

	c.SetCookie("ffxban_panel_session", token, int(s.cfg.PanelSessionTTL.Seconds()), "/", "", true, true)
	return nil
}

func (s *Server) destroyPanelSession(c *gin.Context) {
	if token, err := c.Cookie("ffxban_panel_session"); err == nil && token != "" {
		s.sessionMu.Lock()
		delete(s.sessions, token)
		s.sessionMu.Unlock()
	}
	c.SetCookie("ffxban_panel_session", "", -1, "/", "", true, true)
}

func (s *Server) ensurePanelPasswordHash() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hash, err := s.storage.GetPanelPasswordHash(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(hash) != "" {
		return nil
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(s.cfg.PanelPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	if err := s.storage.SetPanelPasswordHash(ctx, string(newHash)); err != nil {
		return err
	}
	log.Println("Инициализирован пароль панели из PANEL_PASSWORD")
	return nil
}

func (s *Server) verifyPanelPassword(password string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hash, err := s.storage.GetPanelPasswordHash(ctx)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(hash) == "" {
		return false, nil
	}

	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil, nil
}

func (s *Server) updatePanelPassword(newPassword string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.storage.SetPanelPasswordHash(ctx, string(hash))
}

func (s *Server) panelLoginClientIP(c *gin.Context) string {
	ip := strings.TrimSpace(c.ClientIP())
	if ip == "" {
		return "unknown"
	}
	return ip
}

func (s *Server) panelLoginLockInfo(ip string) (bool, int) {
	now := time.Now()

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	for key, state := range s.loginAttempts {
		if state.LockedUntil.IsZero() && state.FailedCount == 0 {
			delete(s.loginAttempts, key)
			continue
		}
		if state.LockedUntil.Before(now.Add(-10 * time.Minute)) {
			delete(s.loginAttempts, key)
		}
	}

	state, ok := s.loginAttempts[ip]
	if !ok {
		return false, 0
	}
	if state.LockedUntil.After(now) {
		retry := int(state.LockedUntil.Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		return true, retry
	}
	return false, 0
}

func (s *Server) panelLoginFailed(ip string) (bool, int) {
	now := time.Now()

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	state := s.loginAttempts[ip]
	state.LastAttempt = now
	if state.LockedUntil.After(now) {
		retry := int(state.LockedUntil.Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		s.loginAttempts[ip] = state
		return true, retry
	}

	state.FailedCount++
	if state.FailedCount >= panelLoginMaxFailedAttempts {
		state.FailedCount = 0
		state.LockedUntil = now.Add(panelLoginLockoutDuration)
		s.loginAttempts[ip] = state
		return true, int(panelLoginLockoutDuration.Seconds())
	}

	s.loginAttempts[ip] = state
	return false, 0
}

func (s *Server) panelLoginSucceeded(ip string) {
	s.loginMu.Lock()
	delete(s.loginAttempts, ip)
	s.loginMu.Unlock()
}

func (s *Server) handlePanelLogin(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.Password = strings.TrimSpace(req.Password)
	if req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password required"})
		return
	}

	clientIP := s.panelLoginClientIP(c)
	if locked, retry := s.panelLoginLockInfo(clientIP); locked {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":               "too many failed attempts",
			"retry_after_seconds": retry,
		})
		return
	}

	ok, err := s.verifyPanelPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		if locked, retry := s.panelLoginFailed(clientIP); locked {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":               "too many failed attempts",
				"retry_after_seconds": retry,
			})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}
	s.panelLoginSucceeded(clientIP)

	if err := s.createPanelSession(c); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handlePanelMe(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"authenticated": true})
}

func (s *Server) handlePanelLogout(c *gin.Context) {
	s.destroyPanelSession(c)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handlePanelPasswordUpdate(c *gin.Context) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	newPassword := strings.TrimSpace(req.NewPassword)
	if len(newPassword) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new password must be at least 8 characters"})
		return
	}

	ok, err := s.verifyPanelPassword(strings.TrimSpace(req.CurrentPassword))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is invalid"})
		return
	}

	if err := s.updatePanelPassword(newPassword); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handleInternalStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	emails, err := s.storage.GetAllUserEmails(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	activeUsers := 0
	totalActiveIPs := 0
	overLimitUsers := 0
	sharingBanUsers := 0
	permanentSharingUsers := 0
	realBannedUsers := 0
	realBannedIPs := 0

	for _, email := range emails {
		activeIPs, err := s.storage.GetUserActiveIPs(ctx, email)
		if err != nil {
			continue
		}
		ipCount := len(activeIPs)
		if ipCount == 0 {
			continue
		}

		activeUsers++
		totalActiveIPs += ipCount

		limit := s.processor.ResolveUserIPLimit(ctx, email)
		overLimit := ipCount > limit
		if s.cfg.SharingDetectionEnabled {
			overLimit = false
			if sharing, ok := s.processor.GetSharingStatus(email); ok {
				overLimit = sharing.Violator
				if sharing.InBanlist {
					sharingBanUsers++
				}
				if sharing.PermanentBan {
					permanentSharingUsers++
				}
			}
		} else {
			if sharing, ok := s.processor.GetSharingStatus(email); ok {
				if sharing.InBanlist {
					sharingBanUsers++
				}
				if sharing.PermanentBan {
					permanentSharingUsers++
				}
			}
		}
		if overLimit {
			overLimitUsers++
		}

		userHasRealBan := false
		for ip := range activeIPs {
			if s.processor.IsIPActivelyBanned(ip) {
				realBannedIPs++
				userHasRealBan = true
			}
		}
		if userHasRealBan {
			realBannedUsers++
		}
	}

	logQueueLen, sideEffectQueueLen := s.processor.GetQueueStats()
	panelStats := panel.Stats{}
	if s.limitProvider != nil {
		panelStats = s.limitProvider.Stats()
	}

	c.JSON(http.StatusOK, gin.H{
		"active_users":            activeUsers,
		"total_active_ips":        totalActiveIPs,
		"over_limit_users":        overLimitUsers,
		"log_queue_len":           logQueueLen,
		"side_effect_queue_len":   sideEffectQueueLen,
		"panel_limits_enabled":    s.limitProvider != nil,
		"panel_loaded":            panelStats.Loaded,
		"panel_users_count":       panelStats.Users,
		"panel_last_reload":       panelStats.LastLoad,
		"panel_last_error":        panelStats.LastError,
		"heuristics_enabled":      s.cfg.HeuristicsEnabled,
		"heuristics_window_secs":  int(s.cfg.HeuristicsWindow.Seconds()),
		"sharing_enabled":         s.cfg.SharingDetectionEnabled,
		"sharing_violators":       overLimitUsers,
		"sharing_banlist_users":   sharingBanUsers,
		"sharing_permanent_users": permanentSharingUsers,
		"real_banned_users":       realBannedUsers,
		"real_active_bans":        realBannedIPs,
	})
}

type userRow struct {
	UserIdentifier  string                   `json:"user_identifier"`
	Username        string                   `json:"username,omitempty"`
	IPCount         int                      `json:"ip_count"`
	Limit           int                      `json:"limit"`
	Blocked         bool                     `json:"blocked"`
	Exceeds         bool                     `json:"exceeds"`
	LastNode        string                   `json:"last_node"`
	LastNodeDisplay string                   `json:"last_node_display"`
	LastSeenAt      time.Time                `json:"last_seen_at,omitempty"`
	Online          bool                     `json:"online"`
	ActiveIPs       []string                 `json:"active_ips"`
	Heuristics      *models.HeuristicMetrics `json:"heuristics,omitempty"`
	Sharing         *models.SharingStatus    `json:"sharing,omitempty"`
}

type trafficUserRow struct {
	UserIdentifier       string    `json:"user_identifier"`
	Username             string    `json:"username,omitempty"`
	Email                string    `json:"email,omitempty"`
	Tariff               string    `json:"tariff,omitempty"`
	Tag                  string    `json:"tag,omitempty"`
	Status               string    `json:"status"`
	DeviceLimit          int       `json:"device_limit"`
	ConnectedIPs         int       `json:"connected_ips"`
	Online               bool      `json:"online"`
	LastNode             string    `json:"last_node,omitempty"`
	LastNodeDisplay      string    `json:"last_node_display,omitempty"`
	TrafficLimitBytes    int64     `json:"traffic_limit_bytes"`
	UsedTrafficBytes     int64     `json:"used_traffic_bytes"`
	LifetimeUsedTraffic  int64     `json:"lifetime_used_traffic_bytes"`
	UsedPercent          float64   `json:"used_percent"`
	FirstConnectedAt     time.Time `json:"first_connected_at,omitempty"`
	OnlineAt             time.Time `json:"online_at,omitempty"`
	ExpireAt             time.Time `json:"expire_at,omitempty"`
	HWIDDevicesCount     int       `json:"hwid_devices_count"`
	ActiveInternalSquads []string  `json:"active_internal_squad_names,omitempty"`
}

func (s *Server) handleInternalTrafficUsers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	s.refreshPanelLimitsIfStale(ctx)

	selectedPeriod := strings.TrimSpace(c.Query("period"))
	if selectedPeriod == "" {
		selectedPeriod = "30d"
	}
	onlineOnly := true
	if raw := strings.TrimSpace(strings.ToLower(c.Query("online_only"))); raw != "" {
		onlineOnly = raw != "0" && raw != "false" && raw != "no"
	}

	if s.limitProvider == nil {
		c.JSON(http.StatusOK, gin.H{
			"rows": []trafficUserRow{},
			"summary": gin.H{
				"total_users":       0,
				"online_users":      0,
				"over_limit_users":  0,
				"total_used_bytes":  0,
				"total_limit_bytes": 0,
				"coverage_percent":  0.0,
			},
			"filters": gin.H{
				"tariffs":  []string{},
				"statuses": []string{},
				"nodes":    []string{},
			},
			"meta": gin.H{
				"generated_at":                 time.Now(),
				"selected_period":              selectedPeriod,
				"period_supported":             false,
				"per_node_breakdown_supported": false,
			},
		})
		return
	}

	profiles := s.limitProvider.ListUserProfiles()
	rows := make([]trafficUserRow, 0, len(profiles))
	tariffSet := make(map[string]struct{})
	statusSet := make(map[string]struct{})
	nodeSet := make(map[string]struct{})

	var totalUsedBytes int64
	var totalLimitBytes int64
	onlineUsers := 0
	overLimitUsers := 0

	for _, profile := range profiles {
		userIdentifier := strings.TrimSpace(profile.UserIdentifier)
		if userIdentifier == "" {
			userIdentifier = strings.TrimSpace(profile.Email)
		}
		if userIdentifier == "" {
			userIdentifier = strings.TrimSpace(profile.Username)
		}
		if userIdentifier == "" {
			userIdentifier = strings.TrimSpace(profile.UUID)
		}
		if userIdentifier == "" {
			continue
		}

		activeIPs, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
		if err != nil {
			activeIPs = map[string]int{}
		}
		connectedIPs := len(activeIPs)
		online := connectedIPs > 0
		if onlineOnly && !online {
			continue
		}

		status := normalizeTrafficStatus(profile.Status, online)
		lastNode := strings.TrimSpace(profile.LastConnectedNodeID)
		if lastNode == "" {
			lastNode = strings.TrimSpace(profile.LastConnectedNode)
		}
		lastNodeDisplay := strings.TrimSpace(s.resolveNodeDisplayName(lastNode, profile))
		if lastNodeDisplay == "" && strings.TrimSpace(profile.LastConnectedNode) != "" {
			lastNodeDisplay = strings.TrimSpace(profile.LastConnectedNode)
		}
		if lastNodeDisplay == "" {
			lastNodeDisplay = "unknown"
		}

		deviceLimit := profile.HWIDDeviceLimit
		if deviceLimit <= 0 {
			if limit, ok := s.limitProvider.GetUserLimit(userIdentifier); ok && limit > 0 {
				deviceLimit = limit
			}
		}
		if deviceLimit <= 0 {
			limit := s.processor.ResolveUserIPLimit(ctx, userIdentifier)
			if limit > 0 {
				deviceLimit = limit
			}
		}

		usedPercent := 0.0
		if profile.TrafficLimitBytes > 0 {
			usedPercent = float64(profile.UsedTrafficBytes) * 100 / float64(profile.TrafficLimitBytes)
		}

		row := trafficUserRow{
			UserIdentifier:       userIdentifier,
			Username:             strings.TrimSpace(profile.Username),
			Email:                strings.TrimSpace(profile.Email),
			Tariff:               strings.TrimSpace(profile.Tariff),
			Tag:                  strings.TrimSpace(profile.Tag),
			Status:               status,
			DeviceLimit:          deviceLimit,
			ConnectedIPs:         connectedIPs,
			Online:               online,
			LastNode:             lastNode,
			LastNodeDisplay:      lastNodeDisplay,
			TrafficLimitBytes:    profile.TrafficLimitBytes,
			UsedTrafficBytes:     profile.UsedTrafficBytes,
			LifetimeUsedTraffic:  profile.LifetimeUsedTrafficBytes,
			UsedPercent:          usedPercent,
			FirstConnectedAt:     profile.FirstConnectedAt,
			OnlineAt:             profile.OnlineAt,
			ExpireAt:             profile.ExpireAt,
			HWIDDevicesCount:     len(profile.HWIDDevices),
			ActiveInternalSquads: profile.ActiveInternalSquadNames,
		}
		rows = append(rows, row)

		if row.Tariff != "" {
			tariffSet[row.Tariff] = struct{}{}
		}
		if row.Status != "" {
			statusSet[row.Status] = struct{}{}
		}
		if row.LastNodeDisplay != "" {
			nodeSet[row.LastNodeDisplay] = struct{}{}
		}
		if row.Online {
			onlineUsers++
		}
		if row.TrafficLimitBytes > 0 {
			totalLimitBytes += row.TrafficLimitBytes
			if row.UsedTrafficBytes >= row.TrafficLimitBytes {
				overLimitUsers++
			}
		}
		if row.UsedTrafficBytes > 0 {
			totalUsedBytes += row.UsedTrafficBytes
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UsedPercent != rows[j].UsedPercent {
			return rows[i].UsedPercent > rows[j].UsedPercent
		}
		if rows[i].UsedTrafficBytes != rows[j].UsedTrafficBytes {
			return rows[i].UsedTrafficBytes > rows[j].UsedTrafficBytes
		}
		return strings.ToLower(rows[i].UserIdentifier) < strings.ToLower(rows[j].UserIdentifier)
	})

	coveragePercent := 0.0
	if totalLimitBytes > 0 {
		coveragePercent = float64(totalUsedBytes) * 100 / float64(totalLimitBytes)
	}

	c.JSON(http.StatusOK, gin.H{
		"rows": rows,
		"summary": gin.H{
			"total_users":       len(rows),
			"online_users":      onlineUsers,
			"over_limit_users":  overLimitUsers,
			"total_used_bytes":  totalUsedBytes,
			"total_limit_bytes": totalLimitBytes,
			"coverage_percent":  coveragePercent,
		},
		"filters": gin.H{
			"tariffs":  sortedSetValues(tariffSet),
			"statuses": sortedSetValues(statusSet),
			"nodes":    sortedSetValues(nodeSet),
		},
		"meta": gin.H{
			"generated_at":                 time.Now(),
			"selected_period":              selectedPeriod,
			"online_only":                  onlineOnly,
			"period_supported":             false,
			"per_node_breakdown_supported": false,
		},
	})
}

func (s *Server) handleInternalUsers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.refreshPanelLimitsIfStale(ctx)

	rowsLimit := 200
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			if parsed > 2000 {
				parsed = 2000
			}
			rowsLimit = parsed
		}
	}

	panelOnly := c.Query("panel_only") == "true"
	rows, err := s.buildUserRows(ctx, rowsLimit, panelOnly)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, rows)
}

func (s *Server) handleInternalUserDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rawID := c.Param("id")
	userIdentifier, _ := url.PathUnescape(rawID)
	userIdentifier = strings.TrimSpace(userIdentifier)
	if userIdentifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user id is required"})
		return
	}
	s.refreshPanelLimitsIfStale(ctx)

	activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	profile, hasProfile := panel.UserProfile{}, false
	if s.limitProvider != nil {
		profile, hasProfile = s.limitProvider.GetUserProfile(userIdentifier)
	}

	activeIPs := make([]models.ActiveIPInfo, 0, len(activeIPsMap))
	rawIPs := make([]string, 0, len(activeIPsMap))
	activeNodeSet := make(map[string]struct{})
	for ip := range activeIPsMap {
		rawIPs = append(rawIPs, ip)
	}
	sort.Strings(rawIPs)

	lastNode := s.processor.GetLastNode(userIdentifier)
	lastSeenAt, hasLastSeen := s.processor.GetLastSeenAt(userIdentifier)
	ipNodes := make(map[string]string)
	if len(rawIPs) > 0 {
		if storedNodes, nodesErr := s.storage.GetUserIPNodes(ctx, userIdentifier, rawIPs); nodesErr == nil {
			for ip, node := range storedNodes {
				ipNodes[ip] = node
			}
		}
	}
	for ip, node := range s.processor.GetUserIPNodes(userIdentifier) {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		ttlSeconds, isActive := activeIPsMap[ip]
		if !isActive {
			continue
		}
		stored := strings.TrimSpace(ipNodes[ip])
		useRuntime := stored == "" || isUnknownNode(stored)
		if !useRuntime {
			storedCanonical := s.canonicalNodeID(stored)
			runtimeCanonical := s.canonicalNodeID(node)
			if storedCanonical != "" && runtimeCanonical != "" && storedCanonical != runtimeCanonical {
				// Runtime-карта отражает фактический текущий поток по IP.
				// Если она расходится с Redis, обновляем Redis, чтобы UI не показывал "старую" ноду.
				useRuntime = true
			}
		}
		if !useRuntime {
			continue
		}

		ipNodes[ip] = node
		ttl := s.cfg.UserIPTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		if ttlSeconds > 0 {
			ttl = time.Duration(ttlSeconds) * time.Second
		}
		if err := s.storage.SetIPNode(ctx, userIdentifier, ip, node, ttl); err != nil {
			log.Printf("Не удалось синхронизировать runtime ip->node для %s (%s): %v", userIdentifier, ip, err)
		}
	}
	ipNodes = s.repairMissingUserIPNodes(ctx, userIdentifier, activeIPsMap, ipNodes, profile, lastNode, !hasLastSeen)

	for ip, ttl := range activeIPsMap {
		netInfo := s.classifyIP(ctx, ip)
		nodeName := strings.TrimSpace(ipNodes[ip])
		nodeDisplay := strings.TrimSpace(s.resolveNodeDisplayName(nodeName, profile))
		if nodeDisplay == "" && nodeName != "" {
			nodeDisplay = nodeName
		}
		if nodeDisplay != "" {
			activeNodeSet[nodeDisplay] = struct{}{}
		}
		activeIPs = append(activeIPs, models.ActiveIPInfo{
			IP:              ip,
			TTLSeconds:      ttl,
			Subnet24:        subnet24(ip),
			NodeName:        nodeName,
			NodeDisplay:     nodeDisplay,
			NetworkType:     netInfo.Type,
			NetworkProvider: netInfo.Provider,
			NetworkCountry:  netInfo.Country,
			NetworkOrg:      netInfo.Organization,
		})
	}
	sort.Slice(activeIPs, func(i, j int) bool { return activeIPs[i].IP < activeIPs[j].IP })

	activeNodes := make([]string, 0, len(activeNodeSet))
	for node := range activeNodeSet {
		activeNodes = append(activeNodes, node)
	}
	sort.Strings(activeNodes)
	if isUnknownNode(lastNode) {
		if recoveredNode := s.primaryNodeFromIPNodes(ipNodes); !isUnknownNode(recoveredNode) {
			lastNode = recoveredNode
		}
	}

	limit := s.processor.ResolveUserIPLimit(ctx, userIdentifier)
	metrics, hasMetrics := s.processor.GetHeuristicMetrics(userIdentifier)
	sharing, hasSharing := s.processor.GetSharingStatus(userIdentifier)
	exceeds := len(rawIPs) > limit
	if s.cfg.SharingDetectionEnabled {
		exceeds = hasSharing && sharing.Violator
	} else if hasSharing {
		exceeds = sharing.Violator
	}
	history := s.processor.GetUserHistory(userIdentifier, 10)
	fullHistory := s.processor.GetUserHistory(userIdentifier, 500)

	blockedEvents := s.processor.GetUserBlockCount(c.Request.Context(), userIdentifier)
	blockedNow := false
	for _, ipInfo := range activeIPs {
		if s.processor.IsIPActivelyBanned(ipInfo.IP) {
			blockedNow = true
			break
		}
	}
	if !blockedNow {
		for _, ip := range s.processor.GetLastBlockedIPs(userIdentifier) {
			if s.processor.IsIPActivelyBanned(ip) {
				blockedNow = true
				break
			}
		}
	}

	firstActivityAt := time.Time{}
	if len(fullHistory) > 0 {
		firstActivityAt = fullHistory[len(fullHistory)-1].Timestamp
	}
	lastActivityAt := time.Time{}
	if hasLastSeen {
		lastActivityAt = lastSeenAt
	} else if len(fullHistory) > 0 {
		lastActivityAt = fullHistory[0].Timestamp
	}
	baseAccount, keySlot := splitPerDeviceIdentifier(userIdentifier, s.cfg.PerDeviceKeyDelimiter)

	resp := gin.H{
		"user_identifier":   userIdentifier,
		"base_account":      baseAccount,
		"key_slot":          keySlot,
		"ip_count":          len(rawIPs),
		"limit":             limit,
		"exceeds":           exceeds,
		"last_node":         lastNode,
		"last_node_display": s.resolveNodeDisplayName(lastNode, profile),
		"online":            hasLastSeen && time.Since(lastSeenAt) <= onlinePresenceWindow,
		"active_ips":        activeIPs,
		"active_nodes":      activeNodes,
		"history":           history,
		"total_events":      len(fullHistory),
		"blocked_events":    blockedEvents,
		"blocked_now":       blockedNow,
	}
	if hasLastSeen {
		resp["last_seen_at"] = lastSeenAt
	}
	if !firstActivityAt.IsZero() {
		resp["first_activity_at"] = firstActivityAt
	}
	if !lastActivityAt.IsZero() {
		resp["last_activity_at"] = lastActivityAt
	}
	if hasProfile && strings.TrimSpace(profile.Username) != "" {
		resp["username"] = profile.Username
	}
	if hasProfile && strings.TrimSpace(profile.TelegramID) != "" {
		resp["telegram_id"] = profile.TelegramID
	}
	if hasProfile && strings.TrimSpace(profile.Description) != "" {
		resp["description"] = profile.Description
	}
	if hasProfile {
		resp["hwid_devices_count"] = len(profile.HWIDDevices)
		if len(profile.HWIDDevices) > 0 {
			resp["hwid_devices"] = profile.HWIDDevices
		}
	}
	// Устройства из Redis как дополнение к данным панели.
	if devs, err := s.storage.GetUserDeviceReports(c.Request.Context(), userIdentifier); err == nil && len(devs) > 0 {
		if !hasProfile || len(profile.HWIDDevices) == 0 {
			resp["hwid_devices"] = devs
			resp["hwid_devices_count"] = len(devs)
		}
	}
	if hasMetrics {
		resp["heuristics"] = metrics
	}
	if hasSharing {
		resp["sharing"] = sharing
	}

	c.JSON(http.StatusOK, resp)
}

func splitPerDeviceIdentifier(userIdentifier string, delimiter string) (string, string) {
	normalized := strings.TrimSpace(userIdentifier)
	if normalized == "" {
		return "", ""
	}
	delimiter = strings.TrimSpace(delimiter)
	if delimiter == "" {
		return normalized, ""
	}

	idx := strings.LastIndex(normalized, delimiter)
	if idx <= 0 {
		return normalized, ""
	}

	base := strings.TrimSpace(normalized[:idx])
	slot := strings.TrimSpace(normalized[idx+len(delimiter):])
	if base == "" || slot == "" {
		return normalized, ""
	}
	return base, slot
}

func (s *Server) refreshPanelLimitsIfStale(ctx context.Context) {
	if s.limitProvider == nil {
		return
	}
	if s.cfg.PanelOnDemandSyncMaxAge <= 0 {
		return
	}

	refresher, ok := s.limitProvider.(panelOnDemandRefresher)
	if !ok {
		return
	}

	refreshTimeout := 5 * time.Second
	if s.cfg.PanelOnDemandSyncMaxAge < refreshTimeout {
		refreshTimeout = s.cfg.PanelOnDemandSyncMaxAge
	}
	if refreshTimeout <= 0 {
		refreshTimeout = 5 * time.Second
	}

	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	refresher.RefreshIfStale(refreshCtx, s.cfg.PanelOnDemandSyncMaxAge)
}

func (s *Server) handleInternalNodes(c *gin.Context) {
	unique := make(map[string]struct{})
	rows := make([]gin.H, 0, 32)
	sshEnabled := s.nodeSSH != nil && s.nodeSSH.Enabled()

	if s.limitProvider != nil {
		for _, node := range s.limitProvider.ListNodes() {
			name := strings.TrimSpace(node.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := unique[key]; ok {
				continue
			}
			unique[key] = struct{}{}
			address := strings.TrimSpace(node.Address)
			rows = append(rows, gin.H{
				"id":          strings.TrimSpace(node.ID),
				"name":        name,
				"address":     address,
				"ssh_enabled": sshEnabled && address != "",
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i]["name"].(string)) < strings.ToLower(rows[j]["name"].(string))
	})
	c.JSON(http.StatusOK, rows)
}

type nodeSSHTarget struct {
	ID      string
	Name    string
	Address string
}

func (s *Server) resolveNodeSSHTargets(refs []string) ([]nodeSSHTarget, []gin.H) {
	if len(refs) == 0 {
		return nil, nil
	}

	index := make(map[string]panel.NodeProfile)
	if s.limitProvider != nil {
		for _, node := range s.limitProvider.ListNodes() {
			id := strings.TrimSpace(node.ID)
			name := strings.TrimSpace(node.Name)
			address := strings.TrimSpace(node.Address)

			if id != "" {
				index[strings.ToLower(id)] = node
			}
			if name != "" {
				index[strings.ToLower(name)] = node
			}
			if address != "" {
				index[strings.ToLower(address)] = node
				if normalized := s.nodeSSH.NormalizeAddress(address); normalized != "" {
					index[strings.ToLower(normalized)] = node
				}
			}
		}
	}

	seen := make(map[string]struct{})
	out := make([]nodeSSHTarget, 0, len(refs))
	unresolved := make([]gin.H, 0, len(refs))

	for _, raw := range refs {
		ref := strings.TrimSpace(raw)
		if ref == "" {
			continue
		}
		key := strings.ToLower(ref)
		if profile, ok := index[key]; ok {
			addr := strings.TrimSpace(profile.Address)
			if addr == "" {
				unresolved = append(unresolved, gin.H{"ref": ref, "error": "node has empty address"})
				continue
			}

			uniqueKey := strings.ToLower(strings.TrimSpace(profile.ID) + "|" + strings.TrimSpace(profile.Name) + "|" + addr)
			if _, exists := seen[uniqueKey]; exists {
				continue
			}
			seen[uniqueKey] = struct{}{}
			out = append(out, nodeSSHTarget{
				ID:      strings.TrimSpace(profile.ID),
				Name:    strings.TrimSpace(profile.Name),
				Address: addr,
			})
			continue
		}

		// Fallback: разрешаем передать адрес ноды напрямую.
		normalized := s.nodeSSH.NormalizeAddress(ref)
		if normalized != "" {
			if _, exists := seen[strings.ToLower(normalized)]; exists {
				continue
			}
			seen[strings.ToLower(normalized)] = struct{}{}
			out = append(out, nodeSSHTarget{
				ID:      "",
				Name:    ref,
				Address: ref,
			})
			continue
		}

		unresolved = append(unresolved, gin.H{"ref": ref, "error": "node not found"})
	}

	return out, unresolved
}

func (s *Server) handleNodeSSHExec(c *gin.Context) {
	if s.nodeSSH == nil || !s.nodeSSH.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "node ssh is disabled"})
		return
	}

	var req struct {
		Nodes          []string `json:"nodes"`
		Command        string   `json:"command"`
		TimeoutSeconds int      `json:"timeout_seconds"`
		ExecID         string   `json:"exec_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command is required"})
		return
	}
	if len(command) > 4000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command is too long"})
		return
	}
	if len(req.Nodes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one node is required"})
		return
	}
	execID := strings.TrimSpace(req.ExecID)
	if execID == "" {
		tokenRaw := make([]byte, 12)
		if _, err := rand.Read(tokenRaw); err == nil {
			execID = hex.EncodeToString(tokenRaw)
		}
	}
	if execID == "" {
		execID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}

	targets, unresolved := s.resolveNodeSSHTargets(req.Nodes)
	if len(targets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      "no resolvable nodes with ssh address",
			"unresolved": unresolved,
		})
		return
	}

	timeout := s.cfg.NodeSSHCommandTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}

	maxParallel := s.cfg.NodeSSHMaxParallel
	if maxParallel <= 0 {
		maxParallel = 5
	}

	type nodeResult struct {
		NodeID     string `json:"node_id,omitempty"`
		NodeName   string `json:"node_name"`
		Address    string `json:"address"`
		Normalized string `json:"normalized_address"`
		OK         bool   `json:"ok"`
		ExitCode   int    `json:"exit_code"`
		DurationMS int64  `json:"duration_ms"`
		Stdout     string `json:"stdout,omitempty"`
		Stderr     string `json:"stderr,omitempty"`
		Error      string `json:"error,omitempty"`
	}

	results := make([]nodeResult, len(targets))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for i, target := range targets {
		i := i
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			nodeKey := strings.TrimSpace(target.ID)
			if nodeKey == "" {
				nodeKey = strings.TrimSpace(target.Name)
			}
			if nodeKey == "" {
				nodeKey = strings.TrimSpace(target.Address)
			}

			s.wsHub.BroadcastJSON(gin.H{
				"type":      "node_ssh_stream",
				"exec_id":   execID,
				"node_key":  nodeKey,
				"node_id":   target.ID,
				"node_name": target.Name,
				"stream":    "system",
				"line":      "starting command...",
				"ts":        time.Now().UTC().Format(time.RFC3339Nano),
			})

			var execRes nodessh.ExecResult
			// Ищем per-node credentials (по ID, потом по имени).
			credKey := target.ID
			if credKey == "" {
				credKey = target.Name
			}
			creds, hasCreds, _ := s.storage.GetNodeSSHCreds(c.Request.Context(), credKey)
			streamCb := func(chunk nodessh.StreamChunk) {
				s.wsHub.BroadcastJSON(gin.H{
					"type":      "node_ssh_stream",
					"exec_id":   execID,
					"node_key":  nodeKey,
					"node_id":   target.ID,
					"node_name": target.Name,
					"stream":    chunk.Stream,
					"line":      chunk.Line,
					"ts":        time.Now().UTC().Format(time.RFC3339Nano),
				})
			}
			if hasCreds && strings.TrimSpace(creds.User) != "" {
				if strings.TrimSpace(creds.Password) != "" || strings.TrimSpace(creds.PrivateKey) != "" {
					execRes = s.nodeSSH.ExecuteWithCredentialsAndPrivateKeyStreaming(
						c.Request.Context(),
						target.Address,
						command,
						creds.User,
						creds.Password,
						creds.PrivateKey,
						timeout,
						streamCb,
					)
				} else {
					// Per-node user without explicit auth data: reuse global auth methods.
					execRes = s.nodeSSH.ExecuteWithUserStreaming(c.Request.Context(), target.Address, command, creds.User, timeout, streamCb)
				}
			} else {
				execRes = s.nodeSSH.ExecuteStreaming(c.Request.Context(), target.Address, command, timeout, streamCb)
			}

			s.wsHub.BroadcastJSON(gin.H{
				"type":        "node_ssh_stream_done",
				"exec_id":     execID,
				"node_key":    nodeKey,
				"node_id":     target.ID,
				"node_name":   target.Name,
				"ok":          execRes.Error == "" && execRes.ExitCode == 0,
				"exit_code":   execRes.ExitCode,
				"duration_ms": execRes.Duration.Milliseconds(),
				"error":       execRes.Error,
				"ts":          time.Now().UTC().Format(time.RFC3339Nano),
			})

			results[i] = nodeResult{
				NodeID:     target.ID,
				NodeName:   target.Name,
				Address:    target.Address,
				Normalized: s.nodeSSH.NormalizeAddress(target.Address),
				OK:         execRes.Error == "" && execRes.ExitCode == 0,
				ExitCode:   execRes.ExitCode,
				DurationMS: execRes.Duration.Milliseconds(),
				Stdout:     execRes.Stdout,
				Stderr:     execRes.Stderr,
				Error:      execRes.Error,
			}
		}()
	}
	wg.Wait()

	success := 0
	failed := 0
	for _, r := range results {
		if r.OK {
			success++
		} else {
			failed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"exec_id":         execID,
		"command":         command,
		"timeout_seconds": int(timeout.Seconds()),
		"max_parallel":    maxParallel,
		"results":         results,
		"resolved_count":  len(results),
		"success_count":   success,
		"failed_count":    failed,
		"unresolved":      unresolved,
	})
}

// handleNodeSSHListCreds возвращает список нод с сохранёнными SSH-учётными данными (без паролей).
func (s *Server) handleNodeSSHListCreds(c *gin.Context) {
	all, err := s.storage.ListNodeSSHCreds(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	items := make([]gin.H, 0, len(all))
	for key, creds := range all {
		items = append(items, gin.H{
			"node_key":        key,
			"user":            creds.User,
			"has_password":    creds.Password != "",
			"has_private_key": strings.TrimSpace(creds.PrivateKey) != "",
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i]["node_key"].(string)) < strings.ToLower(items[j]["node_key"].(string))
	})
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleNodeSSHSaveCreds сохраняет SSH-учётные данные для ноды.
func (s *Server) handleNodeSSHSaveCreds(c *gin.Context) {
	var req struct {
		NodeKey    string `json:"node_key"`
		User       string `json:"user"`
		Password   string `json:"password"`
		PrivateKey string `json:"private_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.NodeKey = strings.TrimSpace(req.NodeKey)
	req.User = strings.TrimSpace(req.User)
	req.PrivateKey = strings.TrimSpace(req.PrivateKey)
	if req.NodeKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_key is required"})
		return
	}
	if req.User == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
		return
	}

	existing, found, _ := s.storage.GetNodeSSHCreds(c.Request.Context(), req.NodeKey)
	creds := storage.NodeSSHCreds{
		User:       req.User,
		Password:   req.Password,
		PrivateKey: req.PrivateKey,
	}
	// For updates, empty fields mean "keep existing".
	if found {
		if creds.Password == "" {
			creds.Password = existing.Password
		}
		if creds.PrivateKey == "" {
			creds.PrivateKey = existing.PrivateKey
		}
	}

	if err := s.storage.SetNodeSSHCreds(c.Request.Context(), req.NodeKey, creds); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleNodeSSHDeleteCreds удаляет SSH-учётные данные ноды.
func (s *Server) handleNodeSSHDeleteCreds(c *gin.Context) {
	var req struct {
		NodeKey string `json:"node_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.NodeKey = strings.TrimSpace(req.NodeKey)
	if req.NodeKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_key is required"})
		return
	}
	if err := s.storage.DeleteNodeSSHCreds(c.Request.Context(), req.NodeKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handleInternalLogs(c *gin.Context) {
	rowsLimit := 500
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			if parsed > 3000 {
				parsed = 3000
			}
			rowsLimit = parsed
		}
	}

	logs := s.processor.GetGlobalHistory(rowsLimit)
	c.JSON(http.StatusOK, logs)
}

func (s *Server) handleInternalActiveBans(c *gin.Context) {
	rowsLimit := 500
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			if parsed > 5000 {
				parsed = 5000
			}
			rowsLimit = parsed
		}
	}

	bans := s.processor.GetActiveBans(rowsLimit)
	if len(bans) == 0 {
		c.JSON(http.StatusOK, []gin.H{})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	emails, err := s.storage.GetAllUserEmails(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ipUsers := make(map[string][]string, len(bans))
	banSet := make(map[string]struct{}, len(bans))
	for _, b := range bans {
		banSet[b.IP] = struct{}{}
	}
	for _, email := range emails {
		activeIPs, err := s.storage.GetUserActiveIPs(ctx, email)
		if err != nil || len(activeIPs) == 0 {
			continue
		}
		for ip := range activeIPs {
			if _, ok := banSet[ip]; ok {
				ipUsers[ip] = append(ipUsers[ip], email)
			}
		}
	}

	rows := make([]gin.H, 0, len(bans))
	for _, ban := range bans {
		rows = append(rows, gin.H{
			"ip":           ban.IP,
			"node_name":    ban.NodeName,
			"duration":     ban.Duration,
			"set_at":       ban.SetAt,
			"expires_at":   ban.ExpiresAt,
			"seconds_left": ban.SecondsLeft,
			"users":        ipUsers[ban.IP],
		})
	}

	c.JSON(http.StatusOK, rows)
}

func (s *Server) handleInternalSharingBanlist(c *gin.Context) {
	rowsLimit := 500
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			if parsed > 5000 {
				parsed = 5000
			}
			rowsLimit = parsed
		}
	}

	statuses := s.processor.ListSharingStatuses(5000)
	if len(statuses) == 0 {
		c.JSON(http.StatusOK, []gin.H{})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows := make([]gin.H, 0, rowsLimit)
	for _, st := range statuses {
		if !st.InBanlist {
			continue
		}
		userIdentifier := strings.TrimSpace(st.UserIdentifier)
		if userIdentifier == "" {
			continue
		}

		activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
		if err != nil {
			activeIPsMap = map[string]int{}
		}
		ipCount := len(activeIPsMap)
		limit := s.processor.ResolveUserIPLimit(ctx, userIdentifier)

		profile, hasProfile := panel.UserProfile{}, false
		if s.limitProvider != nil {
			profile, hasProfile = s.limitProvider.GetUserProfile(userIdentifier)
		}
		lastNode := s.processor.GetLastNode(userIdentifier)
		nodeDisplay := s.resolveNodeDisplayName(lastNode, profile)

		row := gin.H{
			"user_identifier":     userIdentifier,
			"ip_count":            ipCount,
			"limit":               limit,
			"last_node":           lastNode,
			"last_node_display":   nodeDisplay,
			"violator_since":      st.ViolatorSince,
			"banlist_since":       st.BanlistSince,
			"permanent_ban":       st.PermanentBan,
			"permanent_ban_since": st.PermanentBanSince,
			"duration_seconds":    st.ViolationDurationSec,
			"violation_ips":       st.ViolationIPs,
			"violation_nodes":     st.ViolationNodes,
			"trigger_hits":        st.TriggerHits,
			"trigger_required":    st.TriggerRequired,
		}
		if hasProfile && strings.TrimSpace(profile.Username) != "" {
			row["username"] = profile.Username
		}
		if hasProfile && strings.TrimSpace(profile.TelegramID) != "" {
			row["telegram_id"] = profile.TelegramID
		}
		rows = append(rows, row)
		if len(rows) >= rowsLimit {
			break
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		left, _ := rows[i]["banlist_since"].(time.Time)
		right, _ := rows[j]["banlist_since"].(time.Time)
		return left.After(right)
	})

	c.JSON(http.StatusOK, rows)
}

func (s *Server) handleInternalSharingPermanent(c *gin.Context) {
	rowsLimit := 500
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			if parsed > 5000 {
				parsed = 5000
			}
			rowsLimit = parsed
		}
	}

	statuses := s.processor.ListSharingStatuses(5000)
	if len(statuses) == 0 {
		c.JSON(http.StatusOK, []gin.H{})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows := make([]gin.H, 0, rowsLimit)
	for _, st := range statuses {
		if !st.PermanentBan {
			continue
		}
		userIdentifier := strings.TrimSpace(st.UserIdentifier)
		if userIdentifier == "" {
			continue
		}

		activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
		if err != nil {
			activeIPsMap = map[string]int{}
		}
		ipCount := len(activeIPsMap)
		limit := s.processor.ResolveUserIPLimit(ctx, userIdentifier)

		profile, hasProfile := panel.UserProfile{}, false
		if s.limitProvider != nil {
			profile, hasProfile = s.limitProvider.GetUserProfile(userIdentifier)
		}
		lastNode := s.processor.GetLastNode(userIdentifier)
		nodeDisplay := s.resolveNodeDisplayName(lastNode, profile)

		row := gin.H{
			"user_identifier":     userIdentifier,
			"ip_count":            ipCount,
			"limit":               limit,
			"last_node":           lastNode,
			"last_node_display":   nodeDisplay,
			"violator":            st.Violator,
			"violator_since":      st.ViolatorSince,
			"permanent_ban":       true,
			"permanent_ban_since": st.PermanentBanSince,
			"duration_seconds":    st.ViolationDurationSec,
			"violation_ips":       st.ViolationIPs,
			"violation_nodes":     st.ViolationNodes,
			"trigger_hits":        st.TriggerHits,
			"trigger_required":    st.TriggerRequired,
		}
		if hasProfile && strings.TrimSpace(profile.Username) != "" {
			row["username"] = profile.Username
		}
		if hasProfile && strings.TrimSpace(profile.TelegramID) != "" {
			row["telegram_id"] = profile.TelegramID
		}
		rows = append(rows, row)
		if len(rows) >= rowsLimit {
			break
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		left, _ := rows[i]["permanent_ban_since"].(time.Time)
		right, _ := rows[j]["permanent_ban_since"].(time.Time)
		return left.After(right)
	})

	c.JSON(http.StatusOK, rows)
}

func (s *Server) handleInternalNetworkTypes(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	emails, err := s.storage.GetAllUserEmails(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type statsRow struct {
		Type      string
		Users     map[string]struct{}
		IPCount   int
		Providers map[string]int
		Examples  []string
	}

	rowsByType := map[string]*statsRow{
		"mobile":  {Type: "mobile", Users: make(map[string]struct{}), Providers: make(map[string]int)},
		"wifi":    {Type: "wifi", Users: make(map[string]struct{}), Providers: make(map[string]int)},
		"cable":   {Type: "cable", Users: make(map[string]struct{}), Providers: make(map[string]int)},
		"unknown": {Type: "unknown", Users: make(map[string]struct{}), Providers: make(map[string]int)},
	}

	for _, email := range emails {
		activeIPs, err := s.storage.GetUserActiveIPs(ctx, email)
		if err != nil || len(activeIPs) == 0 {
			continue
		}

		for ip := range activeIPs {
			info := s.classifyIP(ctx, ip)
			netType := normalizeNetworkType(info.Type)
			row := rowsByType[netType]
			row.Users[email] = struct{}{}
			row.IPCount++

			provider := strings.TrimSpace(info.Provider)
			if provider != "" {
				row.Providers[provider]++
			}
			if len(row.Examples) < 8 {
				row.Examples = append(row.Examples, ip)
			}
		}
	}

	order := []string{"mobile", "wifi", "cable", "unknown"}
	out := make([]gin.H, 0, len(order))
	for _, tpe := range order {
		row := rowsByType[tpe]
		if row == nil || row.IPCount == 0 {
			continue
		}
		out = append(out, gin.H{
			"network_type": tpe,
			"users_count":  len(row.Users),
			"ips_count":    row.IPCount,
			"providers":    topProviders(row.Providers, 5),
			"examples":     row.Examples,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		li, _ := out[i]["users_count"].(int)
		lj, _ := out[j]["users_count"].(int)
		if li == lj {
			return out[i]["network_type"].(string) < out[j]["network_type"].(string)
		}
		return li > lj
	})

	c.JSON(http.StatusOK, out)
}

func (s *Server) handleInternalNodeHealth(c *gin.Context) {
	runtimeStates := s.processor.GetNodeRuntimeStates()

	now := time.Now()
	timeout := s.cfg.NodeHeartbeatTimeout
	rows := make([]models.NodeHealthInfo, 0, 32)

	if s.limitProvider != nil {
		for _, node := range s.limitProvider.ListNodes() {
			id := strings.TrimSpace(node.ID)
			name := strings.TrimSpace(node.Name)
			if id == "" {
				continue
			}

			// Ищем runtime-состояние: сначала по UUID, затем по display name,
			// затем регистронезависимо. Это покрывает случай когда NODE_NAME на
			// агенте равен display name, а не UUID ноды.
			state, ok := runtimeStates[id]
			if !ok {
				state, ok = runtimeStates[name]
			}
			if !ok {
				nameLower := strings.ToLower(name)
				for k, st := range runtimeStates {
					if strings.ToLower(k) == nameLower {
						state = st
						break
					}
				}
			}

			lastSeen := maxTime(state.LastTrafficAt, state.LastBlockerBeatAt)
			online := !lastSeen.IsZero() && lastSeen.After(now.Add(-timeout))
			displayName := name
			if displayName == "" {
				displayName = id
			}

			rows = append(rows, models.NodeHealthInfo{
				NodeID:              id,
				NodeName:            displayName,
				Status:              map[bool]string{true: "online", false: "offline"}[online],
				Online:              online,
				LastTrafficAt:       state.LastTrafficAt,
				LastBlockerBeatAt:   state.LastBlockerBeatAt,
				LastBlockerActionAt: state.LastBlockerActionAt,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Online != rows[j].Online {
			return rows[i].Online
		}
		return strings.ToLower(rows[i].NodeName) < strings.ToLower(rows[j].NodeName)
	})
	c.JSON(http.StatusOK, rows)
}

func (s *Server) handleManualBlock(c *gin.Context) {
	var req struct {
		UserIdentifier string `json:"user_identifier"`
		Duration       string `json:"duration"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userIdentifier := strings.TrimSpace(req.UserIdentifier)
	if userIdentifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_identifier is required"})
		return
	}

	duration := strings.TrimSpace(req.Duration)
	if duration == "" {
		duration = s.cfg.BlockDuration
	}
	if !durationPattern.MatchString(duration) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid duration format"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var ipSource string
	ips := make([]string, 0, 8)
	if len(activeIPsMap) > 0 {
		ipSource = "active"
		for ip := range activeIPsMap {
			if !s.cfg.ExcludedIPs[ip] {
				ips = append(ips, ip)
			}
		}
	} else {
		// Пользователь офлайн — используем последние заблокированные IP из памяти процессора
		ipSource = "last_blocked"
		for _, ip := range s.processor.GetLastBlockedIPs(userIdentifier) {
			if !s.cfg.ExcludedIPs[ip] {
				ips = append(ips, ip)
			}
		}
	}

	if len(ips) == 0 {
		hint := "нет активных IP. Дождитесь подключения пользователя."
		if ipSource == "last_blocked" {
			hint = "нет активных IP и нет данных о последних блокировках (пользователь не подключался или сервис перезапускался)."
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": hint})
		return
	}

	if err := s.blocker.BlockIPs(ips, duration); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.blocker.BlockUser(userIdentifier, duration); err != nil {
		log.Printf("Manual block user %s failed: %v", userIdentifier, err)
		// Don't fail the request if just user block failed but IPs blocked?
		// Or maybe fail? Let's log.
	}

	s.processor.RecordBlockedIPs(userIdentifier, ips)

	msg := fmt.Sprintf("Ручная блокировка: %d IP, duration=%s", len(ips), duration)
	reason := strings.TrimSpace(req.Reason)
	if reason != "" {
		msg += ", reason=" + reason
	}
	s.processor.RecordManualEvent(userIdentifier, "manual_block", msg)
	if s.notifier != nil {
		netType := s.detectDominantNetworkType(ctx, ips)
		userProfile := panel.UserProfile{}
		if s.limitProvider != nil {
			if p, ok := s.limitProvider.GetUserProfile(userIdentifier); ok {
				userProfile = p
			}
		}
		alertPayload := models.AlertPayload{
			UserIdentifier:   userIdentifier,
			Username:         s.resolveAlertUsername(userIdentifier),
			DetectedIPsCount: len(ips),
			Limit:            s.processor.ResolveUserIPLimit(ctx, userIdentifier),
			AllUserIPs:       ips,
			BlockDuration:    duration,
			ViolationType:    "manual_block",
			Reason:           reason,
			NodeName:         s.resolveNodeDisplayName(s.processor.GetLastNode(userIdentifier), userProfile),
			NetworkType:      netType,
		}
		if err := s.notifier.SendAlert(alertPayload); err != nil {
			log.Printf("Ошибка отправки webhook для ручной блокировки (%s): %v", userIdentifier, err)
		}
	}

	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"user":        userIdentifier,
		"blocked_ips": len(ips),
		"duration":    duration,
	})
}

func (s *Server) handleManualUnblock(c *gin.Context) {
	var req struct {
		UserIdentifier string   `json:"user_identifier"`
		IPs            []string `json:"ips"`
		Reason         string   `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userIdentifier := strings.TrimSpace(req.UserIdentifier)
	if userIdentifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_identifier is required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var ips []string
	if len(req.IPs) > 0 {
		ips = uniqueSortedIPs(req.IPs)
	} else {
		activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, userIdentifier)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for ip := range activeIPsMap {
			ips = append(ips, ip)
		}
		ips = uniqueSortedIPs(ips)
	}

	if len(ips) == 0 {
		ips = s.processor.GetLastBlockedIPs(userIdentifier)
	}
	if len(ips) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no IPs available for unblocking"})
		return
	}

	if err := s.blocker.UnblockIPs(ips); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.blocker.UnblockUser(userIdentifier); err != nil {
		log.Printf("Manual unblock user %s failed: %v", userIdentifier, err)
	}

	msg := fmt.Sprintf("Ручная разблокировка: %d IP", len(ips))
	if strings.TrimSpace(req.Reason) != "" {
		msg += ", reason=" + strings.TrimSpace(req.Reason)
	}
	s.processor.RecordManualEvent(userIdentifier, "manual_unblock", msg)
	s.processor.ResetSharingState(userIdentifier, "manual_unblock")
	if s.notifier != nil {
		alertPayload := models.AlertPayload{
			UserIdentifier: userIdentifier,
			Username:       s.resolveAlertUsername(userIdentifier),
			ViolationType:  "enabled",
			NodeName:       s.processor.GetLastNode(userIdentifier),
		}
		if err := s.notifier.SendAlert(alertPayload); err != nil {
			log.Printf("Ошибка отправки webhook для ручной разблокировки (%s): %v", userIdentifier, err)
		}
	}

	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{
		"status":        "ok",
		"user":          userIdentifier,
		"unblocked_ips": len(ips),
	})
}

func (s *Server) handleManualClear(c *gin.Context) {
	var req struct {
		UserIdentifier string `json:"user_identifier"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	userIdentifier := strings.TrimSpace(req.UserIdentifier)
	if userIdentifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_identifier is required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cleared, err := s.storage.ClearUserIPs(ctx, userIdentifier)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	msg := fmt.Sprintf("Ручная очистка IP, удалено ключей: %d", cleared)
	if strings.TrimSpace(req.Reason) != "" {
		msg += ", reason=" + strings.TrimSpace(req.Reason)
	}
	s.processor.RecordManualEvent(userIdentifier, "manual_clear", msg)
	s.processor.ResetSharingState(userIdentifier, "manual_clear")

	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"user":         userIdentifier,
		"cleared_keys": cleared,
	})
}

func (s *Server) buildUserRows(ctx context.Context, rowsLimit int, panelOnly bool) ([]userRow, error) {
	emails, err := s.storage.GetAllUserEmails(ctx)
	if err != nil {
		return nil, err
	}
	activeBans := s.processor.GetActiveBans(10000)
	activeBanSet := make(map[string]struct{}, len(activeBans))
	for _, b := range activeBans {
		ip := strings.TrimSpace(b.IP)
		if ip != "" {
			activeBanSet[ip] = struct{}{}
		}
	}

	rows := make([]userRow, 0, rowsLimit)
	for _, email := range emails {
		if len(rows) >= rowsLimit {
			break
		}
		// In panel_only mode, skip users not in Remnawave panel
		if panelOnly && s.limitProvider != nil {
			if _, has := s.limitProvider.GetUserProfile(email); !has {
				continue
			}
		}

		activeIPsMap, err := s.storage.GetUserActiveIPs(ctx, email)
		if err != nil {
			continue
		}
		lastBlockedIPs := s.processor.GetLastBlockedIPs(email)
		blockedNow := false
		for ip := range activeIPsMap {
			if _, ok := activeBanSet[ip]; ok {
				blockedNow = true
				break
			}
		}
		if !blockedNow {
			for _, ip := range lastBlockedIPs {
				if _, ok := activeBanSet[strings.TrimSpace(ip)]; ok {
					blockedNow = true
					break
				}
			}
		}
		if len(activeIPsMap) == 0 && !blockedNow {
			continue
		}

		activeIPs := make([]string, 0, len(activeIPsMap))
		for ip := range activeIPsMap {
			activeIPs = append(activeIPs, ip)
		}
		// После очистки IP при блокировке пользователь может временно не иметь active_ips.
		// В этом случае показываем IP из последней блокировки, чтобы не "терялся" из таблицы.
		if len(activeIPs) == 0 && blockedNow {
			for _, ip := range lastBlockedIPs {
				ip = strings.TrimSpace(ip)
				if ip == "" {
					continue
				}
				if _, ok := activeBanSet[ip]; ok {
					activeIPs = append(activeIPs, ip)
				}
			}
		}
		sort.Strings(activeIPs)

		ipCount := len(activeIPs)
		limit := s.processor.ResolveUserIPLimit(ctx, email)
		sharingStatus, hasSharingStatus := s.processor.GetSharingStatus(email)
		exceeds := ipCount > limit
		if s.cfg.SharingDetectionEnabled {
			exceeds = hasSharingStatus && sharingStatus.Violator
		} else if hasSharingStatus {
			exceeds = sharingStatus.Violator
		}
		lastSeenAt, hasLastSeen := s.processor.GetLastSeenAt(email)
		profile, hasProfile := panel.UserProfile{}, false
		if s.limitProvider != nil {
			profile, hasProfile = s.limitProvider.GetUserProfile(email)
		}

		lastNode := s.processor.GetLastNode(email)
		// Для списка пользователей используем только быстрые источники
		// (без тяжелых глобальных SCAN по Redis), чтобы панель не подвисала.
		if isUnknownNode(lastNode) {
			ipNodes := make(map[string]string)
			if storedNodes, nodesErr := s.storage.GetUserIPNodes(ctx, email, activeIPs); nodesErr == nil {
				for ip, node := range storedNodes {
					ipNodes[ip] = node
				}
			}
			for ip, node := range s.processor.GetUserIPNodes(email) {
				if strings.TrimSpace(ipNodes[ip]) == "" {
					ipNodes[ip] = node
				}
			}
			if recoveredNode := s.primaryNodeFromIPNodes(ipNodes); !isUnknownNode(recoveredNode) {
				lastNode = recoveredNode
			}
		}

		row := userRow{
			UserIdentifier:  email,
			Username:        "",
			IPCount:         ipCount,
			Limit:           limit,
			Blocked:         blockedNow,
			Exceeds:         exceeds,
			LastNode:        lastNode,
			LastNodeDisplay: s.resolveNodeDisplayName(lastNode, profile),
			LastSeenAt:      lastSeenAt,
			Online:          hasLastSeen && time.Since(lastSeenAt) <= onlinePresenceWindow,
			ActiveIPs:       activeIPs,
		}
		if !hasLastSeen {
			row.LastSeenAt = time.Time{}
		}
		if hasProfile && strings.TrimSpace(profile.Username) != "" {
			row.Username = profile.Username
		}

		if metrics, ok := s.processor.GetHeuristicMetrics(email); ok {
			row.Heuristics = &metrics
		}
		if hasSharingStatus {
			row.Sharing = &sharingStatus
		}

		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Blocked != rows[j].Blocked {
			return rows[i].Blocked
		}
		if rows[i].Exceeds != rows[j].Exceeds {
			return rows[i].Exceeds
		}
		if rows[i].IPCount != rows[j].IPCount {
			return rows[i].IPCount > rows[j].IPCount
		}
		return rows[i].UserIdentifier < rows[j].UserIdentifier
	})

	return rows, nil
}

func isUnknownNode(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "unknown", "-", "n/a", "none", "null":
		return true
	default:
		return false
	}
}

func normalizeNodeToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	raw = strings.ReplaceAll(raw, " ", "")
	return raw
}

func (s *Server) canonicalNodeID(raw string) string {
	raw = strings.TrimSpace(raw)
	if isUnknownNode(raw) {
		return ""
	}
	if s.limitProvider == nil {
		return raw
	}

	token := normalizeNodeToken(raw)
	nodes := s.limitProvider.ListNodes()
	for _, node := range nodes {
		nodeID := strings.TrimSpace(node.ID)
		nodeName := strings.TrimSpace(node.Name)
		if nodeID != "" && normalizeNodeToken(nodeID) == token {
			return nodeID
		}
		if nodeID != "" && nodeName != "" && normalizeNodeToken(nodeName) == token {
			return nodeID
		}
	}

	if display, ok := s.limitProvider.GetNodeDisplayName(raw); ok {
		displayToken := normalizeNodeToken(display)
		for _, node := range nodes {
			nodeID := strings.TrimSpace(node.ID)
			nodeName := strings.TrimSpace(node.Name)
			if nodeID == "" || nodeName == "" {
				continue
			}
			if normalizeNodeToken(nodeName) == displayToken {
				return nodeID
			}
		}
	}

	return ""
}

func (s *Server) primaryNodeFromIPNodes(ipNodes map[string]string) string {
	if len(ipNodes) == 0 {
		return "unknown"
	}

	unique := make(map[string]struct{})
	for _, rawNode := range ipNodes {
		nodeID := s.canonicalNodeID(rawNode)
		if nodeID == "" {
			continue
		}
		unique[nodeID] = struct{}{}
	}
	if len(unique) != 1 {
		return "unknown"
	}
	for nodeID := range unique {
		return nodeID
	}
	return "unknown"
}

func (s *Server) lookupNodeByGlobalIP(ctx context.Context, ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}

	finder, ok := s.storage.(ipNodeCandidatesFinder)
	if !ok {
		return ""
	}

	candidates, err := finder.FindNodeCandidatesByIP(ctx, ip, 12)
	if err != nil || len(candidates) == 0 {
		return ""
	}

	unique := make(map[string]struct{})
	for _, raw := range candidates {
		nodeID := s.canonicalNodeID(raw)
		if nodeID == "" {
			continue
		}
		unique[nodeID] = struct{}{}
	}
	if len(unique) != 1 {
		return ""
	}
	for nodeID := range unique {
		return nodeID
	}
	return ""
}

func subnet24Prefix(ip string) string {
	parts := strings.Split(strings.TrimSpace(ip), ".")
	if len(parts) != 4 {
		return ""
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[0] + "." + parts[1] + "." + parts[2] + "."
}

func (s *Server) lookupNodeByGlobalSubnet24(ctx context.Context, ip string) string {
	prefix := subnet24Prefix(ip)
	if prefix == "" {
		return ""
	}

	finder, ok := s.storage.(ipNodeSubnetCandidatesFinder)
	if !ok {
		return ""
	}

	candidates, err := finder.FindNodeCandidatesBySubnet24(ctx, prefix, 16)
	if err != nil || len(candidates) == 0 {
		return ""
	}

	unique := make(map[string]struct{})
	for _, raw := range candidates {
		nodeID := s.canonicalNodeID(raw)
		if nodeID == "" {
			continue
		}
		unique[nodeID] = struct{}{}
	}
	if len(unique) != 1 {
		return ""
	}
	for nodeID := range unique {
		return nodeID
	}
	return ""
}

// repairMissingUserIPNodes пытается автоматически восстановить привязки ip->node.
// Использует только безопасные источники (однозначно известная нода), чтобы не
// "угадывать" ноду при параллельной работе через несколько нод.
func (s *Server) repairMissingUserIPNodes(
	ctx context.Context,
	userIdentifier string,
	activeIPs map[string]int,
	ipNodes map[string]string,
	profile panel.UserProfile,
	lastNodeHint string,
	allowProfileFallback bool,
) map[string]string {
	if len(activeIPs) == 0 {
		return ipNodes
	}
	if ipNodes == nil {
		ipNodes = make(map[string]string)
	}

	missingIPs := make([]string, 0, len(activeIPs))
	for ip := range activeIPs {
		if isUnknownNode(ipNodes[ip]) {
			missingIPs = append(missingIPs, ip)
		}
	}
	if len(missingIPs) == 0 {
		return ipNodes
	}

	// 1) Безопасная автопочинка для каждого IP независимо от online/offline:
	// сначала по точному IP, затем по /24 (только если нода определяется однозначно).
	// Это позволяет восстановить БС-ноды и не подменять их "последней нодой" пользователя.
	for _, ip := range missingIPs {
		guessed := s.lookupNodeByGlobalIP(ctx, ip)
		if guessed == "" {
			guessed = s.lookupNodeByGlobalSubnet24(ctx, ip)
		}
		if guessed == "" {
			continue
		}

		ttl := s.cfg.UserIPTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		if ttlSeconds, ok := activeIPs[ip]; ok && ttlSeconds > 0 {
			ttl = time.Duration(ttlSeconds) * time.Second
		}

		ipNodes[ip] = guessed
		if err := s.storage.SetIPNode(ctx, userIdentifier, ip, guessed, ttl); err != nil {
			log.Printf("Автопочинка ip->node (global match) не удалась для %s (%s): %v", userIdentifier, ip, err)
		}
	}

	// Пересчитываем список пропусков после безопасной поключевой автопочинки.
	missingIPs = missingIPs[:0]
	for ip := range activeIPs {
		if isUnknownNode(ipNodes[ip]) {
			missingIPs = append(missingIPs, ip)
		}
	}
	if len(missingIPs) == 0 {
		return ipNodes
	}

	candidateNodeID := s.primaryNodeFromIPNodes(ipNodes)
	if isUnknownNode(candidateNodeID) {
		candidateNodeID = s.canonicalNodeID(lastNodeHint)
	}
	// Для оффлайн-пользователей допускаем fallback из профиля панели:
	// это убирает "unknown" у "холодных" карточек после рестарта.
	if isUnknownNode(candidateNodeID) && allowProfileFallback {
		candidateNodeID = s.canonicalNodeID(profile.LastConnectedNode)
	}
	if isUnknownNode(candidateNodeID) {
		return ipNodes
	}

	// 2) Массовый fallback применяем только для единственного активного IP.
	// Если активных IP несколько, нельзя безопасно "назначить всем одну ноду":
	// это как раз приводит к ложной подмене БС-нод на "Латвия/Германия".
	if len(activeIPs) > 1 {
		return ipNodes
	}

	for _, ip := range missingIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}

		ttl := s.cfg.UserIPTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		if ttlSeconds, ok := activeIPs[ip]; ok && ttlSeconds > 0 {
			ttl = time.Duration(ttlSeconds) * time.Second
		}

		ipNodes[ip] = candidateNodeID
		if err := s.storage.SetIPNode(ctx, userIdentifier, ip, candidateNodeID, ttl); err != nil {
			log.Printf("Автопочинка ip->node не удалась для %s (%s): %v", userIdentifier, ip, err)
		}
	}

	return ipNodes
}

func subnet24(ip string) string {
	parts := strings.Split(strings.TrimSpace(ip), ".")
	if len(parts) == 4 {
		return parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
	}
	return ip
}

func uniqueSortedIPs(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	uniq := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, ip := range raw {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, ok := uniq[ip]; ok {
			continue
		}
		uniq[ip] = struct{}{}
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func normalizeNetworkType(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "mobile", "wifi", "cable":
		return raw
	default:
		return "unknown"
	}
}

func normalizeTrafficStatus(raw string, online bool) string {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch status {
	case "":
		if online {
			return "online"
		}
		return "offline"
	case "active", "enabled", "enable", "ok":
		return "active"
	case "inactive", "disabled", "suspended":
		return "disabled"
	case "expired":
		return "expired"
	case "online":
		return "online"
	case "offline":
		return "offline"
	default:
		return status
	}
}

func sortedSetValues(input map[string]struct{}) []string {
	if len(input) == 0 {
		return nil
	}
	out := make([]string, 0, len(input))
	for item := range input {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func topProviders(m map[string]int, limit int) []string {
	if len(m) == 0 {
		return nil
	}
	type row struct {
		name  string
		count int
	}
	items := make([]row, 0, len(m))
	for name, count := range m {
		items = append(items, row{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return strings.ToLower(items[i].name) < strings.ToLower(items[j].name)
		}
		return items[i].count > items[j].count
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("%s (%d)", item.name, item.count))
	}
	return out
}

func (s *Server) classifyIP(ctx context.Context, ip string) models.NetworkInfo {
	if s.networkClass == nil {
		return models.NetworkInfo{Type: "unknown"}
	}
	info := s.networkClass.Classify(ctx, ip)
	info.Type = normalizeNetworkType(info.Type)
	return info
}

func (s *Server) detectDominantNetworkType(ctx context.Context, ips []string) string {
	if len(ips) == 0 {
		return "unknown"
	}
	counts := map[string]int{
		"mobile":  0,
		"wifi":    0,
		"cable":   0,
		"unknown": 0,
	}
	for _, ip := range ips {
		info := s.classifyIP(ctx, ip)
		counts[normalizeNetworkType(info.Type)]++
	}
	bestType := "unknown"
	bestCount := -1
	for _, typ := range []string{"mobile", "wifi", "cable", "unknown"} {
		if counts[typ] > bestCount {
			bestType = typ
			bestCount = counts[typ]
		}
	}
	return bestType
}

func (s *Server) resolveAlertUsername(userIdentifier string) string {
	username := strings.TrimSpace(userIdentifier)
	if s.limitProvider != nil {
		if profile, ok := s.limitProvider.GetUserProfile(userIdentifier); ok {
			if uname := strings.TrimSpace(profile.Username); uname != "" {
				return uname
			}
		}
	}
	prefix := strings.TrimSpace(s.cfg.AlertWebhookUsernamePrefix)
	if prefix == "" {
		prefix = "user_"
	}
	if strings.HasPrefix(username, prefix) {
		return username
	}
	isDigits := username != ""
	for _, ch := range username {
		if ch < '0' || ch > '9' {
			isDigits = false
			break
		}
	}
	if isDigits {
		return prefix + username
	}
	return username
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func ptrInt(v int) *int { return &v }

func ptrBool(v bool) *bool { return &v }

func ptrString(v string) *string { return &v }

func ptrStringSlice(v []string) *[]string {
	out := cloneStringSlice(v)
	return &out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func newServerMetrics() *serverMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	makeGauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name: name,
			Help: help,
		})
		reg.MustRegister(g)
		return g
	}

	return &serverMetrics{
		registry:           reg,
		handler:            promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		sharingViolators:   makeGauge("ffxban_sharing_violators", "Users currently in violator state"),
		sharingBanlist:     makeGauge("ffxban_sharing_banlist", "Users currently in banlist"),
		sharingPermanent:   makeGauge("ffxban_sharing_permanent", "Users with permanent ban"),
		geoMismatches:      makeGauge("ffxban_geo_mismatches", "Users with geo sharing detected"),
		activeBans:         makeGauge("ffxban_active_bans", "Currently confirmed active IP bans"),
		nodesTotal:         makeGauge("ffxban_nodes_total", "Total number of configured nodes"),
		nodesOnline:        makeGauge("ffxban_nodes_online", "Number of online nodes"),
		logQueueLen:        makeGauge("ffxban_log_queue_length", "Current length of log processing queue"),
		sideEffectQueueLen: makeGauge("ffxban_side_effect_queue_length", "Current length of side effect queue"),
		activeSessions:     makeGauge("ffxban_active_sessions", "Current number of active user sessions"),
		uptimeSeconds:      makeGauge("ffxban_uptime_seconds", "Process uptime in seconds"),
	}
}

func (s *Server) collectNodeOnlineStats() (int, int) {
	nodesTotal, nodesOnline := 0, 0
	if s.limitProvider == nil {
		return nodesTotal, nodesOnline
	}
	nodes := s.limitProvider.ListNodes()
	nodesTotal = len(nodes)

	runtimeStates := s.processor.GetNodeRuntimeStates()
	now := time.Now()
	timeout := s.cfg.NodeHeartbeatTimeout
	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		name := strings.TrimSpace(node.Name)
		state, ok := runtimeStates[id]
		if !ok {
			state, ok = runtimeStates[name]
		}
		if !ok {
			nameLower := strings.ToLower(name)
			for key, runtimeState := range runtimeStates {
				if strings.ToLower(key) == nameLower {
					state = runtimeState
					break
				}
			}
		}
		lastSeen := maxTime(state.LastTrafficAt, state.LastBlockerBeatAt)
		if !lastSeen.IsZero() && lastSeen.After(now.Add(-timeout)) {
			nodesOnline++
		}
	}
	return nodesTotal, nodesOnline
}

// handleMetrics возвращает метрики в формате Prometheus exposition format.
func (s *Server) handleMetrics(c *gin.Context) {
	if !s.cfg.PrometheusEnabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "metrics endpoint is disabled; set PROMETHEUS_ENABLED=true"})
		return
	}

	sharingStatuses := s.processor.ListSharingStatuses(5000)
	violators, banlistUsers, permanentBans, geoMismatches := 0, 0, 0, 0
	for _, st := range sharingStatuses {
		if st.Violator {
			violators++
		}
		if st.InBanlist {
			banlistUsers++
		}
		if st.PermanentBan {
			permanentBans++
		}
		if st.GeoMismatch {
			geoMismatches++
		}
	}

	nodesTotal, nodesOnline := s.collectNodeOnlineStats()
	logQueue, sideEffectQueue := s.processor.GetQueueStats()
	activeSessions := s.processor.CountActiveSessions(onlinePresenceWindow)
	uptime := time.Since(s.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}

	s.metrics.sharingViolators.Set(float64(violators))
	s.metrics.sharingBanlist.Set(float64(banlistUsers))
	s.metrics.sharingPermanent.Set(float64(permanentBans))
	s.metrics.geoMismatches.Set(float64(geoMismatches))
	s.metrics.activeBans.Set(float64(len(s.processor.GetActiveBans(10000))))
	s.metrics.nodesTotal.Set(float64(nodesTotal))
	s.metrics.nodesOnline.Set(float64(nodesOnline))
	s.metrics.logQueueLen.Set(float64(logQueue))
	s.metrics.sideEffectQueueLen.Set(float64(sideEffectQueue))
	s.metrics.activeSessions.Set(float64(activeSessions))
	s.metrics.uptimeSeconds.Set(uptime)

	s.metrics.handler.ServeHTTP(c.Writer, c.Request)
}

func (s *Server) handleResetSharing(c *gin.Context) {
	var req struct {
		UserIdentifier string `json:"user_identifier"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	userIdentifier := strings.TrimSpace(req.UserIdentifier)
	if userIdentifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_identifier is required"})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "manual_panel_reset"
	}
	had := s.processor.ResetSharingState(userIdentifier, reason)
	s.processor.RecordManualEvent(userIdentifier, "sharing_state_reset", "Триггеры сброшены через панель: "+reason)
	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "had_state": had})
}

func (s *Server) handleConfig(c *gin.Context) {
	c.JSON(http.StatusOK, s.configPayload())
}

func (s *Server) runtimeOverridesSnapshot() runtimeConfigOverrides {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.configOverrides
}

func (s *Server) applyRuntimeOverrides(overrides runtimeConfigOverrides) {
	s.configMu.Lock()
	s.configOverrides = overrides
	base := s.baseConfig

	s.cfg.MaxIPsPerUser = base.MaxIPsPerUser
	s.cfg.BlockDuration = base.BlockDuration
	s.cfg.UserIPTTL = base.UserIPTTL
	s.cfg.ClearIPsDelay = base.ClearIPsDelay
	s.cfg.SharingDetectionEnabled = base.SharingDetectionEnabled
	s.cfg.TriggerCount = base.TriggerCount
	s.cfg.TriggerPeriod = base.TriggerPeriod
	s.cfg.TriggerMinInterval = base.TriggerMinInterval
	s.cfg.SharingSustain = base.SharingSustain
	s.cfg.BanlistThreshold = base.BanlistThreshold
	s.cfg.SharingSourceWindow = base.SharingSourceWindow
	s.cfg.SubnetGrouping = base.SubnetGrouping
	s.cfg.SharingBlockOnBanlistOnly = base.SharingBlockOnBanlistOnly
	s.cfg.SharingBlockOnViolatorOnly = base.SharingBlockOnViolatorOnly
	s.cfg.SharingHardwareGuardEnabled = base.SharingHardwareGuardEnabled
	s.cfg.SharingPermanentBanEnabled = base.SharingPermanentBanEnabled
	s.cfg.SharingPermanentBanDuration = base.SharingPermanentBanDuration
	s.cfg.BanEscalationEnabled = base.BanEscalationEnabled
	s.cfg.BanEscalationDurations = cloneStringSlice(base.BanEscalationDurations)
	s.cfg.GeoSharingEnabled = base.GeoSharingEnabled
	s.cfg.GeoSharingMinCountries = base.GeoSharingMinCountries
	s.cfg.NetworkPolicyEnabled = base.NetworkPolicyEnabled
	s.cfg.NetworkMobileGraceIPs = base.NetworkMobileGraceIPs
	s.cfg.NetworkForceBlockExcessIPs = base.NetworkForceBlockExcessIPs
	s.cfg.HeuristicsEnabled = base.HeuristicsEnabled
	s.cfg.HeuristicsWindow = base.HeuristicsWindow
	s.cfg.NodeHeartbeatTimeout = base.NodeHeartbeatTimeout
	s.cfg.PrometheusEnabled = base.PrometheusEnabled

	if overrides.MaxIPsPerUser != nil {
		s.cfg.MaxIPsPerUser = *overrides.MaxIPsPerUser
	}
	if overrides.BlockDuration != nil {
		s.cfg.BlockDuration = strings.TrimSpace(*overrides.BlockDuration)
	}
	if overrides.UserIPTTLSeconds != nil {
		s.cfg.UserIPTTL = time.Duration(*overrides.UserIPTTLSeconds) * time.Second
	}
	if overrides.ClearIPsDelaySeconds != nil {
		s.cfg.ClearIPsDelay = time.Duration(*overrides.ClearIPsDelaySeconds) * time.Second
	}
	if overrides.SharingDetectionEnabled != nil {
		s.cfg.SharingDetectionEnabled = *overrides.SharingDetectionEnabled
	}
	if overrides.TriggerCount != nil {
		s.cfg.TriggerCount = *overrides.TriggerCount
	}
	if overrides.TriggerPeriodSeconds != nil {
		s.cfg.TriggerPeriod = time.Duration(*overrides.TriggerPeriodSeconds) * time.Second
	}
	if overrides.TriggerMinIntervalSeconds != nil {
		s.cfg.TriggerMinInterval = time.Duration(*overrides.TriggerMinIntervalSeconds) * time.Second
	}
	if overrides.SharingSustainSeconds != nil {
		s.cfg.SharingSustain = time.Duration(*overrides.SharingSustainSeconds) * time.Second
	}
	if overrides.BanlistThresholdSeconds != nil {
		s.cfg.BanlistThreshold = time.Duration(*overrides.BanlistThresholdSeconds) * time.Second
	}
	if overrides.SharingSourceWindowSeconds != nil {
		s.cfg.SharingSourceWindow = time.Duration(*overrides.SharingSourceWindowSeconds) * time.Second
	}
	if overrides.SubnetGrouping != nil {
		s.cfg.SubnetGrouping = *overrides.SubnetGrouping
	}
	if overrides.SharingBlockOnBanlistOnly != nil {
		s.cfg.SharingBlockOnBanlistOnly = *overrides.SharingBlockOnBanlistOnly
	}
	if overrides.SharingBlockOnViolatorOnly != nil {
		s.cfg.SharingBlockOnViolatorOnly = *overrides.SharingBlockOnViolatorOnly
	}
	if overrides.SharingHardwareGuard != nil {
		s.cfg.SharingHardwareGuardEnabled = *overrides.SharingHardwareGuard
	}
	if overrides.SharingPermanentBanEnabled != nil {
		s.cfg.SharingPermanentBanEnabled = *overrides.SharingPermanentBanEnabled
	}
	if overrides.SharingPermanentBanDuration != nil {
		s.cfg.SharingPermanentBanDuration = strings.TrimSpace(*overrides.SharingPermanentBanDuration)
	}
	if overrides.BanEscalationEnabled != nil {
		s.cfg.BanEscalationEnabled = *overrides.BanEscalationEnabled
	}
	if overrides.BanEscalationDurations != nil {
		s.cfg.BanEscalationDurations = cloneStringSlice(*overrides.BanEscalationDurations)
	}
	if overrides.GeoSharingEnabled != nil {
		s.cfg.GeoSharingEnabled = *overrides.GeoSharingEnabled
	}
	if overrides.GeoSharingMinCountries != nil {
		s.cfg.GeoSharingMinCountries = *overrides.GeoSharingMinCountries
	}
	if overrides.NetworkPolicyEnabled != nil {
		s.cfg.NetworkPolicyEnabled = *overrides.NetworkPolicyEnabled
	}
	if overrides.NetworkMobileGraceIPs != nil {
		s.cfg.NetworkMobileGraceIPs = *overrides.NetworkMobileGraceIPs
	}
	if overrides.NetworkForceBlockExcessIPs != nil {
		s.cfg.NetworkForceBlockExcessIPs = *overrides.NetworkForceBlockExcessIPs
	}
	if overrides.HeuristicsEnabled != nil {
		s.cfg.HeuristicsEnabled = *overrides.HeuristicsEnabled
	}
	if overrides.HeuristicsWindowSeconds != nil {
		s.cfg.HeuristicsWindow = time.Duration(*overrides.HeuristicsWindowSeconds) * time.Second
	}
	if overrides.NodeHeartbeatTimeoutSeconds != nil {
		s.cfg.NodeHeartbeatTimeout = time.Duration(*overrides.NodeHeartbeatTimeoutSeconds) * time.Second
	}
	if overrides.PrometheusEnabled != nil {
		s.cfg.PrometheusEnabled = *overrides.PrometheusEnabled
	}
	s.configMu.Unlock()

	processorOverrides := processor.RuntimeOverrides{}
	if overrides.MaxIPsPerUser != nil {
		processorOverrides.MaxIPsPerUser = *overrides.MaxIPsPerUser
	}
	if overrides.BlockDuration != nil {
		processorOverrides.BlockDuration = strings.TrimSpace(*overrides.BlockDuration)
	}
	if overrides.TriggerCount != nil {
		processorOverrides.TriggerCount = *overrides.TriggerCount
	}
	s.processor.SetRuntimeOverrides(processorOverrides)
}

func (s *Server) runtimeOverridesMap(overrides runtimeConfigOverrides) map[string]string {
	out := map[string]string{}
	putInt := func(key string, val *int) {
		if val != nil {
			out[key] = strconv.Itoa(*val)
		}
	}
	putBool := func(key string, val *bool) {
		if val != nil {
			out[key] = strconv.FormatBool(*val)
		}
	}
	putString := func(key string, val *string) {
		if val != nil {
			trimmed := strings.TrimSpace(*val)
			if trimmed != "" {
				out[key] = trimmed
			}
		}
	}

	putInt("max_ips_per_user", overrides.MaxIPsPerUser)
	putString("block_duration", overrides.BlockDuration)
	putInt("user_ip_ttl_seconds", overrides.UserIPTTLSeconds)
	putInt("clear_ips_delay_seconds", overrides.ClearIPsDelaySeconds)
	putBool("sharing_detection_enabled", overrides.SharingDetectionEnabled)
	putInt("trigger_count", overrides.TriggerCount)
	putInt("trigger_period_seconds", overrides.TriggerPeriodSeconds)
	putInt("trigger_min_interval_seconds", overrides.TriggerMinIntervalSeconds)
	putInt("sharing_sustain_seconds", overrides.SharingSustainSeconds)
	putInt("banlist_threshold_seconds", overrides.BanlistThresholdSeconds)
	putInt("sharing_source_window_seconds", overrides.SharingSourceWindowSeconds)
	putBool("subnet_grouping", overrides.SubnetGrouping)
	putBool("sharing_block_on_banlist_only", overrides.SharingBlockOnBanlistOnly)
	putBool("sharing_block_on_violator_only", overrides.SharingBlockOnViolatorOnly)
	putBool("sharing_hardware_guard", overrides.SharingHardwareGuard)
	putBool("sharing_permanent_ban_enabled", overrides.SharingPermanentBanEnabled)
	putString("sharing_permanent_ban_duration", overrides.SharingPermanentBanDuration)
	putBool("ban_escalation_enabled", overrides.BanEscalationEnabled)
	if overrides.BanEscalationDurations != nil {
		if data, err := json.Marshal(*overrides.BanEscalationDurations); err == nil {
			out["ban_escalation_durations"] = string(data)
		}
	}
	putBool("geo_sharing_enabled", overrides.GeoSharingEnabled)
	putInt("geo_sharing_min_countries", overrides.GeoSharingMinCountries)
	putBool("network_policy_enabled", overrides.NetworkPolicyEnabled)
	putInt("network_mobile_grace_ips", overrides.NetworkMobileGraceIPs)
	putInt("network_force_block_excess_ips", overrides.NetworkForceBlockExcessIPs)
	putBool("heuristics_enabled", overrides.HeuristicsEnabled)
	putInt("heuristics_window_seconds", overrides.HeuristicsWindowSeconds)
	putInt("node_heartbeat_timeout_seconds", overrides.NodeHeartbeatTimeoutSeconds)
	putBool("prometheus_enabled", overrides.PrometheusEnabled)
	return out
}

func (s *Server) runtimeOverridesJSON(overrides runtimeConfigOverrides) gin.H {
	out := gin.H{}
	putInt := func(key string, val *int) {
		if val != nil {
			out[key] = *val
		}
	}
	putBool := func(key string, val *bool) {
		if val != nil {
			out[key] = *val
		}
	}
	putString := func(key string, val *string) {
		if val != nil {
			out[key] = strings.TrimSpace(*val)
		}
	}

	putInt("max_ips_per_user", overrides.MaxIPsPerUser)
	putString("block_duration", overrides.BlockDuration)
	putInt("user_ip_ttl_seconds", overrides.UserIPTTLSeconds)
	putInt("clear_ips_delay_seconds", overrides.ClearIPsDelaySeconds)
	putBool("sharing_detection_enabled", overrides.SharingDetectionEnabled)
	putInt("trigger_count", overrides.TriggerCount)
	putInt("trigger_period_seconds", overrides.TriggerPeriodSeconds)
	putInt("trigger_min_interval_seconds", overrides.TriggerMinIntervalSeconds)
	putInt("sharing_sustain_seconds", overrides.SharingSustainSeconds)
	putInt("banlist_threshold_seconds", overrides.BanlistThresholdSeconds)
	putInt("sharing_source_window_seconds", overrides.SharingSourceWindowSeconds)
	putBool("subnet_grouping", overrides.SubnetGrouping)
	putBool("sharing_block_on_banlist_only", overrides.SharingBlockOnBanlistOnly)
	putBool("sharing_block_on_violator_only", overrides.SharingBlockOnViolatorOnly)
	putBool("sharing_hardware_guard", overrides.SharingHardwareGuard)
	putBool("sharing_permanent_ban_enabled", overrides.SharingPermanentBanEnabled)
	putString("sharing_permanent_ban_duration", overrides.SharingPermanentBanDuration)
	putBool("ban_escalation_enabled", overrides.BanEscalationEnabled)
	if overrides.BanEscalationDurations != nil {
		out["ban_escalation_durations"] = cloneStringSlice(*overrides.BanEscalationDurations)
	}
	putBool("geo_sharing_enabled", overrides.GeoSharingEnabled)
	putInt("geo_sharing_min_countries", overrides.GeoSharingMinCountries)
	putBool("network_policy_enabled", overrides.NetworkPolicyEnabled)
	putInt("network_mobile_grace_ips", overrides.NetworkMobileGraceIPs)
	putInt("network_force_block_excess_ips", overrides.NetworkForceBlockExcessIPs)
	putBool("heuristics_enabled", overrides.HeuristicsEnabled)
	putInt("heuristics_window_seconds", overrides.HeuristicsWindowSeconds)
	putInt("node_heartbeat_timeout_seconds", overrides.NodeHeartbeatTimeoutSeconds)
	putBool("prometheus_enabled", overrides.PrometheusEnabled)
	return out
}

func parseRuntimeOverrides(raw map[string]string) runtimeConfigOverrides {
	var overrides runtimeConfigOverrides
	if len(raw) == 0 {
		return overrides
	}

	parseInt := func(key string, min int) *int {
		value := strings.TrimSpace(raw[key])
		if value == "" {
			return nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < min {
			return nil
		}
		return ptrInt(parsed)
	}
	parseBool := func(key string) *bool {
		value := strings.TrimSpace(raw[key])
		if value == "" {
			return nil
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil
		}
		return ptrBool(parsed)
	}
	parseString := func(key string, pattern *regexp.Regexp) *string {
		value := strings.TrimSpace(raw[key])
		if value == "" {
			return nil
		}
		if pattern != nil && !pattern.MatchString(value) {
			return nil
		}
		return ptrString(value)
	}

	overrides.MaxIPsPerUser = parseInt("max_ips_per_user", 1)
	overrides.BlockDuration = parseString("block_duration", durationPattern)
	overrides.UserIPTTLSeconds = parseInt("user_ip_ttl_seconds", 1)
	overrides.ClearIPsDelaySeconds = parseInt("clear_ips_delay_seconds", 1)
	overrides.SharingDetectionEnabled = parseBool("sharing_detection_enabled")
	overrides.TriggerCount = parseInt("trigger_count", 1)
	overrides.TriggerPeriodSeconds = parseInt("trigger_period_seconds", 1)
	overrides.TriggerMinIntervalSeconds = parseInt("trigger_min_interval_seconds", 1)
	overrides.SharingSustainSeconds = parseInt("sharing_sustain_seconds", 1)
	overrides.BanlistThresholdSeconds = parseInt("banlist_threshold_seconds", 1)
	overrides.SharingSourceWindowSeconds = parseInt("sharing_source_window_seconds", 1)
	overrides.SubnetGrouping = parseBool("subnet_grouping")
	overrides.SharingBlockOnBanlistOnly = parseBool("sharing_block_on_banlist_only")
	overrides.SharingBlockOnViolatorOnly = parseBool("sharing_block_on_violator_only")
	overrides.SharingHardwareGuard = parseBool("sharing_hardware_guard")
	overrides.SharingPermanentBanEnabled = parseBool("sharing_permanent_ban_enabled")
	overrides.SharingPermanentBanDuration = parseString("sharing_permanent_ban_duration", durationPattern)
	overrides.BanEscalationEnabled = parseBool("ban_escalation_enabled")
	overrides.GeoSharingEnabled = parseBool("geo_sharing_enabled")
	overrides.GeoSharingMinCountries = parseInt("geo_sharing_min_countries", 1)
	overrides.NetworkPolicyEnabled = parseBool("network_policy_enabled")
	overrides.NetworkMobileGraceIPs = parseInt("network_mobile_grace_ips", 0)
	overrides.NetworkForceBlockExcessIPs = parseInt("network_force_block_excess_ips", 0)
	overrides.HeuristicsEnabled = parseBool("heuristics_enabled")
	overrides.HeuristicsWindowSeconds = parseInt("heuristics_window_seconds", 1)
	overrides.NodeHeartbeatTimeoutSeconds = parseInt("node_heartbeat_timeout_seconds", 1)
	overrides.PrometheusEnabled = parseBool("prometheus_enabled")

	if rawDurations := strings.TrimSpace(raw["ban_escalation_durations"]); rawDurations != "" {
		var items []string
		if strings.HasPrefix(rawDurations, "[") {
			_ = json.Unmarshal([]byte(rawDurations), &items)
		}
		if len(items) == 0 {
			for _, part := range strings.Split(rawDurations, ",") {
				item := strings.TrimSpace(part)
				if item == "" {
					continue
				}
				items = append(items, item)
			}
		}
		valid := make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" || !durationPattern.MatchString(item) {
				continue
			}
			valid = append(valid, item)
		}
		if len(valid) > 0 {
			overrides.BanEscalationDurations = ptrStringSlice(valid)
		}
	}
	return overrides
}

func (s *Server) loadRuntimeConfigOverrides() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := s.storage.GetConfigOverrides(ctx)
	if err != nil {
		return err
	}
	overrides := parseRuntimeOverrides(raw)
	s.applyRuntimeOverrides(overrides)
	return nil
}

func (s *Server) configPayload() gin.H {
	s.configMu.RLock()
	cfg := *s.cfg
	cfg.BanEscalationDurations = cloneStringSlice(s.cfg.BanEscalationDurations)
	overrides := s.configOverrides
	s.configMu.RUnlock()

	return gin.H{
		"max_ips_per_user":               cfg.MaxIPsPerUser,
		"block_duration":                 cfg.BlockDuration,
		"user_ip_ttl_seconds":            int(cfg.UserIPTTL.Seconds()),
		"clear_ips_delay_seconds":        int(cfg.ClearIPsDelay.Seconds()),
		"sharing_detection_enabled":      cfg.SharingDetectionEnabled,
		"trigger_count":                  cfg.TriggerCount,
		"trigger_period_seconds":         int(cfg.TriggerPeriod.Seconds()),
		"trigger_min_interval_seconds":   int(cfg.TriggerMinInterval.Seconds()),
		"sharing_sustain_seconds":        int(cfg.SharingSustain.Seconds()),
		"banlist_threshold_seconds":      int(cfg.BanlistThreshold.Seconds()),
		"sharing_source_window_seconds":  int(cfg.SharingSourceWindow.Seconds()),
		"subnet_grouping":                cfg.SubnetGrouping,
		"sharing_block_on_banlist_only":  cfg.SharingBlockOnBanlistOnly,
		"sharing_block_on_violator_only": cfg.SharingBlockOnViolatorOnly,
		"sharing_hardware_guard":         cfg.SharingHardwareGuardEnabled,
		"sharing_permanent_ban_enabled":  cfg.SharingPermanentBanEnabled,
		"sharing_permanent_ban_duration": cfg.SharingPermanentBanDuration,
		"ban_escalation_enabled":         cfg.BanEscalationEnabled,
		"ban_escalation_durations":       cfg.BanEscalationDurations,
		"geo_sharing_enabled":            cfg.GeoSharingEnabled,
		"geo_sharing_min_countries":      cfg.GeoSharingMinCountries,
		"network_policy_enabled":         cfg.NetworkPolicyEnabled,
		"network_mobile_grace_ips":       cfg.NetworkMobileGraceIPs,
		"network_force_block_excess_ips": cfg.NetworkForceBlockExcessIPs,
		"heuristics_enabled":             cfg.HeuristicsEnabled,
		"heuristics_window_seconds":      int(cfg.HeuristicsWindow.Seconds()),
		"node_heartbeat_timeout_seconds": int(cfg.NodeHeartbeatTimeout.Seconds()),
		"prometheus_enabled":             cfg.PrometheusEnabled,
		"runtime_overrides":              s.runtimeOverridesJSON(overrides),
	}
}

func intFromAny(value any) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case float32:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	default:
		return 0, fmt.Errorf("expected integer")
	}
}

func boolFromAny(value any) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(v))
	default:
		return false, fmt.Errorf("expected boolean")
	}
}

func stringSliceFromAny(value any) ([]string, error) {
	switch v := value.(type) {
	case []string:
		return cloneStringSlice(v), nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text := strings.TrimSpace(fmt.Sprintf("%v", item))
			if text == "" {
				continue
			}
			out = append(out, text)
		}
		return out, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return nil, nil
		}
		var decoded []string
		if strings.HasPrefix(raw, "[") && json.Unmarshal([]byte(raw), &decoded) == nil {
			return decoded, nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			text := strings.TrimSpace(part)
			if text == "" {
				continue
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected string array")
	}
}

func validateDurationList(items []string) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		if !durationPattern.MatchString(value) {
			return nil, fmt.Errorf("invalid duration %q", value)
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *Server) handleConfigOverridesUpdate(c *gin.Context) {
	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if nested, ok := payload["overrides"].(map[string]any); ok {
		payload = nested
	}

	overrides := s.runtimeOverridesSnapshot()
	setInt := func(value any, key string, min, max int) (*int, error) {
		if value == nil {
			return nil, nil
		}
		parsed, err := intFromAny(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
		if parsed < min || (max > 0 && parsed > max) {
			if max > 0 {
				return nil, fmt.Errorf("%s must be between %d and %d", key, min, max)
			}
			return nil, fmt.Errorf("%s must be >= %d", key, min)
		}
		return ptrInt(parsed), nil
	}
	setBool := func(value any, key string) (*bool, error) {
		if value == nil {
			return nil, nil
		}
		parsed, err := boolFromAny(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be boolean", key)
		}
		return ptrBool(parsed), nil
	}
	setDurationString := func(value any, key string) (*string, error) {
		if value == nil {
			return nil, nil
		}
		text := strings.TrimSpace(fmt.Sprintf("%v", value))
		if text == "" {
			return nil, nil
		}
		if !durationPattern.MatchString(text) {
			return nil, fmt.Errorf("%s has invalid duration format", key)
		}
		return ptrString(text), nil
	}

	for key, value := range payload {
		switch key {
		case "max_ips_per_user":
			v, err := setInt(value, key, 1, 100)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.MaxIPsPerUser = v
		case "block_duration":
			v, err := setDurationString(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.BlockDuration = v
		case "user_ip_ttl_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.UserIPTTLSeconds = v
		case "clear_ips_delay_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.ClearIPsDelaySeconds = v
		case "sharing_detection_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingDetectionEnabled = v
		case "trigger_count":
			v, err := setInt(value, key, 1, 100)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.TriggerCount = v
		case "trigger_period_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.TriggerPeriodSeconds = v
		case "trigger_min_interval_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.TriggerMinIntervalSeconds = v
		case "sharing_sustain_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingSustainSeconds = v
		case "banlist_threshold_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.BanlistThresholdSeconds = v
		case "sharing_source_window_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingSourceWindowSeconds = v
		case "subnet_grouping":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SubnetGrouping = v
		case "sharing_block_on_banlist_only":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingBlockOnBanlistOnly = v
		case "sharing_block_on_violator_only":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingBlockOnViolatorOnly = v
		case "sharing_hardware_guard":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingHardwareGuard = v
		case "sharing_permanent_ban_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingPermanentBanEnabled = v
		case "sharing_permanent_ban_duration":
			v, err := setDurationString(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.SharingPermanentBanDuration = v
		case "ban_escalation_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.BanEscalationEnabled = v
		case "ban_escalation_durations":
			if value == nil {
				overrides.BanEscalationDurations = nil
				continue
			}
			items, err := stringSliceFromAny(value)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "ban_escalation_durations must be string list"})
				return
			}
			validated, err := validateDurationList(items)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if len(validated) == 0 {
				overrides.BanEscalationDurations = nil
			} else {
				overrides.BanEscalationDurations = ptrStringSlice(validated)
			}
		case "geo_sharing_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.GeoSharingEnabled = v
		case "geo_sharing_min_countries":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.GeoSharingMinCountries = v
		case "network_policy_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.NetworkPolicyEnabled = v
		case "network_mobile_grace_ips":
			v, err := setInt(value, key, 0, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.NetworkMobileGraceIPs = v
		case "network_force_block_excess_ips":
			v, err := setInt(value, key, 0, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.NetworkForceBlockExcessIPs = v
		case "heuristics_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.HeuristicsEnabled = v
		case "heuristics_window_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.HeuristicsWindowSeconds = v
		case "node_heartbeat_timeout_seconds":
			v, err := setInt(value, key, 1, 0)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.NodeHeartbeatTimeoutSeconds = v
		case "prometheus_enabled":
			v, err := setBool(value, key)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			overrides.PrometheusEnabled = v
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.storage.SetConfigOverrides(ctx, s.runtimeOverridesMap(overrides)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	s.applyRuntimeOverrides(overrides)
	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"config": s.configPayload(),
	})
}

func (s *Server) handleConfigOverridesReset(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.storage.SetConfigOverrides(ctx, nil); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.applyRuntimeOverrides(runtimeConfigOverrides{})
	s.wsHub.Notify()
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"config": s.configPayload(),
	})
}

func (s *Server) handleWebSocket(c *gin.Context) {
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	client := &wsClient{
		hub:  s.wsHub,
		conn: conn,
		send: make(chan []byte, 8),
	}
	s.wsHub.register(client)
	// Сразу шлём refresh чтобы клиент загрузил данные при подключении
	client.send <- wsRefreshMsg
	go client.writePump()
	client.readPump() // блокирует до закрытия соединения
}

func (s *Server) resolveNodeDisplayName(raw string, profile panel.UserProfile) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "unknown"
	}
	if s.limitProvider != nil {
		if name, ok := s.limitProvider.GetNodeDisplayName(raw); ok && strings.TrimSpace(name) != "" {
			return name
		}
	}
	// Fallback из профиля безопасен только когда сама нода неизвестна.
	// Иначе можно ошибочно подменить реальный node_id IP-а на "последнюю ноду" пользователя.
	if isUnknownNode(raw) && strings.TrimSpace(profile.LastConnectedNode) != "" {
		return strings.TrimSpace(profile.LastConnectedNode)
	}
	return raw
}

// handleSysInfo возвращает метрики сервера: CPU, RAM, диск, Redis, Go runtime.
func (s *Server) handleSysInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	// --- Go runtime ---
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	goInfo := gin.H{
		"goroutines":  runtime.NumGoroutine(),
		"heap_alloc":  mem.HeapAlloc,
		"heap_sys":    mem.HeapSys,
		"heap_inuse":  mem.HeapInuse,
		"stack_inuse": mem.StackInuse,
		"sys_total":   mem.Sys,
		"gc_runs":     mem.NumGC,
		"next_gc":     mem.NextGC,
		"go_version":  runtime.Version(),
		"num_cpu":     runtime.NumCPU(),
	}

	// --- System RAM (/proc/meminfo) ---
	ramInfo := gin.H{}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		fields := map[string]string{}
		for sc.Scan() {
			parts := strings.Fields(sc.Text())
			if len(parts) >= 2 {
				key := strings.TrimSuffix(parts[0], ":")
				fields[key] = parts[1]
			}
		}
		toBytes := func(kb string) uint64 {
			v, _ := strconv.ParseUint(kb, 10, 64)
			return v * 1024
		}
		total := toBytes(fields["MemTotal"])
		free := toBytes(fields["MemFree"])
		avail := toBytes(fields["MemAvailable"])
		buffers := toBytes(fields["Buffers"])
		cached := toBytes(fields["Cached"])
		used := total - free - buffers - cached
		ramInfo = gin.H{
			"total":     total,
			"used":      used,
			"free":      free,
			"available": avail,
			"buffers":   buffers,
			"cached":    cached,
		}
	}

	// --- CPU load (/proc/loadavg) ---
	cpuInfo := gin.H{}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			cpuInfo = gin.H{
				"load1":  parts[0],
				"load5":  parts[1],
				"load15": parts[2],
			}
		}
	}

	// --- Uptime (/proc/uptime) ---
	uptimeInfo := gin.H{}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 1 {
			if secs, err := strconv.ParseFloat(parts[0], 64); err == nil {
				uptimeInfo = gin.H{
					"uptime_seconds": int64(secs),
					"uptime_human":   formatUptime(int64(secs)),
				}
			}
		}
	}

	// --- Disk usage ---
	diskInfo := gin.H{}
	if ds, ok := getDiskStats("/"); ok {
		diskInfo = gin.H{
			"total":    ds.Total,
			"used":     ds.Used,
			"free":     ds.Free,
			"avail":    ds.Avail,
			"used_pct": fmt.Sprintf("%.1f", ds.UsedPct),
		}
	}

	// --- Redis ---
	redisInfo := s.storage.GetRedisInfo(ctx)

	c.JSON(http.StatusOK, gin.H{
		"go":     goInfo,
		"ram":    ramInfo,
		"cpu":    cpuInfo,
		"uptime": uptimeInfo,
		"disk":   diskInfo,
		"redis":  redisInfo,
	})
}

func formatUptime(secs int64) string {
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// handleDeviceReport принимает отчёты об устройствах от агентов (Vector на нодах).
// POST /device-report — массив объектов {user_email, user_agent, node_name}
func (s *Server) handleDeviceReport(c *gin.Context) {
	var reports []struct {
		UserEmail string `json:"user_email"`
		UserAgent string `json:"user_agent"`
		NodeName  string `json:"node_name,omitempty"`
	}
	if err := c.ShouldBindJSON(&reports); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	now := time.Now()
	saved := 0
	for _, r := range reports {
		if r.UserEmail == "" || r.UserAgent == "" {
			continue
		}
		if err := s.storage.SaveDeviceReport(ctx, r.UserEmail, r.UserAgent, now); err == nil {
			saved++
		}
	}
	c.JSON(http.StatusAccepted, gin.H{"saved": saved})
}
