#!/bin/bash
# =============================================================================
# FFXBan Agent Uninstaller
# Полностью удаляет blocker (systemd) и Vector (Docker) с ноды.
#
# Использование:
#   sudo bash uninstall.sh
# =============================================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
info()   { echo -e "${BLUE}[i]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }

INSTALL_DIR="/opt/ffxban-agent"
SERVICE="ffxban-blocker"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║    FFXBan Agent Uninstaller v2.0    ║"
echo "╚══════════════════════════════════════╝"
echo ""

# --- Root check ---
[ "$(id -u)" = "0" ] || fail "Запустите от root: sudo bash uninstall.sh"

# --- Подтверждение ---
echo -e "${YELLOW}  Это удалит ffxban-blocker (systemd) и ffxban-vector (Docker).${NC}"
echo -e "${YELLOW}  Директория ${INSTALL_DIR} будет удалена.${NC}"
echo ""
if [ -z "${FORCE:-}" ]; then
    read -r -p "  Продолжить? [y/N] " CONFIRM
    [[ "${CONFIRM,,}" == "y" ]] || { echo "  Отменено."; exit 0; }
fi

# =============================================================================
# 1. Systemd — blocker
# =============================================================================
header "Остановка blocker"

if systemctl is-active --quiet "${SERVICE}" 2>/dev/null; then
    systemctl stop "${SERVICE}"
    ok "Сервис остановлен: ${SERVICE}"
else
    warn "Сервис не был запущен: ${SERVICE}"
fi

if systemctl is-enabled --quiet "${SERVICE}" 2>/dev/null; then
    systemctl disable "${SERVICE}" &>/dev/null
    ok "Автозапуск отключён: ${SERVICE}"
fi

SERVICE_FILE="/etc/systemd/system/${SERVICE}.service"
if [ -f "${SERVICE_FILE}" ]; then
    rm -f "${SERVICE_FILE}"
    systemctl daemon-reload
    ok "Файл сервиса удалён: ${SERVICE_FILE}"
else
    warn "Файл сервиса не найден: ${SERVICE_FILE}"
fi

# =============================================================================
# 2. Docker — Vector
# =============================================================================
header "Удаление Vector"

VECTOR_NAMES=("ffxban-vector" "ffxban-node-vector" "vector")

if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
    for name in "${VECTOR_NAMES[@]}"; do
        if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "${name}"; then
            docker rm -f "${name}" &>/dev/null
            ok "Docker контейнер удалён: ${name}"
        fi
    done
else
    warn "Docker недоступен — пропуск удаления контейнеров"
fi

# =============================================================================
# 3. Файлы
# =============================================================================
header "Удаление файлов"

if [ -d "${INSTALL_DIR}" ]; then
    rm -rf "${INSTALL_DIR}"
    ok "Директория удалена: ${INSTALL_DIR}"
else
    warn "Директория не найдена: ${INSTALL_DIR}"
fi

# Удаляем бинарник, если остался в других местах
for leftover in "/usr/local/bin/blocker-worker" "/opt/ffxban-blocker/blocker-worker"; do
    if [ -f "${leftover}" ]; then
        rm -f "${leftover}"
        ok "Удалён: ${leftover}"
    fi
done

# =============================================================================
# 4. Финал
# =============================================================================
echo ""
echo "╔══════════════════════════════════════╗"
echo "║        Удаление завершено!          ║"
echo "╚══════════════════════════════════════╝"
echo ""
info "nftables-правила НЕ затронуты — очистите вручную при необходимости:"
echo "   nft flush set inet firewall user_blacklist"
echo ""
