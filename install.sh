#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="${APP_NAME:-video-site-91}"
GITHUB_REPO="${GITHUB_REPO:-nianzhibai/91}"
INSTALL_PATH="${INSTALL_PATH:-/opt/video-site-91}"
SERVICE_NAME="${SERVICE_NAME:-video-site-91}"
FRONTEND_PORT_WAS_SET="${FRONTEND_PORT+x}"
FRONTEND_PORT="${FRONTEND_PORT:-9191}"
VERSION="${VERSION:-latest}"
PACKAGE_FILE=""
PACKAGE_SHA256=""
GH_PROXY="${GH_PROXY:-}"
CONFIGURE_UFW="${CONFIGURE_UFW:-1}"
INSTALL_DEPS="${INSTALL_DEPS:-1}"
SELF_UPDATE="${SELF_UPDATE:-1}"
FORCE_UPDATE="${FORCE_UPDATE:-0}"
INSTALL_SCRIPT_REF="${INSTALL_SCRIPT_REF:-main}"
INSTALL_SCRIPT_URL="${INSTALL_SCRIPT_URL:-${GH_PROXY}https://raw.githubusercontent.com/${GITHUB_REPO}/${INSTALL_SCRIPT_REF}/install.sh}"
VIDEO_SITE_SKIP_SELF_UPDATE="${VIDEO_SITE_SKIP_SELF_UPDATE:-0}"
SERVICE_READY_TIMEOUT="${SERVICE_READY_TIMEOUT:-90}"
VERSION_FILE="$INSTALL_PATH/.version"
MANAGER_PATH="/usr/local/sbin/${APP_NAME}-manager"
COMMAND_LINK="/usr/local/bin/91"
APP_COMMAND_LINK="/usr/local/bin/${APP_NAME}"

RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
BLUE='\033[1;34m'
RESET='\033[0m'

log() {
  printf "${BLUE}[install]${RESET} %s\n" "$*"
}

warn() {
  printf "${YELLOW}[install]${RESET} %s\n" "$*" >&2
}

die() {
  printf "${RED}[install]${RESET} %s\n" "$*" >&2
  exit 1
}

usage() {
  local action="${1:-}"
  case "$action" in
    install)
      cat <<EOF
Usage: sudo bash install.sh [install] [--package FILE]

Install the prebuilt release to $INSTALL_PATH and start the native service.
Running install.sh without a command also installs 91.

  --package FILE  Use a local release archive instead of downloading

Environment:
  INSTALL_PATH=$INSTALL_PATH
  FRONTEND_PORT=$FRONTEND_PORT
  VERSION=$VERSION          Release tag or latest
  GH_PROXY=$GH_PROXY
  INSTALL_DEPS=$INSTALL_DEPS          Set to 0 to skip dependency installation
  CONFIGURE_UFW=$CONFIGURE_UFW         Set to 0 to skip UFW configuration
EOF
      ;;
    update)
      cat <<EOF
Usage: 91 update [--package FILE]

Update the native installation; keep configuration and data, then restart 91.
Local packages are identified by SHA-256 and skip manager self-update.

  --package FILE  Use a local release archive instead of downloading

Environment:
  VERSION=$VERSION          Release tag or latest
  FORCE_UPDATE=$FORCE_UPDATE          Set to 1 to reinstall the same version
  GH_PROXY=$GH_PROXY
EOF
      ;;
    start|restart)
      printf 'Usage: 91 %s\n\n' "$action"
      if [[ "$action" == start ]]; then
        printf 'Start the native service and wait for readiness.\n'
      else
        printf 'Restart the native service and wait for readiness.\n'
      fi
      printf '\nEnvironment:\n  SERVICE_READY_TIMEOUT=%s  Readiness timeout in seconds\n' "$SERVICE_READY_TIMEOUT"
      ;;
    stop)
      printf 'Usage: 91 stop\n\nStop the native service.\n'
      ;;
    status)
      printf 'Usage: 91 status\n\nShow the native service status; exit code follows systemctl.\n'
      ;;
    logs)
      printf 'Usage: 91 logs\n\nFollow native service logs. Press Ctrl+C to stop following.\n'
      ;;
    reset-password)
      cat <<'EOF'
