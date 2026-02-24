package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stats представляет состояние кэша лимитов, загруженных из панели.
type Stats struct {
	Loaded    bool      `json:"loaded"`
	Users     int       `json:"users"`
	LastLoad  time.Time `json:"last_load,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// HWIDDeviceProfile содержит информацию об устройстве пользователя.
type HWIDDeviceProfile struct {
	HWID      string    `json:"hwid,omitempty"`
	Platform  string    `json:"platform,omitempty"`
	Model     string    `json:"model,omitempty"`
	OS        string    `json:"os,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// UserProfile содержит метаданные пользователя из панели.
type UserProfile struct {
	Username          string              `json:"username,omitempty"`
	LastConnectedNode string              `json:"last_connected_node,omitempty"`
	TelegramID        string              `json:"telegram_id,omitempty"`
	Description       string              `json:"description,omitempty"`
	HWIDDevices       []HWIDDeviceProfile `json:"hwid_devices,omitempty"`
}

// NodeProfile содержит метаданные ноды из панели.
type NodeProfile struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

// UserLimitProvider возвращает лимиты устройств пользователей.
type UserLimitProvider interface {
	GetUserLimit(userIdentifier string) (int, bool)
	GetUserProfile(userIdentifier string) (UserProfile, bool)
	GetNodeDisplayName(nodeIdentifier string) (string, bool)
	ListNodes() []NodeProfile
	Stats() Stats
}

// Client загружает лимиты пользователей из панели и кэширует их в памяти.
type Client struct {
	baseURL        string
	token          string
	keyDelimiter   string
	reloadInterval time.Duration
	httpClient     *http.Client

	reloadMu  sync.Mutex
	mu        sync.RWMutex
	userLimit map[string]int
	profiles  map[string]UserProfile
	nodeMap   map[string]string
	nodes     []NodeProfile
	stats     Stats
}

const (
	panelHTTPTimeout   = 35 * time.Second
	panelUsersPageSize = 200
	panelHWIDPageSize  = 200
	panelReqRetries    = 3
)

// NewClient создает клиента для загрузки лимитов из панели.
func NewClient(baseURL, token string, reloadInterval time.Duration, keyDelimiter string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if reloadInterval <= 0 {
		reloadInterval = 5 * time.Minute
	}

	return &Client{
		baseURL:        baseURL,
		token:          strings.TrimSpace(token),
		keyDelimiter:   strings.TrimSpace(keyDelimiter),
		reloadInterval: reloadInterval,
		httpClient: &http.Client{
			Timeout: panelHTTPTimeout,
		},
		userLimit: make(map[string]int),
		profiles:  make(map[string]UserProfile),
		nodeMap:   make(map[string]string),
	}
}

// Enabled сообщает, что клиент имеет достаточно настроек для работы.
func (c *Client) Enabled() bool {
	return c.baseURL != "" && c.token != ""
}

// Run периодически обновляет лимиты пользователей до завершения контекста.
func (c *Client) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	if !c.Enabled() {
		log.Println("Panel limit provider отключен: PANEL_URL/PANEL_TOKEN не заданы")
		return
	}

	c.reload(ctx)

	ticker := time.NewTicker(c.reloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reload(ctx)
		}
	}
}

// SyncNow выполняет немедленную синхронизацию лимитов и профилей из панели.
func (c *Client) SyncNow(ctx context.Context) {
	if !c.Enabled() {
		return
	}
	c.reload(ctx)
}

// RefreshIfStale синхронизирует панель, если кэш устарел относительно maxAge.
// Возвращает true, если синхронизация была запущена.
func (c *Client) RefreshIfStale(ctx context.Context, maxAge time.Duration) bool {
	if !c.Enabled() {
		return false
	}
	if maxAge <= 0 {
		maxAge = 20 * time.Second
	}

	stats := c.Stats()
	if stats.Loaded && !stats.LastLoad.IsZero() && time.Since(stats.LastLoad) < maxAge {
		return false
	}

	c.reload(ctx)
	return true
}

