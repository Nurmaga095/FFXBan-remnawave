package models

import "time"

// LogEntry представляет запись лога, поступающую в сервис.
type LogEntry struct {
	UserEmail string `json:"user_email" binding:"required"`
	SourceIP  string `json:"source_ip" binding:"required"`
	NodeName  string `json:"node_name,omitempty"`
	UserAgent string `json:"user_agent,omitempty"` // Если задан — сохраняется как отчёт об устройстве
}

// DeviceInfo хранит информацию об устройстве пользователя.
type DeviceInfo struct {
	HWID      string `json:"hwid"`
	Platform  string `json:"platform"`
	Model     string `json:"model"`
	OS        string `json:"os"`
	UserAgent string `json:"user_agent"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// AlertPayload представляет данные для отправки в вебхук.
type AlertPayload struct {
	UserIdentifier   string   `json:"user_identifier"`
	Username         string   `json:"username,omitempty"`
	DetectedIPsCount int      `json:"detected_ips_count"`
	Limit            int      `json:"limit"`
	AllUserIPs       []string `json:"all_user_ips"`
	BlockDuration    string   `json:"block_duration"`
	ViolationType    string   `json:"violation_type"`
	Reason           string   `json:"reason,omitempty"`
	NodeName         string   `json:"node_name,omitempty"`
	NetworkType      string   `json:"network_type,omitempty"`
}

// UserIPStats содержит статистику по IP-адресам пользователя для мониторинга.
type UserIPStats struct {
	Email            string   `json:"email"`
	IPCount          int      `json:"ip_count"`
	Limit            int      `json:"limit"`
	IPs              []string `json:"ips"`
	IPsWithTTL       []string `json:"ips_with_ttl"`
	MinTTLHours      float64  `json:"min_ttl_hours"`
	MaxTTLHours      float64  `json:"max_ttl_hours"`
	Status           string   `json:"status"`
	HasAlertCooldown bool     `json:"has_alert_cooldown"`
	IsExcluded       bool     `json:"excluded"`
	IsDebug          bool     `json:"is_debug"`
}

// BlockMessage представляет сообщение для отправки в очередь на блокировку.
type BlockMessage struct {
	Action   string   `json:"action,omitempty"`
	IPs      []string `json:"ips"`
	Duration string   `json:"duration"`
}

// CheckResult представляет результат выполнения Lua-скрипта.
type CheckResult struct {
	StatusCode     int64
	CurrentIPCount int64
	IsNewIP        bool
	AllUserIPs     []string
}

// HeuristicMetrics содержит метрики anti-false-positive для пользователя.
type HeuristicMetrics struct {
	Samples          int       `json:"samples"`
	UniqueIPs        int       `json:"unique_ips"`
	UniqueSubnet24   int       `json:"unique_subnet24"`
	SwitchRate       float64   `json:"switch_rate"`
	DiversityRatio   float64   `json:"diversity_ratio"`
	LastNode         string    `json:"last_node"`
	LastEventAt      time.Time `json:"last_event_at,omitempty"`
	PassedHeuristics bool      `json:"passed_heuristics"`
}

// UserEventLog представляет событие по пользователю для истории в карточке.
type UserEventLog struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	SourceIP  string    `json:"source_ip,omitempty"`
	NodeName  string    `json:"node_name,omitempty"`
}

// ActiveIPInfo содержит активный IP пользователя с TTL.
type ActiveIPInfo struct {
	IP              string `json:"ip"`
	TTLSeconds      int    `json:"ttl_seconds"`
	Subnet24        string `json:"subnet24"`
	NodeName        string `json:"node_name,omitempty"`
	NodeDisplay     string `json:"node_display,omitempty"`
	NetworkType     string `json:"network_type,omitempty"`
	NetworkProvider string `json:"network_provider,omitempty"`
	NetworkCountry  string `json:"network_country,omitempty"`
	NetworkOrg      string `json:"network_org,omitempty"`
}

// NetworkInfo содержит классификацию IP по типу сети.
type NetworkInfo struct {
	Type         string  `json:"type"`
	Provider     string  `json:"provider,omitempty"`
	Country      string  `json:"country,omitempty"`
	Organization string  `json:"organization,omitempty"`
	Source       string  `json:"source,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// GlobalUserEventLog представляет событие для общего журнала по всем пользователям.
type GlobalUserEventLog struct {
	UserIdentifier string    `json:"user_identifier"`
	Timestamp      time.Time `json:"timestamp"`
	Type           string    `json:"type"`
	Message        string    `json:"message"`
	SourceIP       string    `json:"source_ip,omitempty"`
	NodeName       string    `json:"node_name,omitempty"`
}

// BlockerReport представляет сообщение от blocker о факте блокировки/разблокировки или heartbeat.
type BlockerReport struct {
	Type      string    `json:"type"` // action | heartbeat
	Action    string    `json:"action,omitempty"`
	IP        string    `json:"ip,omitempty"`
	Duration  string    `json:"duration,omitempty"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
	NodeName  string    `json:"node_name,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// ActiveBanInfo представляет подтвержденную активную блокировку IP.
type ActiveBanInfo struct {
	IP          string    `json:"ip"`
	NodeName    string    `json:"node_name,omitempty"`
	Duration    string    `json:"duration,omitempty"`
	SetAt       time.Time `json:"set_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	SecondsLeft int       `json:"seconds_left,omitempty"`
}

// SharingStatus содержит runtime-состояние детекции шаринга по пользователю.
type SharingStatus struct {
	UserIdentifier         string    `json:"user_identifier"`
	ConcurrentIPs          int       `json:"concurrent_ips"`
	ConcurrentLimit        int       `json:"concurrent_limit"`
	TriggerHits            int       `json:"trigger_hits"`
	TriggerRequired        int       `json:"trigger_required"`
	TriggerPeriodSeconds   int       `json:"trigger_period_seconds"`
	Violator               bool      `json:"violator"`
	ViolatorSince          time.Time `json:"violator_since,omitempty"`
	InBanlist              bool      `json:"in_banlist"`
	BanlistSince           time.Time `json:"banlist_since,omitempty"`
	PermanentBan           bool      `json:"permanent_ban"`
	PermanentBanSince      time.Time `json:"permanent_ban_since,omitempty"`
	ViolationDurationSec   int       `json:"violation_duration_seconds,omitempty"`
	ViolationIPs           []string  `json:"violation_ips,omitempty"`
	ViolationNodes         []string  `json:"violation_nodes,omitempty"`
	LastTriggerAt          time.Time `json:"last_trigger_at,omitempty"`
	CurrentExceedsInWindow bool      `json:"current_exceeds_in_window"`
	BanCount               int       `json:"ban_count,omitempty"`
	GeoCountries           []string  `json:"geo_countries,omitempty"`
	GeoMismatch            bool      `json:"geo_mismatch,omitempty"`
}

// NodeHealthInfo представляет состояние узла по heartbeat/трафику.
type NodeHealthInfo struct {
	NodeID              string    `json:"node_id"`
	NodeName            string    `json:"node_name"`
	Status              string    `json:"status"` // online | offline
	Online              bool      `json:"online"`
	LastTrafficAt       time.Time `json:"last_traffic_at,omitempty"`
	LastBlockerBeatAt   time.Time `json:"last_blocker_heartbeat_at,omitempty"`
	LastBlockerActionAt time.Time `json:"last_blocker_action_at,omitempty"`
}

// NodeRuntimeState внутреннее состояние узла в рантайме.
type NodeRuntimeState struct {
	LastTrafficAt       time.Time
	LastBlockerBeatAt   time.Time
	LastBlockerActionAt time.Time
}