Usage: 91 reset-password [--user-id ID] [--config PATH]

Generate a random 12-character password for an existing user.
The native service may be stopped.

  --user-id ID   User to reset; omitted to select interactively
  --config PATH  Existing configuration file; defaults to the installation config
EOF
      ;;
    uninstall)
      cat <<EOF
Usage: 91 uninstall

Remove the native service, Bot API resources and management commands.
Permanently delete the installation, configured application data and database,
including Telegram videos and sessions stored in the application data directory.
EOF
      ;;
    *)
      cat <<'EOF'
Usage: 91 [COMMAND] [options]

Manage the native (systemd) deployment of 91.

  install         Install 91
  update          Update 91
  start           Start service
  restart         Restart service
  stop            Stop service
  status          Show service status
  logs            Follow service logs
  reset-password  Reset a user's password
  uninstall       Uninstall 91 and related services

Run '91 COMMAND --help' for command details.
EOF
      ;;
  esac
  printf '\n  -h, --help     Show command help\n'
}

is_manager_invocation() {
  local name
  name="$(basename "$0")"
  [[ "$name" == "91" || "$name" == "$APP_NAME" || "$name" == "$(basename "$MANAGER_PATH")" ]]
}

need_root() {
  if [[ "$(id -u)" == "0" ]]; then
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -E bash "$0" "$@"
  fi
  die "please run as root"
}

detect_arch() {
  local machine
  machine="$(uname -m)"
  case "$machine" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $machine" ;;
  esac
}

download_base_url() {
  if [[ "$VERSION" == "latest" ]]; then
    printf '%shttps://github.com/%s/releases/latest/download' "$GH_PROXY" "$GITHUB_REPO"
  else
    printf '%shttps://github.com/%s/releases/download/%s' "$GH_PROXY" "$GITHUB_REPO" "$VERSION"
  fi
}

asset_name() {
  printf '%s-linux-%s.tar.gz' "$APP_NAME" "$ARCH"
}

verify_runtime_deps() {
  local cmd
  for cmd in curl tar ffmpeg ffprobe python3; do
    command -v "$cmd" >/dev/null 2>&1 || die "missing command: $cmd"
  done

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

install_deps() {
  if [[ "$INSTALL_DEPS" != "1" ]]; then
    return
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    log "installing runtime dependencies"
    apt-get update
    apt-get install -y ca-certificates curl tar ffmpeg iproute2 python3 python3-requests python3-bs4 python3-lxml python3-socks
    verify_runtime_deps
    return
  fi

  verify_runtime_deps
}

check_system() {
  [[ "$(uname -s)" == "Linux" ]] || die "Linux is required"
  command -v systemctl >/dev/null 2>&1 || die "systemd is required"
  detect_arch
}

check_disk_space() {
  local parent avail
  parent="$(dirname "$INSTALL_PATH")"
  mkdir -p "$parent"
  avail="$(df -Pm "$parent" | awk 'NR==2 {print $4}')"
  if [[ "$avail" =~ ^[0-9]+$ ]] && (( avail < 512 )); then
    die "not enough free space under $parent, need at least 512 MB"
  fi
}

download_file() {
  local url="$1"
  local output="$2"
  local retry=0
  while (( retry < 3 )); do
    if curl -fL --connect-timeout 15 --retry 2 --retry-delay 2 "$url" -o "$output"; then
      [[ -s "$output" ]] && return 0
    fi
    retry=$((retry + 1))
    warn "download failed, retry $retry/3"
    sleep $((retry * 2))
  done
  return 1
}

backup_install_files() {
  local backup="$1"
  mkdir -p "$backup"
  cp -a "$INSTALL_PATH/server" "$backup/server"
  for item in dist config.example.yaml config.yaml .version; do
    if [[ -e "$INSTALL_PATH/$item" ]]; then
      cp -a "$INSTALL_PATH/$item" "$backup/$item"
    fi
  done
}

restore_install_files() {
  local backup="$1"
  mkdir -p "$INSTALL_PATH"
  cp -a "$backup/server" "$INSTALL_PATH/server"
  for item in dist config.example.yaml config.yaml .version; do
    rm -rf "${INSTALL_PATH:?}/$item"
    if [[ -e "$backup/$item" ]]; then
      cp -a "$backup/$item" "$INSTALL_PATH/$item"
    fi
  done
  chmod +x "$INSTALL_PATH/server"
}

prepare_config() {
  local cfg="$INSTALL_PATH/config.yaml"
  local example="$INSTALL_PATH/config.example.yaml"
  mkdir -p "$INSTALL_PATH/data"

  if [[ ! -f "$cfg" ]]; then
    cp "$example" "$cfg"
    sed -i -E "s#listen: \".*\"#listen: \"0.0.0.0:${FRONTEND_PORT}\"#" "$cfg"
    chmod 600 "$cfg"
    log "created $cfg"
  else
    log "keeping existing $cfg"
    if [[ -n "$FRONTEND_PORT_WAS_SET" ]]; then
      sed -i -E "s#listen: \".*\"#listen: \"0.0.0.0:${FRONTEND_PORT}\"#" "$cfg"
      log "updated listen port to ${FRONTEND_PORT}"
    fi
  fi

}

write_service() {
  cat >"/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=Video Site 91
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_PATH}
ExecStart=${INSTALL_PATH}/server
Restart=on-failure
RestartForceExitStatus=75
RestartSec=5
TimeoutStopSec=20
Environment=VIDEO_CONFIG=${INSTALL_PATH}/config.yaml
Environment=VIDEO_FRONTEND_DIR=${INSTALL_PATH}/dist
Environment=VIDEO_VERSION_FILE=${VERSION_FILE}
Environment=VIDEO_GITHUB_REPO=${GITHUB_REPO}
Environment=VIDEO_RESTART_MANAGED=true
Environment=HOME=/root
Environment=PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}.service" >/dev/null
}

