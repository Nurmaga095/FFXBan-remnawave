#!/bin/bash
# =============================================================================
# FFXBan Agent Deploy Script (3x-ui)
#
# Usage (Interactive):
#   bash deploy.sh
#
# Usage (Automated):
#   NODE_NAME="MyNode" NODE_IP="1.2.3.4" NODE_USER="root" OBSERVER_DOMAIN="ffx.example.com" bash deploy.sh
# =============================================================================

set -e

# --- Colors ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
ask()    { echo -e "${BLUE}[?]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_TMP="/tmp/ffxban-agent-3xui"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║   FFXBan Agent Deployer (3x-ui)     ║"
echo "╚══════════════════════════════════════╝"

# =============================================================================
# 1. Configuration
# =============================================================================
header "Configuration"

if [ -z "${NODE_NAME:-}" ]; then
    ask "Node Name (must match FFX config):"
    read -r NODE_NAME
fi
[ -n "${NODE_NAME:-}" ] || fail "NODE_NAME is required"

if [ -z "${NODE_IP:-}" ]; then
    ask "Node IP (e.g. 1.2.3.4):"
    read -r NODE_IP
fi
[ -n "${NODE_IP:-}" ] || fail "NODE_IP is required"

NODE_USER="${NODE_USER:-root}"
if [ -z "${SSH_KEY:-}" ]; then
    # Try default keys
    for key in ~/.ssh/id_ed25519 ~/.ssh/id_rsa; do
        if [ -f "$key" ]; then
            SSH_KEY="$key"
            break
        fi
    done
fi
if [ -z "${SSH_KEY:-}" ]; then
    ask "Path to SSH key (or leave empty for default):"
    read -r SSH_KEY
fi

SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10 -o BatchMode=yes"
if [ -n "${SSH_KEY:-}" ] && [ -f "${SSH_KEY}" ]; then
    SSH_OPTS="${SSH_OPTS} -i ${SSH_KEY}"
    ok "SSH Key: ${SSH_KEY}"
else
    warn "SSH Key not specified, using agent/default"
fi

if [ -z "${OBSERVER_DOMAIN:-}" ]; then
    ask "Observer Domain/IP (e.g. observer.example.com):"
    read -r OBSERVER_DOMAIN
fi
[ -n "${OBSERVER_DOMAIN:-}" ] || fail "OBSERVER_DOMAIN is required"

echo ""
echo "  Node:      ${NODE_NAME} (${NODE_IP})"
echo "  User:      ${NODE_USER}"
echo "  Observer:  ${OBSERVER_DOMAIN}"
echo ""

# =============================================================================
# 2. Connection Check
# =============================================================================
header "Checking Connection"

if ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "echo ok" &>/dev/null; then
    ok "SSH connection successful"
else
    fail "SSH connection failed to ${NODE_USER}@${NODE_IP}"
fi

# =============================================================================
# 3. Copy Files
# =============================================================================
header "Copying Files"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "mkdir -p ${REMOTE_TMP}"
scp ${SSH_OPTS} "${SCRIPT_DIR}/install.sh" "${NODE_USER}@${NODE_IP}:${REMOTE_TMP}/install.sh"
ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "chmod +x ${REMOTE_TMP}/install.sh"

ok "Files copied to ${REMOTE_TMP}"

# =============================================================================
# 4. Remote Installation
# =============================================================================
header "Remote Installation"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" \
    "NODE_NAME='${NODE_NAME}' \
     OBSERVER_DOMAIN='${OBSERVER_DOMAIN}' \
     bash ${REMOTE_TMP}/install.sh"

# =============================================================================
# 5. Cleanup
# =============================================================================
header "Cleanup"

ssh ${SSH_OPTS} "${NODE_USER}@${NODE_IP}" "rm -rf ${REMOTE_TMP}" 2>/dev/null || true
ok "Temp files removed"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║      Deployment Successful!         ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "  Node: ${NODE_NAME}"
echo ""
echo "  Verify:"
echo "    ssh ${NODE_USER}@${NODE_IP} 'docker logs ffxban-vector-3xui'"
echo ""
