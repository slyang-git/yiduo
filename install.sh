#!/usr/bin/env sh
set -e

TOKEN=""
SERVER=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --token)
      TOKEN="$2"
      shift 2
      ;;
    --server)
      SERVER="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ -z "$TOKEN" ]; then
  echo "❌ Missing --token. Generate the command from the dashboard."
  exit 1
fi

if [ -z "$SERVER" ]; then
  SERVER="http://localhost:8000"
fi

BIN_DIR="$HOME/.local/bin"
CONFIG_DIR="$HOME/.yiduo"
CONFIG_PATH="$CONFIG_DIR/config.json"
TMP_DIR="$(mktemp -d)"
DEVICE_ID=""

COLOR_RESET=""
COLOR_BOLD=""
COLOR_GREEN=""
COLOR_RED=""
if [ -t 1 ]; then
  COLOR_RESET="$(printf '\033[0m')"
  COLOR_BOLD="$(printf '\033[1m')"
  COLOR_GREEN="$(printf '\033[0;32m')"
  COLOR_RED="$(printf '\033[0;31m')"
fi

status_ok() {
  printf "%s✔%s %s\n" "$COLOR_GREEN" "$COLOR_RESET" "$1"
}

status_err() {
  printf "%s✖%s %s\n" "$COLOR_RED" "$COLOR_RESET" "$1"
}

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux|darwin) ;;
  *)
    status_err "Unsupported OS: $OS"
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    status_err "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

TAG="${YIDUO_TAG:-latest}"
if [ "$TAG" = "latest" ]; then
  ARCHIVE_URL="https://github.com/slyang-git/yiduo/releases/latest/download/yiduo_${OS}_${ARCH}.tar.gz"
else
  ARCHIVE_URL="https://github.com/slyang-git/yiduo/releases/download/${TAG}/yiduo_${OS}_${ARCH}.tar.gz"
fi

printf "\n%sYiduo Agent Installer%s\n\n" "$COLOR_BOLD" "$COLOR_RESET"
status_ok "Server: $SERVER"
status_ok "Detected ${OS}/${ARCH}"
printf "%sℹ%s Downloading agent binary...\n" "$COLOR_BOLD" "$COLOR_RESET"
curl -fsSL "$ARCHIVE_URL" -o "$TMP_DIR/yiduo.tar.gz"
tar -xzf "$TMP_DIR/yiduo.tar.gz" -C "$TMP_DIR"
status_ok "Package downloaded and extracted"

mkdir -p "$BIN_DIR"
if [ ! -f "$TMP_DIR/yiduo" ]; then
  status_err "Failed to locate yiduo binary in archive."
  exit 1
fi
mv "$TMP_DIR/yiduo" "$BIN_DIR/yiduo"
status_ok "Binary installed to $BIN_DIR"

mkdir -p "$CONFIG_DIR"
if command -v uuidgen >/dev/null 2>&1; then
  DEVICE_ID="$(uuidgen | tr '[:upper:]' '[:lower:]')"
elif command -v python3 >/dev/null 2>&1; then
  DEVICE_ID="$(python3 - <<'PY'
import uuid
print(uuid.uuid4())
PY
)"
elif command -v python >/dev/null 2>&1; then
  DEVICE_ID="$(python - <<'PY'
import uuid
print(uuid.uuid4())
PY
)"
elif command -v openssl >/dev/null 2>&1; then
  DEVICE_ID="$(openssl rand -hex 16)"
else
  DEVICE_ID="$(date +%s)$$"
fi
cat > "$CONFIG_PATH" <<EOT
{
  "device_token": "$TOKEN",
  "server": "$SERVER",
  "device_id": "$DEVICE_ID"
}
EOT
status_ok "Config saved to $CONFIG_PATH"

echo "🧩 Device ID: $DEVICE_ID"
echo "✅ Installed to: $BIN_DIR/yiduo"
echo "📝 Config saved: $CONFIG_PATH"
echo "🔄 Syncing sessions..."
AI_WRAPPED_DEVICE_TOKEN="$TOKEN" AI_WRAPPED_SYNC_TOKEN="$TOKEN" AI_WRAPPED_SERVER="$SERVER" "$BIN_DIR/yiduo" sync --source auto --server "$SERVER"

printf "\n%sInstallation complete!%s\n\n" "$COLOR_GREEN" "$COLOR_RESET"
echo "Start using Yiduo:"
echo "  $BIN_DIR/yiduo sync"
