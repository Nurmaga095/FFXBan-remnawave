package alerter

import (
	"bytes"
	"encoding/json"
	"ffxban/internal/models"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Notifier определяет интерфейс для отправки уведомлений.
type Notifier interface {
	SendAlert(payload models.AlertPayload) error
}

// WebhookAlerter реализует Notifier для отправки вебхуков.
type WebhookAlerter struct {
	client         *http.Client
	url            string
	mode           string
	authToken      string
	authHeader     string
	usernamePrefix string
}

// NewWebhookAlerter создает новый экземпляр WebhookAlerter.
func NewWebhookAlerter(url, mode, authToken, authHeader, usernamePrefix string) *WebhookAlerter {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "legacy"
	}
	authHeader = strings.TrimSpace(authHeader)
	if authHeader == "" {
		authHeader = "X-API-Key"
	}
	if strings.TrimSpace(usernamePrefix) == "" {
		usernamePrefix = "user_"
	}
	return &WebhookAlerter{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		url:            strings.TrimSpace(url),
		mode:           mode,
		authToken:      strings.TrimSpace(authToken),
		authHeader:     authHeader,
		usernamePrefix: usernamePrefix,
	}
}

// SendAlert отправляет уведомление на заданный URL.
func (a *WebhookAlerter) SendAlert(payload models.AlertPayload) error {
	if a.url == "" {
		log.Println("ALERT_WEBHOOK_URL не задан, вебхук не отправляется")
		return nil
	}

	body, err := a.buildPayload(payload)
	if err != nil {
		return fmt.Errorf("ошибка сборки payload: %w", err)
	}

	log.Printf("Попытка отправить вебхук на URL: %s", a.url)
	req, err := http.NewRequest(http.MethodPost, a.url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("ошибка создания запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	a.applyAuth(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("сетевая ошибка при отправке вебхука: %w", err)
	}
	defer resp.Body.Close()

	var respBody bytes.Buffer
	_, _ = respBody.ReadFrom(resp.Body)

	log.Printf("Вебхук-уведомление для %s отправлен. Статус ответа: %d. Тело ответа: %s",
		payload.UserIdentifier, resp.StatusCode, respBody.String())

	if resp.StatusCode >= 400 {
		return fmt.Errorf("сервер вебхука ответил ошибкой: %s", resp.Status)
	}
	if a.mode == "ban_api" || a.mode == "remnawave_bot" {
		var apiResp banAPIResponse
		if err := json.Unmarshal(respBody.Bytes(), &apiResp); err == nil {
			if !apiResp.Success || !apiResp.Sent {
				msg := strings.TrimSpace(apiResp.Message)
				if msg == "" {
					msg = "unknown webhook rejection"
				}
				return fmt.Errorf("ban-api отклонил уведомление: %s", msg)
			}
		}
	}

	return nil
}

type banNotificationPayload struct {
	NotificationType string `json:"notification_type"`
	UserIdentifier   string `json:"user_identifier"`
	Username         string `json:"username"`
	IPCount          int    `json:"ip_count,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	BanMinutes       int    `json:"ban_minutes,omitempty"`
	NodeName         string `json:"node_name,omitempty"`
	NetworkType      string `json:"network_type,omitempty"`
	WarningMessage   string `json:"warning_message,omitempty"`
}

type banAPIResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Sent    bool   `json:"sent"`
}

func (a *WebhookAlerter) buildPayload(payload models.AlertPayload) ([]byte, error) {
	switch a.mode {
	case "ban_api", "remnawave_bot":
		notificationType := mapNotificationType(payload)
		username := strings.TrimSpace(payload.Username)
		if username == "" {
			username = a.deriveUsername(payload.UserIdentifier)
		}
		if a.shouldPrefixUsername(username) {
			username = a.deriveUsername(username)
		}
		identifier := strings.TrimSpace(payload.UserIdentifier)
		if username != "" {
			// ban-api в боте ищет пользователя по user_identifier формата user_<telegram_id>.
			// В наших данных это обычно поле username из панели.
			identifier = username
		} else if identifier == "" {
			identifier = a.deriveUsername(payload.UserIdentifier)
		}
		req := banNotificationPayload{
			NotificationType: notificationType,
			UserIdentifier:   identifier,
			Username:         username,
			NodeName:         strings.TrimSpace(payload.NodeName),
			NetworkType:      strings.TrimSpace(payload.NetworkType),
		}
		if req.NotificationType == "punishment" || req.NotificationType == "network_wifi" || req.NotificationType == "network_mobile" {
			req.IPCount = payload.DetectedIPsCount
			req.Limit = payload.Limit
			req.BanMinutes = parseBanMinutes(payload.BlockDuration)
		}
		if req.NotificationType == "warning" {
			req.WarningMessage = buildWarningMessage(payload)
		}
		return json.Marshal(req)
	default:
		return json.Marshal(payload)
	}
}

func (a *WebhookAlerter) applyAuth(req *http.Request) {
	if strings.TrimSpace(a.authToken) == "" {
		return
	}
	header := strings.TrimSpace(a.authHeader)
	if strings.EqualFold(header, "authorization") {
		req.Header.Set("Authorization", "Bearer "+a.authToken)
		return
	}
	if header == "" {
		header = "X-API-Key"
	}
	req.Header.Set(header, a.authToken)
}

func (a *WebhookAlerter) deriveUsername(userIdentifier string) string {
	id := strings.TrimSpace(userIdentifier)
	if id == "" {
		return "unknown"
	}
	prefix := strings.TrimSpace(a.usernamePrefix)
	if prefix != "" && !strings.HasPrefix(id, prefix) {
		return prefix + id
	}
	return id
}

func (a *WebhookAlerter) shouldPrefixUsername(username string) bool {
	username = strings.TrimSpace(username)
	prefix := strings.TrimSpace(a.usernamePrefix)
	if username == "" || prefix == "" || strings.HasPrefix(username, prefix) {
		return false
	}
	for _, ch := range username {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func mapNotificationType(payload models.AlertPayload) string {
	switch strings.ToLower(strings.TrimSpace(payload.ViolationType)) {
	case "sharing_permanent_ban", "permanent_ban", "permanent":
		return "warning"
	case "enabled":
		return "enabled"
	case "network_wifi", "network_mobile", "sharing_suspected", "ip_limit_exceeded":
		return "warning"
	}

	return "warning"
}

func parseBanMinutes(raw string) int {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 5
	}
	if s == "permanent" {
		return 525600 // 1 год для отображения как "очень долго"
	}
	if d, err := time.ParseDuration(s); err == nil {
		minutes := int(d / time.Minute)
		if d%time.Minute != 0 {
			minutes++
		}
		if minutes < 1 {
			minutes = 1
		}
		return minutes
	}
	if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "h") || strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "m"), "h"), "d"))
		if err == nil && n > 0 {
			switch {
			case strings.HasSuffix(s, "m"):
				return n
			case strings.HasSuffix(s, "h"):
				return n * 60
			case strings.HasSuffix(s, "d"):
				return n * 1440
			}
		}
	}
	return 5
}

func normalizeNetworkType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "wifi", "mobile", "cable":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "unknown"
	}
}

func buildWarningMessage(payload models.AlertPayload) string {
	node := strings.TrimSpace(payload.NodeName)
	if node == "" {
		node = "unknown"
	}
	networkType := normalizeNetworkType(payload.NetworkType)
	if networkType == "unknown" {
		networkType = "-"
	}
	banMinutes := parseBanMinutes(payload.BlockDuration)

	if strings.EqualFold(strings.TrimSpace(payload.ViolationType), "manual_block") {
		reason := strings.TrimSpace(payload.Reason)
		if reason == "" {
			reason = "ручная блокировка администратором"
		}
		if strings.EqualFold(strings.TrimSpace(payload.BlockDuration), "permanent") {
			return fmt.Sprintf(
				"⛔ АККАУНТ ЗАБЛОКИРОВАН НАВСЕГДА\n\n❌ Причина: %s\n💻 Нода: %s\n📊 Лимит устройств: %d\n📶 Зафиксировано IP: %d\n\n💡 Если блокировка ошибочная, обратитесь в поддержку.",
				reason,
				node,
				payload.Limit,
				payload.DetectedIPsCount,
			)
		}
		return fmt.Sprintf(
			"🚫 АККАУНТ ВРЕМЕННО ЗАБЛОКИРОВАН\n\n❌ Причина: %s\n💻 Нода: %s\n📊 Детали:\n  ├ Лимит устройств: %d\n  ├ Зафиксировано IP: %d\n  ├ Тип сети: %s\n  └ Время блокировки: %d мин\n\n🔄 Доступ восстановится автоматически",
			reason,
			node,
			payload.Limit,
			payload.DetectedIPsCount,
			networkType,
			banMinutes,
		)
	}

	if strings.EqualFold(strings.TrimSpace(payload.ViolationType), "sharing_permanent_ban") ||
		strings.EqualFold(strings.TrimSpace(payload.BlockDuration), "permanent") {
		return fmt.Sprintf(
			"⛔ АККАУНТ ЗАБЛОКИРОВАН НАВСЕГДА\n\n❌ Причина: подтверждена передача подписки (стабильное нарушение лимита устройств)\n💻 Нода: %s\n📊 Лимит устройств: %d\n📶 Зафиксировано IP: %d\n\n💡 Что делать:\n1. Обратитесь в поддержку\n2. Подготовьте объяснение по использованию аккаунта\n3. После проверки возможен пересмотр блокировки",
			node,
			payload.Limit,
			payload.DetectedIPsCount,
		)
	}

	return fmt.Sprintf(
		"🚫 АККАУНТ ВРЕМЕННО ЗАБЛОКИРОВАН\n\n❌ Причина: подозрение на передачу подписки\n💻 Нода: %s\n📊 Детали:\n  ├ Лимит устройств: %d\n  ├ Зафиксировано IP: %d\n  ├ Тип сети: %s\n  └ Время блокировки: %d мин\n\n💡 Что делать:\n1. Отключите лишние устройства\n2. Дождитесь окончания блокировки\n3. Не передавайте доступ третьим лицам\n\n🔄 Доступ восстановится автоматически",
		node,
		payload.Limit,
		payload.DetectedIPsCount,
		networkType,
		banMinutes,
	)
}
