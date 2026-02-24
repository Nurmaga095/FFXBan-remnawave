-- KEYS[1]: ключ множества IP пользователя (например, user_ips:email@example.com)
-- KEYS[2]: ключ кулдауна алертов для пользователя (например, alert_sent:email@example.com)
-- ARGV[1]: IP-адрес для добавления
-- ARGV[2]: TTL для IP-адреса в секундах
-- ARGV[3]: Лимит IP-адресов для пользователя
-- ARGV[4]: TTL для кулдауна алертов в секундах

local userKey = string.sub(KEYS[1], 10)
local ipTtlPrefix = 'ip_ttl:' .. userKey .. ':'
local ipNodePrefix = 'ip_node:' .. userKey .. ':'

-- Перед проверкой лимитов чистим "протухшие" IP:
-- если TTL-ключа уже нет, IP удаляется из множества user_ips.
local existingIps = redis.call('SMEMBERS', KEYS[1])
for _, existingIp in ipairs(existingIps) do
    local ttlKey = ipTtlPrefix .. existingIp
    if redis.call('EXISTS', ttlKey) == 0 then
        redis.call('SREM', KEYS[1], existingIp)
        redis.call('DEL', ipNodePrefix .. existingIp)
    end
end

-- Добавляем IP в множество. Если он уже там, команда ничего не сделает, но вернет 0.
local isNewIp = redis.call('SADD', KEYS[1], ARGV[1])

-- Устанавливаем/обновляем TTL для конкретного IP-адреса.
-- Ключ для TTL формируется из ключа множества и самого IP.
local ipTtlKey = ipTtlPrefix .. ARGV[1]
redis.call('SETEX', ipTtlKey, ARGV[2], '1')

-- Обновляем TTL для самого множества IP-адресов пользователя.
-- Это гарантирует, что множество будет автоматически удалено, если пользователь неактивен.
redis.call('EXPIRE', KEYS[1], ARGV[2])

-- Получаем текущее количество IP в множестве. Это быстрая операция O(1).
local currentIpCount = redis.call('SCARD', KEYS[1])
local ipLimit = tonumber(ARGV[3])

-- Проверяем, превышен ли лимит
if currentIpCount > ipLimit then
    -- Лимит превышен. Проверяем, был ли уже отправлен алерт (существует ли ключ кулдауна).
    local alertSent = redis.call('EXISTS', KEYS[2])
    if alertSent == 0 then
        -- Кулдауна нет. Устанавливаем его и возвращаем сигнал на блокировку.
        redis.call('SETEX', KEYS[2], ARGV[4], '1')
        -- Получаем все IP пользователя для отправки в сообщении о блокировке.
        local allIps = redis.call('SMEMBERS', KEYS[1])
        -- Возвращаем статус 1 (блокировать) и список IP
        return {1, allIps}
    else
        -- Кулдаун уже есть. Возвращаем статус 2 и список активных IP.
        local allIps = redis.call('SMEMBERS', KEYS[1])
        return {2, allIps}
    end
end

-- Лимит не превышен. Возвращаем статус 0 (все в порядке) и текущее количество IP.
return {0, currentIpCount, isNewIp}
