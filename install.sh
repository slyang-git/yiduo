#!/usr/bin/env bash
set -euo pipefail

BIN_NAME=${YIDUO_BIN_NAME:-yiduo}
INSTALL_DIR=${YIDUO_INSTALL_DIR:-"${HOME}/.local/bin"}
REPO_INPUT=${YIDUO_REPO:-"slyang-git/yiduo"}
TAG=${YIDUO_TAG:-latest}
SOURCE_REF=${YIDUO_REF:-""}

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

parse_repo() {
  local input=$1
  if [[ "$input" == http://* || "$input" == https://* ]]; then
    REPO_URL=$input
    local slug=${input#*github.com/}
    slug=${slug%.git}
    REPO_SLUG=$slug
  else
    REPO_SLUG=$input
    REPO_URL="https://github.com/${input}"
  fi
}

detect_os() {
  local uname_out
  uname_out=$(uname -s)
  case "$uname_out" in
    Darwin) printf 'darwin' ;;
    Linux) printf 'linux' ;;
    *) fail "unsupported OS: ${uname_out}" ;;
  esac
}

detect_arch() {
  local arch_out
  arch_out=$(uname -m)
  case "$arch_out" in
    x86_64|amd64) printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) fail "unsupported architecture: ${arch_out}" ;;
  esac
}

resolve_tag() {
  if [[ -n "$TAG" && "$TAG" != "latest" ]]; then
    printf '%s' "$TAG"
    return 0
  fi
  if ! need_cmd curl; then
    fail "curl is required to resolve latest release tag."
  fi
  local api_url="https://api.github.com/repos/${REPO_SLUG}/releases/latest"
  local tag_name
  tag_name=$(curl -fsSL "$api_url" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name"\s*:\s*"([^"]+)".*/\1/')
  if [[ -z "$tag_name" ]]; then
    fail "failed to resolve latest release tag. Set YIDUO_TAG explicitly."
  fi
  printf '%s' "$tag_name"
}

ensure_install_dir() {
  mkdir -p "$INSTALL_DIR"
}

cleanup() {
  if [[ -n "${WORKDIR:-}" && -d "${WORKDIR}" ]]; then
    rm -rf "$WORKDIR"
  fi
}
trap cleanup EXIT

parse_repo "$REPO_INPUT"
ensure_install_dir

WORKDIR=$(mktemp -d)

install_from_release() {
  if ! need_cmd curl; then
    return 1
  fi
  local os arch resolved_tag asset url
  os=$(detect_os)
  arch=$(detect_arch)
  resolved_tag=$(resolve_tag)
  asset="${BIN_NAME}_${os}_${arch}.tar.gz"
  url="https://github.com/${REPO_SLUG}/releases/download/${resolved_tag}/${asset}"

  log "Downloading ${BIN_NAME} ${resolved_tag} (${os}/${arch})..."
  if ! curl -fL "$url" -o "$WORKDIR/${asset}"; then
    return 1
  fi

  tar -xzf "$WORKDIR/${asset}" -C "$WORKDIR"
  if [[ ! -f "$WORKDIR/${BIN_NAME}" ]]; then
    fail "downloaded archive did not contain ${BIN_NAME}."
  fi
  chmod +x "$WORKDIR/${BIN_NAME}"
  mv "$WORKDIR/${BIN_NAME}" "$INSTALL_DIR/${BIN_NAME}"
  log "Installed ${BIN_NAME} to $INSTALL_DIR/${BIN_NAME}"
  return 0
}

if install_from_release; then
  if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
    log "Add $INSTALL_DIR to your PATH to run ${BIN_NAME} globally."
  fi
  exit 0
fi

log "Release asset not found. Falling back to source build."

if [[ -z "$SOURCE_REF" ]]; then
  if [[ "$TAG" != "latest" ]]; then
    SOURCE_REF="$TAG"
  else
    SOURCE_REF="main"
  fi
fi

if ! need_cmd go; then
  fail "Go is required to build ${BIN_NAME}. Install Go 1.22+ and re-run."
fi

if need_cmd git; then
  log "Cloning ${REPO_URL} (ref: ${SOURCE_REF})..."
  git clone --depth 1 --branch "$SOURCE_REF" "$REPO_URL" "$WORKDIR/repo" >/dev/null 2>&1 || \
    fail "git clone failed. Set YIDUO_REPO and YIDUO_REF if needed."
  SRC_DIR="$WORKDIR/repo"
else
  if [[ -z "$REPO_SLUG" ]]; then
    fail "git is missing and repo is not a GitHub slug. Install git or set YIDUO_REPO=owner/name."
  fi
  if ! need_cmd curl; then
    fail "curl is required when git is unavailable."
  fi
  log "Downloading source from GitHub (ref: ${SOURCE_REF})..."
  TAG_URL="https://github.com/${REPO_SLUG}/archive/refs/tags/${SOURCE_REF}.tar.gz"
  HEAD_URL="https://github.com/${REPO_SLUG}/archive/refs/heads/${SOURCE_REF}.tar.gz"
  if ! curl -fsSL "$TAG_URL" -o "$WORKDIR/src.tgz"; then
    curl -fsSL "$HEAD_URL" -o "$WORKDIR/src.tgz" || \
      fail "failed to download source. Set YIDUO_REF or install git."
  fi
  tar -xzf "$WORKDIR/src.tgz" -C "$WORKDIR"
  SRC_DIR=$(find "$WORKDIR" -maxdepth 1 -type d -name "${REPO_SLUG##*/}-*" | head -n 1)
  if [[ -z "${SRC_DIR}" ]]; then
    fail "failed to unpack source archive."
  fi
fi

log "Building ${BIN_NAME}..."
cd "$SRC_DIR"
GO111MODULE=on go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/$BIN_NAME" .

log "Installed $BIN_NAME to $INSTALL_DIR/$BIN_NAME"
if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
  log "Add $INSTALL_DIR to your PATH to run ${BIN_NAME} globally."
fi
