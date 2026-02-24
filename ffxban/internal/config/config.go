package config

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config хранит всю конфигурацию приложения.
type Config struct {
	Port                        string
	RedisURL                    string
	RedisHousekeepingEnabled    bool
	RedisHousekeepingInterval   time.Duration
	RedisHousekeepingMaxKeys    int
	RabbitMQURL                 string
	MaxIPsPerUser               int
	AlertWebhookURL             string
	AlertWebhookMode            string
	AlertWebhookToken           string
	AlertWebhookAuthHeader      string
	AlertWebhookUsernamePrefix  string
	InternalAPIToken            string
	PanelPassword               string
	PanelSessionTTL             time.Duration
	PanelURL                    string
	PanelToken                  string
	PanelReloadInterval         time.Duration
	PanelOnDemandSyncMaxAge     time.Duration
	UserIPTTL                   time.Duration
	AlertCooldown               time.Duration
	ClearIPsDelay               time.Duration
	BlockDuration               string
	BlockingExchangeName        string
	BlockingStatusExchangeName  string
	MonitoringInterval          time.Duration
	NodeHeartbeatTimeout        time.Duration
	DebugEmail                  string
	DebugIPLimit                int
	ExcludedUsers               map[string]bool
	ExcludedIPs                 map[string]bool
	WorkerPoolSize              int
	LogChannelBufferSize        int
	SideEffectWorkerPoolSize    int
	SideEffectChannelBufferSize int
	HeuristicsEnabled           bool
	HeuristicsWindow            time.Duration
	HeuristicsMinSamples        int
	HeuristicsMaxSamples        int
	HeuristicsMinSubnet24       int
	HeuristicsMinSwitchRate     float64
	HeuristicsMinDiversityRatio float64
	NetworkDetectEnabled        bool
	NetworkLookupProvider       string
	NetworkLookupURL            string
	NetworkLookupToken          string
	NetworkLookupTimeout        time.Duration
	NetworkCacheTTL             time.Duration
	NetworkPolicyEnabled        bool
	NetworkMobileGraceIPs       int
	NetworkWifiGraceIPs         int
	NetworkCableGraceIPs        int
	NetworkUnknownGraceIPs      int
	NetworkMixedGraceIPs        int
	NetworkForceBlockExcessIPs  int
	PerDeviceKeyDelimiter       string
	SharingDetectionEnabled     bool
	ConcurrentWindow            time.Duration
	SharingSourceWindow         time.Duration
	SharingSustain              time.Duration
	TriggerPeriod               time.Duration
	TriggerCount                int
	TriggerMinInterval          time.Duration
	BanlistThreshold            time.Duration
	SharingPermanentBanEnabled  bool
	SharingPermanentBanDuration string
	SharingHardwareGuardEnabled bool
	SharingBlockOnViolatorOnly  bool
	SharingBlockOnBanlistOnly   bool
	SubnetGrouping              bool
	BanEscalationEnabled        bool
	BanEscalationDurations      []string
	GeoSharingEnabled           bool
	GeoSharingMinCountries      int
	PrometheusEnabled           bool

	// 3x-ui Configuration
	ThreexuiEnabled   bool
	ThreexuiServers   []ThreexuiServer
	ThreexuiBlockMode string // "api_only", "api_and_nftables"
}

