#!/usr/bin/env bash
set -Eeuo pipefail

SELF_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

APP_NAME="${APP_NAME:-91}"
SERVICE_NAME="${SERVICE_NAME:-91}"
# Kept only to retire installations created by the old two-service topology.
FRONTEND_SERVICE="${FRONTEND_SERVICE:-video-site-frontend}"
FRONTEND_PORT="${FRONTEND_PORT:-9191}"
BACKEND_LISTEN_WAS_SET="${BACKEND_LISTEN+x}"
BACKEND_LISTEN="${BACKEND_LISTEN:-0.0.0.0:${FRONTEND_PORT}}"
NODE_VERSION="$(<"$REPO_DIR/.nvmrc")"
GO_VERSION="${GO_VERSION:-1.27.3}"
INSTALL_DEPS="${INSTALL_DEPS:-1}"
CONFIGURE_UFW="${CONFIGURE_UFW:-1}"

export PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

log() {
  printf '\033[1;34m[deploy]\033[0m %s\n' "$*"
}

warn() {
  printf '\033[1;33m[deploy]\033[0m %s\n' "$*" >&2
}

die() {
  printf '\033[1;31m[deploy]\033[0m %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage: sudo bash deploy.sh [install|update|restart|stop|status|logs|uninstall]

Default action:
  install    Install dependencies, build, create systemd services, and start.

Actions:
  install    First deployment or full repair
  update     Rebuild current code and restart the service
  restart    Restart the systemd service
  stop       Stop the systemd service
  status     Show service status
  logs       Follow backend logs
  uninstall  Remove systemd service files only; keep repo, config, and data

Common overrides:
  FRONTEND_PORT=9191      Public web port
  BACKEND_LISTEN=0.0.0.0:9191
                          Frontend, API, and media bind address
  GO_VERSION=1.27.3
  INSTALL_DEPS=0          Do not install missing Node/Go/ffmpeg/Python runtime deps
  CONFIGURE_UFW=0         Do not open UFW port automatically
  DEPLOY_USER=<user>      Service user; defaults to sudo user or root

Examples:
  sudo bash deploy.sh
  FRONTEND_PORT=8080 sudo -E bash deploy.sh
  sudo bash deploy.sh update
  sudo bash deploy.sh logs
EOF
}

need_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    return
  fi
  if ! command -v sudo >/dev/null 2>&1; then
    die "this action needs root. Re-run as root or install sudo."
  fi
  log "root permission required; re-running with sudo"
  exec sudo -E bash "$SELF_PATH" "$@"
}