// GetUserLimit возвращает лимит устройств для userIdentifier, если он загружен.
func (c *Client) GetUserLimit(userIdentifier string) (int, bool) {
	keys := lookupUserKeys(userIdentifier, c.keyDelimiter)
	if len(keys) == 0 {
		return 0, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, key := range keys {
		if limit, ok := c.userLimit[key]; ok {
			return limit, true
		}
	}
	return 0, false
}

// Stats возвращает состояние кэша лимитов.
func (c *Client) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// GetUserProfile возвращает профиль пользователя, если он загружен.
func (c *Client) GetUserProfile(userIdentifier string) (UserProfile, bool) {
	keys := lookupUserKeys(userIdentifier, c.keyDelimiter)
	if len(keys) == 0 {
		return UserProfile{}, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, key := range keys {
		if profile, ok := c.profiles[key]; ok {
			return profile, true
		}
	}
	return UserProfile{}, false
}

// GetNodeDisplayName возвращает отображаемое имя ноды по ее идентификатору.
func (c *Client) GetNodeDisplayName(nodeIdentifier string) (string, bool) {
	key := normalizeUserID(nodeIdentifier)
	if key == "" {
		return "", false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if name, ok := c.nodeMap[key]; ok && strings.TrimSpace(name) != "" {
		return name, true
	}

	// Fallback: частичное совпадение (например, "ger" -> "Германия").
	best := ""
	bestLen := 0
	for alias, name := range c.nodeMap {
		if len(alias) < 3 {
			continue
		}
		if strings.Contains(key, alias) || strings.Contains(alias, key) {
			if len(alias) > bestLen && strings.TrimSpace(name) != "" {
				best = name
				bestLen = len(alias)
			}
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

// ListNodes возвращает список нод из панели.
func (c *Client) ListNodes() []NodeProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.nodes) == 0 {
		return nil
	}
	out := make([]NodeProfile, len(c.nodes))
	copy(out, c.nodes)
	return out
}

func (c *Client) reload(ctx context.Context) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	limits, profiles, userNodeAliases, limitsErr := c.fetchAllLimits(ctx)
	nodes, nodeAliases, nodeErr := c.fetchAllNodes(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	if limitsErr == nil {
		c.userLimit = limits
		c.profiles = profiles
		c.stats.Loaded = true
		c.stats.Users = len(limits)
		c.stats.LastLoad = time.Now()
	}

	if nodeErr == nil {
		c.nodes = nodes
	}

	mergedNodeAliases := make(map[string]string, len(c.nodeMap)+len(userNodeAliases)+len(nodeAliases))
	for k, v := range c.nodeMap {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			mergedNodeAliases[k] = v
		}
	}
	for k, v := range userNodeAliases {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			mergedNodeAliases[k] = v
		}
	}
	for k, v := range nodeAliases {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			mergedNodeAliases[k] = v
		}
	}
	if len(mergedNodeAliases) > 0 {
		c.nodeMap = mergedNodeAliases
	}

	switch {
	case limitsErr == nil && nodeErr == nil:
		c.stats.LastError = ""
		log.Printf("Лимиты пользователей из панели обновлены: %d записей", len(limits))
	case limitsErr != nil && nodeErr == nil:
		c.stats.LastError = "users sync: " + limitsErr.Error()
		log.Printf("Не удалось обновить лимиты пользователей из панели: %v", limitsErr)
	case limitsErr == nil && nodeErr != nil:
		c.stats.LastError = "nodes sync: " + nodeErr.Error()
		log.Printf("Не удалось обновить список нод из панели: %v", nodeErr)
	default:
		c.stats.LastError = fmt.Sprintf("users sync: %v; nodes sync: %v", limitsErr, nodeErr)
		log.Printf("Не удалось обновить лимиты и ноды из панели: users=%v; nodes=%v", limitsErr, nodeErr)
	}
}

func (c *Client) fetchAllLimits(ctx context.Context) (map[string]int, map[string]UserProfile, map[string]string, error) {
	limits := make(map[string]int)
	profiles := make(map[string]UserProfile)
	nodeAliases := make(map[string]string)
	userUUIDToKeys := make(map[string][]string)
	start := 0
	size := panelUsersPageSize

	for {
		users, err := c.fetchUsersPage(ctx, start, size)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(users) == 0 {
			break
		}

		for _, user := range users {
			limit, hasLimit := extractUserLimit(user)
			displayName := extractUserDisplayName(user)
			lastNodeName, aliases := extractLastConnectedNode(user)
			telegramID := extractUserTelegramID(user)
			description := extractUserDescription(user)
			userUUID := extractUserUUID(user)
			keys := extractUserKeys(user)
			if userUUID != "" && len(keys) > 0 {
				seen := make(map[string]struct{}, len(userUUIDToKeys[userUUID]))
				for _, existing := range userUUIDToKeys[userUUID] {
					seen[existing] = struct{}{}
				}
				for _, key := range keys {
					if key == "" {
						continue
					}
					if _, ok := seen[key]; ok {
						continue
					}
					userUUIDToKeys[userUUID] = append(userUUIDToKeys[userUUID], key)
					seen[key] = struct{}{}
				}
			}
			for _, key := range keys {
				if key != "" {
					if hasLimit && limit > 0 {
						limits[key] = limit
					}
					profile := profiles[key]
					if displayName != "" {
						profile.Username = displayName
					}
					if lastNodeName != "" {
						profile.LastConnectedNode = lastNodeName
					}
					if telegramID != "" {
						profile.TelegramID = telegramID
					}
					if description != "" {
						profile.Description = description
					}
					if profile.Username != "" || profile.LastConnectedNode != "" || profile.TelegramID != "" || profile.Description != "" {
						profiles[key] = profile
					}
				}
			}
			if lastNodeName != "" {
				for _, alias := range aliases {
					alias = normalizeUserID(alias)
					if alias != "" {
						nodeAliases[alias] = lastNodeName
					}
				}
			}
		}

		if len(users) < size {
			break
		}
		start += size
	}

	hwidByUserUUID, hwidErr := c.fetchAllHWIDDevices(ctx)
	if hwidErr != nil {
		log.Printf("Warning: не удалось обновить HWID-устройства из панели: %v", hwidErr)
	} else {
		for userUUID, devices := range hwidByUserUUID {
			keys := userUUIDToKeys[userUUID]
			if len(keys) == 0 {
				continue
			}
			for _, key := range keys {
				profile := profiles[key]
				profile.HWIDDevices = devices
				profiles[key] = profile
			}
		}
	}

	return limits, profiles, nodeAliases, nil
}

func (c *Client) fetchUsersPage(ctx context.Context, start, size int) ([]map[string]any, error) {
	url := fmt.Sprintf("%s/api/users?start=%d&size=%d", c.baseURL, start, size)
	var lastErr error

	for attempt := 1; attempt <= panelReqRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !isRetryablePanelError(err) || attempt == panelReqRetries {
				break
			}
			if !waitRetry(ctx, attempt) {
				break
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("panel API временно недоступен, статус %d", resp.StatusCode)
			resp.Body.Close()
			if attempt == panelReqRetries || !waitRetry(ctx, attempt) {
				break
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("panel API вернул статус %d", resp.StatusCode)
		}

		var payload struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()

		if len(payload.Response) == 0 {
			return nil, nil
		}

		var wrapped struct {
			Users []map[string]any `json:"users"`
		}
		if err := json.Unmarshal(payload.Response, &wrapped); err == nil && len(wrapped.Users) > 0 {
			return wrapped.Users, nil
		}

		var direct []map[string]any
		if err := json.Unmarshal(payload.Response, &direct); err == nil {
			return direct, nil
		}

		return nil, fmt.Errorf("неожиданный формат ответа panel API")
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("не удалось загрузить страницу пользователей")
	}
	return nil, lastErr
}

func (c *Client) fetchAllHWIDDevices(ctx context.Context) (map[string][]HWIDDeviceProfile, error) {
	byUser := make(map[string][]HWIDDeviceProfile)
	start := 0
	size := panelHWIDPageSize

	for {
		devices, total, err := c.fetchHWIDDevicesPage(ctx, start, size)
		if err != nil {
			return nil, err
		}
		if len(devices) == 0 {
			break
		}

		for _, raw := range devices {
			userUUID := normalizeUserID(extractHWIDDeviceUserUUID(raw))
			if userUUID == "" {
				continue
			}

			device := HWIDDeviceProfile{
				HWID:      extractHWIDDeviceValue(raw, "hwid", "deviceId", "device_id"),
				Platform:  extractHWIDDeviceValue(raw, "platform", "devicePlatform", "device_platform"),
				Model:     extractHWIDDeviceValue(raw, "deviceModel", "model", "device_model"),
				OS:        extractHWIDDeviceValue(raw, "osVersion", "os", "os_version"),
				UserAgent: extractHWIDDeviceValue(raw, "userAgent", "ua", "user_agent"),
				CreatedAt: extractHWIDDeviceTime(raw, "createdAt", "created_at"),
				UpdatedAt: extractHWIDDeviceTime(raw, "updatedAt", "updated_at"),
			}

			// Не добавляем полностью пустые записи.
			if device.HWID == "" && device.Platform == "" && device.Model == "" && device.OS == "" && device.UserAgent == "" {
				continue
			}

			byUser[userUUID] = append(byUser[userUUID], device)
		}

		start += size
		if total > 0 && start >= total {
			break
		}
		if len(devices) < size {
			break
		}
	}

	for userUUID, items := range byUser {
		if len(items) <= 1 {
			continue
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].HWID < items[j].HWID
			}
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		})
		byUser[userUUID] = items
	}

	return byUser, nil
}

