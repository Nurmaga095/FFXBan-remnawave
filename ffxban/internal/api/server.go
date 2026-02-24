package api

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"ffxban/internal/config"
	"ffxban/internal/models"
	"ffxban/internal/processor"
	"ffxban/internal/services/alerter"
	"ffxban/internal/services/blocker"
	"ffxban/internal/services/panel"
	"ffxban/internal/services/storage"
	"ffxban/internal/services/threexui"
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
	"golang.org/x/crypto/bcrypt"
)

//go:embed panel.html
var panelHTML []byte

var durationPattern = regexp.MustCompile(`^(\d+[smhd]|permanent)$`)
var onlinePresenceWindow = 3 * time.Minute

type panelOnDemandRefresher interface {
	RefreshIfStale(ctx context.Context, maxAge time.Duration) bool
}

type panelSession struct {
	ExpiresAt time.Time
}

type Server struct {
	router        *gin.Engine
	processor     *processor.LogProcessor
	storage       storage.IPStorage
	blocker       blocker.Blocker
	threexui      *threexui.Manager
	notifier      alerter.Notifier
	limitProvider panel.UserLimitProvider
	networkClass  networkClassifier
	cfg           *config.Config
	port          string
	wsHub         *wsHub

	sessionMu sync.Mutex
	sessions  map[string]panelSession
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
	txManager *threexui.Manager,
	limitProvider panel.UserLimitProvider,
	networkClass networkClassifier,
) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	hub := newWsHub()
	go hub.Run()

	s := &Server{
		router:        router,
		processor:     proc,
		storage:       storage,
		blocker:       blk,
		threexui:      txManager,
		notifier:      alerter.NewWebhookAlerter(cfg.AlertWebhookURL, cfg.AlertWebhookMode, cfg.AlertWebhookToken, cfg.AlertWebhookAuthHeader, cfg.AlertWebhookUsernamePrefix),
		limitProvider: limitProvider,
		networkClass:  networkClass,
		cfg:           cfg,
		port:          cfg.Port,
		sessions:      make(map[string]panelSession),
		wsHub:         hub,
	}

	// Подписываем hub на события из processor'а
	proc.OnBatchProcessed = hub.Notify

	if err := s.ensurePanelPasswordHash(); err != nil {
		log.Printf("Warning: не удалось инициализировать пароль панели: %v", err)
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
	internal.GET("/users/:id", s.handleInternalUserDetails)
	internal.GET("/logs", s.handleInternalLogs)
	internal.GET("/bans/active", s.handleInternalActiveBans)
	internal.GET("/banlist/users", s.handleInternalSharingBanlist)
	internal.GET("/sharing/permanent", s.handleInternalSharingPermanent)
	internal.GET("/networks/types", s.handleInternalNetworkTypes)
	internal.GET("/nodes/health", s.handleInternalNodeHealth)
	internal.GET("/nodes", s.handleInternalNodes)
	internal.POST("/actions/block", s.handleManualBlock)
	internal.POST("/actions/unblock", s.handleManualUnblock)
	internal.POST("/actions/clear", s.handleManualClear)
	internal.POST("/actions/reset_sharing", s.handleResetSharing)
	internal.GET("/config", s.handleConfig)
	internal.GET("/ws", s.handleWebSocket)

	if s.cfg.ThreexuiEnabled {
		txGroup := internal.Group("/3xui")
		txGroup.GET("/users", s.handleThreexuiUsers)
		txGroup.GET("/servers", s.handleThreexuiServers)
	}
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
	response := gin.H{
		"redis_connection":    "ok",
		"rabbitmq_connection": "ok",
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

	ok, err := s.verifyPanelPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}

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
	// Устройства из Redis (для 3x-ui и как дополнение к Remnawave).
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
			rows = append(rows, gin.H{
				"id":   strings.TrimSpace(node.ID),
				"name": name,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i]["name"].(string)) < strings.ToLower(rows[j]["name"].(string))
	})
	c.JSON(http.StatusOK, rows)
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