install_cli() {
  local src
  src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
  install_cli_from_file "$src"
}

install_cli_from_file() {
  local src="$1"
  local tmp
  [[ -f "$src" ]] || return 0
  mkdir -p "$(dirname "$MANAGER_PATH")" "$(dirname "$COMMAND_LINK")" "$(dirname "$APP_COMMAND_LINK")"
  tmp="${MANAGER_PATH}.tmp.$$"
  cp "$src" "$tmp"
  chmod 755 "$tmp"
  mv "$tmp" "$MANAGER_PATH"
  ln -sfn "$MANAGER_PATH" "$COMMAND_LINK"
  ln -sfn "$MANAGER_PATH" "$APP_COMMAND_LINK"
}

self_update_manager() {
  [[ "$SELF_UPDATE" == "1" ]] || return 1
  [[ "$VIDEO_SITE_SKIP_SELF_UPDATE" != "1" ]] || return 1
  [[ -n "$INSTALL_SCRIPT_URL" ]] || return 1

  local tmp
  tmp="$(mktemp)"
  log "checking latest manager script"
  if ! download_file "$INSTALL_SCRIPT_URL" "$tmp"; then
    warn "manager self-update skipped: cannot download $INSTALL_SCRIPT_URL"
    rm -f "$tmp"
    return 1
  fi
  if ! bash -n "$tmp"; then
    warn "manager self-update skipped: downloaded script has syntax errors"
    rm -f "$tmp"
    return 1
  fi
  if [[ -f "$MANAGER_PATH" ]] && cmp -s "$tmp" "$MANAGER_PATH"; then
    rm -f "$tmp"
    return 1
  fi

  install_cli_from_file "$tmp"
  rm -f "$tmp"
  log "manager script updated"
  return 0
}

