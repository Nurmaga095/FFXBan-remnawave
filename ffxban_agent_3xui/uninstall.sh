#!/bin/bash
# =============================================================================
# FFXBan Agent Uninstall Script (3x-ui)
# =============================================================================

set -e

# --- Colors ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
ask()    { echo -e "${BLUE}[?]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

INSTALL_DIR="/opt/ffxban-agent-3xui"
VECTOR_NAME="ffxban-vector-3xui"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║   FFXBan Agent Uninstall (3x-ui)    ║"
echo "╚══════════════════════════════════════╝"

# Root check
[ "$(id -u)" = "0" ] || fail "Run as root: sudo bash uninstall.sh"

# =============================================================================
# 1. Stop Services
# =============================================================================
header "Stopping Services"

if docker ps -a --format '{{.Names}}' | grep -q "^${VECTOR_NAME}$"; then
    echo "Stopping Vector container..."
    docker stop "${VECTOR_NAME}" &>/dev/null || true
    docker rm "${VECTOR_NAME}" &>/dev/null || true
    ok "Vector container removed"
else
    ok "Vector container not found"
fi

# =============================================================================
# 2. Remove Files
# =============================================================================
header "Removing Files"

if [ -d "${INSTALL_DIR}" ]; then
    echo "Removing installation directory..."
    rm -rf "${INSTALL_DIR}"
    ok "Directory removed: ${INSTALL_DIR}"
else
    ok "Directory not found: ${INSTALL_DIR}"
fi

echo ""
echo "╔══════════════════════════════════════╗"
echo "║      Uninstallation Complete!       ║"
echo "╚══════════════════════════════════════╝"
echo ""