func (c *Client) fetchHWIDDevicesPage(ctx context.Context, start, size int) ([]map[string]any, int, error) {
	url := fmt.Sprintf("%s/api/hwid/devices?start=%d&size=%d", c.baseURL, start, size)
	var lastErr error

	for attempt := 1; attempt <= panelReqRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !isRetryablePanelError(err) || attempt == panelReqRetries {
				break
			}
			if !waitRetry(ctx, attempt) {
				break
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("panel HWID API временно недоступен, статус %d", resp.StatusCode)
			resp.Body.Close()
			if attempt == panelReqRetries || !waitRetry(ctx, attempt) {
				break
			}
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, 0, nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("panel HWID API вернул статус %d", resp.StatusCode)
		}

		rawBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, 0, err
		}

		var payload struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			return nil, 0, err
		}

		candidate := payload.Response
		if len(candidate) == 0 {
			candidate = rawBody
		}
		candidate = json.RawMessage(strings.TrimSpace(string(candidate)))
		if len(candidate) == 0 {
			return nil, 0, nil
		}

		var wrapped struct {
			Devices []map[string]any `json:"devices"`
			Total   any              `json:"total"`
		}
		if err := json.Unmarshal(candidate, &wrapped); err == nil {
			total, _ := anyToInt(wrapped.Total)
			return wrapped.Devices, total, nil
		}

		var direct []map[string]any
		if err := json.Unmarshal(candidate, &direct); err == nil {
			return direct, len(direct), nil
		}

		return nil, 0, fmt.Errorf("неожиданный формат ответа panel HWID API")
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("не удалось загрузить HWID-устройства")
	}
	return nil, 0, lastErr
}

