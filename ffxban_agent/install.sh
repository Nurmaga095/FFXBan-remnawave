#!/bin/bash
# =============================================================================
# FFXBan Agent Installer v3.0
# Устанавливает blocker как systemd-сервис + Vector в Docker.
# Автоматически устанавливает Docker и nftables при необходимости.
# Патчит docker-compose remnanode для доступа к логам Xray.
#
# Использование (интерактивно):
#   bash install.sh
#
# Использование (автоматически через deploy.sh):
#   NODE_NAME="Латвия" RABBITMQ_URL="amqp://..." OBSERVER_DOMAIN="observer.example.com" bash install.sh
# =============================================================================
set -u

# --- Цвета ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
ask()    { echo -e "${BLUE}[?]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

INSTALL_DIR="/opt/ffxban-agent"
SERVICE="ffxban-blocker"
VECTOR_NAME="ffxban-vector"
VECTOR_IMAGE="timberio/vector:0.48.0-alpine"
NFT_TABLE="${NFT_TABLE:-firewall}"
NFT_SET="${NFT_SET:-user_blacklist}"
NFT_FAMILY="${NFT_FAMILY:-inet}"
LOG_HOST_DIR="/var/log/remnanode"
LOG_CONTAINER_PATH="/var/log/supervisor"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║     FFXBan Agent Installer v3.0     ║"
echo "╚══════════════════════════════════════╝"

# --- Root check ---
[ "$(id -u)" = "0" ] || fail "Запустите от root: sudo bash install.sh"

# =============================================================================
# 1. Конфигурация
# =============================================================================
header "Конфигурация"

if [ -z "${NODE_NAME:-}" ]; then
    echo "Пытаемся получить имя ноды из ${OBSERVER_DOMAIN}..."
    PROVISION_URL="https://${OBSERVER_DOMAIN}:38213/provision"
    if RESPONSE=$(curl -sk --connect-timeout 5 --max-time 10 "${PROVISION_URL}"); then
        DETECTED_NAME=$(echo "$RESPONSE" | sed -n 's/.*"node_name":"\([^"]*\)".*/\1/p')
        if [ -n "$DETECTED_NAME" ]; then
            ok "Автоматически определено имя ноды: ${DETECTED_NAME}"
            NODE_NAME="$DETECTED_NAME"
        else
            warn "Не удалось определить имя автоматически (ответ: ${RESPONSE})"
        fi
    else
        warn "Не удалось подключиться к ${PROVISION_URL}"
    fi
fi

if [ -z "${NODE_NAME:-}" ]; then
    ask "Имя ноды (точно как в панели Remnawave, например 'Латвия'):"
    read -r NODE_NAME
fi
[ -n "${NODE_NAME:-}" ] || fail "NODE_NAME не задан"

if [ -z "${RABBITMQ_URL:-}" ]; then
    ask "RABBITMQ_URL (amqp://user:pass@OBSERVER_IP:5672/):"
    read -r RABBITMQ_URL
fi
[ -n "${RABBITMQ_URL:-}" ] || fail "RABBITMQ_URL не задан"

echo ""
echo "  Нода:      ${NODE_NAME}"
echo "  RabbitMQ:  ${RABBITMQ_URL}"
echo "  Observer:  ${OBSERVER_DOMAIN}"
echo "  nftables:  ${NFT_FAMILY} ${NFT_TABLE} / ${NFT_SET}"
echo ""

# =============================================================================
# 2. Установка зависимостей
# =============================================================================
header "Установка зависимостей"

# --- nftables ---
if ! command -v nft &>/dev/null; then
    warn "nft не найден — устанавливаем..."
    apt-get update -qq && apt-get install -y -qq nftables \
        || fail "Не удалось установить nftables"
    ok "nftables: установлен"
else
    ok "nftables: уже установлен"
fi
systemctl enable nftables &>/dev/null || true
systemctl start nftables &>/dev/null || true

# --- Docker ---
if ! command -v docker &>/dev/null; then
    warn "Docker не найден — устанавливаем..."
    curl -fsSL https://get.docker.com | sh \
        || fail "Не удалось установить Docker"
    systemctl enable docker &>/dev/null
    systemctl start docker
    ok "Docker: установлен"
else
    ok "Docker: уже установлен"
fi
if ! docker info &>/dev/null; then
    systemctl start docker
    sleep 3
    docker info &>/dev/null || fail "Docker daemon не запускается"
fi

# =============================================================================
# 3. Настройка nftables
# =============================================================================
header "Настройка nftables"

if ! nft list table "${NFT_FAMILY}" "${NFT_TABLE}" &>/dev/null; then
    warn "Таблица '${NFT_FAMILY} ${NFT_TABLE}' не найдена — создаём..."
    nft add table "${NFT_FAMILY}" "${NFT_TABLE}" \
        || fail "Не удалось создать таблицу nftables"
fi

if ! nft list set "${NFT_FAMILY}" "${NFT_TABLE}" "${NFT_SET}" &>/dev/null; then
    warn "Сет '${NFT_SET}' не найден — создаём..."
    nft add set "${NFT_FAMILY}" "${NFT_TABLE}" "${NFT_SET}" \
        "{ type ipv4_addr ; flags timeout ; timeout 24h ; size 8192 ; }" \
        || fail "Не удалось создать сет nftables"
fi
ok "nftables: таблица '${NFT_FAMILY} ${NFT_TABLE}', сет '${NFT_SET}' — готовы"

# Сохраняем правила для автозапуска
nft list ruleset > /etc/nftables.conf 2>/dev/null || true

# =============================================================================
# 4. Настройка логов Remnawave (docker-compose patching)
# =============================================================================
header "Настройка логов Remnawave"

mkdir -p "${LOG_HOST_DIR}"
ok "Директория логов: ${LOG_HOST_DIR}"

# Ищем docker-compose.yml для remnanode по стандартным путям
REMNANODE_COMPOSE=""
for candidate in \
    "/opt/remnanode/docker-compose.yml" \
    "/opt/remnanode/docker-compose.yaml" \
    "/root/remnanode/docker-compose.yml" \
    "/root/remnanode/docker-compose.yaml" \
    "/home/ubuntu/remnanode/docker-compose.yml" \
    "/home/ubuntu/remnanode/docker-compose.yaml"
do
    if [ -f "$candidate" ]; then
        REMNANODE_COMPOSE="$candidate"
        break
    fi
done

# Если не нашли — спрашиваем Docker через inspect
if [ -z "$REMNANODE_COMPOSE" ] && docker ps --format '{{.Names}}' 2>/dev/null | grep -q "remnanode"; then
    COMPOSE_DIR=$(docker inspect remnanode 2>/dev/null \
        | sed -n 's/.*"com.docker.compose.project.working_dir": *"\([^"]*\)".*/\1/p' \
        | head -1)
    if [ -n "$COMPOSE_DIR" ] && [ -f "${COMPOSE_DIR}/docker-compose.yml" ]; then
        REMNANODE_COMPOSE="${COMPOSE_DIR}/docker-compose.yml"
    elif [ -n "$COMPOSE_DIR" ] && [ -f "${COMPOSE_DIR}/docker-compose.yaml" ]; then
        REMNANODE_COMPOSE="${COMPOSE_DIR}/docker-compose.yaml"
    fi
fi

if [ -n "$REMNANODE_COMPOSE" ]; then
    COMPOSE_DIR="$(dirname "${REMNANODE_COMPOSE}")"
    ok "Найден docker-compose remnanode: ${REMNANODE_COMPOSE}"

    if grep -q "${LOG_HOST_DIR}" "${REMNANODE_COMPOSE}"; then
        ok "Маппинг логов уже присутствует — пропускаем"
    else
        # Бэкап
        cp "${REMNANODE_COMPOSE}" "${REMNANODE_COMPOSE}.ffxban.bak"

        # Добавляем volume через Python (без зависимости от yaml-модуля)
        python3 - "${REMNANODE_COMPOSE}" "${LOG_HOST_DIR}" "${LOG_CONTAINER_PATH}" << 'PYEOF'
import sys, re

compose_file, host_dir, container_path = sys.argv[1], sys.argv[2], sys.argv[3]
volume_line = f"      - {host_dir}:{container_path}"

with open(compose_file) as f:
    content = f.read()

if host_dir in content:
    print("already present")
    sys.exit(0)

# Случай 1: секция volumes: уже есть — добавляем строку сразу после неё
if re.search(r'^    volumes:', content, re.M):
    content = re.sub(
        r'(^    volumes:\n)',
        f'\\1{volume_line}\n',
        content, count=1, flags=re.M
    )
else:
    # Случай 2: нет секции volumes — вставляем перед restart:/environment:/ports:
    content = re.sub(
        r'^(    (?:restart|environment|ports|networks|depends_on):)',
        f'    volumes:\n{volume_line}\n\\1',
        content, count=1, flags=re.M
    )

with open(compose_file, 'w') as f:
    f.write(content)

print(f"Added: {volume_line}")
PYEOF

        ok "Маппинг логов добавлен в ${REMNANODE_COMPOSE}"

        # Определяем compose-команду (v1 vs v2)
        if docker compose version &>/dev/null 2>&1; then
            COMPOSE_CMD="docker compose"
        else
            COMPOSE_CMD="docker-compose"
        fi

        warn "Перезапускаем remnanode для применения маппинга логов..."
        (cd "${COMPOSE_DIR}" && ${COMPOSE_CMD} down && ${COMPOSE_CMD} up -d) \
            || warn "Не удалось перезапустить remnanode — выполните вручную: cd ${COMPOSE_DIR} && docker compose up -d"

        sleep 5
        if [ -f "${LOG_HOST_DIR}/xray.out.log" ]; then
            ok "Логи Xray доступны: ${LOG_HOST_DIR}/xray.out.log"
        else
            warn "Лог ещё не появился — remnanode запускается, подождите 10-20 секунд"
        fi
    fi
else
    warn "docker-compose для remnanode не найден автоматически."
    warn "Добавьте volume вручную и перезапустите remnanode:"
    warn "  volumes:"
    warn "    - ${LOG_HOST_DIR}:${LOG_CONTAINER_PATH}"
fi

# =============================================================================
# 5. Бинарник
# =============================================================================
header "Бинарник"

BINARY=""
for path in "./blocker-worker" "/tmp/ffxban-install/blocker-worker" "${INSTALL_DIR}/blocker-worker"; do
    if [ -f "$path" ]; then
        BINARY="$path"
        break
    fi
done
[ -n "$BINARY" ] || fail "Бинарник blocker-worker не найден. Запустите deploy.sh на Observer."
ok "Бинарник: ${BINARY}"

# =============================================================================
# 6. Установка blocker (systemd)
# =============================================================================
header "Установка blocker"

mkdir -p "${INSTALL_DIR}"
cp "${BINARY}" "${INSTALL_DIR}/blocker-worker"
chmod +x "${INSTALL_DIR}/blocker-worker"
ok "Бинарник скопирован в ${INSTALL_DIR}"

cat > "${INSTALL_DIR}/config.env" << EOF
NODE_NAME=${NODE_NAME}
RABBITMQ_URL=${RABBITMQ_URL}
NFT_BINARY=nft
NFT_FAMILY=${NFT_FAMILY}
NFT_TABLE=${NFT_TABLE}
NFT_SET=${NFT_SET}
BLOCKING_STATUS_EXCHANGE_NAME=blocking_status_exchange
HEARTBEAT_INTERVAL_SECONDS=30
RECONNECT_DELAY_SECONDS=5
EOF
chmod 600 "${INSTALL_DIR}/config.env"
ok "Конфиг: ${INSTALL_DIR}/config.env"

cat > "/etc/systemd/system/${SERVICE}.service" << EOF
[Unit]
Description=FFXBan Blocker Agent (${NODE_NAME})
After=network-online.target nftables.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${INSTALL_DIR}/config.env
ExecStart=${INSTALL_DIR}/blocker-worker
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ffxban-blocker

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "${SERVICE}" &>/dev/null
systemctl restart "${SERVICE}"
ok "Systemd сервис запущен: ${SERVICE}"

# =============================================================================
# 7. Установка Vector
# =============================================================================
header "Установка Vector"

mkdir -p "${INSTALL_DIR}/vector"
cat > "${INSTALL_DIR}/vector/vector.toml" << EOF
[sources.xray_file]
  type      = "file"
  include   = ["${LOG_HOST_DIR}/xray.out.log"]
  read_from = "end"

[transforms.parse_log]
  type   = "remap"
  inputs = ["xray_file"]
  source = '''
    pattern = r'from (tcp:)?(?P<ip>\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):\d+.*? email: (?P<email>\S+)'
    parsed, err = parse_regex(.message, pattern)
    if err != null { abort }
    . = {
      "user_email": parsed.email,
      "source_ip":  parsed.ip,
      "node_name":  "${NODE_NAME}",
      "timestamp":  to_string(now())
    }
  '''

[sinks.ffxban_api]
  type           = "http"
  inputs         = ["parse_log"]
  uri            = "https://${OBSERVER_DOMAIN}:38213/"
  method         = "post"
  encoding.codec = "json"
  compression    = "gzip"

  [sinks.ffxban_api.batch]
    max_events   = 100
    timeout_secs = 5

  [sinks.ffxban_api.request]
    retry_attempts     = 5
    retry_backoff_secs = 2
    timeout_secs       = 10

  [sinks.ffxban_api.tls]
    verify_certificate = false
    verify_hostname    = false
EOF
ok "Vector конфиг создан"

# Убираем старые Vector контейнеры (разные имена из предыдущих установок)
for old in ffxban-node-vector ffxban-vector vector; do
    if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "${old}"; then
        warn "Удаляем старый контейнер: ${old}"
        docker rm -f "${old}" &>/dev/null || true
    fi
done

docker run -d \
    --name "${VECTOR_NAME}" \
    --restart unless-stopped \
    --network host \
    -v "${LOG_HOST_DIR}:${LOG_HOST_DIR}:ro" \
    -v "${INSTALL_DIR}/vector/vector.toml:/etc/vector/vector.toml:ro" \
    "${VECTOR_IMAGE}" \
    --config /etc/vector/vector.toml
ok "Vector запущен: ${VECTOR_NAME}"

# =============================================================================
# 8. Финальная проверка
# =============================================================================
header "Проверка"

sleep 2
if systemctl is-active --quiet "${SERVICE}"; then
    ok "Blocker: активен"
else
    warn "Blocker: не запустился — проверьте: journalctl -u ${SERVICE} -n 30"
fi

if docker ps --format '{{.Names}}' | grep -qx "${VECTOR_NAME}"; then
    ok "Vector: активен"
else
    warn "Vector: не запустился — проверьте: docker logs ${VECTOR_NAME}"
fi

echo ""
echo "╔══════════════════════════════════════╗"
echo "║        Установка завершена!         ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "  Нода: ${NODE_NAME}"
echo ""
echo "  Blocker лог:  journalctl -u ${SERVICE} -f"
echo "  Vector лог:   docker logs ${VECTOR_NAME} -f"
echo "  Обновление:   bash install.sh  (повторный запуск безопасен)"
echo ""
