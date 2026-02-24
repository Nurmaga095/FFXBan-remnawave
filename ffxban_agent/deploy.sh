#!/bin/bash
# =============================================================================
# FFXBan Deploy Script
# Запускается на Observer-сервере.
# Собирает blocker-worker, копирует на ноду и запускает install.sh.
#
# Использование (интерактивно):
#   bash deploy.sh
#
# Использование (автоматически):
#   NODE_NAME="Латвия" NODE_IP="1.2.3.4" NODE_USER="root" \
#   RABBITMQ_URL="amqp://user:pass@127.0.0.1:5672/" \
#   OBSERVER_DOMAIN="observer.example.com" bash deploy.sh
# =============================================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
ask()    { echo -e "${BLUE}[?]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

# --- Директория со скриптом (ffxban_agent/) ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Корень проекта — один уровень выше
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
BLOCKER_SRC="${PROJECT_ROOT}/ffxban_blocker"
REMOTE_TMP="/tmp/ffxban-install"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║      FFXBan Deploy Script v2.0      ║"
echo "╚══════════════════════════════════════╝"

# =============================================================================
# 1. Конфигурация
# =============================================================================
header "Конфигурация"

if [ -z "${NODE_NAME:-}" ]; then
    ask "Имя ноды (точно как в панели Remnawave, например 'Латвия'):"
    read -r NODE_NAME
fi
[ -n "${NODE_NAME:-}" ] || fail "NODE_NAME не задан"

if [ -z "${NODE_IP:-}" ]; then
    ask "IP-адрес ноды (например 1.2.3.4):"
    read -r NODE_IP
fi
[ -n "${NODE_IP:-}" ] || fail "NODE_IP не задан"

NODE_USER="${NODE_USER:-root}"
if [ -z "${SSH_KEY:-}" ]; then
    # Ищем стандартные ключи
    for key in ~/.ssh/id_ed25519 ~/.ssh/id_rsa; do
        if [ -f "$key" ]; then
            SSH_KEY="$key"
            break
        fi
    done
fi
if [ -z "${SSH_KEY:-}" ]; then
    ask "Путь к SSH-ключу (или Enter для ключа по умолчанию):"
    read -r SSH_KEY
fi

SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=15 -o BatchMode=yes"
if [ -n "${SSH_KEY:-}" ] && [ -f "${SSH_KEY}" ]; then
    SSH_OPTS="${SSH_OPTS} -i ${SSH_KEY}"
    ok "SSH-ключ: ${SSH_KEY}"
else
    warn "SSH-ключ не задан — будет использован ssh-agent или ключ по умолчанию"
fi

if [ -z "${RABBITMQ_URL:-}" ]; then
    ask "RABBITMQ_URL (amqp://user:pass@OBSERVER_IP:5672/):"
    read -r RABBITMQ_URL
fi
[ -n "${RABBITMQ_URL:-}" ] || fail "RABBITMQ_URL не задан"

if [ -z "${OBSERVER_DOMAIN:-}" ]; then
    ask "Домен Observer (например observer.example.com):"
    read -r OBSERVER_DOMAIN
fi
[ -n "${OBSERVER_DOMAIN:-}" ] || fail "OBSERVER_DOMAIN не задан"

NFT_TABLE="${NFT_TABLE:-firewall}"
NFT_SET="${NFT_SET:-user_blacklist}"
NFT_FAMILY="${NFT_FAMILY:-inet}"

echo ""
echo "  Нода:      ${NODE_NAME}"
echo "  IP:        ${NODE_USER}@${NODE_IP}"
echo "  RabbitMQ:  ${RABBITMQ_URL}"
echo "  Observer:  ${OBSERVER_DOMAIN}"
echo "  nftables:  ${NFT_FAMILY} ${NFT_TABLE} / ${NFT_SET}"
echo ""

# =============================================================================
# 2. Сборка бинарника
# =============================================================================
header "Сборка blocker-worker"

command -v go &>/dev/null || fail "Go не установлен. Установите: https://go.dev/dl/"

[ -d "${BLOCKER_SRC}" ] || fail "Директория исходников не найдена: ${BLOCKER_SRC}"
[ -f "${BLOCKER_SRC}/go.mod" ] || fail "go.mod не найден в ${BLOCKER_SRC}"

BINARY_OUT="${SCRIPT_DIR}/blocker-worker"

echo "  Компиляция для linux/amd64..."
(
    cd "${BLOCKER_SRC}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-w -s" -o "${BINARY_OUT}" ./cmd/blocker_worker
)
ok "Бинарник собран: ${BINARY_OUT} ($(du -sh "${BINARY_OUT}" | cut -f1))"

# =============================================================================
# 3. Проверка SSH-доступа
# =============================================================================
header "Проверка подключения к ноде"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "echo ok" &>/dev/null \
    || fail "Не удаётся подключиться по SSH к ${NODE_USER}@${NODE_IP}"
ok "SSH-доступ: есть"

# =============================================================================
# 4. Копирование файлов на ноду
# =============================================================================
header "Копирование файлов"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "mkdir -p ${REMOTE_TMP}"

scp ${SSH_OPTS} \
    "${BINARY_OUT}" \
    "${SCRIPT_DIR}/install.sh" \
    "${NODE_USER}@${NODE_IP}:${REMOTE_TMP}/"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "chmod +x ${REMOTE_TMP}/blocker-worker ${REMOTE_TMP}/install.sh"
ok "Файлы скопированы в ${REMOTE_TMP} на ноде"

# =============================================================================
# 5. Запуск install.sh на ноде
# =============================================================================
header "Установка на ноде ${NODE_IP}"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" \
    "NODE_NAME='${NODE_NAME}' \
     RABBITMQ_URL='${RABBITMQ_URL}' \
     OBSERVER_DOMAIN='${OBSERVER_DOMAIN}' \
     NFT_TABLE='${NFT_TABLE}' \
     NFT_SET='${NFT_SET}' \
     NFT_FAMILY='${NFT_FAMILY}' \
     bash ${REMOTE_TMP}/install.sh"

# =============================================================================
# 6. Очистка
# =============================================================================
header "Очистка"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "rm -rf ${REMOTE_TMP}" 2>/dev/null || true
ok "Временные файлы удалены"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║      Деплой завершён успешно!       ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "  Нода: ${NODE_NAME} (${NODE_IP})"
echo ""
echo "  Проверка на ноде:"
echo "    ssh ${NODE_USER}@${NODE_IP}"
echo "    journalctl -u ffxban-blocker -f"
echo "    docker logs ffxban-vector -f"
echo ""