exec_latest_manager_update() {
  local env_args=(
    "VIDEO_SITE_SKIP_SELF_UPDATE=1"
    "APP_NAME=$APP_NAME"
    "GITHUB_REPO=$GITHUB_REPO"
    "INSTALL_PATH=$INSTALL_PATH"
    "SERVICE_NAME=$SERVICE_NAME"
    "VERSION=$VERSION"
    "GH_PROXY=$GH_PROXY"
    "CONFIGURE_UFW=$CONFIGURE_UFW"
    "INSTALL_DEPS=$INSTALL_DEPS"
    "SELF_UPDATE=$SELF_UPDATE"
    "FORCE_UPDATE=$FORCE_UPDATE"
    "INSTALL_SCRIPT_REF=$INSTALL_SCRIPT_REF"
    "INSTALL_SCRIPT_URL=$INSTALL_SCRIPT_URL"
    "SERVICE_READY_TIMEOUT=$SERVICE_READY_TIMEOUT"
  )
  if [[ -n "$FRONTEND_PORT_WAS_SET" ]]; then
    env_args+=("FRONTEND_PORT=$FRONTEND_PORT")
  fi
  exec env "${env_args[@]}" bash "$MANAGER_PATH" update
}

open_firewall_port() {
  [[ "$CONFIGURE_UFW" == "1" ]] || return 0
  command -v ufw >/dev/null 2>&1 || return 0
  if ufw status 2>/dev/null | grep -qi "Status: active"; then
    log "allowing ${FRONTEND_PORT}/tcp in UFW"
    ufw allow "${FRONTEND_PORT}/tcp"
  fi
}

listen_port_from_config() {
  local cfg="$INSTALL_PATH/config.yaml"
  local listen="" port
  if [[ -f "$cfg" ]]; then
    listen="$(sed -nE 's/^[[:space:]]*listen:[[:space:]]*"?([^" #]+)"?.*/\1/p' "$cfg" | head -n1)"
  fi
  port="${listen##*:}"
  if [[ "$port" =~ ^[0-9]+$ ]]; then
    printf '%s' "$port"
    return
  fi
  printf '%s' "$FRONTEND_PORT"
}

has_interactive_tty() {
  [[ -t 0 ]]
}

confirm_uninstall_app() {
  if ! has_interactive_tty; then
    return 0
  fi

  local confirm=""
  printf 'Permanently delete 91, its configuration and all application and Telegram data? [y/N]: ' >/dev/tty
  IFS= read -r confirm </dev/tty || confirm=""
  case "$confirm" in
    [yY]) return 0 ;;
    *)
      log "uninstall cancelled"
      return 1
      ;;
  esac
}

service_health_url() {
  printf 'http://127.0.0.1:%s/admin/api/setup' "$(listen_port_from_config)"
}

local_service_curl() {
  # Local health checks must bypass curl settings and download proxy variables.
  curl --disable --noproxy '*' "$@"
}

report_service_readiness_failure() {
  local port listeners=""
  port="$(listen_port_from_config)"

  if systemctl is-active --quiet "${SERVICE_NAME}.service"; then
    warn "service process is active, but its local health endpoint is unreachable"
  else
    warn "service process is not active"
  fi

  if command -v ss >/dev/null 2>&1; then
    listeners="$(
      ss -ltnp 2>/dev/null \
        | awk -v port="$port" '$4 ~ ":" port "$" {print}' \
        || true
    )"
    if [[ -n "$listeners" ]]; then
      warn "listener(s) found on port $port:"
      printf '%s\n' "$listeners" >&2
    else
      warn "nothing is listening on port $port"
    fi
  fi
}

wait_for_service_ready() {
  local url deadline
  url="$(service_health_url)"
  deadline=$((SECONDS + SERVICE_READY_TIMEOUT))
  log "waiting for service at $url"
  while (( SECONDS < deadline )); do
    if local_service_curl -fsS --connect-timeout 2 --max-time 5 "$url" >/dev/null 2>&1; then
      log "service is ready"
      return 0
    fi
    sleep 2
  done

  warn "service readiness check timed out after ${SERVICE_READY_TIMEOUT}s: $url"
  # Show curl errors on the final probe so readiness failures have diagnostics.
  if local_service_curl -fsS --connect-timeout 2 --max-time 5 "$url" >/dev/null; then
    log "service is ready"
    return 0
  fi
  return 1
}

