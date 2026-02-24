package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"ffxban/internal/models"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Скрипт для атомарной очистки всех ключей пользователя.
// Он получает все IP из множества пользователя, формирует список всех связанных ключей
// (само множество и ключи TTL для каждого IP) и удаляет их одной командой DEL.
// Это гарантирует, что никакие новые IP не "просочатся" между чтением и удалением.
//
// KEYS[1]: ключ множества IP пользователя
// ARGV[1]: префикс для ключей TTL
// ARGV[2]: префикс для ключей ноды IP
const clearUserIPsScript = `
local ips = redis.call('SMEMBERS', KEYS[1])
local keysToDelete = { KEYS[1] }

for i, ip in ipairs(ips) do
    table.insert(keysToDelete, ARGV[1] .. ':' .. ip)
    if ARGV[2] and ARGV[2] ~= '' then
        table.insert(keysToDelete, ARGV[2] .. ':' .. ip)
    end
end

return redis.call('DEL', unpack(keysToDelete))
`

// IPStorage определяет интерфейс для работы с хранилищем IP-адресов.
type IPStorage interface {
	CheckAndAddIP(ctx context.Context, email, ip string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error)
	ClearUserIPs(ctx context.Context, email string) (int, error)
	GetUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error)
	SetIPNode(ctx context.Context, userEmail, ip, nodeName string, ttl time.Duration) error
	GetUserIPNodes(ctx context.Context, userEmail string, ips []string) (map[string]string, error)
	GetAllUserEmails(ctx context.Context) ([]string, error)
	HasAlertCooldown(ctx context.Context, userEmail string) (bool, error)
	GetPanelPasswordHash(ctx context.Context) (string, error)
	SetPanelPasswordHash(ctx context.Context, hash string) error
	GetRedisInfo(ctx context.Context) map[string]string
	IncrUserBanCount(ctx context.Context, userEmail string) (int, error)
	GetUserBanCount(ctx context.Context, userEmail string) (int, error)
	IncrUserBlockCount(ctx context.Context, userEmail string) (int, error)
	GetUserBlockCount(ctx context.Context, userEmail string) (int, error)
	Ping(ctx context.Context) error
	Close() error
	SaveDeviceReport(ctx context.Context, userEmail, userAgent string, now time.Time) error
	GetUserDeviceReports(ctx context.Context, userEmail string) ([]models.DeviceInfo, error)
}

// RedisStore реализует IPStorage с использованием Redis.
type RedisStore struct {
	client            *redis.Client
	addCheckScriptSHA string
	clearScriptSHA    string
}

type userIPKey struct {
	key       string
	userEmail string
	ip        string
}

// NewRedisStore создает новый экземпляр RedisStore.
func NewRedisStore(ctx context.Context, redisURL, scriptPath string) (*RedisStore, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга Redis URL: %w", err)
	}
	client := redis.NewClient(opt)

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ошибка подключения к Redis: %w", err)
	}

	// Загрузка скрипта проверки и добавления IP из файла
	addCheckScript, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения Lua-скрипта '%s': %w", scriptPath, err)
	}
	addCheckScriptSHA, err := client.ScriptLoad(ctx, string(addCheckScript)).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (add/check) в Redis: %w", err)
	}

	// Загрузка скрипта атомарной очистки из константы
	clearScriptSHA, err := client.ScriptLoad(ctx, clearUserIPsScript).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки Lua-скрипта (clear) в Redis: %w", err)
	}

	log.Println("Успешное подключение к Redis и загрузка Lua-скриптов.")
	return &RedisStore{
		client:            client,
		addCheckScriptSHA: addCheckScriptSHA,
		clearScriptSHA:    clearScriptSHA,
	}, nil
}

