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
  echo "Missing --token. Generate the command from the dashboard."
  exit 1
fi

if [ -z "$SERVER" ]; then
  SERVER="http://localhost:8000"
fi

BIN_DIR="$HOME/.local/bin"
CONFIG_DIR="$HOME/.yiduo"
CONFIG_PATH="$CONFIG_DIR/config.json"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux|darwin) ;;
  *)
    echo "Unsupported OS: $OS"
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

TAG="${YIDUO_TAG:-latest}"
if [ "$TAG" = "latest" ]; then
  ARCHIVE_URL="https://github.com/slyang-git/yiduo/releases/latest/download/yiduo_${OS}_${ARCH}.tar.gz"
else
  ARCHIVE_URL="https://github.com/slyang-git/yiduo/releases/download/${TAG}/yiduo_${OS}_${ARCH}.tar.gz"
fi

echo "Downloading agent binary..."
curl -fsSL "$ARCHIVE_URL" -o "$TMP_DIR/yiduo.tar.gz"
tar -xzf "$TMP_DIR/yiduo.tar.gz" -C "$TMP_DIR"

mkdir -p "$BIN_DIR"
if [ ! -f "$TMP_DIR/yiduo" ]; then
  echo "Failed to locate yiduo binary in archive."
  exit 1
fi
mv "$TMP_DIR/yiduo" "$BIN_DIR/yiduo"

mkdir -p "$CONFIG_DIR"
cat > "$CONFIG_PATH" <<EOT
{
  "device_token": "$TOKEN",
  "server": "$SERVER"
}
EOT

echo "Syncing sessions..."
AI_WRAPPED_DEVICE_TOKEN="$TOKEN" AI_WRAPPED_SERVER="$SERVER" "$BIN_DIR/yiduo" sync --source auto --server "$SERVER"

echo "Done. You can re-run the agent with:"
echo "  $BIN_DIR/yiduo sync"
