#!/bin/sh
set -eu

APP_DIR="/opt/91"
LEGACY_DIR="/opt/video-site-91"
EXAMPLE="$APP_DIR/config.example.yaml"
PORT="${VIDEO_LISTEN_PORT:-9191}"

# Select the installation once, before deriving any default paths. Explicit
# configuration takes precedence over automatic legacy mount detection.
if [ -n "${VIDEO_DATA_DIR:-}" ]; then
  DATA_DIR="$VIDEO_DATA_DIR"
elif [ -n "${VIDEO_CONFIG:-}" ]; then
  case "$VIDEO_CONFIG" in
    "$LEGACY_DIR/data/"*) DATA_DIR="$LEGACY_DIR/data" ;;
    *) DATA_DIR="$APP_DIR/data" ;;
  esac
elif [ -d "$LEGACY_DIR/data" ]; then
  if [ -f "$APP_DIR/data/config.yaml" ] || [ -f "$APP_DIR/data/91.db" ] || [ -f "$APP_DIR/data/video-site.db" ]; then
    echo "[entrypoint] both legacy and current data exist; set VIDEO_DATA_DIR to select the installation" >&2
    exit 1
  fi
  DATA_DIR="$LEGACY_DIR/data"
else
  DATA_DIR="$APP_DIR/data"
fi
CONFIG="${VIDEO_CONFIG:-$DATA_DIR/config.yaml}"
export VIDEO_CONFIG="$CONFIG" VIDEO_DATA_DIR="$DATA_DIR"
export VIDEO_VERSION_FILE="${VIDEO_VERSION_FILE:-$DATA_DIR/.version}"

# Existing relative paths must still resolve from the original working directory.
if [ "$DATA_DIR" = "$LEGACY_DIR/data" ] || [ "$CONFIG" = "$LEGACY_DIR/data/config.yaml" ]; then
  cd "$LEGACY_DIR"
fi
if [ "${1:-}" = ./server ]; then shift; set -- "$APP_DIR/server" "$@"; fi

case "${1:-}" in
  "$APP_DIR/server")
    case "${2:-}" in
      reset-password|frontend) exec "$@" ;;
    esac
    ;;
esac

mkdir -p "$DATA_DIR" "$DATA_DIR/previews" "$DATA_DIR/uploads" "$DATA_DIR/spider91"

if [ ! -f "$CONFIG" ]; then
  if [ ! -f "$EXAMPLE" ]; then
    echo "[entrypoint] missing config template: $EXAMPLE" >&2
    exit 1
  fi

  mkdir -p "$(dirname "$CONFIG")"
  cp "$EXAMPLE" "$CONFIG"

  sed -i -E "s#^([[:space:]]*listen:[[:space:]]*).*\$#\1\"0.0.0.0:${PORT}\"#" "$CONFIG"
  sed -i -E "s#^([[:space:]]*data_dir:[[:space:]]*).*\$#\1\"${DATA_DIR}\"#" "$CONFIG"
  chmod 600 "$CONFIG"

  echo "[entrypoint] generated $CONFIG"
else
  echo "[entrypoint] using existing $CONFIG"
fi

if [ -n "${VIDEO_VERSION_FILE:-}" ] && [ -n "${VIDEO_IMAGE_VERSION:-}" ]; then
  mkdir -p "$(dirname "$VIDEO_VERSION_FILE")"
  printf '%s\n' "$VIDEO_IMAGE_VERSION" > "$VIDEO_VERSION_FILE"
fi

exec "$@"