// CheckAndAddIP выполняет Lua-скрипт для атомарной проверки и добавления IP.
func (s *RedisStore) CheckAndAddIP(ctx context.Context, email, ip string, limit int, ttl, cooldown time.Duration) (*models.CheckResult, error) {
	userIPsSetKey := fmt.Sprintf("user_ips:%s", email)
	alertSentKey := fmt.Sprintf("alert_sent:%s", email)

	args := []interface{}{
		ip,
		int(ttl.Seconds()),
		limit,
		int(cooldown.Seconds()),
	}

	result, err := s.client.EvalSha(ctx, s.addCheckScriptSHA, []string{userIPsSetKey, alertSentKey}, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("ошибка выполнения Lua-скрипта для %s: %w", email, err)
	}

	resSlice, ok := result.([]interface{})
	if !ok || len(resSlice) < 1 {
		return nil, fmt.Errorf("неожиданный результат от Lua-скрипта для %s", email)
	}

	statusCode, _ := resSlice[0].(int64)
	checkResult := &models.CheckResult{StatusCode: statusCode}

	switch statusCode {
	case 0: // OK
		checkResult.CurrentIPCount, _ = resSlice[1].(int64)
		isNew, _ := resSlice[2].(int64)
		checkResult.IsNewIP = isNew == 1
	case 1: // Limit exceeded, block
		ipInterfaces, _ := resSlice[1].([]interface{})
		for _, ipInt := range ipInterfaces {
			if ipStr, ok := ipInt.(string); ok {
				checkResult.AllUserIPs = append(checkResult.AllUserIPs, ipStr)
			}
		}
		checkResult.CurrentIPCount = int64(len(checkResult.AllUserIPs))
	case 2: // Limit exceeded, on cooldown
		switch raw := resSlice[1].(type) {
		case int64:
			checkResult.CurrentIPCount = raw
		case []interface{}:
			for _, ipInt := range raw {
				ipStr, ok := ipInt.(string)
				if !ok {
					continue
				}
				ipStr = strings.TrimSpace(ipStr)
				if ipStr == "" {
					continue
				}
				checkResult.AllUserIPs = append(checkResult.AllUserIPs, ipStr)
			}
			checkResult.CurrentIPCount = int64(len(checkResult.AllUserIPs))
		default:
			checkResult.CurrentIPCount = 0
		}
	}

	return checkResult, nil
}

// ClearUserIPs атомарно удаляет все ключи, связанные с пользователем, используя Lua-скрипт.
func (s *RedisStore) ClearUserIPs(ctx context.Context, email string) (int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", email)
	ipTtlPrefix := fmt.Sprintf("ip_ttl:%s", email)
	ipNodePrefix := fmt.Sprintf("ip_node:%s", email)

	// Выполняем Lua-скрипт, который атомарно получает список IP и удаляет все связанные ключи.
	deleted, err := s.client.EvalSha(ctx, s.clearScriptSHA, []string{userIpsKey}, ipTtlPrefix, ipNodePrefix).Int64()
	if err != nil {
		// Скрипт DEL не должен возвращать redis.Nil, но на всякий случай проверяем.
		if err == redis.Nil {
			return 0, nil
		}
		return 0, fmt.Errorf("ошибка выполнения Lua-скрипта (clear) для %s: %w", email, err)
	}

	return int(deleted), nil
}

// GetUserActiveIPs возвращает активные IP пользователя с их TTL.
func (s *RedisStore) GetUserActiveIPs(ctx context.Context, userEmail string) (map[string]int, error) {
	userIpsKey := fmt.Sprintf("user_ips:%s", userEmail)
	ips, err := s.client.SMembers(ctx, userIpsKey).Result()
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return make(map[string]int), nil
	}

	activeIPs := make(map[string]int)
	pipe := s.client.Pipeline()
	ttlResults := make(map[string]*redis.DurationCmd)

	for _, ip := range ips {
		ipTtlKey := fmt.Sprintf("ip_ttl:%s:%s", userEmail, ip)
		ttlResults[ip] = pipe.TTL(ctx, ipTtlKey)
	}
	_, err = pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	for ip, cmd := range ttlResults {
		ttl, err := cmd.Result()
		if err != nil || ttl <= 0 {
			continue
		}
		activeIPs[ip] = int(ttl.Seconds())
	}
	return activeIPs, nil
}

// SetIPNode сохраняет привязку IP пользователя к ноде с TTL.
func (s *RedisStore) SetIPNode(ctx context.Context, userEmail, ip, nodeName string, ttl time.Duration) error {
	userEmail = strings.TrimSpace(userEmail)
	ip = strings.TrimSpace(ip)
	nodeName = strings.TrimSpace(nodeName)
	if userEmail == "" || ip == "" || nodeName == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	key := fmt.Sprintf("ip_node:%s:%s", userEmail, ip)
	return s.client.SetEx(ctx, key, nodeName, ttl).Err()
}