restart_service_ready() {
  if systemctl restart "${SERVICE_NAME}.service" && wait_for_service_ready; then
    return 0
  fi

  warn "service did not become ready; retrying restart"
  if systemctl restart "${SERVICE_NAME}.service" && wait_for_service_ready; then
    return 0
  fi

  warn "service failed to become ready"
  report_service_readiness_failure
  systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
  journalctl -u "${SERVICE_NAME}.service" -n 80 --no-pager || true
  return 1
}

fetch_and_unpack() {
  local tmp archive url root
  tmp="$(mktemp -d)"
  archive="$tmp/$(asset_name)"
  if [[ -n "$PACKAGE_FILE" ]]; then
    log "using local package $PACKAGE_FILE"
    if ! cp -- "$PACKAGE_FILE" "$archive"; then
      rm -rf "$tmp"
      return 1
    fi
    local checksum
    checksum="$(sha256sum -- "$archive")"
    if [[ "${checksum%% *}" != "$PACKAGE_SHA256" ]]; then
      warn "local package changed during installation; retry with the current file"
      rm -rf "$tmp"
      return 1
    fi
  else
    url="$(download_base_url)/$(asset_name)"
    log "downloading $url"
    if ! download_file "$url" "$archive"; then
      warn "download failed: $url"
      rm -rf "$tmp"
      return 1
    fi
  fi

  if ! tar -xzf "$archive" -C "$tmp"; then
    warn "extract failed"
    rm -rf "$tmp"
    return 1
  fi
  root="$tmp/${APP_NAME}-linux-${ARCH}"
  if [[ ! -f "$root/server" || ! -d "$root/dist" || ! -f "$root/config.example.yaml" ]]; then
    warn "release package layout is invalid"
    rm -rf "$tmp"
    return 1
  fi

  mkdir -p "$INSTALL_PATH"
  cp "$root/server" "$INSTALL_PATH/server"
  rm -rf "$INSTALL_PATH/dist"
  cp -R "$root/dist" "$INSTALL_PATH/dist"
  cp "$root/config.example.yaml" "$INSTALL_PATH/config.example.yaml"
  chmod +x "$INSTALL_PATH/server"
  rm -rf "$tmp"
}

installed_version() {
  if [[ -f "$VERSION_FILE" ]]; then
    head -n1 "$VERSION_FILE" 2>/dev/null | tr -d '\r'
  fi
}

target_version() {
  if [[ -n "$PACKAGE_FILE" ]]; then
    printf 'local-%s' "${PACKAGE_SHA256:0:12}"
    return
  fi
  if [[ "$VERSION" != "latest" ]]; then
    printf '%s' "$VERSION"
    return
  fi

  local body version effective_url
  body="$(curl -fsSL \
    -H "Accept: application/vnd.github+json" \
    -H "User-Agent: video-site-91-installer" \
    "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" 2>/dev/null || true)"
  version="$(printf '%s\n' "$body" \
    | sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' \
    | head -n1)"
  if [[ -n "$version" ]]; then
    printf '%s' "$version"
    return
  fi

  effective_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$(download_base_url)/$(asset_name)" 2>/dev/null || true)"
  printf '%s\n' "$effective_url" \
    | sed -nE 's#.*/releases/download/([^/]+)/.*#\1#p' \
    | head -n1
}

should_skip_update() {
  [[ "$FORCE_UPDATE" != "1" ]] || return 1

  local current target
  current="$(installed_version)"
  target="$(target_version || true)"

  if [[ -z "$target" ]]; then
    warn "cannot determine target version; continuing update"
    return 1
  fi

  if [[ -z "$current" ]]; then
    log "installed version: unknown"
    log "target version: $target"
    return 1
  fi

  log "installed version: $current"
  log "target version: $target"
  [[ "$current" == "$target" ]]
}

record_version() {
  local version
  version="$(target_version || true)"
  [[ -n "$version" ]] || version="$VERSION"
  {
    echo "$version"
    date '+%Y-%m-%d %H:%M:%S'
  } >"$VERSION_FILE"
}

