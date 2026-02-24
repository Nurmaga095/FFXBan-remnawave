package netclass

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ffxban/internal/config"
	"ffxban/internal/models"
)

type cacheEntry struct {
	info      models.NetworkInfo
	expiresAt time.Time
}

// Detector определяет тип сети IP (mobile/wifi/cable/unknown) и кэширует результат.
type Detector struct {
	enabled     bool
	provider    string
	lookupURL   string
	lookupToken string
	cacheTTL    time.Duration
	httpClient  *http.Client
	resolver    *net.Resolver
	mu          sync.RWMutex
	cache       map[string]cacheEntry
}

// NewDetector создает новый детектор сети по конфигу.
func NewDetector(cfg *config.Config) *Detector {
	timeout := cfg.NetworkLookupTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	cacheTTL := cfg.NetworkCacheTTL
	if cacheTTL <= 0 {
		cacheTTL = time.Hour
	}
	return &Detector{
		enabled:     cfg.NetworkDetectEnabled,
		provider:    normalizeLookupProvider(cfg.NetworkLookupProvider),
		lookupURL:   strings.TrimSpace(cfg.NetworkLookupURL),
		lookupToken: strings.TrimSpace(cfg.NetworkLookupToken),
		cacheTTL:    cacheTTL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		resolver: net.DefaultResolver,
		cache:    make(map[string]cacheEntry),
	}
}

// Classify определяет сетевой тип для IP.
func (d *Detector) Classify(ctx context.Context, ip string) models.NetworkInfo {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return models.NetworkInfo{Type: "unknown"}
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return models.NetworkInfo{Type: "unknown"}
	}
	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() {
		return models.NetworkInfo{Type: "unknown", Source: "local", Provider: "local", Confidence: 1}
	}

	if !d.enabled {
		return models.NetworkInfo{Type: "unknown", Source: "disabled"}
	}

	if cached, ok := d.getCached(ip); ok {
		return cached
	}

	info := d.lookup(ctx, ip)
	if info.Type == "" {
		info.Type = "unknown"
	}
	if info.Confidence <= 0 {
		if info.Type == "unknown" {
			info.Confidence = 0.2
		} else {
			info.Confidence = 0.7
		}
	}

	d.setCached(ip, info)
	return info
}

func (d *Detector) getCached(ip string) (models.NetworkInfo, bool) {
	now := time.Now()
	d.mu.RLock()
	entry, ok := d.cache[ip]
	d.mu.RUnlock()
	if !ok || now.After(entry.expiresAt) {
		return models.NetworkInfo{}, false
	}
	return entry.info, true
}

func (d *Detector) setCached(ip string, info models.NetworkInfo) {
	d.mu.Lock()
	d.cache[ip] = cacheEntry{
		info:      info,
		expiresAt: time.Now().Add(d.cacheTTL),
	}
	d.mu.Unlock()
}

func (d *Detector) lookup(ctx context.Context, ip string) models.NetworkInfo {
	switch d.provider {
	case "ipinfo":
		if info, ok := d.lookupIPInfoLite(ctx, ip); ok {
			return info
		}
		// fallback на generic lookup URL (например ipwho), если ipinfo недоступен.
		if info, ok := d.lookupRemote(ctx, ip); ok {
			return info
		}
	default:
		if info, ok := d.lookupRemote(ctx, ip); ok {
			return info
		}
	}
	if info, ok := d.lookupRDNS(ctx, ip); ok {
		return info
	}
	return models.NetworkInfo{Type: "unknown"}
}

func (d *Detector) lookupRemote(ctx context.Context, ip string) (models.NetworkInfo, bool) {
	if d.lookupURL == "" {
		return models.NetworkInfo{}, false
	}

	endpoint := strings.ReplaceAll(d.lookupURL, "{ip}", url.QueryEscape(ip))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return models.NetworkInfo{}, false
	}
	req.Header.Set("Accept", "application/json")
	if d.lookupToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.lookupToken)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return models.NetworkInfo{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return models.NetworkInfo{}, false
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return models.NetworkInfo{}, false
	}

	if success, exists := asBool(payload["success"]); exists && !success {
		return models.NetworkInfo{}, false
	}

	provider := firstNonEmpty(
		extractNestedString(payload, "connection", "isp"),
		extractNestedString(payload, "connection", "org"),
		extractNestedString(payload, "isp"),
		extractNestedString(payload, "org"),
		extractNestedString(payload, "company", "name"),
		extractNestedString(payload, "as", "name"),
	)
	organization := firstNonEmpty(
		extractNestedString(payload, "connection", "org"),
		extractNestedString(payload, "org"),
		extractNestedString(payload, "company", "name"),
		extractNestedString(payload, "as", "name"),
		provider,
	)
	country := firstNonEmpty(
		extractNestedString(payload, "country_code"),
		extractNestedString(payload, "country"),
		extractNestedString(payload, "countryCode"),
	)
	hint := firstNonEmpty(
		extractNestedString(payload, "connection", "type"),
		extractNestedString(payload, "company", "type"),
		extractNestedString(payload, "type"),
	)
	mobile, _ := asBool(payload["mobile"])
	if !mobile {
		mobile, _ = asBool(payload["is_mobile"])
	}
	if !mobile {
		mobile, _ = asBool(payload["cellular"])
	}

	netType := classifyType(provider, hint, mobile)
	conf := 0.7
	if mobile {
		conf = 0.95
	} else if netType == "unknown" {
		conf = 0.35
	}

	return models.NetworkInfo{
		Type:         netType,
		Provider:     provider,
		Country:      country,
		Organization: organization,
		Source:       "lookup",
		Confidence:   conf,
	}, true
}