// GetUserIPNodes возвращает привязки IP -> нода для пользователя.
func (s *RedisStore) GetUserIPNodes(ctx context.Context, userEmail string, ips []string) (map[string]string, error) {
	out := make(map[string]string)
	userEmail = strings.TrimSpace(userEmail)
	if userEmail == "" || len(ips) == 0 {
		return out, nil
	}

	pipe := s.client.Pipeline()
	results := make(map[string]*redis.StringCmd, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		key := fmt.Sprintf("ip_node:%s:%s", userEmail, ip)
		results[ip] = pipe.Get(ctx, key)
	}
	if len(results) == 0 {
		return out, nil
	}

	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}
	for ip, cmd := range results {
		val, getErr := cmd.Result()
		if getErr == redis.Nil || getErr != nil {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		out[ip] = val
	}
	return out, nil
}

// FindNodeCandidatesByIP возвращает уникальные значения нод для точного IP среди всех пользователей.
// Используется для безопасной "автопочинки" unknown-ноды у холодных пользователей.
func (s *RedisStore) FindNodeCandidatesByIP(ctx context.Context, ip string, limit int) ([]string, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 16
	}

	pattern := fmt.Sprintf("ip_node:*:%s", ip)
	var cursor uint64
	uniq := make(map[string]struct{})
	out := make([]string, 0, 8)

	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return nil, err
		}
		cursor = next

		if len(keys) > 0 {
			pipe := s.client.Pipeline()
			cmds := make([]*redis.StringCmd, 0, len(keys))
			for _, key := range keys {
				cmds = append(cmds, pipe.Get(ctx, key))
			}
			_, execErr := pipe.Exec(ctx)
			if execErr != nil && execErr != redis.Nil {
				return nil, execErr
			}

			for _, cmd := range cmds {
				val, getErr := cmd.Result()
				if getErr == redis.Nil || getErr != nil {
					continue
				}
				val = strings.TrimSpace(val)
				if val == "" {
					continue
				}
				if _, ok := uniq[val]; ok {
					continue
				}
				uniq[val] = struct{}{}
				out = append(out, val)
				if len(out) >= limit {
					sort.Strings(out)
					return out, nil
				}
			}
		}

		if cursor == 0 {
			break
		}
	}

	sort.Strings(out)
	return out, nil
}

// FindNodeCandidatesBySubnet24 возвращает уникальные значения нод для подсети /24.
// subnetPrefix должен быть вида "A.B.C." (с точкой на конце).
func (s *RedisStore) FindNodeCandidatesBySubnet24(ctx context.Context, subnetPrefix string, limit int) ([]string, error) {
	subnetPrefix = strings.TrimSpace(subnetPrefix)
	if subnetPrefix == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 32
	}

	pattern := fmt.Sprintf("ip_node:*:%s*", subnetPrefix)
	var cursor uint64
	uniq := make(map[string]struct{})
	out := make([]string, 0, 8)

	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return nil, err
		}
		cursor = next

		if len(keys) > 0 {
			pipe := s.client.Pipeline()
			cmds := make([]*redis.StringCmd, 0, len(keys))
			for _, key := range keys {
				cmds = append(cmds, pipe.Get(ctx, key))
			}
			_, execErr := pipe.Exec(ctx)
			if execErr != nil && execErr != redis.Nil {
				return nil, execErr
			}

			for _, cmd := range cmds {
				val, getErr := cmd.Result()
				if getErr == redis.Nil || getErr != nil {
					continue
				}
				val = strings.TrimSpace(val)
				if val == "" {
					continue
				}
				if _, ok := uniq[val]; ok {
					continue
				}
				uniq[val] = struct{}{}
				out = append(out, val)
				if len(out) >= limit {
					sort.Strings(out)
					return out, nil
				}
			}
		}

		if cursor == 0 {
			break
		}
	}

	sort.Strings(out)
	return out, nil
}

// GetAllUserEmails сканирует ключи Redis для получения всех username (email) пользователей.
func (s *RedisStore) GetAllUserEmails(ctx context.Context) ([]string, error) {
	var cursor uint64
	var emails []string
	for {
		var keys []string
		var err error
		keys, cursor, err = s.client.Scan(ctx, cursor, "user_ips:*", 500).Result()
		if err != nil {
			return nil, fmt.Errorf("ошибка при сканировании ключей (SCAN): %w", err)
		}
		for _, key := range keys {
			parts := strings.SplitN(key, ":", 2)
			if len(parts) == 2 {
				emails = append(emails, parts[1])
			}
		}
		if cursor == 0 {
			break
		}
	}
	return emails, nil
}