show_success() {
  local local_ip public_ip version
  local_ip="$(ip addr show 2>/dev/null | awk '/inet / && $2 !~ /^127/ {sub(/\/.*/, "", $2); print $2; exit}')"
  public_ip="$(curl -s4 --connect-timeout 5 ip.sb 2>/dev/null || true)"
  version="$(head -n1 "$VERSION_FILE" 2>/dev/null || echo unknown)"

  echo
  printf '%bInstallation complete%b\n' "$GREEN" "$RESET"
  echo "Version: $version"
  [[ -n "$local_ip" ]] && echo "Local:  http://${local_ip}:${FRONTEND_PORT}/"
  [[ -n "$public_ip" ]] && echo "Public: http://${public_ip}:${FRONTEND_PORT}/"
  echo "Admin:  http://SERVER_IP:${FRONTEND_PORT}/admin"
  echo "Data:   $INSTALL_PATH/data"
  echo
  echo "Set the admin username and password on your first visit."
  echo "Run '91' for the management menu or '91 --help' for commands."
}

install_app() {
  check_system
  check_disk_space
  install_deps
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  fetch_and_unpack || die "install failed"
  prepare_config
  write_service
  install_cli
  open_firewall_port
  restart_service_ready || die "service readiness check failed; see diagnostics above"
  record_version
  show_success
}

update_app() {
  check_system
  [[ -f "$INSTALL_PATH/server" ]] || die "not installed at $INSTALL_PATH"

  if [[ -z "$PACKAGE_FILE" ]] && self_update_manager; then
    log "re-running update with latest manager script"
    exec_latest_manager_update
  fi

  install_deps

  if should_skip_update; then
    log "already up to date; skipped app update"
    return 0
  fi

  check_disk_space

  local backup
  backup="$(mktemp -d)"
  backup_install_files "$backup"

  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  if ! (fetch_and_unpack && prepare_config && write_service && install_cli); then
    warn "update failed; restoring previous files"
    restore_install_files "$backup"
    systemctl start "${SERVICE_NAME}.service" 2>/dev/null || true
    rm -rf "$backup"
    exit 1
  fi

  if ! restart_service_ready; then
    warn "new version failed its readiness check; restoring previous files"
    restore_install_files "$backup"
    restart_service_ready 2>/dev/null || true
    rm -rf "$backup"
    exit 1
  fi
  record_version
  rm -rf "$backup"
  log "updated"
}

validate_uninstall_paths() {
  python3 - "$1" "$INSTALL_PATH" "${BASH_SOURCE[0]}" "${2:-}" <<'PY'
import json
import os
import sys

plan_path, installation, script, incoming = sys.argv[1:]
installation = os.path.abspath(installation)
real_installation = os.path.realpath(installation)
protected = {
    os.path.realpath(path) for path in (
        "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/media", "/mnt",
        "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var",
        "/var/lib", "/var/lib/docker", "/var/lib/containerd", os.path.expanduser("~"),
    )
}

def contains(parent, child):
    return os.path.commonpath((parent, child)) == parent

real_script = os.path.realpath(script)
if not contains(real_installation, real_script):
    protected.add(real_script)

plan = {"directories": [installation], "files": []}
if os.path.exists(plan_path):
    with open(plan_path) as stream:
        plan = json.load(stream)
if incoming:
    with open(incoming) as stream:
        additions = json.load(stream)
    if "data_dir" in additions or "db_path" in additions:
        if not all(isinstance(additions.get(key), str) and additions[key] for key in ("data_dir", "db_path")):
            sys.exit("Invalid storage path report; uninstall cancelled")
        plan["directories"].append(additions["data_dir"])
        database = additions["db_path"]
        if not os.path.isabs(database):
            sys.exit("Database path must be absolute; uninstall cancelled")
        for base in {database, os.path.realpath(database)}:
            plan["files"].extend(base + suffix for suffix in ("", "-wal", "-shm", "-journal"))
    else:
        if not isinstance(additions.get("directories"), list) or not isinstance(additions.get("files"), list):
            sys.exit("Invalid cleanup path report; uninstall cancelled")
        for kind in ("directories", "files"):
            plan[kind].extend(additions[kind])

validated = {"directories": set(), "files": set()}
for kind in validated:
    for source in plan[kind]:
        if not isinstance(source, str) or not os.path.isabs(source) or any(ord(char) < 32 for char in source):
            sys.exit("Cleanup paths must be absolute and contain no control characters")
        path = os.path.realpath(source)
        if (path in protected or any(contains(path, item) for item in protected)
                or (kind == "directories" and path != real_installation and contains(path, real_installation))
                or (kind == "files" and os.path.isdir(path))):
            sys.exit("Refusing to delete unsafe application data path: " + source)
        validated[kind].update((os.path.normpath(source), path))

# Delete application data first and the installation last, keeping configuration
# and management commands available when data cleanup fails.
installations = {installation, real_installation}
data_directories = validated["directories"] - installations
directories = sorted(path for path in data_directories
                     if not any(path != parent and contains(parent, path) for parent in data_directories))
files = sorted(path for path in validated["files"] if not any(contains(parent, path) for parent in directories))
result = {"directories": directories + sorted(installations), "files": files}
with open(plan_path + ".tmp", "w") as stream:
    json.dump(result, stream)
os.replace(plan_path + ".tmp", plan_path)
PY
}

