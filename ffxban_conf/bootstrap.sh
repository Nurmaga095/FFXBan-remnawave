#!/bin/bash
# =============================================================================
# FFXBan Observer Bootstrap
# Быстрый запуск: подготовка .env + старт docker compose.
# =============================================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
info()   { echo -e "${BLUE}[i]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
ENV_EXAMPLE="${SCRIPT_DIR}/.env.example"

NO_START=0
OBSERVER_HOST="${OBSERVER_HOST:-}"
OBSERVER_DOMAIN="${OBSERVER_DOMAIN:-}"
PANEL_DOMAIN="${PANEL_DOMAIN:-}"
NGINX_CONF="${SCRIPT_DIR}/nginx.conf"

usage() {
    cat << 'EOF'
Usage:
  bash bootstrap.sh [--observer-domain <domain>] [--panel-domain <domain>] [--observer-host <domain-or-ip>] [--no-start]

Options:
  --observer-domain Домен observer (для /log-entry и TLS cert path в nginx.conf).
  --panel-domain    Домен панели. По умолчанию: panel.<observer-domain>.
  --observer-host  Хост, который будет выведен в подсказке для RABBITMQ_URL нод.
  --no-start       Только подготовить .env, без запуска docker compose.
EOF
}

sanitize_host() {
    local v="$1"
    v="${v#http://}"
    v="${v#https://}"
    v="${v%%/*}"
    v="${v%%:*}"
    echo "${v}"
}

detect_observer_host() {
    if [ -n "${OBSERVER_HOST}" ]; then
        sanitize_host "${OBSERVER_HOST}"
        return
    fi
    if [ -n "${OBSERVER_DOMAIN}" ]; then
        sanitize_host "${OBSERVER_DOMAIN}"
        return
    fi
    hostname -I 2>/dev/null | awk '{for(i=1;i<=NF;i++) if ($i !~ /^127\./) {print $i; exit}}'
}

generate_secret_hex() {
    local bytes="$1"
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex "${bytes}"
        return
    fi
    if command -v xxd >/dev/null 2>&1; then
        xxd -l "${bytes}" -p /dev/urandom | tr -d '\n'
        return
    fi
    date +%s%N | sha256sum | cut -c1-$((bytes * 2))
}

read_env_value() {
    local key="$1"
    if [ ! -f "${ENV_FILE}" ]; then
        echo ""
        return
    fi
    awk -F= -v key="${key}" '$1==key {print substr($0, length(key)+2); exit}' "${ENV_FILE}"
}

set_env_value() {
    local key="$1"
    local value="$2"
    local escaped
    escaped="$(printf '%s' "${value}" | sed -e 's/[\/&#]/\\&/g')"
    if grep -q "^${key}=" "${ENV_FILE}"; then
        sed -i "s#^${key}=.*#${key}=${escaped}#" "${ENV_FILE}"
    else
        printf '%s=%s\n' "${key}" "${value}" >> "${ENV_FILE}"
    fi
}

ensure_env_value() {
    local key="$1"
    local value="$2"
    local current
    current="$(read_env_value "${key}")"
    if [ -z "${current}" ]; then
        set_env_value "${key}" "${value}"
        ok "Заполнено ${key}"
    fi
}

apply_nginx_domains() {
    if [ -z "${OBSERVER_DOMAIN}" ]; then
        return
    fi
    [ -f "${NGINX_CONF}" ] || fail "Не найден ${NGINX_CONF}"
    if [ -z "${PANEL_DOMAIN}" ]; then
        PANEL_DOMAIN="panel.${OBSERVER_DOMAIN}"
    fi

    # Сначала заменяем panel-домен, потом общий observer-домен.
    sed -i "s#panel\\.observer\\.example\\.com#${PANEL_DOMAIN}#g" "${NGINX_CONF}"
    sed -i "s#www\\.observer\\.example\\.com#www.${OBSERVER_DOMAIN}#g" "${NGINX_CONF}"
    sed -i "s#observer\\.example\\.com#${OBSERVER_DOMAIN}#g" "${NGINX_CONF}"
    ok "Обновлён nginx.conf: observer=${OBSERVER_DOMAIN}, panel=${PANEL_DOMAIN}"
}

COMPOSE_CMD=()
resolve_compose_cmd() {
    if docker compose version >/dev/null 2>&1; then
        COMPOSE_CMD=(docker compose)
        return
    fi
    if command -v docker-compose >/dev/null 2>&1; then
        COMPOSE_CMD=(docker-compose)
        return
    fi
    fail "docker compose не найден. Установите Docker Compose."
}

while [ $# -gt 0 ]; do
    case "$1" in
        --observer-host)
            [ $# -ge 2 ] || fail "Для --observer-host нужен аргумент."
            OBSERVER_HOST="$2"
            shift 2
            ;;
        --observer-domain)
            [ $# -ge 2 ] || fail "Для --observer-domain нужен аргумент."
            OBSERVER_DOMAIN="$(sanitize_host "$2")"
            shift 2
            ;;
        --panel-domain)
            [ $# -ge 2 ] || fail "Для --panel-domain нужен аргумент."
            PANEL_DOMAIN="$(sanitize_host "$2")"
            shift 2
            ;;
        --no-start)
            NO_START=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "Неизвестный аргумент: $1"
            ;;
    esac
done

if [ -n "${OBSERVER_DOMAIN}" ]; then
    OBSERVER_DOMAIN="$(sanitize_host "${OBSERVER_DOMAIN}")"
fi
if [ -n "${PANEL_DOMAIN}" ]; then
    PANEL_DOMAIN="$(sanitize_host "${PANEL_DOMAIN}")"
fi

echo ""
echo "╔══════════════════════════════════════╗"
echo "║    FFXBan Observer Bootstrap v1.0   ║"
echo "╚══════════════════════════════════════╝"

command -v docker >/dev/null 2>&1 || fail "docker не найден. Установите Docker."
resolve_compose_cmd

header "Подготовка .env"
[ -f "${ENV_EXAMPLE}" ] || fail "Не найден ${ENV_EXAMPLE}"
if [ ! -f "${ENV_FILE}" ]; then
    cp "${ENV_EXAMPLE}" "${ENV_FILE}"
    ok "Создан ${ENV_FILE} из .env.example"
else
    info "Используем существующий ${ENV_FILE}"
fi

ensure_env_value "PANEL_PASSWORD" "$(generate_secret_hex 12)"
ensure_env_value "INTERNAL_API_TOKEN" "$(generate_secret_hex 32)"
ensure_env_value "RABBIT_USER" "ffxban"
ensure_env_value "RABBIT_PASSWD" "$(generate_secret_hex 24)"

RABBIT_USER_VALUE="$(read_env_value "RABBIT_USER")"
RABBIT_PASS_VALUE="$(read_env_value "RABBIT_PASSWD")"
RABBIT_URL_VALUE="$(read_env_value "RABBITMQ_URL")"
if [ -z "${RABBIT_URL_VALUE}" ]; then
    set_env_value "RABBITMQ_URL" "amqp://${RABBIT_USER_VALUE}:${RABBIT_PASS_VALUE}@rabbitmq:5672/"
    ok "Заполнено RABBITMQ_URL"
fi

ensure_env_value "EXCLUDED_IPS" "127.0.0.1"

header "Настройка nginx"
if [ -n "${OBSERVER_DOMAIN}" ]; then
    apply_nginx_domains
else
    warn "Домен не передан. При необходимости замените observer.example.com в nginx.conf вручную."
fi

header "Запуск сервисов"
if [ "${NO_START}" = "1" ]; then
    warn "Пропускаем запуск (указан --no-start)"
    echo "Команда запуска: cd ${SCRIPT_DIR} && ${COMPOSE_CMD[*]} up -d --build"
else
    (
        cd "${SCRIPT_DIR}"
        "${COMPOSE_CMD[@]}" up -d --build
    )
    ok "Сервисы запущены"
fi

OBSERVER_HINT="$(detect_observer_host || true)"
if [ -z "${OBSERVER_HINT}" ]; then
    OBSERVER_HINT="<IP_OBSERVER>"
fi
NODE_RABBIT_URL="amqp://${RABBIT_USER_VALUE}:${RABBIT_PASS_VALUE}@${OBSERVER_HINT}:5672/"

header "Готово"
echo "Panel URL:      https://panel.<your-domain>/"
echo "PANEL_PASSWORD: $(read_env_value "PANEL_PASSWORD")"
echo "RABBIT_USER:    ${RABBIT_USER_VALUE}"
echo "RABBIT_PASSWD:  ${RABBIT_PASS_VALUE}"
echo "RABBITMQ_URL для нод:"
echo "  ${NODE_RABBIT_URL}"
echo ""
warn "Ограничьте порт 5672 только IP ваших нод (firewall)."