// CleanupOrphanMappings удаляет "сиротские" ключи ip_ttl/ip_node, для которых IP уже
// отсутствует в user_ips:<user>, и проставляет TTL ключам, где он случайно отсутствует.
// Возвращает: scanned, deleted, ttlRepaired.
func (s *RedisStore) CleanupOrphanMappings(ctx context.Context, maxKeys int, enforceTTL time.Duration) (int, int, int, error) {
	if maxKeys <= 0 {
		maxKeys = 5000
	}
	if enforceTTL <= 0 {
		enforceTTL = 24 * time.Hour
	}

	type patternRule struct {
		pattern string
		prefix  string
	}

	rules := []patternRule{
		{pattern: "ip_node:*", prefix: "ip_node:"},
		{pattern: "ip_ttl:*", prefix: "ip_ttl:"},
	}

	totalScanned := 0
	totalDeleted := 0
	totalTTLRepaired := 0

	for _, rule := range rules {
		budget := maxKeys - totalScanned
		if budget <= 0 {
			break
		}
		scanned, deleted, ttlRepaired, err := s.cleanupByPattern(ctx, rule.pattern, rule.prefix, budget, enforceTTL)
		if err != nil {
			return totalScanned, totalDeleted, totalTTLRepaired, err
		}
		totalScanned += scanned
		totalDeleted += deleted
		totalTTLRepaired += ttlRepaired
	}

	return totalScanned, totalDeleted, totalTTLRepaired, nil
}

func (s *RedisStore) cleanupByPattern(ctx context.Context, pattern, prefix string, budget int, enforceTTL time.Duration) (int, int, int, error) {
	scanned := 0
	deleted := 0
	ttlRepaired := 0
	var cursor uint64

	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return scanned, deleted, ttlRepaired, err
		}
		cursor = next

		batch := make([]userIPKey, 0, len(keys))
		for _, key := range keys {
			userEmail, ip, ok := parseUserIPKey(key, prefix)
			if !ok {
				continue
			}
			batch = append(batch, userIPKey{
				key:       key,
				userEmail: userEmail,
				ip:        ip,
			})
			scanned++
			if scanned >= budget {
				break
			}
		}

		if len(batch) > 0 {
			memberPipe := s.client.Pipeline()
			memberCmds := make([]*redis.BoolCmd, 0, len(batch))
			for _, item := range batch {
				memberCmds = append(memberCmds, memberPipe.SIsMember(ctx, fmt.Sprintf("user_ips:%s", item.userEmail), item.ip))
			}
			_, execErr := memberPipe.Exec(ctx)
			if execErr != nil && execErr != redis.Nil {
				return scanned, deleted, ttlRepaired, execErr
			}

			staleKeys := make([]string, 0, len(batch))
			liveKeys := make([]string, 0, len(batch))
			for i, cmd := range memberCmds {
				isMember, memberErr := cmd.Result()
				if memberErr != nil && memberErr != redis.Nil {
					continue
				}
				if !isMember {
					staleKeys = append(staleKeys, batch[i].key)
				} else {
					liveKeys = append(liveKeys, batch[i].key)
				}
			}

			if len(staleKeys) > 0 {
				d, delErr := s.client.Del(ctx, staleKeys...).Result()
				if delErr != nil {
					return scanned, deleted, ttlRepaired, delErr
				}
				deleted += int(d)
			}

			// Для "живых" ключей гарантируем наличие TTL (исправление старых бессрочных ключей).
			if len(liveKeys) > 0 {
				ttlPipe := s.client.Pipeline()
				ttlCmds := make([]*redis.DurationCmd, 0, len(liveKeys))
				for _, key := range liveKeys {
					ttlCmds = append(ttlCmds, ttlPipe.TTL(ctx, key))
				}
				_, ttlExecErr := ttlPipe.Exec(ctx)
				if ttlExecErr != nil && ttlExecErr != redis.Nil {
					return scanned, deleted, ttlRepaired, ttlExecErr
				}

				expirePipe := s.client.Pipeline()
				repairCount := 0
				for i, cmd := range ttlCmds {
					ttl, ttlErr := cmd.Result()
					if ttlErr != nil && ttlErr != redis.Nil {
						continue
					}
					// -1: ключ без TTL (старые данные).
					// > enforceTTL: конфиг TTL был уменьшен, "длинные хвосты" нужно подрезать.
					if ttl == -1 || ttl > enforceTTL {
						expirePipe.Expire(ctx, liveKeys[i], enforceTTL)
						repairCount++
					}
				}
				if repairCount > 0 {
					if _, expErr := expirePipe.Exec(ctx); expErr != nil && expErr != redis.Nil {
						return scanned, deleted, ttlRepaired, expErr
					}
					ttlRepaired += repairCount
				}
			}
		}

		if cursor == 0 || scanned >= budget {
			break
		}
	}

	return scanned, deleted, ttlRepaired, nil
}