uninstall_app() (
  confirm_uninstall_app || return 1

  command -v python3 >/dev/null 2>&1 || die "python3 is required to resolve cleanup paths"
  if [[ -d "$INSTALL_PATH" ]]; then
    local uninstall_lock
    exec {uninstall_lock}< "$INSTALL_PATH"
    flock -n "$uninstall_lock" || die "Another deployment operation is running; retry later"
  fi
  umask 077
  local work_dir config_file="$INSTALL_PATH/config.yaml" storage_report="" path
  local -a paths
  work_dir=$(mktemp -d)
  trap 'rm -rf -- "$work_dir"' EXIT
  if [[ -f "$config_file" ]]; then
    [[ -x "$INSTALL_PATH/server" ]] || die "Server binary is required to resolve configured data paths"
    storage_report="$work_dir/storage.json"
    (cd "$INSTALL_PATH" && ./server storage-paths --config "$config_file") > "$storage_report" ||
      die "Could not resolve storage paths; check the configuration and server storage-paths support"
  fi
  validate_uninstall_paths "$work_dir/paths.json" "$storage_report"

  # Stop the service before removing its managed data.
  systemctl disable --now "${SERVICE_NAME}.service" || die "could not stop and disable ${SERVICE_NAME}.service; installation has been kept"
  python3 - "$work_dir/paths.json" > "$work_dir/paths" <<'PY'
import json
import sys
with open(sys.argv[1]) as stream:
    plan = json.load(stream)
for path in plan["files"] + plan["directories"]:
    sys.stdout.write(path + "\0")
PY
  mapfile -d '' -t paths < "$work_dir/paths"
  for path in "${paths[@]}"; do
    log "Deleting $path"
    rm -rf --one-file-system -- "$path" || die "Data cleanup failed at $path; management commands kept for retry"
  done
  rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
  systemctl daemon-reload
  rm -f "$COMMAND_LINK" "$APP_COMMAND_LINK" "$MANAGER_PATH"
  log "Uninstall complete: 91 and its application data removed"
)

reset_password() {
  if [[ ! -x "$INSTALL_PATH/server" ]]; then
    die "Server binary is missing or not executable: $INSTALL_PATH/server"
  fi
  local install_dir
  install_dir="$(cd "$INSTALL_PATH" && pwd)"
  (
    cd "$install_dir"
    VIDEO_CONFIG="${VIDEO_CONFIG:-$install_dir/config.yaml}" ./server reset-password "$@"
  )
}

