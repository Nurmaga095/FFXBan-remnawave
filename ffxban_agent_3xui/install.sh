#!/bin/bash
# =============================================================================
# FFXBan Agent for 3x-ui Servers (v1.0)
#
# This script installs Vector to parse Xray logs and send them to the FFXBan server.
# It does NOT install the blocker-worker, as blocking is handled via the 3x-ui API.
#
# Requirements:
#   - Root privileges
#   - 3x-ui installed (logs at /usr/local/x-ui/access.log)
#   - Docker (will be installed if missing)
#
# Usage (Interactive):
#   bash install.sh
#
# Usage (Automated):
#   NODE_NAME="MyNode" OBSERVER_DOMAIN="ffx.example.com" bash install.sh
# =============================================================================

set -e

# --- Colors ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()     { echo -e "${GREEN}[✓]${NC} $*"; }
warn()   { echo -e "${YELLOW}[!]${NC} $*"; }
fail()   { echo -e "${RED}[✗]${NC} $*"; exit 1; }
ask()    { echo -e "${BLUE}[?]${NC} $*"; }
header() { echo -e "\n${BLUE}━━━ $* ━━━${NC}"; }

# --- Config ---
INSTALL_DIR="/opt/ffxban-agent-3xui"
VECTOR_NAME="ffxban-vector-3xui"
VECTOR_IMAGE="timberio/vector:0.48.0-alpine"
LOG_HOST_DIR="/usr/local/x-ui"  # Default log location for 3x-ui
LOG_CONTAINER_PATH="/var/log/x-ui"
LOG_FILE="access.log"

echo ""
echo "╔══════════════════════════════════════╗"
echo "║   FFXBan Agent Installer (3x-ui)    ║"
echo "╚══════════════════════════════════════╝"

# Root check
[ "$(id -u)" = "0" ] || fail "Run as root: sudo bash install.sh"

# =============================================================================
# 1. Configuration
# =============================================================================
header "Configuration"

if [ -z "${NODE_NAME:-}" ]; then
    ask "Node Name (must match the name in FFX config):"
    read -r NODE_NAME
fi
[ -n "${NODE_NAME:-}" ] || fail "NODE_NAME is required"

if [ -z "${OBSERVER_DOMAIN:-}" ]; then
    ask "Observer Domain/IP (e.g. observer.example.com):"
    read -r OBSERVER_DOMAIN
fi
[ -n "${OBSERVER_DOMAIN:-}" ] || fail "OBSERVER_DOMAIN is required"

echo ""
echo "  Node:      ${NODE_NAME}"
echo "  Observer:  ${OBSERVER_DOMAIN}"
echo "  Logs:      ${LOG_HOST_DIR}/${LOG_FILE}"
echo ""

# =============================================================================
# 2. Dependencies
# =============================================================================
header "Dependencies"

if ! command -v docker &>/dev/null; then
    warn "Docker not found. Installing..."
    curl -fsSL https://get.docker.com | sh || fail "Docker installation failed"
    systemctl enable docker
    systemctl start docker
    ok "Docker installed"
else
    ok "Docker is already installed"
fi

# Ensure log directory exists (3x-ui specific)
if [ ! -d "${LOG_HOST_DIR}" ]; then
    warn "Log directory ${LOG_HOST_DIR} not found!"
    warn "Make sure 3x-ui is installed or specify correct path."
    # We continue anyway, maybe user customizes path later
fi

# =============================================================================
# 3. Install Vector
# =============================================================================
header "Installing Vector"

mkdir -p "${INSTALL_DIR}/vector"

# Create vector.toml
cat > "${INSTALL_DIR}/vector/vector.toml" << EOF
[sources.xray_file]
  type      = "file"
  include   = ["${LOG_CONTAINER_PATH}/${LOG_FILE}"]
  read_from = "end"

[transforms.parse_log]
  type   = "remap"
  inputs = ["xray_file"]
  source = '''
    # Parse standard Xray access log format
    # Example: 2024/02/24 10:00:00 ... from 1.2.3.4:1234 accepted ... email: user@example.com
    # В 3x-ui встречаются варианты "email:user@x" и "email: user@x" — допускаем оба.
    pattern = r'from (?:tcp:)?(?P<ip>\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):\d+.*?email:\s*(?P<email>\S+)'
    parsed, err = parse_regex(.message, pattern)
    if err != null { abort }
    
    . = {
      "user_email": string!(parsed.email),
      "source_ip":  string!(parsed.ip),
      "node_name":  "${NODE_NAME}",
      "timestamp":  to_string(now())
    }
  '''

[sinks.ffxban_api]
  type           = "http"
  inputs         = ["parse_log"]
  # Важно: на Observer-сервере запросы от агентов обычно принимаются Nginx'ом
  # и проксируются в Vector Aggregator (HTTP source). По умолчанию этот source
  # принимает только путь "/", поэтому используем корень.
  # Если вы настроили Nginx rewrite /log-entry -> /, можно оставить /log-entry.
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

ok "Config created: ${INSTALL_DIR}/vector/vector.toml"

# Remove old container if exists
if docker ps -a --format '{{.Names}}' | grep -q "^${VECTOR_NAME}$"; then
    warn "Removing existing container ${VECTOR_NAME}..."
    docker rm -f "${VECTOR_NAME}" &>/dev/null || true
fi

# Run Vector
docker run -d \
    --name "${VECTOR_NAME}" \
    --restart unless-stopped \
    --network host \
    -v "${LOG_HOST_DIR}:${LOG_CONTAINER_PATH}:ro" \
    -v "${INSTALL_DIR}/vector/vector.toml:/etc/vector/vector.toml:ro" \
    "${VECTOR_IMAGE}" \
    --config /etc/vector/vector.toml

ok "Vector container started: ${VECTOR_NAME}"

# =============================================================================
# 4. Verification
# =============================================================================
header "Verification"

sleep 2
if docker ps --format '{{.Names}}' | grep -q "^${VECTOR_NAME}$"; then
    ok "Status: Active"
else
    fail "Status: Failed to start (check 'docker logs ${VECTOR_NAME}')"
fi

echo ""
echo "╔══════════════════════════════════════╗"
echo "║      Installation Complete!         ║"
echo "╚══════════════════════════════════════╝"
echo ""
echo "  Node Name: ${NODE_NAME}"
echo "  Logs:      docker logs -f ${VECTOR_NAME}"
echo ""