func parseUserIPKey(key, prefix string) (string, string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, prefix)
	if rest == "" {
		return "", "", false
	}
	sep := strings.LastIndex(rest, ":")
	if sep <= 0 || sep >= len(rest)-1 {
		return "", "", false
	}
	userEmail := strings.TrimSpace(rest[:sep])
	ip := strings.TrimSpace(rest[sep+1:])
	if userEmail == "" || ip == "" {
		return "", "", false
	}
	return userEmail, ip, true
}

// HasAlertCooldown проверяет наличие ключа кулдауна для пользователя.
func (s *RedisStore) HasAlertCooldown(ctx context.Context, userEmail string) (bool, error) {
	alertCooldownKey := fmt.Sprintf("alert_sent:%s", userEmail)
	res, err := s.client.Exists(ctx, alertCooldownKey).Result()
	if err != nil {
		return false, err
	}
	return res > 0, nil
}

// GetPanelPasswordHash возвращает хеш пароля панели.
func (s *RedisStore) GetPanelPasswordHash(ctx context.Context) (string, error) {
	key := "panel:password_hash"
	val, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

// SetPanelPasswordHash сохраняет хеш пароля панели.
func (s *RedisStore) SetPanelPasswordHash(ctx context.Context, hash string) error {
	key := "panel:password_hash"
	return s.client.Set(ctx, key, hash, 0).Err()
}

// IncrUserBanCount атомарно увеличивает счётчик попаданий в ban-list и возвращает новое значение.
func (s *RedisStore) IncrUserBanCount(ctx context.Context, userEmail string) (int, error) {
	key := "ban_count:" + strings.ToLower(strings.TrimSpace(userEmail))
	val, err := s.client.Incr(ctx, key).Result()
	return int(val), err
}

// GetUserBanCount возвращает сохранённое число попаданий в ban-list.
func (s *RedisStore) GetUserBanCount(ctx context.Context, userEmail string) (int, error) {
	key := "ban_count:" + strings.ToLower(strings.TrimSpace(userEmail))
	val, err := s.client.Get(ctx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

// IncrUserBlockCount атомарно увеличивает счётчик блокировок и возвращает новое значение.
func (s *RedisStore) IncrUserBlockCount(ctx context.Context, userEmail string) (int, error) {
	key := "block_count:" + strings.ToLower(strings.TrimSpace(userEmail))
	val, err := s.client.Incr(ctx, key).Result()
	return int(val), err
}

// GetUserBlockCount возвращает сохранённое число блокировок.
func (s *RedisStore) GetUserBlockCount(ctx context.Context, userEmail string) (int, error) {
	key := "block_count:" + strings.ToLower(strings.TrimSpace(userEmail))
	val, err := s.client.Get(ctx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

// GetRedisInfo возвращает ключевые метрики из Redis INFO.
func (s *RedisStore) GetRedisInfo(ctx context.Context) map[string]string {
	result := make(map[string]string)
	info, err := s.client.Info(ctx, "memory", "clients", "stats", "server").Result()
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			result[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
		}
	}
	return result
}

// SaveDeviceReport сохраняет или обновляет информацию об устройстве пользователя по User-Agent.
func (s *RedisStore) SaveDeviceReport(ctx context.Context, userEmail, userAgent string, now time.Time) error {
	if userAgent == "" {
		return nil
	}
	key := "devreport:" + strings.ToLower(strings.TrimSpace(userEmail))
	h := sha256.Sum256([]byte(userAgent))
	hwid := fmt.Sprintf("%x", h[:8]) // 16 hex chars

	existing, err := s.client.HGet(ctx, key, hwid).Result()
	if err == nil && existing != "" {
		var info models.DeviceInfo
		if json.Unmarshal([]byte(existing), &info) == nil {
			info.UpdatedAt = now.UTC().Format(time.RFC3339)
			data, _ := json.Marshal(info)
			s.client.HSet(ctx, key, hwid, string(data))
			s.client.Expire(ctx, key, 30*24*time.Hour)
			return nil
		}
	}
	info := models.DeviceInfo{
		HWID:      hwid,
		Platform:  detectDevicePlatform(userAgent),
		Model:     detectDeviceModel(userAgent),
		OS:        detectDeviceOS(userAgent),
		UserAgent: userAgent,
		CreatedAt: now.UTC().Format(time.RFC3339),
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(info)
	s.client.HSet(ctx, key, hwid, string(data))
	s.client.Expire(ctx, key, 30*24*time.Hour)
	return nil
}

// GetUserDeviceReports возвращает все устройства пользователя.
func (s *RedisStore) GetUserDeviceReports(ctx context.Context, userEmail string) ([]models.DeviceInfo, error) {
	key := "devreport:" + strings.ToLower(strings.TrimSpace(userEmail))
	result, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	devices := make([]models.DeviceInfo, 0, len(result))
	for _, v := range result {
		var info models.DeviceInfo
		if json.Unmarshal([]byte(v), &info) == nil {
			devices = append(devices, info)
		}
	}
	return devices, nil
}

func detectDevicePlatform(ua string) string {
	u := strings.ToLower(ua)
	switch {
	case strings.Contains(u, "iphone") || strings.Contains(u, "ipad") || strings.Contains(u, "ios") ||
		strings.Contains(u, "shadowrocket") || strings.Contains(u, "quantumult") || strings.Contains(u, "loon") ||
		strings.Contains(u, "surge") || strings.Contains(u, "stash"):
		return "iOS"
	case strings.Contains(u, "android") || strings.Contains(u, "v2rayng"):
		return "Android"
	case strings.Contains(u, "windows") || strings.Contains(u, "v2rayn") || strings.Contains(u, "clash for windows") ||
		strings.Contains(u, "nekoray"):
		return "Windows"
	case strings.Contains(u, "mac os") || strings.Contains(u, "macos") || strings.Contains(u, "clashx"):
		return "macOS"
	case strings.Contains(u, "linux"):
		return "Linux"
	case strings.Contains(u, "hiddify"):
		return "Hiddify"
	default:
		return "Unknown"
	}
}

func detectDeviceModel(ua string) string {
	// iOS devices
	for _, model := range []string{"iPhone", "iPad"} {
		if idx := strings.Index(ua, model); idx >= 0 {
			end := idx + len(model)
			for end < len(ua) && (ua[end] == ' ' || (ua[end] >= '0' && ua[end] <= '9') || ua[end] == ',') {
				end++
			}
			return strings.TrimSpace(ua[idx:end])
		}
	}
	// Android: "Samsung Galaxy", "Xiaomi", etc.
	if strings.Contains(strings.ToLower(ua), "android") {
		if idx := strings.Index(ua, "; "); idx >= 0 {
			rest := ua[idx+2:]
			if end := strings.IndexAny(rest, ";)"); end >= 0 {
				model := strings.TrimSpace(rest[:end])
				if !strings.HasPrefix(strings.ToLower(model), "android") && len(model) < 50 {
					return model
				}
			}
		}
	}
	// App name as model fallback
	for _, app := range []string{"Shadowrocket", "v2rayNG", "v2rayN", "ClashX", "Clash", "Hiddify", "Quantumult", "Loon", "Surge", "NekoRay"} {
		if strings.Contains(ua, app) {
			return app
		}
	}
	return "-"
}

func detectDeviceOS(ua string) string {
	u := strings.ToLower(ua)
	if strings.Contains(u, "ios") || strings.Contains(u, "iphone os") || strings.Contains(u, "cpu os") {
		return "iOS"
	}
	if strings.Contains(u, "android") {
		if idx := strings.Index(u, "android "); idx >= 0 {
			ver := u[idx+8:]
			if end := strings.IndexAny(ver, " ;)"); end > 0 {
				return "Android " + ver[:end]
			}
		}
		return "Android"
	}
	if strings.Contains(u, "windows nt 10") {
		return "Windows 10/11"
	}
	if strings.Contains(u, "windows nt 6") {
		return "Windows 7/8"
	}
	if strings.Contains(u, "windows") {
		return "Windows"
	}
	if strings.Contains(u, "mac os x") || strings.Contains(u, "macos") {
		return "macOS"
	}
	if strings.Contains(u, "linux") {
		return "Linux"
	}
	return "-"
}

// Ping проверяет соединение с Redis.
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Close закрывает соединение с Redis.
func (s *RedisStore) Close() error {
	return s.client.Close()
}
