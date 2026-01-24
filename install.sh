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

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build the agent. Please install Go and rerun."
  exit 1
fi

AGENT_HOME="$HOME/.ai-wrapped"
BIN_DIR="$AGENT_HOME/bin"
CONFIG_DIR="$HOME/.yiduo"
CONFIG_PATH="$CONFIG_DIR/config.json"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

echo "Downloading agent source..."
curl -fsSL "https://codeload.github.com/slyang-git/yiduo/tar.gz/refs/heads/main" | tar -xz -C "$TMP_DIR"

AGENT_SRC_DIR=$(find "$TMP_DIR" -maxdepth 1 -type d -name "yiduo-*" | head -n 1)
if [ -z "$AGENT_SRC_DIR" ]; then
  echo "Failed to locate agent source."
  exit 1
fi

mkdir -p "$BIN_DIR"
cd "$AGENT_SRC_DIR"

echo "Building agent..."
go build -o "$BIN_DIR/yiduo" .

mkdir -p "$AGENT_HOME"
cat > "$AGENT_HOME/agent.env" <<EOT
AI_WRAPPED_DEVICE_TOKEN=$TOKEN
AI_WRAPPED_SERVER=$SERVER
EOT

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