func (d *Detector) lookupIPInfoLite(ctx context.Context, ip string) (models.NetworkInfo, bool) {
	endpoint := fmt.Sprintf("https://api.ipinfo.io/lite/%s", url.PathEscape(ip))
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return models.NetworkInfo{}, false
	}
	if d.lookupToken != "" {
		q := parsed.Query()
		if strings.TrimSpace(q.Get("token")) == "" {
			q.Set("token", d.lookupToken)
		}
		parsed.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return models.NetworkInfo{}, false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return models.NetworkInfo{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return models.NetworkInfo{}, false
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return models.NetworkInfo{}, false
	}

	// ipinfo lite: country_code, as_name, as_domain.
	provider := firstNonEmpty(
		extractNestedString(payload, "as_name"),
		extractNestedString(payload, "as_domain"),
	)
	organization := firstNonEmpty(
		extractNestedString(payload, "as_name"),
		extractNestedString(payload, "as_domain"),
		provider,
	)
	country := firstNonEmpty(
		extractNestedString(payload, "country_code"),
		extractNestedString(payload, "country"),
	)

	netType := classifyType(provider, "", false)
	conf := 0.75
	if netType == "unknown" {
		conf = 0.45
	}

	return models.NetworkInfo{
		Type:         netType,
		Provider:     provider,
		Country:      country,
		Organization: organization,
		Source:       "ipinfo",
		Confidence:   conf,
	}, true
}

func (d *Detector) lookupRDNS(ctx context.Context, ip string) (models.NetworkInfo, bool) {
	rdnsCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	names, err := d.resolver.LookupAddr(rdnsCtx, ip)
	if err != nil || len(names) == 0 {
		return models.NetworkInfo{}, false
	}
	provider := strings.TrimSuffix(strings.TrimSpace(names[0]), ".")
	if provider == "" {
		return models.NetworkInfo{}, false
	}

	netType := classifyType(provider, "", false)
	conf := 0.55
	if netType == "unknown" {
		conf = 0.3
	}

	return models.NetworkInfo{
		Type:         netType,
		Provider:     provider,
		Organization: provider,
		Source:       "rdns",
		Confidence:   conf,
	}, true
}

func normalizeLookupProvider(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "ipwho", "lookup", "generic":
		return "ipwho"
	case "ipinfo", "ipinfo-lite", "ipinfolite":
		return "ipinfo"
	default:
		return "ipwho"
	}
}

func classifyType(provider, hint string, mobileFlag bool) string {
	if mobileFlag {
		return "mobile"
	}

	target := strings.ToLower(strings.TrimSpace(provider + " " + hint))
	if target == "" {
		return "unknown"
	}

	if hasAny(target,
		"mobile", "cellular", "lte", "4g", "5g", "3g", "gsm", "umts", "cdma",
		"megafon", "mts", "beeline", "tele2", "yota", "vodafone", "tmobile", "t-mobile", "verizon",
	) {
		return "mobile"
	}

	if hasAny(target, "wifi", "wi-fi", "wireless", "wlan", "hotspot") {
		return "wifi"
	}

	if hasAny(target,
		"cable", "fiber", "fibre", "ftth", "xdsl", "dsl", "broadband", "telecom", "isp", "ethernet",
		"comcast", "rostelecom", "mgts", "beeline home", "dom.ru",
		"erline", "subnet llc", "subnet",
	) {
		return "cable"
	}

	return "unknown"
}

func hasAny(target string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(target, k) {
			return true
		}
	}
	return false
}

func extractNestedString(m map[string]any, path ...string) string {
	current := any(m)
	for _, key := range path {
		node, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = node[key]
		if !ok {
			return ""
		}
	}
	return asString(current)
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return strings.TrimSpace(x.String())
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%.0f", x))
	case int:
		return strings.TrimSpace(fmt.Sprintf("%d", x))
	case int64:
		return strings.TrimSpace(fmt.Sprintf("%d", x))
	default:
		return ""
	}
}

func asBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		val := strings.TrimSpace(strings.ToLower(x))
		switch val {
		case "1", "true", "yes", "y", "on":
			return true, true
		case "0", "false", "no", "n", "off":
			return false, true
		}
	case float64:
		return x != 0, true
	case int:
		return x != 0, true
	case int64:
		return x != 0, true
	}
	return false, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