detect_deploy_user() {
  DEPLOY_USER="${DEPLOY_USER:-${SUDO_USER:-$(id -un)}}"
  if [[ "$REPO_DIR" == /root/* && "$DEPLOY_USER" != "root" ]]; then
    warn "repo is under /root; using root as service user so systemd can read it"
    DEPLOY_USER="root"
  fi
  if ! id "$DEPLOY_USER" >/dev/null 2>&1; then
    die "DEPLOY_USER does not exist: $DEPLOY_USER"
  fi
  DEPLOY_GROUP="${DEPLOY_GROUP:-$(id -gn "$DEPLOY_USER")}"
  DEPLOY_HOME="$(getent passwd "$DEPLOY_USER" | cut -d: -f6)"
  if [[ -z "$DEPLOY_HOME" ]]; then
    DEPLOY_HOME="/root"
  fi
}

as_deploy_user() {
  if [[ "$DEPLOY_USER" == "root" ]]; then
    HOME="$DEPLOY_HOME" PATH="$PATH" "$@"
    return
  fi
  runuser -u "$DEPLOY_USER" -- env HOME="$DEPLOY_HOME" PATH="$PATH" "$@"
}

require_repo() {
  [[ -f "$REPO_DIR/package.json" ]] || die "package.json not found; run this script from the project root"
  [[ -d "$REPO_DIR/backend" ]] || die "backend directory not found; run this script from the project root"
}

version_ge() {
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

node_ok() {
  command -v node >/dev/null 2>&1 || return 1
  command -v npm >/dev/null 2>&1 || return 1
  npm -v >/dev/null 2>&1 || return 1
  local version
  version="$(node -v)" || return 1
  version="${version#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
    [[ "${version%%.*}" == "${NODE_VERSION%%.*}" ]] &&
    version_ge "$version" "$NODE_VERSION"
}

go_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local version
  version="$(go env GOVERSION 2>/dev/null || true)"
  if [[ -z "$version" ]]; then
    version="$(go version | awk '{print $3}')"
  fi
  version="${version#go}"
  version_ge "$version" "1.27.3"
}

apt_install() {
  [[ "$INSTALL_DEPS" == "1" ]] || die "missing dependencies and INSTALL_DEPS=0"
  command -v apt-get >/dev/null 2>&1 || die "automatic install currently supports Debian/Ubuntu with apt-get"
  export DEBIAN_FRONTEND=noninteractive
  log "installing base packages"
  apt-get update
  apt-get install -y ca-certificates curl git ffmpeg iproute2 build-essential \
    python3 python3-requests python3-bs4 python3-lxml python3-socks
}

verify_crawler_python_deps() {
  command -v python3 >/dev/null 2>&1 || die "python3 is required for crawler scripts"
  python3 - <<'PY' || die "missing Python modules for crawler scripts: requests, bs4, lxml, socks"
import importlib.util
import sys

missing = [
    name
    for name in ("requests", "bs4", "lxml", "socks")
    if importlib.util.find_spec(name) is None
]
if missing:
    print("missing Python modules: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
PY
}

install_node() {
  if node_ok; then
    log "Node $(node -v) and npm $(npm -v) are ready"
    return
  fi
  [[ "$INSTALL_DEPS" == "1" ]] || die "Node.js ${NODE_VERSION}+ (${NODE_VERSION%%.*}.x LTS) and npm are required"

  local arch node_arch archive install_dir tmp checksum executable
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) node_arch="x64" ;;
    aarch64|arm64) node_arch="arm64" ;;
    *) die "unsupported CPU architecture for automatic Node.js install: $arch" ;;
  esac

  archive="node-v${NODE_VERSION}-linux-${node_arch}.tar.gz"
  install_dir="/usr/local/lib/nodejs/${archive%.tar.gz}"
  mkdir -p /usr/local/lib/nodejs /usr/local/bin
  if [[ ! -d "$install_dir" ]]; then
    tmp="$(mktemp -d /usr/local/lib/nodejs/.node-install.XXXXXX)"
    log "installing Node.js ${NODE_VERSION} from nodejs.org"
    curl -fL "https://nodejs.org/dist/v${NODE_VERSION}/${archive}" -o "$tmp/$archive"
    curl -fL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" -o "$tmp/SHASUMS256.txt"
    checksum="$(awk -v archive="$archive" '$2 == archive { print $1 }' "$tmp/SHASUMS256.txt")"
    [[ "$checksum" =~ ^[a-f0-9]{64}$ ]] || die "Node.js archive checksum is missing or invalid"
    printf '%s  %s\n' "$checksum" "$tmp/$archive" | sha256sum --check
    tar -xzf "$tmp/$archive" -C "$tmp"
    mv "$tmp/${archive%.tar.gz}" "$install_dir"
    unlink "$tmp/$archive"
    unlink "$tmp/SHASUMS256.txt"
    rmdir "$tmp"
  fi

  [[ "$("$install_dir/bin/node" -v)" == "v$NODE_VERSION" ]] || die "Node.js installation has an unexpected version: $install_dir"
  for executable in node npm npx; do
    ln -sfn "$install_dir/bin/$executable" "/usr/local/bin/$executable"
  done
  if [[ -x "$install_dir/bin/corepack" ]]; then
    ln -sfn "$install_dir/bin/corepack" /usr/local/bin/corepack
  fi
  hash -r
  node_ok || die "Node.js install finished, but node/npm version check still failed"
  log "Node $(node -v) and npm $(npm -v) are ready"
}

install_npm() {
  local package_manager version
  package_manager="$(node -p 'require(process.argv[1]).packageManager' "$REPO_DIR/package.json")"
  [[ "$package_manager" =~ ^npm@([0-9]+\.[0-9]+\.[0-9]+)$ ]] || die "package.json must pin an npm version in packageManager"
  version="${BASH_REMATCH[1]}"
  if [[ "$(npm -v)" == "$version" ]]; then
    log "npm $version is ready"
    return
  fi
  [[ "$INSTALL_DEPS" == "1" ]] || die "npm $version is required"
  log "installing npm $version"
  npm install --global "$package_manager"
  hash -r
  [[ "$(npm -v)" == "$version" ]] || die "npm install finished, but version check still failed"
}

install_go() {
  if go_ok; then
    log "Go $(go env GOVERSION 2>/dev/null || go version | awk '{print $3}') is ready"
    return
  fi
  [[ "$INSTALL_DEPS" == "1" ]] || die "Go 1.27.3+ is required"

  local arch go_arch tmp url
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) go_arch="amd64" ;;
    aarch64|arm64) go_arch="arm64" ;;
    *) die "unsupported CPU architecture for automatic Go install: $arch" ;;
  esac

  url="https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz"
  tmp="$(mktemp -d)"
  log "installing Go ${GO_VERSION} from ${url}"
  curl -fL "$url" -o "$tmp/go.tgz"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/go.tgz"
  rm -rf "$tmp"
  go_ok || die "Go install finished, but go version check still failed"
  log "Go $(go env GOVERSION) is ready"
}

install_dependencies() {
  if [[ "$INSTALL_DEPS" == "1" ]]; then
    apt_install
  fi
  install_node
  install_npm
  install_go
  command -v ffmpeg >/dev/null 2>&1 || die "ffmpeg is required"
  command -v ffprobe >/dev/null 2>&1 || die "ffprobe is required"
  verify_crawler_python_deps
}

ensure_ownership() {
  local paths=()
  [[ -e "$REPO_DIR/backend/config.yaml" ]] && paths+=("$REPO_DIR/backend/config.yaml")
  [[ -d "$REPO_DIR/backend/data" ]] && paths+=("$REPO_DIR/backend/data")
  [[ -d "$REPO_DIR/dist" ]] && paths+=("$REPO_DIR/dist")
  [[ -d "$REPO_DIR/node_modules" ]] && paths+=("$REPO_DIR/node_modules")
  [[ -e "$REPO_DIR/backend/server" ]] && paths+=("$REPO_DIR/backend/server")
  if (( ${#paths[@]} > 0 )); then
    chown -R "$DEPLOY_USER:$DEPLOY_GROUP" "${paths[@]}"
  fi
}

prepare_config() {
  local cfg="$REPO_DIR/backend/config.yaml"
  local example="$REPO_DIR/backend/config.example.yaml"
  mkdir -p "$REPO_DIR/backend/data"

  if [[ ! -f "$cfg" ]]; then
    log "creating backend/config.yaml from example"
    cp "$example" "$cfg"
    sed -i -E "s#listen: \".*\"#listen: \"$BACKEND_LISTEN\"#" "$cfg"
  elif [[ -n "$BACKEND_LISTEN_WAS_SET" ]]; then
    log "updating backend listen address from BACKEND_LISTEN"
    sed -i -E "s#listen: \".*\"#listen: \"$BACKEND_LISTEN\"#" "$cfg"
  elif grep -Eq '^[[:space:]]*listen:[[:space:]]*"?127\.0\.0\.1:9192"?[[:space:]]*$' "$cfg"; then
    log "migrating legacy two-service listen address to $BACKEND_LISTEN"
    sed -i -E "s#listen: \"?127\.0\.0\.1:9192\"?#listen: \"$BACKEND_LISTEN\"#" "$cfg"
  else
    log "backend/config.yaml already exists; keeping it"
  fi

  ensure_ownership
}

install_frontend() {
  log "preparing the pinned frontend release"
  mkdir -p "$REPO_DIR/tmp"
  DEPLOY_BUILD_DIR="$(mktemp -d "$REPO_DIR/tmp/deploy.XXXXXX")"
  chown "$DEPLOY_USER:$DEPLOY_GROUP" "$DEPLOY_BUILD_DIR"
  as_deploy_user bash "$REPO_DIR/scripts/prepare-frontend.sh" "$DEPLOY_BUILD_DIR/dist"
}

build_backend() {
  log "building backend binary"
  as_deploy_user bash -lc "cd '$REPO_DIR/backend' && go build -o '$DEPLOY_BUILD_DIR/server' ./cmd/server"
}

publish_application() {
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  if [[ -f "$REPO_DIR/backend/server" ]]; then cp -p "$REPO_DIR/backend/server" "$DEPLOY_BUILD_DIR/previous-server"; fi
  if [[ -d "$REPO_DIR/dist" ]]; then mv "$REPO_DIR/dist" "$DEPLOY_BUILD_DIR/previous-dist"; fi
  mv "$DEPLOY_BUILD_DIR/dist" "$REPO_DIR/dist"
  mv "$DEPLOY_BUILD_DIR/server" "$REPO_DIR/backend/server"
}

systemd_env_lines() {
  local lines=""
  if [[ -n "${HTTP_PROXY:-}" ]]; then
    lines+="Environment=HTTP_PROXY=${HTTP_PROXY}"$'\n'
  fi
  if [[ -n "${HTTPS_PROXY:-}" ]]; then
    lines+="Environment=HTTPS_PROXY=${HTTPS_PROXY}"$'\n'
  fi
  if [[ -n "${NO_PROXY:-}" ]]; then
    lines+="Environment=NO_PROXY=${NO_PROXY}"$'\n'
  fi
  printf '%s' "$lines"
}

retire_legacy_frontend_service() {
  local frontend_unit="/etc/systemd/system/${FRONTEND_SERVICE}.service"
  if [[ -e "$frontend_unit" ]] || systemctl is-active --quiet "${FRONTEND_SERVICE}.service" 2>/dev/null; then
    log "retiring legacy frontend service; backend now serves dist directly"
  fi
  systemctl disable --now "${FRONTEND_SERVICE}.service" >/dev/null 2>&1 || true
  rm -f "$frontend_unit"
}

retire_legacy_backend_service() {
  local legacy_service="video-site-backend"
  local legacy_unit="/etc/systemd/system/${legacy_service}.service"
  [[ "$SERVICE_NAME" != "$legacy_service" ]] || return 0
  if [[ -e "$legacy_unit" ]] || systemctl is-active --quiet "${legacy_service}.service" 2>/dev/null; then
    log "retiring legacy backend service; service is now ${SERVICE_NAME}.service"
    systemctl disable --now "${legacy_service}.service"
    rm -f "$legacy_unit"
  fi
}

write_systemd_units() {
  local backend_unit env_lines
  backend_unit="/etc/systemd/system/${SERVICE_NAME}.service"
  env_lines="$(systemd_env_lines)"

  log "writing systemd unit: $backend_unit"
  cat >"$backend_unit" <<EOF
[Unit]
Description=91
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${DEPLOY_USER}
Group=${DEPLOY_GROUP}
WorkingDirectory=${REPO_DIR}/backend
ExecStart=${REPO_DIR}/backend/server
Restart=on-failure
RestartForceExitStatus=75
RestartSec=5
TimeoutStopSec=20
Environment=HOME=${DEPLOY_HOME}
Environment=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
Environment=VIDEO_RESTART_MANAGED=true
${env_lines}
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

  retire_legacy_frontend_service
  retire_legacy_backend_service
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}.service" >/dev/null
}

open_firewall_port() {
  [[ "$CONFIGURE_UFW" == "1" ]] || return 0
  command -v ufw >/dev/null 2>&1 || return 0
  if ufw status 2>/dev/null | grep -qi "Status: active"; then
    log "UFW is active; allowing ${FRONTEND_PORT}/tcp"
    ufw allow "${FRONTEND_PORT}/tcp"
  fi
}

restart_services() {
  log "starting backend service (frontend and API share one listener)"
  systemctl restart "${SERVICE_NAME}.service"
}

show_summary() {
  echo
  log "deployment finished"
  echo "  frontend: http://<server-ip>:${FRONTEND_PORT}/"
  echo "  admin:    http://<server-ip>:${FRONTEND_PORT}/admin"
  echo "  listener: ${BACKEND_LISTEN} (frontend + API + media)"
  echo
  echo "First visit will ask you to create the admin username and password."
  echo "Useful commands:"
  echo "  sudo bash deploy.sh status"
  echo "  sudo bash deploy.sh logs"
  echo "  sudo bash deploy.sh update"
}

show_status() {
  systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
}

install_or_update() {
  local mode="$1"
  require_repo
  detect_deploy_user
  install_dependencies
  install_frontend
  build_backend
  # Migrate the listen address only after both builds succeed. A failed build
  # must leave the currently running two-service installation restart-safe.
  # Deployment credentials and mounts belong to the user, not the release.
  prepare_config
  write_systemd_units
  open_firewall_port
  publish_application
  restart_services
  show_status
  if [[ "$mode" == "install" ]]; then
    show_summary
  fi
}

uninstall_services() {
  systemctl disable --now "${SERVICE_NAME}.service" 2>/dev/null || true
  systemctl disable --now "${FRONTEND_SERVICE}.service" 2>/dev/null || true
  rm -f "/etc/systemd/system/${FRONTEND_SERVICE}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
  systemctl daemon-reload
  log "removed systemd services; repo, config, and data were kept"
}

main() {
  local action="${1:-install}"
  case "$action" in
    install|deploy)
      need_root "$@"
      install_or_update "install"
      ;;
    update)
      need_root "$@"
      install_or_update "update"
      ;;
    restart)
      need_root "$@"
      require_repo
      detect_deploy_user
      prepare_config
      retire_legacy_frontend_service
      systemctl daemon-reload
      restart_services
      show_status
      ;;
    stop)
      need_root "$@"
      systemctl stop "${SERVICE_NAME}.service"
      systemctl stop "${FRONTEND_SERVICE}.service" 2>/dev/null || true
      ;;
    status)
      show_status
      ;;
    logs)
      journalctl -u "${SERVICE_NAME}.service" -f
      ;;
    uninstall)
      need_root "$@"
      uninstall_services
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