show_menu() {
  if [[ ! -t 0 ]]; then
    usage
    return 0
  fi

  local choice action
  while true; do
    clear || true
    echo "91 native deployment (systemd)"
    echo
    echo "1) Service status"
    echo "2) Service logs"
    echo "3) Start 91"
    echo "4) Restart 91"
    echo "5) Stop 91"
    echo "6) Update 91"
    echo "7) Reset user password"
    echo "8) Uninstall 91 and related services"
    echo "0) Exit"
    echo
    read -r -p "Select an option [0-8]: " choice || return 0

    case "$choice" in
      1) action=status ;;
      2) action=logs ;;
      3) action=start ;;
      4) action=restart ;;
      5) action=stop ;;
      6) action=update ;;
      7) action=reset-password ;;
      8) action=uninstall ;;
      0) return 0 ;;
      *) echo "Invalid option. Enter 0-8."; continue ;;
    esac

    # A separate shell keeps action failures from exiting the menu, while each
    # action retains errexit instead of inheriting an `if main ...` context.
    if bash "${BASH_SOURCE[0]}" "$action"; then
      [[ "$action" != uninstall ]] || return 0
    else
      warn "Command '$action' exited with code $?. See output above."
    fi

    echo
    read -r -n1 -s -p "Press any key to continue..." || return 0
  done
}

main() {
  local action="${1:-}"
  local -a action_args
  PACKAGE_FILE=""
  PACKAGE_SHA256=""
  if [[ "$action" == --package ]]; then
    action="install"
  elif [[ -z "$action" ]]; then
    if is_manager_invocation; then
      show_menu
      return
    fi
    action="install"
  else
    shift
  fi

  case "$action" in
    install|start|stop|restart|status|logs|update|uninstall|reset-password|-h|--help|help) ;;
    *)
      warn "unknown command: $action"
      usage >&2
      return 2
      ;;
  esac
  action_args=("$@")
  if (( $# == 1 )) && [[ "$1" == -h || "$1" == --help ]]; then
    usage "$action"
    return 0
  fi
  if [[ "$action" == install || "$action" == update ]]; then
    while (( $# )); do
      case "$1" in
        -h|--help)
          usage "$action"
          return 0
          ;;
        --package)
          if (( $# < 2 )) || [[ -z "$2" || "$2" == -* || -n "$PACKAGE_FILE" ]]; then
            warn "--package requires one file path and may only be specified once"
            return 2
          fi
          PACKAGE_FILE="$2"
          shift 2
          ;;
        *)
          warn "unexpected arguments for $action: $*"
          usage "$action" >&2
          return 2
          ;;
      esac
    done
    if [[ -n "$PACKAGE_FILE" ]]; then
      [[ -f "$PACKAGE_FILE" && -r "$PACKAGE_FILE" ]] || { warn "local package is not a readable file: $PACKAGE_FILE"; return 2; }
      command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required for local packages"
      PACKAGE_FILE="$(cd -- "$(dirname -- "$PACKAGE_FILE")" && pwd -P)/$(basename -- "$PACKAGE_FILE")"
      PACKAGE_SHA256="$(sha256sum < "$PACKAGE_FILE")"
      PACKAGE_SHA256="${PACKAGE_SHA256%% *}"
    fi
  elif [[ "$action" != reset-password ]] && (( $# )); then
    warn "unexpected arguments for $action: $*"
    usage "$action" >&2
    return 2
  fi

  case "$action" in
    install)
      need_root "$action" "${action_args[@]}"
      install_app
      ;;
    update)
      need_root "$action" "${action_args[@]}"
      update_app
      ;;
    start)
      need_root "$action" "$@"
      systemctl start "${SERVICE_NAME}.service"
      if ! wait_for_service_ready; then
        report_service_readiness_failure
        die "service readiness check failed; see diagnostics above"
      fi
      ;;
    restart)
      need_root "$action" "$@"
      restart_service_ready || die "service readiness check failed; see diagnostics above"
      ;;
    stop)
      need_root "$action" "$@"
      systemctl stop "${SERVICE_NAME}.service"
      ;;
    status)
      systemctl --no-pager --full status "${SERVICE_NAME}.service"
      ;;
    logs)
      journalctl -u "${SERVICE_NAME}.service" -f
      ;;
    reset-password)
      need_root "$action" "$@"
      reset_password "$@"
      ;;
    uninstall)
      need_root "$action" "$@"
      uninstall_app
      ;;
    -h|--help|help)
      usage
      ;;
  esac
}

main "$@"