type ThreexuiServer struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// New загружает конфигурацию из переменных окружения.
func New() *Config {
	cfg := &Config{
		Port:                        getEnv("PORT", "9000"),
		RedisURL:                    getEnv("REDIS_URL", "redis://localhost:6379/0"),
		RedisHousekeepingEnabled:    getEnvBool("REDIS_HOUSEKEEPING_ENABLED", true),
		RedisHousekeepingInterval:   time.Duration(getEnvInt("REDIS_HOUSEKEEPING_INTERVAL_SECONDS", 600)) * time.Second,
		RedisHousekeepingMaxKeys:    getEnvInt("REDIS_HOUSEKEEPING_MAX_KEYS", 5000),
		RabbitMQURL:                 getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost/"),
		MaxIPsPerUser:               getEnvInt("MAX_IPS_PER_USER", 3),
		AlertWebhookURL:             getEnv("ALERT_WEBHOOK_URL", ""),
		AlertWebhookMode:            strings.ToLower(strings.TrimSpace(getEnv("ALERT_WEBHOOK_MODE", "legacy"))),
		AlertWebhookToken:           getEnv("ALERT_WEBHOOK_TOKEN", ""),
		AlertWebhookAuthHeader:      getEnv("ALERT_WEBHOOK_AUTH_HEADER", "X-API-Key"),
		AlertWebhookUsernamePrefix:  getEnv("ALERT_WEBHOOK_USERNAME_PREFIX", "user_"),
		InternalAPIToken:            getEnv("INTERNAL_API_TOKEN", ""),
		PanelPassword:               getEnv("PANEL_PASSWORD", ""),
		PanelSessionTTL:             time.Duration(getEnvInt("PANEL_SESSION_TTL_HOURS", 24)) * time.Hour,
		PanelURL:                    getEnv("PANEL_URL", ""),
		PanelToken:                  getEnv("PANEL_TOKEN", ""),
		PanelReloadInterval:         time.Duration(getEnvInt("PANEL_RELOAD_INTERVAL_SECONDS", 300)) * time.Second,
		PanelOnDemandSyncMaxAge:     time.Duration(getEnvInt("PANEL_ON_DEMAND_SYNC_MAX_AGE_SECONDS", 20)) * time.Second,
		UserIPTTL:                   time.Duration(getEnvInt("USER_IP_TTL_SECONDS", 24*60*60)) * time.Second,
		AlertCooldown:               time.Duration(getEnvInt("ALERT_COOLDOWN_SECONDS", 60*60)) * time.Second,
		ClearIPsDelay:               time.Duration(getEnvInt("CLEAR_IPS_DELAY_SECONDS", 30)) * time.Second,
		BlockDuration:               getEnv("BLOCK_DURATION", "5m"),
		BlockingExchangeName:        getEnv("BLOCKING_EXCHANGE_NAME", "blocking_exchange"),
		BlockingStatusExchangeName:  getEnv("BLOCKING_STATUS_EXCHANGE_NAME", "blocking_status_exchange"),
		MonitoringInterval:          time.Duration(getEnvInt("MONITORING_INTERVAL", 300)) * time.Second,
		NodeHeartbeatTimeout:        time.Duration(getEnvInt("NODE_HEARTBEAT_TIMEOUT_SECONDS", 120)) * time.Second,
		DebugEmail:                  getEnv("DEBUG_EMAIL", ""),
		DebugIPLimit:                getEnvInt("DEBUG_IP_LIMIT", 1),
		ExcludedUsers:               mergeSets(parseSet(getEnv("EXCLUDED_USERS", "")), parseSet(getEnv("WHITELIST_EMAILS", ""))),
		ExcludedIPs:                 parseSet(getEnv("EXCLUDED_IPS", "")),
		WorkerPoolSize:              getEnvInt("WORKER_POOL_SIZE", 20),
		LogChannelBufferSize:        getEnvInt("LOG_CHANNEL_BUFFER_SIZE", 100),
		SideEffectWorkerPoolSize:    getEnvInt("SIDE_EFFECT_WORKER_POOL_SIZE", 10),
		SideEffectChannelBufferSize: getEnvInt("SIDE_EFFECT_CHANNEL_BUFFER_SIZE", 50),
		HeuristicsEnabled:           getEnvBool("HEURISTICS_ENABLED", true),
		HeuristicsWindow:            time.Duration(getEnvInt("HEURISTICS_WINDOW_SECONDS", 120)) * time.Second,
		HeuristicsMinSamples:        getEnvInt("HEURISTICS_MIN_SAMPLES", 8),
		HeuristicsMaxSamples:        getEnvInt("HEURISTICS_MAX_SAMPLES", 30),
		HeuristicsMinSubnet24:       getEnvInt("HEURISTICS_MIN_SUBNET24", 2),
		HeuristicsMinSwitchRate:     getEnvFloat("HEURISTICS_MIN_SWITCH_RATE", 0.45),
		HeuristicsMinDiversityRatio: getEnvFloat("HEURISTICS_MIN_DIVERSITY_RATIO", 0.35),
		NetworkDetectEnabled:        getEnvBool("NETWORK_DETECT_ENABLED", true),
		NetworkLookupProvider:       strings.ToLower(strings.TrimSpace(getEnv("NETWORK_LOOKUP_PROVIDER", "ipwho"))),
		NetworkLookupURL:            getEnv("NETWORK_LOOKUP_URL", "https://ipwho.is/{ip}"),
		NetworkLookupToken:          getEnv("NETWORK_LOOKUP_TOKEN", ""),
		NetworkLookupTimeout:        time.Duration(getEnvInt("NETWORK_LOOKUP_TIMEOUT_SECONDS", 3)) * time.Second,
		NetworkCacheTTL:             time.Duration(getEnvInt("NETWORK_CACHE_TTL_SECONDS", 3600)) * time.Second,
		NetworkPolicyEnabled:        getEnvBool("NETWORK_POLICY_ENABLED", true),
		NetworkMobileGraceIPs:       getEnvInt("NETWORK_MOBILE_GRACE_IPS", 1),
		NetworkWifiGraceIPs:         getEnvInt("NETWORK_WIFI_GRACE_IPS", 0),
		NetworkCableGraceIPs:        getEnvInt("NETWORK_CABLE_GRACE_IPS", 0),
		NetworkUnknownGraceIPs:      getEnvInt("NETWORK_UNKNOWN_GRACE_IPS", 0),
		NetworkMixedGraceIPs:        getEnvInt("NETWORK_MIXED_GRACE_IPS", 0),
		NetworkForceBlockExcessIPs:  getEnvInt("NETWORK_FORCE_BLOCK_EXCESS_IPS", 3),
		PerDeviceKeyDelimiter:       strings.TrimSpace(getEnv("PER_DEVICE_KEY_DELIMITER", "#")),
		SharingDetectionEnabled:     getEnvBool("SHARING_DETECTION_ENABLED", true),
		ConcurrentWindow:            time.Duration(getEnvInt("CONCURRENT_WINDOW_SECONDS", 2)) * time.Second,
		SharingSourceWindow:         time.Duration(getEnvInt("SHARING_SOURCE_WINDOW_SECONDS", 180)) * time.Second,
		SharingSustain:              time.Duration(getEnvInt("SHARING_SUSTAIN_SECONDS", 90)) * time.Second,
		TriggerPeriod:               time.Duration(getEnvInt("TRIGGER_PERIOD_SECONDS", 30)) * time.Second,
		TriggerCount:                getEnvInt("TRIGGER_COUNT", 5),
		TriggerMinInterval:          time.Duration(getEnvInt("TRIGGER_MIN_INTERVAL_SECONDS", 6)) * time.Second,
		BanlistThreshold:            time.Duration(getEnvInt("BANLIST_THRESHOLD_SECONDS", 300)) * time.Second,
		SharingPermanentBanEnabled:  getEnvBool("SHARING_PERMANENT_BAN_ENABLED", true),
		SharingPermanentBanDuration: strings.TrimSpace(getEnv("SHARING_PERMANENT_BAN_DURATION", "permanent")),
		SharingHardwareGuardEnabled: getEnvBool("SHARING_HARDWARE_GUARD_ENABLED", true),
		SharingBlockOnViolatorOnly:  getEnvBool("SHARING_BLOCK_ON_VIOLATOR_ONLY", true),
		SharingBlockOnBanlistOnly:   getEnvBool("SHARING_BLOCK_ON_BANLIST_ONLY", true),
		SubnetGrouping:              getEnvBool("SUBNET_GROUPING", true),
		BanEscalationEnabled:        getEnvBool("BAN_ESCALATION_ENABLED", false),
		BanEscalationDurations:      parseStringSlice(getEnv("BAN_ESCALATION_DURATIONS", "1h,6h,24h,permanent")),
		GeoSharingEnabled:           getEnvBool("GEO_SHARING_ENABLED", false),
		GeoSharingMinCountries:      getEnvInt("GEO_SHARING_MIN_COUNTRIES", 2),
		PrometheusEnabled:           getEnvBool("PROMETHEUS_ENABLED", false),
		ThreexuiEnabled:             getEnvBool("THREEXUI_ENABLED", false),
		ThreexuiServers:             parseThreexuiServers(getEnv("THREEXUI_SERVERS", "[]")),
		ThreexuiBlockMode:           getEnv("THREEXUI_BLOCK_MODE", "api_only"),
	}
	if cfg.SharingPermanentBanDuration == "" {
		cfg.SharingPermanentBanDuration = "permanent"
	}

	// Обязательные параметры безопасности
	if strings.TrimSpace(cfg.PanelPassword) == "" {
		log.Fatal("Критическая ошибка: переменная PANEL_PASSWORD не задана. Установите надёжный пароль для панели администратора в файле .env")
	}
	if strings.TrimSpace(cfg.InternalAPIToken) == "" {
		log.Println("Предупреждение: INTERNAL_API_TOKEN не задан. API-эндпоинты будут доступны только через сессию панели.")
	}

	log.Printf("Конфигурация загружена. Порт: %s", cfg.Port)
	log.Printf("Пул воркеров обработки логов: %d воркеров, размер буфера канала: %d", cfg.WorkerPoolSize, cfg.LogChannelBufferSize)
	log.Printf("Пул воркеров побочных задач (алерты, очистка): %d воркеров, размер буфера канала: %d", cfg.SideEffectWorkerPoolSize, cfg.SideEffectChannelBufferSize)
	if len(cfg.ExcludedUsers) > 0 {
		log.Printf("Загружен список исключений: %d пользователей", len(cfg.ExcludedUsers))
	}
	if len(cfg.ExcludedIPs) > 0 {
		log.Printf("Загружен список исключений IP-адресов: %d", len(cfg.ExcludedIPs))
	}
	if cfg.DebugEmail != "" {
		log.Printf("Режим дебага включен для email: %s с лимитом IP: %d", cfg.DebugEmail, cfg.DebugIPLimit)
	}
	if cfg.PanelURL != "" && cfg.PanelToken != "" {
		log.Printf("Динамические лимиты из панели включены: %s", cfg.PanelURL)
		log.Printf(
			"Синхронизация лимитов панели: interval=%v on_demand_max_age=%v",
			cfg.PanelReloadInterval,
			cfg.PanelOnDemandSyncMaxAge,
		)
	}
	log.Printf(
		"Эвристики anti-false-positive: enabled=%t window=%v min_samples=%d min_subnet24=%d min_switch=%.2f min_diversity=%.2f",
		cfg.HeuristicsEnabled,
		cfg.HeuristicsWindow,
		cfg.HeuristicsMinSamples,
		cfg.HeuristicsMinSubnet24,
		cfg.HeuristicsMinSwitchRate,
		cfg.HeuristicsMinDiversityRatio,
	)
	log.Printf(
		"Runtime health: node_heartbeat_timeout=%v, status_exchange=%s",
		cfg.NodeHeartbeatTimeout,
		cfg.BlockingStatusExchangeName,
	)
	log.Printf(
		"Network detection: enabled=%t provider=%s lookup=%s cache_ttl=%v",
		cfg.NetworkDetectEnabled,
		cfg.NetworkLookupProvider,
		cfg.NetworkLookupURL,
		cfg.NetworkCacheTTL,
	)
	log.Printf(
		"Network policy: enabled=%t grace(mobile=%d,wifi=%d,cable=%d,unknown=%d,mixed=%d) force_excess=%d",
		cfg.NetworkPolicyEnabled,
		cfg.NetworkMobileGraceIPs,
		cfg.NetworkWifiGraceIPs,
		cfg.NetworkCableGraceIPs,
		cfg.NetworkUnknownGraceIPs,
		cfg.NetworkMixedGraceIPs,
		cfg.NetworkForceBlockExcessIPs,
	)
	log.Printf(
		"Per-device key mapping: delimiter=%q",
		cfg.PerDeviceKeyDelimiter,
	)
	log.Printf(
		"Sharing detection: enabled=%t simplified_mode=%t source_window=%v sustain=%v trigger_count=%d trigger_min_interval=%v banlist_threshold=%v subnet_grouping=%t permanent_ban=%t duration=%s block_on_violator_only=%t block_on_banlist_only=%t",
		cfg.SharingDetectionEnabled,
		true,
		cfg.SharingSourceWindow,
		cfg.SharingSustain,
		cfg.TriggerCount,
		cfg.TriggerMinInterval,
		cfg.BanlistThreshold,
		cfg.SubnetGrouping,
		cfg.SharingPermanentBanEnabled,
		cfg.SharingPermanentBanDuration,
		cfg.SharingBlockOnViolatorOnly,
		cfg.SharingBlockOnBanlistOnly,
	)
	log.Printf(
		"Redis housekeeping: enabled=%t interval=%v max_keys=%d",
		cfg.RedisHousekeepingEnabled,
		cfg.RedisHousekeepingInterval,
		cfg.RedisHousekeepingMaxKeys,
	)
	log.Printf(
		"Alert webhook: mode=%s url_set=%t auth_header=%s token_set=%t",
		cfg.AlertWebhookMode,
		strings.TrimSpace(cfg.AlertWebhookURL) != "",
		strings.TrimSpace(cfg.AlertWebhookAuthHeader),
		strings.TrimSpace(cfg.AlertWebhookToken) != "",
	)
	log.Printf(
		"Ban escalation: enabled=%t durations=%v",
		cfg.BanEscalationEnabled,
		cfg.BanEscalationDurations,
	)
	log.Printf(
		"Geo sharing: enabled=%t min_countries=%d",
		cfg.GeoSharingEnabled,
		cfg.GeoSharingMinCountries,
	)
	log.Printf("Prometheus metrics: enabled=%t", cfg.PrometheusEnabled)

	return cfg
}

func parseThreexuiServers(jsonStr string) []ThreexuiServer {
	var servers []ThreexuiServer
	if jsonStr == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(jsonStr), &servers); err != nil {
		log.Printf("Ошибка парсинга THREEXUI_SERVERS: %v", err)
		return nil
	}
	return servers
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatValue, err := strconv.ParseFloat(value, 64); err == nil {
			return floatValue
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return defaultValue
	}

	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return defaultValue
	}
}

func parseSet(value string) map[string]bool {
	set := make(map[string]bool)
	if value == "" {
		return set
	}
	items := strings.Split(value, ",")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			set[item] = true
		}
	}
	return set
}

func parseStringSlice(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func mergeSets(base map[string]bool, extra map[string]bool) map[string]bool {
	if base == nil {
		base = make(map[string]bool)
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}
