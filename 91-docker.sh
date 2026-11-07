#!/usr/bin/env bash
# jq expressions use single quotes intentionally; jq variables use --arg.
# shellcheck disable=SC2016
set -Eeuo pipefail

SERVICE="91"
SOURCE_SERVICE="91"
LEGACY_SERVICE="video-site-91"
MIGRATION_FILES_WRITTEN=false
MIGRATION_ORIGINALS=()
MIGRATION_CANDIDATES=()
IMAGE_REPOSITORY="ghcr.io/nianzhibai/91"
DEFAULT_IMAGE="$IMAGE_REPOSITORY:stable"
TEMPLATE_DIR="/usr/local/share/91/deploy"
# Individual files can reference services/resources supplied by overlays. The
# complete candidate is checked for consistency after the files are combined.
RAW_OPTIONS=(--no-interpolate --no-path-resolution --no-normalize --no-env-resolution --no-consistency)
COMPOSE_FILES=()
PROJECT_OPTIONS=()
GPU_CHOICES="[]"
ACTION=""
SELECTED_GPU="keep"
SELECTED_GPU_DEVICE=""
SELECTED_GPU_DEVICES="[]"
SELECTED_GPU_LABEL=""
FORCE_RECREATE=false
REQUESTED_IMAGE=""
TARGET_IMAGE=""
TARGET_IMAGE_ID=""
WAIT_TIMEOUT=90
WORK_DIR=""
TARGET=""
OLD_IMAGE=""
TEMPLATE_CONTAINER=""
NEEDS_ROLLBACK=false
DATABASE_LAYOUT_CHANGED=false
TARGET_START_ATTEMPTED=false
UPDATE_LOCK_FILE=""