func (c *Client) fetchAllNodes(ctx context.Context) ([]NodeProfile, map[string]string, error) {
	url := fmt.Sprintf("%s/api/nodes", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("nodes API вернул статус %d", resp.StatusCode)
	}

	items, err := decodeObjectsResponse(resp.Body, []string{"nodes", "items", "data"})
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, nil
	}

	profiles := make([]NodeProfile, 0, len(items))
	aliases := make(map[string]string)
	seen := make(map[string]struct{})
	for _, item := range items {
		name := extractNodeDisplayName(item)
		if name == "" {
			continue
		}
		nodeID := extractNodeID(item)
		profileKey := normalizeUserID(nodeID + "|" + name)
		if _, ok := seen[profileKey]; ok {
			continue
		}
		seen[profileKey] = struct{}{}
		profiles = append(profiles, NodeProfile{
			ID:      nodeID,
			Name:    name,
			Address: extractNodeAddress(item),
		})
		for _, alias := range extractNodeAliases(item) {
			alias = normalizeUserID(alias)
			if alias != "" {
				aliases[alias] = name
			}
		}
	}

	sort.Slice(profiles, func(i, j int) bool {
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	return profiles, aliases, nil
}

func extractUserLimit(user map[string]any) (int, bool) {
	for _, key := range []string{"hwidDeviceLimit", "ipLimit", "limit"} {
		raw, ok := user[key]
		if !ok {
			continue
		}
		limit, ok := anyToInt(raw)
		if ok {
			return limit, true
		}
	}
	return 0, false
}

func extractUserKeys(user map[string]any) []string {
	var keys []string
	for _, key := range []string{"id", "user_identifier", "email", "username"} {
		raw, ok := user[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			normalized := normalizeUserID(str)
			if normalized != "" {
				keys = append(keys, normalized)
			}
		}
	}
	return keys
}

func extractUserUUID(user map[string]any) string {
	for _, key := range []string{"uuid", "userUuid", "user_uuid"} {
		raw, ok := user[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = normalizeUserID(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractHWIDDeviceUserUUID(device map[string]any) string {
	for _, key := range []string{"userUuid", "user_uuid", "uuid"} {
		raw, ok := device[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractHWIDDeviceValue(device map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := device[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractHWIDDeviceTime(device map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		raw, ok := device[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(str)); err == nil {
				return parsed
			}
			if parsed, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(str)); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}

func extractUserDisplayName(user map[string]any) string {
	for _, key := range []string{"username", "email", "id", "user_identifier"} {
		raw, ok := user[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractUserTelegramID(user map[string]any) string {
	for _, key := range []string{"telegramId", "telegram_id", "tg_id", "tgId", "telegram"} {
		raw, ok := user[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractUserDescription(user map[string]any) string {
	for _, key := range []string{"description", "remark", "remarks", "note", "comment"} {
		raw, ok := user[key]
		if !ok || raw == nil {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractLastConnectedNode(user map[string]any) (string, []string) {
	for _, key := range []string{"lastConnectedNode", "last_connected_node", "node"} {
		raw, ok := user[key]
		if !ok || raw == nil {
			continue
		}

		switch v := raw.(type) {
		case string:
			name := strings.TrimSpace(v)
			if name != "" {
				return name, []string{name}
			}
		case map[string]any:
			name := extractNodeDisplayName(v)
			if name == "" {
				continue
			}
			return name, extractNodeAliases(v)
		}
	}
	return "", nil
}

func extractNodeDisplayName(node map[string]any) string {
	for _, key := range []string{"name", "nodeName", "displayName", "remark", "remarks", "tag", "country", "label"} {
		raw, ok := node[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractNodeID(node map[string]any) string {
	for _, key := range []string{"uuid", "id", "nodeId"} {
		raw, ok := node[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				return str
			}
		}
	}
	return ""
}

func extractNodeAliases(node map[string]any) []string {
	aliases := make([]string, 0, 8)
	for _, key := range []string{"uuid", "id", "nodeId", "name", "nodeName", "address", "host", "domain", "country", "tag", "label"} {
		raw, ok := node[key]
		if !ok {
			continue
		}
		if str, ok := anyToString(raw); ok {
			str = strings.TrimSpace(str)
			if str != "" {
				aliases = append(aliases, str)
			}
		}
	}
	return aliases
}

func decodeObjectsResponse(body io.Reader, collectionKeys []string) ([]map[string]any, error) {
	rawBody, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	payload := struct {
		Response json.RawMessage `json:"response"`
	}{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, err
	}

	candidate := payload.Response
	if len(candidate) == 0 {
		candidate = rawBody
	}

	var direct []map[string]any
	if err := json.Unmarshal(candidate, &direct); err == nil {
		return direct, nil
	}

	var wrapped map[string]any
	if err := json.Unmarshal(candidate, &wrapped); err != nil {
		return nil, fmt.Errorf("неожиданный формат response")
	}
	for _, key := range collectionKeys {
		raw, ok := wrapped[key]
		if !ok {
			continue
		}
		if arr, ok := raw.([]any); ok {
			out := make([]map[string]any, 0, len(arr))
			for _, item := range arr {
				if obj, ok := item.(map[string]any); ok {
					out = append(out, obj)
				}
			}
			return out, nil
		}
	}
	return nil, nil
}

func anyToInt(v any) (int, bool) {
	switch value := v.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case json.Number:
		i, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

func anyToString(v any) (string, bool) {
	switch value := v.(type) {
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case float64:
		return strconv.FormatInt(int64(value), 10), true
	case int:
		return strconv.Itoa(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	default:
		return "", false
	}
}

func lookupUserKeys(rawID string, delimiter string) []string {
	normalized := normalizeUserID(rawID)
	if normalized == "" {
		return nil
	}
	keys := []string{normalized}
	delimiter = strings.TrimSpace(delimiter)
	if delimiter == "" {
		return keys
	}
	if idx := strings.LastIndex(normalized, delimiter); idx > 0 {
		base := strings.TrimSpace(normalized[:idx])
		if base != "" && base != normalized {
			keys = append(keys, base)
		}
	}
	return keys
}

func normalizeUserID(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func isRetryablePanelError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "tempor") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused")
}

func waitRetry(ctx context.Context, attempt int) bool {
	delay := time.Duration(attempt) * time.Second
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