// handleMetrics возвращает метрики в формате Prometheus text exposition.
// Включается переменной окружения PROMETHEUS_ENABLED=true.
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

	activeBans := s.processor.GetActiveBans(10000)

	nodesTotal, nodesOnline := 0, 0
	if s.limitProvider != nil {
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
				for k, st := range runtimeStates {
					if strings.ToLower(k) == nameLower {
						state = st
						break
					}
				}
			}
			lastSeen := maxTime(state.LastTrafficAt, state.LastBlockerBeatAt)
			if !lastSeen.IsZero() && lastSeen.After(now.Add(-timeout)) {
				nodesOnline++
			}
		}
	}

	logQueue, sideEffectQueue := s.processor.GetQueueStats()

	var sb strings.Builder
	write := func(help, metricType, name string, value int) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, metricType, name, value)
	}

	write("Users currently in violator state", "gauge", "ffxban_sharing_violators", violators)
	write("Users currently in banlist", "gauge", "ffxban_sharing_banlist", banlistUsers)
	write("Users with permanent ban", "gauge", "ffxban_sharing_permanent", permanentBans)
	write("Users with geo sharing detected (IPs from multiple countries)", "gauge", "ffxban_geo_mismatches", geoMismatches)
	write("Currently confirmed active IP bans", "gauge", "ffxban_active_bans", len(activeBans))
	write("Total number of configured nodes", "gauge", "ffxban_nodes_total", nodesTotal)
	write("Number of online nodes (recent heartbeat or traffic)", "gauge", "ffxban_nodes_online", nodesOnline)
	write("Current length of the log processing queue", "gauge", "ffxban_log_queue_length", logQueue)
	write("Current length of the side effect task queue", "gauge", "ffxban_side_effect_queue_length", sideEffectQueue)

	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(http.StatusOK, sb.String())
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
	c.JSON(http.StatusOK, gin.H{
		"max_ips_per_user":               s.cfg.MaxIPsPerUser,
		"block_duration":                 s.cfg.BlockDuration,
		"user_ip_ttl_seconds":            int(s.cfg.UserIPTTL.Seconds()),
		"clear_ips_delay_seconds":        int(s.cfg.ClearIPsDelay.Seconds()),
		"sharing_detection_enabled":      s.cfg.SharingDetectionEnabled,
		"trigger_count":                  s.cfg.TriggerCount,
		"trigger_period_seconds":         int(s.cfg.TriggerPeriod.Seconds()),
		"trigger_min_interval_seconds":   int(s.cfg.TriggerMinInterval.Seconds()),
		"sharing_sustain_seconds":        int(s.cfg.SharingSustain.Seconds()),
		"banlist_threshold_seconds":      int(s.cfg.BanlistThreshold.Seconds()),
		"sharing_source_window_seconds":  int(s.cfg.SharingSourceWindow.Seconds()),
		"subnet_grouping":                s.cfg.SubnetGrouping,
		"sharing_block_on_banlist_only":  s.cfg.SharingBlockOnBanlistOnly,
		"sharing_block_on_violator_only": s.cfg.SharingBlockOnViolatorOnly,
		"sharing_hardware_guard":         s.cfg.SharingHardwareGuardEnabled,
		"sharing_permanent_ban_enabled":  s.cfg.SharingPermanentBanEnabled,
		"sharing_permanent_ban_duration": s.cfg.SharingPermanentBanDuration,
		"ban_escalation_enabled":         s.cfg.BanEscalationEnabled,
		"ban_escalation_durations":       s.cfg.BanEscalationDurations,
		"geo_sharing_enabled":            s.cfg.GeoSharingEnabled,
		"geo_sharing_min_countries":      s.cfg.GeoSharingMinCountries,
		"network_policy_enabled":         s.cfg.NetworkPolicyEnabled,
		"network_mobile_grace_ips":       s.cfg.NetworkMobileGraceIPs,
		"network_force_block_excess_ips": s.cfg.NetworkForceBlockExcessIPs,
		"heuristics_enabled":             s.cfg.HeuristicsEnabled,
		"heuristics_window_seconds":      int(s.cfg.HeuristicsWindow.Seconds()),
		"node_heartbeat_timeout_seconds": int(s.cfg.NodeHeartbeatTimeout.Seconds()),
		"prometheus_enabled":             s.cfg.PrometheusEnabled,
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
		"goroutines":   runtime.NumGoroutine(),
		"heap_alloc":   mem.HeapAlloc,
		"heap_sys":     mem.HeapSys,
		"heap_inuse":   mem.HeapInuse,
		"stack_inuse":  mem.StackInuse,
		"sys_total":    mem.Sys,
		"gc_runs":      mem.NumGC,
		"next_gc":      mem.NextGC,
		"go_version":   runtime.Version(),
		"num_cpu":      runtime.NumCPU(),
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