# Keep JSON manipulation in one jq library. Shell values are passed with --arg;
# Compose parses YAML and preserves interpolation, env_file and relative paths.
JQ_LIBRARY=$(cat <<'JQ'
def environment_map:
  (. // {}) | if type == "array" then
    reduce .[] as $entry ({};
      ($entry | index("=")) as $index |
      if $index == null then .[$entry] = null
      else .[$entry[0:$index]] = $entry[$index + 1:] end)
  else . end;

def normalize_environment:
  .services[$service].environment |= environment_map;

def device_source:
  if type == "object" then (.source // "") else split(":")[0] end;

def device_target:
  if type == "object" then (.target // .source // "") else split(":") | .[1] // .[0] end;

def is_dri: . == "/dev/dri" or startswith("/dev/dri/");

def is_rockchip_device:
  . == "/dev/mpp_service" or . == "/dev/mpp-service" or
  . == "/dev/rga" or . == "/dev/dma_heap" or startswith("/dev/dma_heap/");

def is_gpu_device: is_dri or is_rockchip_device or startswith("/dev/nvidia");

def is_gpu_mapping: (device_source | is_gpu_device) or (device_target | is_gpu_device);

def gpu_modes:
  any((.devices // [])[]; (device_source | is_rockchip_device) or
      (device_target | is_rockchip_device)) as $rockchip |
  [if ((.gpus // [] | length) > 0 or .runtime == "nvidia" or
       any((.deploy.resources.reservations.devices // [])[]; .driver == "nvidia"))
   then "nvidia" else empty end,
   if $rockchip then "rockchip" else empty end,
   if $rockchip == false and any((.devices // [])[]; (device_source | is_dri) or (device_target | is_dri))
   then "vaapi" else empty end];

def remove_gpu:
  del(.gpus) |
  (if .runtime == "nvidia" then del(.runtime) else . end) |
  (if has("devices") then .devices |= map(select(is_gpu_mapping | not)) else . end) |
  (if .deploy.resources.reservations.devices != null then
     .deploy.resources.reservations.devices |= map(select(.driver != "nvidia"))
   else . end) |
  .environment |= (environment_map | del(.NVIDIA_DRIVER_CAPABILITIES, .NVIDIA_VISIBLE_DEVICES, .CUDA_VISIBLE_DEVICES));

def merge_deployment($template; $local):
  if ($template.services | has($service)) and ($local.services | has($service)) then
    ($template | normalize_environment) as $base |
    ($local | normalize_environment) as $existing |
    # Missing local ports/mounts can be intentional, e.g. network_mode: host.
    reduce ["ports", "expose", "volumes", "devices", "gpus", "runtime", "env_file",
            "container_name", "networks", "network_mode", "build", "profiles"][] as $key
      ($base * $existing;
       if ($existing.services[$service] | has($key)) then .
       else del(.services[$service][$key]) end)
  else error("Both deployment files must contain service " + $service) end;

def configure_gpu($mode; $device; $devices; $template):
  .services[$service] |= (
    if $mode == "keep" then .
    elif $mode == "cpu" then remove_gpu
    else
      (.environment | environment_map | .NVIDIA_DRIVER_CAPABILITIES // "") as $capabilities |
      remove_gpu |
      if $mode == "vaapi" then
        .devices = ((.devices // []) + [{source: $device, target: $device, permissions: "rwm"}])
      elif $mode == "rockchip" then
        .devices = ((.devices // []) + ($devices | map({source: ., target: ., permissions: "rwm"})))
      else
        .deploy.resources.reservations.devices =
          ((.deploy.resources.reservations.devices // []) +
           [($template.services[$service].deploy.resources.reservations.devices[0] |
             del(.count) | .device_ids = [$device])]) |
        .environment |= environment_map |
        if ($capabilities | contains("$")) then
          error("NVIDIA_DRIVER_CAPABILITIES uses interpolation; include compute,video,utility there before configuring NVIDIA access")
        else
          ($capabilities | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $values |
          if ($values | index("all")) != null then .environment.NVIDIA_DRIVER_CAPABILITIES = $capabilities
          else
            ($template.services[$service].environment | environment_map |
             .NVIDIA_DRIVER_CAPABILITIES | split(",")) as $required |
            .environment.NVIDIA_DRIVER_CAPABILITIES =
              (reduce $required[] as $value ($values;
                 if index($value) == null then . + [$value] else . end) | join(","))
          end
        end
      end
    end);
JQ
)

log() { printf '[docker-update] %s\n' "$*"; }
die() { printf '[docker-update] %s\n' "$*" >&2; exit 1; }

self_update_script() {
  [[ "${SELF_UPDATE:-1}" == 1 && "${VIDEO_SKIP_SELF_UPDATE:-0}" != 1 ]] || return 0
  if ! command -v curl >/dev/null 2>&1; then
    log "Script self-update skipped: curl is unavailable; continuing with the current script"
    return 0
  fi

  local script candidate header
  local url="${DOCKER_SCRIPT_URL:-https://raw.githubusercontent.com/nianzhibai/91/main/91-docker.sh}"
  if ! script=$(readlink -f -- "${BASH_SOURCE[0]}") || [[ ! -f "$script" ]]; then
    log "Script self-update skipped: run a local copy of 91-docker.sh to enable it"
    return 0
  fi
  if ! candidate=$(mktemp "$script.tmp.XXXXXX"); then
    log "Script self-update skipped: cannot write beside $script; continuing with the current script"
    return 0
  fi
  log "Checking for an updated Docker management script"
  if ! curl -fsSL --connect-timeout 10 --max-time 30 "$url" -o "$candidate"; then
    rm -f -- "$candidate"
    log "Script self-update skipped: download failed; continuing with the current script"
    return 0
  fi
  IFS= read -r header < "$candidate" || header=""
  if [[ "$header" != '#!/usr/bin/env bash' && "$header" != '#!/bin/bash' ]] || ! bash -n "$candidate"; then
    rm -f -- "$candidate"
    log "Script self-update skipped: downloaded file is not a valid Bash script"
    return 0
  fi
  if cmp -s -- "$candidate" "$script"; then
    rm -f -- "$candidate"
    return 0
  fi
  if ! chmod --reference="$script" "$candidate" ||
      ! chown --reference="$script" "$candidate" || ! mv -f -- "$candidate" "$script"; then
    rm -f -- "$candidate"
    log "Script self-update skipped: could not replace $script; continuing with the current script"
    return 0
  fi
  log "Management script updated; restarting the requested update"
  exec env VIDEO_SKIP_SELF_UPDATE=1 bash "$script" "$@"
}

ensure_jq() {
  if command -v jq >/dev/null 2>&1; then return; fi

  local manager package_manager=""
  local -a privilege=() install_command=()
  for manager in apt-get dnf yum apk zypper pacman; do
    if command -v "$manager" >/dev/null 2>&1; then
      package_manager="$manager"
      break
    fi
  done
  [[ -n "$package_manager" ]] || die "jq is missing and no supported package manager was found; install jq manually"
  if [[ "$(id -u)" != 0 ]]; then
    command -v sudo >/dev/null 2>&1 || die "Installing jq requires root privileges; run with sudo or install jq manually"
    privilege=(sudo)
  fi

  log "jq is missing; installing it automatically with $package_manager"
  case "$package_manager" in
    apt-get)
      "${privilege[@]}" env DEBIAN_FRONTEND=noninteractive apt-get update || die "Could not refresh package lists for jq installation"
      install_command=(env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends jq)
      ;;
    dnf|yum) install_command=("$package_manager" install -y jq) ;;
    apk) install_command=(apk add --no-cache jq) ;;
    zypper) install_command=(zypper --non-interactive install jq) ;;
    pacman) install_command=(pacman -S --needed --noconfirm jq) ;;
  esac
  "${privilege[@]}" "${install_command[@]}" || die "Automatic jq installation failed; fix the package manager error and retry"
  hash -r
  if ! command -v jq >/dev/null 2>&1 || ! jq --version >/dev/null 2>&1; then
    die "The package manager finished but jq is still unavailable; install jq manually"
  fi
  log "jq installed successfully"
}

json() {
  local -a options=()
  while (( $# > 1 )); do options+=("$1"); shift; done
  jq --arg service "$SERVICE" "${options[@]}" "$JQ_LIBRARY"$'\n'"$1"
}

usage() {
  local action="${1:-}"
  case "$action" in
    install|update)
      printf 'Usage: bash 91-docker.sh %s [--image IMAGE]\n\n' "$action"
      if [[ "$action" == install ]]; then
        printf 'Install 91 (default: stable); refuse to overwrite existing config.\n\n'
      else
        printf 'Update 91 using its configured image; keep config and data.\n\n'
      fi
      printf '  --image IMAGE          Image reference, stable, main, stable-rockchip or main-rockchip\n'
      if [[ "$action" == update ]]; then
        printf '\n  SELF_UPDATE=0          Skip management script self-update\n'
        printf '  DOCKER_SCRIPT_URL      Override the script download URL (default: repository main branch)\n'
      fi
      ;;
    gpu)
      cat <<'EOF'
Usage: bash 91-docker.sh gpu

Configure GPU access; choose a GPU when multiple are available.
Use CPU if none are usable. Requires a local Linux Docker Engine.
Rockchip requires install/update --image stable-rockchip first.
EOF
      ;;
    reset-password)
      cat <<'EOF'
Usage: bash 91-docker.sh reset-password

Reset a user's password. Requires 91 to be running.
EOF
      ;;
    uninstall)
      cat <<'EOF'
Usage: bash 91-docker.sh uninstall

Remove deployment containers, networks, images, volumes, config and backups.
Permanently delete 91/Telegram data; keep external video directories.
Host data cleanup requires a local Linux Docker Engine.
EOF
      ;;
    *)
      cat <<'EOF'
Usage: bash 91-docker.sh COMMAND [options]

Manage the 91 Docker deployment.

  install [--image IMAGE]  Install 91 (default: stable)
  update [--image IMAGE]   Update 91 (default: configured image)
  uninstall               Remove the deployment, images, configuration and application data (you'd better know what you're doing)
  reset-password          Reset a user's password (91 must be running)
  gpu                     Configure GPU access (select if multiple)
EOF
      ;;
  esac
  printf '\n  -h, --help             Show command help\n'
}

parse_args() {
  if (( $# == 0 )); then usage; exit 0; fi
  case "$1" in
    -h|--help) usage; exit 0 ;;
    install|update|uninstall|reset-password|gpu) ACTION="$1"; shift ;;
    *) die "Unknown command: $1 (see --help)" ;;
  esac
  while (( $# )); do
    case "$1" in
      -h|--help) usage "$ACTION"; exit 0 ;;
      --image)
        [[ "$ACTION" == install || "$ACTION" == update ]] || die "--image is only supported by install and update"
        if (( $# < 2 )) || [[ -z "${2:-}" || "${2:-}" == -* ]]; then die "Missing value for $1"; fi
        REQUESTED_IMAGE="$2"
        shift 2
        ;;
      *) die "Unknown argument for $ACTION: $1 (see $ACTION --help)" ;;
    esac
  done
  case "$REQUESTED_IMAGE" in
    main|stable|main-rockchip|stable-rockchip) REQUESTED_IMAGE="$IMAGE_REPOSITORY:$REQUESTED_IMAGE" ;;
  esac
}

compose_with() {
  local -a options=()
  while [[ "$1" != -- ]]; do
    options+=(-f "$1")
    shift
  done
  shift
  docker compose "${PROJECT_OPTIONS[@]}" "${options[@]}" "$@"
}

compose() { compose_with "${COMPOSE_FILES[@]}" -- "$@"; }

select_existing_service() {
  local names has_current=false has_legacy=false
  names=$(compose config --services)
  if [[ $'\n'"$names"$'\n' == *$'\n91\n'* ]]; then has_current=true; fi
  if [[ $'\n'"$names"$'\n' == *$'\nvideo-site-91\n'* ]]; then has_legacy=true; fi
  [[ "$has_current" != true || "$has_legacy" != true ]] || die "Both legacy and current services exist; resolve the deployment conflict first"
  if [[ "$has_legacy" == true ]]; then SERVICE="$LEGACY_SERVICE"
  elif [[ "$has_current" == true ]]; then SERVICE=91
  else die "Deployment must contain service 91 or video-site-91"
  fi
}

reset_password() {
  local -a options=()
  # Compose allocates a TTY by default. Preserve piped input and automation too.
  if [[ ! -t 0 || ! -t 1 ]]; then options+=(-T); fi
  compose exec "${options[@]}" "$SERVICE" docker-entrypoint.sh ./server reset-password
}

uninstall_deployment() {
  local source path protected endpoint operating_system file project_dir
  project_dir=$(dirname -- "$TARGET")
  local -a data_paths=() config_files=("${COMPOSE_FILES[@]}" "$project_dir/.env")
  compose config --format json > "$WORK_DIR/uninstall.json"
  json -e '.services | has($service)' < "$WORK_DIR/uninstall.json" >/dev/null ||
    die "Deployment must contain service $SERVICE"
  # Container mount targets identify application-owned storage. Never infer
  # ownership from a host directory name or delete every mounted video library.
  json -r '
    .services[$service] as $site |
    ($site.environment.VIDEO_DATA_DIR //
      (if any($site.volumes[]?; .target == "/opt/video-site-91/data" or .target == "/opt/video-site-91")
       then "/opt/video-site-91/data" else "/opt/91/data" end) | rtrimstr("/")) as $data |
    ($site.environment.VIDEO_CONFIG // ($data + "/config.yaml")) as $config |
    [.services | to_entries[] |
      select(.key == $service or .key == "telegram-bot-api") |
      .key as $name | .value.volumes[]? |
      select(.type == "bind") |
      select(if $name == $service then
        .target == $data or (.target | startswith($data + "/")) or .target == $config
      else .target == "/var/lib/telegram-bot-api" or
        (.target | startswith("/var/lib/telegram-bot-api/")) end) |
      .source] | unique | .[] |
      if startswith("/") and (test("[\u0000-\u001f]") | not) then .
      else error("Application data mount needs an absolute host path without control characters") end
  ' < "$WORK_DIR/uninstall.json" > "$WORK_DIR/data-paths"

  if [[ -s "$WORK_DIR/data-paths" ]]; then
    endpoint="${DOCKER_HOST:-}"
    if [[ -n "${DOCKER_CONTEXT:-}" || -z "$endpoint" ]]; then
      endpoint=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
    fi
    operating_system=$(docker info --format '{{.OperatingSystem}}')
    [[ "$(uname -s)" == Linux && "$endpoint" == unix://* && "${operating_system,,}" != *"docker desktop"* ]] ||
      die "Data bind-mount cleanup requires a local Linux Docker Engine; run uninstall on the Docker host"
  fi
  while IFS= read -r source; do
    path=$(realpath -m -- "$source")
    # Check every path before stopping services. Ancestors of the deployment,
    # script or home directory must never become recursive deletion targets.
    for protected in / /bin /boot /dev /etc /home /lib /lib64 /media /mnt /opt /proc /root /run /sbin /srv /sys /tmp /usr /var /var/lib /var/lib/docker /var/lib/containerd \
      "$project_dir" "$HOME" "${COMPOSE_FILES[@]}" "$(realpath -m -- "${BASH_SOURCE[0]}")"; do
      [[ "$protected" != "$path" && "$protected" != "$path/"* ]] ||
        die "Refusing to delete unsafe application data path: $source ($path)"
    done
    data_paths+=("$path")
    if [[ "$source" != "$path" ]]; then data_paths+=("$source"); fi
  done < "$WORK_DIR/data-paths"

  log "Removing deployment containers, networks, images and volumes"
  compose down --remove-orphans --volumes --rmi all
  for path in "${data_paths[@]}"; do
    log "Deleting application data: $path"
    rm -rf --one-file-system -- "$path"
  done
  # Keep deployment files until Docker and data cleanup succeed, so failures can
  # be retried with the original project name, mounts and image references.
  for file in "${COMPOSE_FILES[@]}"; do
    for path in "$file".bak-*; do
      [[ -e "$path" || -L "$path" ]] || continue
      rm -f -- "$path"
    done
  done
  rm -f -- "${config_files[@]}"
  rm -rf -- "$WORK_DIR"
  WORK_DIR=""
  # All deployment mutations are finished before the lock pathname is removed.
  rm -f -- "$UPDATE_LOCK_FILE"
  log "Uninstall complete: deployment resources, configuration and application data removed"
}

raw_json() { compose_with "$@" -- config --format json "${RAW_OPTIONS[@]}"; }
start_with() {
  local -a options=()
  if [[ "$ACTION" == gpu ]]; then options+=(--no-deps); fi
  if [[ "$FORCE_RECREATE" == true ]]; then options+=(--force-recreate); fi
  compose_with "$@" -- up -d --no-build --pull never --wait --wait-timeout "$WAIT_TIMEOUT" "${options[@]}" "$SERVICE"
}

deployment_files() {
  local environment line configured_files="" separator=: path directory name
  if (( ${#COMPOSE_FILES[@]} == 0 )); then
    # An empty stdin project resolves .env even before the first deployment.
    # Never source user configuration as shell code.
    environment=$(docker compose --project-directory "$PWD" -f - config --environment <<< 'services: {}')
    while IFS= read -r line; do
      case "$line" in
        COMPOSE_FILE=*) configured_files="${line#*=}" ;;
        COMPOSE_PATH_SEPARATOR=*) separator="${line#*=}" ;;
      esac
    done <<< "$environment"
    if [[ -n "$configured_files" ]]; then
      [[ ${#separator} -eq 1 ]] || die "COMPOSE_PATH_SEPARATOR must be one character"
      IFS="$separator" read -r -a COMPOSE_FILES <<< "$configured_files"
    else
      for name in compose.yaml compose.yml docker-compose.yml docker-compose.yaml; do
        if [[ -f "$name" ]]; then COMPOSE_FILES+=("$name"); break; fi
      done
      if (( ${#COMPOSE_FILES[@]} == 0 )); then COMPOSE_FILES=(docker-compose.yml); fi
      for name in compose.override.yml compose.override.yaml docker-compose.override.yml docker-compose.override.yaml; do
        if [[ -f "$name" ]]; then COMPOSE_FILES+=("$name"); break; fi
      done
    fi
  fi
  local -a absolute=()
  for path in "${COMPOSE_FILES[@]}"; do
    if (( ${#absolute[@]} > 0 )) || [[ -e "$path" || -L "$path" ]]; then
      [[ -f "$path" && ! -L "$path" ]] || die "Compose file must be a regular file, not a symlink: $path"
    fi
    directory=$(cd -- "$(dirname -- "$path")" && pwd -P)
    absolute+=("$directory/$(basename -- "$path")")
  done
  COMPOSE_FILES=("${absolute[@]}")
  TARGET="${COMPOSE_FILES[0]}"
  PROJECT_OPTIONS=(--project-directory "$(dirname -- "$TARGET")")
  if [[ -f "$TARGET" ]]; then
    [[ "$ACTION" != install ]] || die "Compose configuration already exists; use update to preserve its settings"
  else
    [[ "$ACTION" == install ]] || die "No Compose deployment found; $ACTION requires an existing Compose configuration; use install for first deployment"
  fi
}

list_render_devices() {
  local device
  for device in /dev/dri/renderD*; do
    if [[ -c "$device" ]]; then printf '%s\n' "$device"; fi
  done
}

render_vendor() { cat "/sys/class/drm/${1##*/}/device/vendor"; }

pci_gpu_vendors() {
  local vendor_file device_class
  for vendor_file in /sys/bus/pci/devices/*/vendor; do
    [[ -r "$vendor_file" && -r "${vendor_file%/*}/class" ]] || continue
    read -r device_class < "${vendor_file%/*}/class"
    if [[ "$device_class" == 0x03* ]]; then cat "$vendor_file"; fi
  done
}

gpu_ready() {
  json -e --arg mode "$1" 'any(.[]; .mode == $mode)' <<< "$GPU_CHOICES" >/dev/null
}

add_gpu_choice() {
  GPU_CHOICES=$(json --arg mode "$1" --arg device "$2" --arg label "$3" '
    . + [{mode: $mode, device: $device, label: $label}]
  ' <<< "$GPU_CHOICES")
}

render_device_name() {
  local device_path="/sys/class/drm/${1##*/}/device" name line
  if [[ -r "$device_path/product_name" ]]; then
    IFS= read -r name < "$device_path/product_name" || true
    if [[ -n "$name" ]]; then printf '%s\n' "$name"; return; fi
  fi
  if command -v lspci >/dev/null 2>&1 && [[ -r "$device_path/uevent" ]]; then
    while IFS= read -r line; do
      if [[ "$line" == PCI_SLOT_NAME=* ]]; then
        if name=$(lspci -s "${line#*=}") && [[ -n "$name" ]]; then
          printf '%s\n' "$name"
          return
        fi
        break
      fi
    done < "$device_path/uevent"
  fi
  case "$2" in
    0x8086) printf 'Intel GPU\n' ;;
    0x1002) printf 'AMD GPU\n' ;;
  esac
}

select_gpu() {
  local count choice selected
  count=$(json 'length' <<< "$GPU_CHOICES")
  if (( count == 0 )); then
    SELECTED_GPU=cpu
    log "No usable GPU detected; 91 will use the CPU to generate video covers and previews."
    return
  fi
  choice=1
  if (( count > 1 )); then
    printf 'Available GPUs:\n' >&2
    json -r 'to_entries[] | "  \(.key + 1)) \(.value.label) [\(.value.mode), \(.value.device)]"' <<< "$GPU_CHOICES" >&2
    while true; do
      printf 'Select GPU [1-%s] (q to cancel): ' "$count" >&2
      IFS= read -r choice || die "GPU selection cancelled before deployment changes; select a GPU interactively or provide its number on stdin"
      case "$choice" in q|Q) die "GPU selection cancelled before deployment changes" ;; esac
      if [[ "$choice" =~ ^[1-9][0-9]*$ ]] &&
        json -e --arg choice "$choice" '($choice | tonumber) <= length' <<< "$GPU_CHOICES" >/dev/null; then
        break
      fi
      printf 'Enter a number from 1 to %s, or q to cancel.\n' "$count" >&2
    done
  fi
  selected=$(json --arg choice "$choice" '.[$choice | tonumber - 1]' <<< "$GPU_CHOICES")
  SELECTED_GPU=$(json -r '.mode' <<< "$selected")
  SELECTED_GPU_DEVICE=$(json -r '.device' <<< "$selected")
  SELECTED_GPU_LABEL=$(json -r '.label' <<< "$selected")
  SELECTED_GPU_DEVICES=$(json '.devices // []' <<< "$selected")
}

rockchip_devices() {
  local device mpp=""
  for device in /dev/mpp_service /dev/mpp-service; do
    if [[ -c "$device" ]]; then mpp="$device"; break; fi
  done
  [[ -n "$mpp" ]] || return 0
  printf '%s\n' "$mpp"
  for device in /dev/dri /dev/dma_heap /dev/rga; do
    if [[ -e "$device" ]]; then printf '%s\n' "$device"; fi
  done
}

detect_host_gpu() {
  local info="$1" endpoint="${DOCKER_HOST:-}" operating_system rootless device vendor name devices vendors
  GPU_CHOICES="[]"
  if [[ -n "${DOCKER_CONTEXT:-}" || -z "$endpoint" ]]; then
    endpoint=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
  fi
  operating_system=$(json -r '.OperatingSystem // "" | ascii_downcase' < "$info")
  [[ "$(uname -s)" == Linux && "$endpoint" == unix://* && "$operating_system" != *"docker desktop"* ]] ||
    die "GPU detection requires a local Linux Docker Engine; configure GPU access in Compose for other Docker hosts"
  rootless=$(json -r 'any((.SecurityOptions // [])[]; contains("rootless"))' < "$info")
  devices=$(rockchip_devices)
  if [[ -n "$devices" ]]; then
    local mpp inaccessible=false
    mpp="${devices%%$'\n'*}"
    while IFS= read -r device; do
      if [[ "$rootless" == true && ( ! -r "$device" || ! -w "$device" ) ]]; then
        inaccessible=true
        log "Cannot access $device; check Rockchip media device permissions"
      fi
    done <<< "$devices"
    if [[ "$inaccessible" == false ]]; then
      devices=$(json -Rn '[inputs | select(length > 0)]' <<< "$devices")
      GPU_CHOICES=$(json --arg device "$mpp" --argjson devices "$devices" '
        . + [{mode: "rockchip", device: $device, label: "Rockchip MPP / RGA", devices: $devices}]
      ' <<< "$GPU_CHOICES")
    fi
  fi
  while IFS= read -r device; do
    [[ -n "$device" ]] || continue
    vendor=$(render_vendor "$device" 2>/dev/null) || continue
    case "$vendor" in 0x8086|0x1002) ;; *) continue ;; esac
    if [[ "$rootless" != true || ( -r "$device" && -w "$device" ) ]]; then
      add_gpu_choice vaapi "$device" "$(render_device_name "$device" "$vendor")"
    else
      log "Cannot access $device; check render/video group permissions"
    fi
  done < <(list_render_devices)
  vendors=$(pci_gpu_vendors)
  if [[ "$vendors" == *0x8086* || "$vendors" == *0x1002* ]] && ! gpu_ready vaapi; then
    log "Intel/AMD GPU found, but no usable /dev/dri/renderD* device; check the host kernel driver and device permissions"
  fi
  if command -v nvidia-smi >/dev/null 2>&1; then
    if devices=$(timeout 10s nvidia-smi --query-gpu=uuid,name --format=csv,noheader) && [[ -n "$devices" ]]; then
      if json -e '.Runtimes | has("nvidia")' < "$info" >/dev/null; then
        while IFS=, read -r device name; do
          device="${device//[[:space:]]/}"
          name="${name#"${name%%[![:space:]]*}"}"
          [[ "$device" =~ ^GPU-[[:xdigit:]-]+$ && -n "$name" ]] ||
            die "Cannot identify an NVIDIA GPU from nvidia-smi output; check the host driver"
          add_gpu_choice nvidia "$device" "$name"
        done <<< "$devices"
      else
        log "NVIDIA driver works, but Docker has no NVIDIA runtime; install NVIDIA Container Toolkit and configure Docker"
      fi
    elif [[ "$vendors" == *0x10de* ]]; then
      log "NVIDIA GPU found, but nvidia-smi failed or timed out; check the host driver"
    fi
  elif [[ "$vendors" == *0x10de* ]]; then
    log "NVIDIA GPU found, but nvidia-smi is missing; install the host NVIDIA driver"
  fi
}

restore_deployment() {
  if [[ "$ACTION" == install ]]; then
    log "Installation failed; generated Compose configuration and application data have been kept. Fix the issue and run the script again"
    return
  fi
  if [[ "$DATABASE_LAYOUT_CHANGED" == true && "$TARGET_START_ATTEMPTED" == true ]]; then
    compose stop "$SERVICE" || true
    log "Automatic image rollback skipped: the new version may have renamed the database. Configuration and data kept; fix the startup error and retry update"
    return
  fi
  log "Deployment failed; restoring the original Compose configuration"
  if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then
    if [[ "$MIGRATION_FILES_WRITTEN" == true ]]; then
      compose stop "$SERVICE" >/dev/null 2>&1 || true
      compose rm -f "$SERVICE" >/dev/null 2>&1 || true
    fi
    local index
    for index in "${!COMPOSE_FILES[@]}"; do
      if ! cp -p -- "${MIGRATION_ORIGINALS[index]}" "$WORK_DIR/restore-$index.yml" ||
          ! mv -f -- "$WORK_DIR/restore-$index.yml" "${COMPOSE_FILES[index]}"; then
        log "Configuration restore failed; recover Compose files from their .bak backups"
        return
      fi
    done
    SERVICE="$SOURCE_SERVICE"
  else
    if ! mv -f -- "$WORK_DIR/original.yml" "$TARGET"; then
      log "Configuration restore failed; recover from $BACKUP_FILE"
      return
    fi
  fi
  if [[ -z "$OLD_IMAGE" ]]; then
    log "No previous local image was available for container rollback"
    return
  fi
  if ! json -n --arg image "$OLD_IMAGE" '
      {services: {($service): {image: $image}}}
    ' > "$WORK_DIR/rollback.yml"; then
    log "Could not prepare the previous image rollback"
    return
  fi
  if start_with "${COMPOSE_FILES[@]}" "$WORK_DIR/rollback.yml"; then
    log "Previous container image restored"
  else
    log "Container rollback failed; configuration is restored. Check docker compose logs"
  fi
}

finish() {
  local status="$1"
  trap - EXIT INT TERM HUP
  set +e
  if [[ "$NEEDS_ROLLBACK" == true ]]; then restore_deployment; fi
  if [[ -n "$TEMPLATE_CONTAINER" ]]; then docker rm -v "$TEMPLATE_CONTAINER" >/dev/null; fi
  if [[ -n "$WORK_DIR" ]]; then rm -rf -- "$WORK_DIR"; fi
  if (( status )); then log "$ACTION failed (exit $status)" >&2; fi
  exit "$status"
}

load_deployment_templates() {
  [[ -z "$TEMPLATE_CONTAINER" ]] || return 0
  TEMPLATE_CONTAINER=$(docker create --pull never --entrypoint /bin/true "$TARGET_IMAGE_ID")
  if ! docker cp "$TEMPLATE_CONTAINER:$TEMPLATE_DIR/docker-compose.yml" "$WORK_DIR/docker-compose.yml"; then
    die "The image has no 91 deployment templates; select an image built with the updated Dockerfile"
  fi
  raw_json "$WORK_DIR/docker-compose.yml" > "$WORK_DIR/template.json"
}

prepare_deployment() {
  local old_reference container_ids container previous_id=""
  local -a candidate_files
  TARGET_IMAGE="$REQUESTED_IMAGE"
  if [[ "$ACTION" != install ]]; then
    cp -p -- "$TARGET" "$WORK_DIR/original.yml"
    raw_json "$TARGET" > "$WORK_DIR/local.json"
    compose config --format json > "$WORK_DIR/effective.json"
    if json -e --arg legacy "$LEGACY_SERVICE" '.services | has($legacy)' < "$WORK_DIR/effective.json" >/dev/null; then
      json -e '.services | has($service) | not' < "$WORK_DIR/effective.json" >/dev/null || die "Both legacy and current services exist; resolve the deployment conflict first"
      SOURCE_SERVICE="$LEGACY_SERVICE"
      local index
      for index in "${!COMPOSE_FILES[@]}"; do
        MIGRATION_ORIGINALS+=("$WORK_DIR/original-$index.yml")
        cp -p -- "${COMPOSE_FILES[index]}" "${MIGRATION_ORIGINALS[index]}"
      done
    else
      json -e '.services | has($service)' < "$WORK_DIR/effective.json" >/dev/null || die "Deployment must contain service $SERVICE"
    fi
    old_reference=$(json -r --arg source "$SOURCE_SERVICE" '.services[$source].image // ""' < "$WORK_DIR/effective.json")
    if [[ -z "$TARGET_IMAGE" ]]; then TARGET_IMAGE="$old_reference"; fi
    # Preserve the actual container image even if its local tag has moved.
    container_ids=$(compose ps -a -q "$SOURCE_SERVICE")
    while IFS= read -r container; do
      [[ -n "$container" ]] || continue
      previous_id=$(docker inspect --format '{{.Image}}' "$container")
      [[ -z "$OLD_IMAGE" || "$OLD_IMAGE" == "$previous_id" ]] || die "Service replicas use different images; update them separately"
      OLD_IMAGE="$previous_id"
    done <<< "$container_ids"
    if [[ -z "$OLD_IMAGE" && -n "$old_reference" ]]; then
      if previous_id=$(docker image inspect --format '{{.Id}}' "$old_reference" 2>/dev/null); then OLD_IMAGE="$previous_id"; fi
    fi
  elif [[ -z "$TARGET_IMAGE" ]]; then
    TARGET_IMAGE="$DEFAULT_IMAGE"
  fi
  [[ -n "$TARGET_IMAGE" && "$TARGET_IMAGE" != -* ]] || die "The deployment must specify a 91 image"
  if [[ "$ACTION" == install || "$ACTION" == update ]]; then
    log "Pulling $TARGET_IMAGE"
    docker pull "$TARGET_IMAGE"
    TARGET_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$TARGET_IMAGE")
    if [[ -n "$OLD_IMAGE" ]]; then
      local old_database new_database
      old_database=$(docker image inspect --format '{{ index .Config.Labels "io.91.database-filename" }}' "$OLD_IMAGE")
      new_database=$(docker image inspect --format '{{ index .Config.Labels "io.91.database-filename" }}' "$TARGET_IMAGE_ID")
      # Images predating the filename label use video-site.db.
      case "$old_database" in ""|"<no value>") old_database=video-site.db ;; esac
      case "$new_database" in ""|"<no value>") new_database=video-site.db ;; esac
      if [[ "$old_database" != "$new_database" ]]; then DATABASE_LAYOUT_CHANGED=true; fi
    fi
  else
    [[ -n "$OLD_IMAGE" ]] || die "No local 91 image is available; run install or update explicitly"
    TARGET_IMAGE_ID="$OLD_IMAGE"
  fi
  printf '{}\n' > "$WORK_DIR/template.json"
  if [[ "$ACTION" == install || "$ACTION" == update ]]; then load_deployment_templates; fi
  if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then prepare_legacy_migration; fi
  if [[ "$ACTION" == install ]]; then
    cp -- "$WORK_DIR/template.json" "$WORK_DIR/local.json"
    candidate_files=("$WORK_DIR/docker-compose.yml" "${COMPOSE_FILES[@]:1}")
    compose_with "${candidate_files[@]}" -- config --format json > "$WORK_DIR/effective.json"
    container_ids=$(compose_with "${candidate_files[@]}" -- ps -a -q "$SERVICE")
    [[ -z "$container_ids" ]] || die "Existing service containers were found; restore their original Compose configuration and use update"
  fi
}

prepare_legacy_migration() {
  [[ "$ACTION" == update ]] || die "Update the legacy deployment before changing GPU settings"
  local index data_path
  data_path=$(json -r --arg source "$SOURCE_SERVICE" '.services[$source].environment | environment_map | .VIDEO_DATA_DIR // "/opt/video-site-91/data"' < "$WORK_DIR/effective.json")
  # Removing the old container must not detach anonymous or unmounted data.
  json -e --arg source "$SOURCE_SERVICE" --arg data "$data_path" '
    any(.services[$source].volumes[]?; .target as $target |
      .source != null and ($target == $data or ($data | startswith($target + "/"))))
  ' < "$WORK_DIR/effective.json" >/dev/null ||
    die "Legacy migration needs an explicit application data mount; map the existing data volume before retrying"
  for index in "${!COMPOSE_FILES[@]}"; do
    MIGRATION_CANDIDATES+=("$WORK_DIR/migrated-$index.yml")
    local -a migration_options=()
    if (( index == 0 )) && json -e --arg source "$SOURCE_SERVICE" '(.services[$source].networks // {} | keys) == ["default"]' < "$WORK_DIR/effective.json" >/dev/null; then
      migration_options+=(--default-network-alias)
    fi
    docker run --rm -i --network none --entrypoint python3 "$TARGET_IMAGE_ID" /usr/local/share/91/migrate-compose.py \
      "${migration_options[@]}" \
      < "${MIGRATION_ORIGINALS[index]}" > "${MIGRATION_CANDIDATES[index]}"
  done
  raw_json "${MIGRATION_CANDIDATES[0]}" > "$WORK_DIR/local.json"
  compose_with "${MIGRATION_CANDIDATES[@]}" -- config --format json > "$WORK_DIR/effective.json"
  json -e --arg legacy "$LEGACY_SERVICE" '.services | has($service) and (has($legacy) | not)' < "$WORK_DIR/effective.json" >/dev/null ||
    die "Included Compose files still define the legacy service; migrate those files before retrying"
}

prepare_gpu() {
  local mode
  printf '{}\n' > "$WORK_DIR/gpu.json"
  [[ "$ACTION" == gpu ]] || return 0
  command -v timeout >/dev/null 2>&1 || die "timeout is required for GPU detection"
  docker info --format '{{json .}}' > "$WORK_DIR/docker-info.json"
  detect_host_gpu "$WORK_DIR/docker-info.json"
  select_gpu
  case "$SELECTED_GPU" in
    rockchip)
      log "Selected Rockchip media devices; the application's hardware acceleration switch will not be changed"
      ;;
    nvidia|vaapi)
      log "Selected GPU: $SELECTED_GPU_LABEL ($SELECTED_GPU, $SELECTED_GPU_DEVICE); the application's GPU switch will not be changed"
      load_deployment_templates
      mode="docker-compose.gpu-$SELECTED_GPU.yml"
      docker cp "$TEMPLATE_CONTAINER:$TEMPLATE_DIR/$mode" "$WORK_DIR/$mode"
      raw_json "$WORK_DIR/docker-compose.yml" "$WORK_DIR/$mode" > "$WORK_DIR/gpu.json"
      ;;
  esac
}

build_candidate() {
  local -a candidate_files
  json -n --slurpfile template "$WORK_DIR/template.json" --slurpfile local "$WORK_DIR/local.json" \
    --slurpfile gpu "$WORK_DIR/gpu.json" --arg mode "$SELECTED_GPU" --arg device "$SELECTED_GPU_DEVICE" --arg image "$TARGET_IMAGE" \
    --argjson devices "$SELECTED_GPU_DEVICES" \
    --arg action "$ACTION" --arg requested_image "$REQUESTED_IMAGE" '
      (if $action == "install" or $action == "update" then merge_deployment($template[0]; $local[0]) else $local[0] end) |
      (if $action == "install" or $requested_image != "" then .services[$service].image = $image else . end) |
      configure_gpu($mode; $device; $devices; $gpu[0])
    ' > "$WORK_DIR/proposed.json"
  compose_with "$WORK_DIR/proposed.json" -- config --format yaml "${RAW_OPTIONS[@]}" > "$WORK_DIR/candidate.yml"
  if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then
    candidate_files=("$WORK_DIR/candidate.yml" "${MIGRATION_CANDIDATES[@]:1}")
  else
    candidate_files=("$WORK_DIR/candidate.yml" "${COMPOSE_FILES[@]:1}")
  fi
  compose_with "${candidate_files[@]}" -- config --format json > "$WORK_DIR/resolved.json"
}

validate_candidate() {
  json -e --arg image "$TARGET_IMAGE" '.services[$service].image == $image' < "$WORK_DIR/resolved.json" >/dev/null ||
    die "An overlay overrides the target image; update that overlay or its environment variable first"
  json -e --arg mode "$SELECTED_GPU" --slurpfile before "$WORK_DIR/effective.json" '
    (.services[$service] | gpu_modes) ==
    (if $mode == "keep" then ($before[0].services[$service] | gpu_modes)
     elif $mode == "cpu" then [] else [$mode] end)
  ' < "$WORK_DIR/resolved.json" >/dev/null || die "An overlay conflicts with the GPU configuration; update that overlay first"
  if json -e '.services[$service] | gpu_modes | index("rockchip") != null' < "$WORK_DIR/resolved.json" >/dev/null; then
    local variant
    variant=$(docker image inspect --format '{{ index .Config.Labels "io.91.media-variant" }}' "$TARGET_IMAGE_ID")
    [[ "$variant" == rockchip ]] ||
      die "Rockchip devices require a Rockchip image; run update --image stable-rockchip before gpu (custom images need the io.91.media-variant=rockchip label)"
  fi
  if [[ "$ACTION" == gpu && "$SELECTED_GPU" != cpu ]]; then
    # Matching the backend type is insufficient: overlays must not expose other
    # GPUs or override the card the user selected.
    json -e --arg mode "$SELECTED_GPU" --arg device "$SELECTED_GPU_DEVICE" --argjson devices "$SELECTED_GPU_DEVICES" '
      .services[$service] |
      ((.gpus // [] | length) == 0) and .runtime != "nvidia" and
      .privileged != true and
      all(.volumes[]?; .target != "/dev" and (.target | is_gpu_device | not)) and
      ((.environment | environment_map | .NVIDIA_VISIBLE_DEVICES) == null) and
      ((.environment | environment_map | .CUDA_VISIBLE_DEVICES) == null) and
      if $mode == "vaapi" then
        [.devices[]? | select(is_gpu_mapping)] ==
          [{source: $device, target: $device, permissions: "rwm"}]
      elif $mode == "rockchip" then
        [.devices[]? | select(is_gpu_mapping)] ==
          ($devices | map({source: ., target: ., permissions: "rwm"})) and
        all((.deploy.resources.reservations.devices // [])[]; .driver != "nvidia")
      else
        ([.devices[]? | select(is_gpu_mapping)] | length) == 0 and
        ([.deploy.resources.reservations.devices[]? | select(.driver == "nvidia")] |
          length == 1 and .[0].device_ids == [$device] and .[0].count == null and
          (.[0].capabilities | index("gpu")) != null) and
        ((.environment.NVIDIA_DRIVER_CAPABILITIES // "" | split(",") |
          map(gsub("^\\s+|\\s+$"; ""))) as $capabilities |
          ($capabilities | index("all")) != null or
          (["compute", "video", "utility"] - $capabilities | length) == 0)
      end
    ' < "$WORK_DIR/resolved.json" >/dev/null ||
      die "Compose settings override the selected GPU; remove conflicting GPU mappings, visibility settings or privileged access"
  fi
}

apply_candidate() {
  if [[ "$ACTION" == gpu ]] &&
    json -e --slurpfile before "$WORK_DIR/effective.json" '. == $before[0]' < "$WORK_DIR/resolved.json" >/dev/null; then
    return
  fi
  if [[ "$ACTION" != install ]]; then
    json -e --slurpfile before "$WORK_DIR/effective.json" '.name == $before[0].name' < "$WORK_DIR/resolved.json" >/dev/null ||
      die "The operation would change the Compose project name"
    cmp -s -- "$TARGET" "$WORK_DIR/original.yml" || die "Compose file changed during the operation; retry with the latest local configuration"
    if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then
      local index overlay_backup
      for index in "${!COMPOSE_FILES[@]}"; do
        cmp -s -- "${COMPOSE_FILES[index]}" "${MIGRATION_ORIGINALS[index]}" || die "Compose file changed during migration: ${COMPOSE_FILES[index]}"
      done
    fi
    BACKUP_FILE=$(mktemp "$TARGET.bak-$(date -u +%Y%m%dT%H%M%S).XXXXXX")
    cp -p -- "$TARGET" "$BACKUP_FILE"
    log "Configuration backup: $BACKUP_FILE"
    if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then
      for index in "${!COMPOSE_FILES[@]}"; do
        if (( index > 0 )); then
          overlay_backup=$(mktemp "${COMPOSE_FILES[index]}.bak-$(date -u +%Y%m%dT%H%M%S).XXXXXX")
          cp -p -- "${COMPOSE_FILES[index]}" "$overlay_backup"
        fi
      done
      NEEDS_ROLLBACK=true
      compose stop "$SOURCE_SERVICE"
      compose rm -f "$SOURCE_SERVICE"
      for index in "${!COMPOSE_FILES[@]}"; do
        if (( index > 0 )); then
          chmod --reference="${COMPOSE_FILES[index]}" "${MIGRATION_CANDIDATES[index]}"
          chown --reference="${COMPOSE_FILES[index]}" "${MIGRATION_CANDIDATES[index]}"
          mv -f -- "${MIGRATION_CANDIDATES[index]}" "${COMPOSE_FILES[index]}"
        fi
      done
    fi
    chmod --reference="$WORK_DIR/original.yml" "$WORK_DIR/candidate.yml"
    chown --reference="$WORK_DIR/original.yml" "$WORK_DIR/candidate.yml"
    NEEDS_ROLLBACK=true
    mv -f -- "$WORK_DIR/candidate.yml" "$TARGET"
    if [[ "$SOURCE_SERVICE" != "$SERVICE" ]]; then MIGRATION_FILES_WRITTEN=true; fi
  else
    # Publishing with a hard link is atomic and cannot overwrite a file created
    # by another process after discovery. Both paths are on the same filesystem.
    ln -- "$WORK_DIR/candidate.yml" "$TARGET" || die "Compose configuration appeared during installation; it has been left unchanged"
    NEEDS_ROLLBACK=true
  fi
  if [[ "$ACTION" == gpu ]]; then
    # Recreating a container must not follow a tag that has changed locally.
    json -n --arg image "$OLD_IMAGE" '{services: {($service): {image: $image}}}' > "$WORK_DIR/runtime-image.yml"
    start_with "${COMPOSE_FILES[@]}" "$WORK_DIR/runtime-image.yml"
  else
    TARGET_START_ATTEMPTED=true
    start_with "${COMPOSE_FILES[@]}"
  fi
  NEEDS_ROLLBACK=false
  log "Deployment complete. Compose configuration: $TARGET"
  case "$SELECTED_GPU" in nvidia|vaapi|rockchip) log "Enable hardware acceleration in the administration panel when ready" ;; esac
}

apply_deployment() {
  prepare_deployment
  prepare_gpu
  build_candidate
  validate_candidate
  apply_candidate
}

main() {
  parse_args "$@"
  # Restart before acquiring deployment locks or creating rollback state.
  if [[ "$ACTION" == update ]]; then self_update_script "$@"; fi
  if [[ "$ACTION" == reset-password ]]; then
    command -v docker >/dev/null 2>&1 || die "docker is required; install it before running this script"
    deployment_files
    select_existing_service
    reset_password
    return
  fi
  local command help_text option
  local -a required_commands=(docker flock cmp mktemp cp mv ln chmod chown)
  if [[ "$ACTION" == uninstall ]]; then required_commands=(docker flock mktemp realpath rm); fi
  for command in "${required_commands[@]}"; do
    command -v "$command" >/dev/null 2>&1 || die "$command is required; install it before running this script"
  done
  if [[ "$ACTION" != uninstall ]]; then
    help_text=$(docker compose config --help)
    for option in "${RAW_OPTIONS[@]}"; do
      [[ "$help_text" == *"$option"* ]] || die "Upgrade Docker Compose: config must support $option"
    done
  fi
  umask 077
  deployment_files
  ensure_jq
  if [[ "$ACTION" == uninstall ]]; then select_existing_service; fi
  # The directory lock remains stable even when uninstall removes the lock file.
  exec {DEPLOYMENT_DIRECTORY_LOCK}< "$(dirname -- "$TARGET")"
  flock -n "$DEPLOYMENT_DIRECTORY_LOCK" || die "Another deployment operation is running"
  # Lock a separate inode because the main Compose file is replaced atomically.
  UPDATE_LOCK_FILE="$(dirname -- "$TARGET")/.$(basename -- "$TARGET").update.lock"
  exec {UPDATE_LOCK}> "$UPDATE_LOCK_FILE"
  flock -n "$UPDATE_LOCK" || die "Another deployment operation is running"
  WORK_DIR=$(mktemp -d "$(dirname -- "$TARGET")/.docker-update.XXXXXX")
  trap 'finish "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
  if [[ "$ACTION" == uninstall ]]; then
    uninstall_deployment
  else
    apply_deployment
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
