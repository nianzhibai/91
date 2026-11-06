#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${OUT_DIR:-$ROOT_DIR/release}"
APP_NAME="${APP_NAME:-video-site-91}"
VERSION="${VERSION:-$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || date +%Y%m%d%H%M%S)}"

log() {
  printf '[release] %s\n' "$*"
}

usage() {
  cat <<EOF
Usage: scripts/build-release.sh

Builds precompiled release packages:
  release/video-site-91-linux-amd64.tar.gz
  release/video-site-91-linux-arm64.tar.gz

Environment overrides:
  OUT_DIR=$OUT_DIR
  APP_NAME=$APP_NAME
  VERSION=$VERSION
EOF
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

build_frontend() {
  need_cmd npm
  log "installing frontend dependencies"
  if [[ -f "$ROOT_DIR/package-lock.json" ]]; then
    npm --prefix "$ROOT_DIR" ci
  else
    npm --prefix "$ROOT_DIR" install
  fi

  log "verifying and building frontend"
  npm --prefix "$ROOT_DIR" run verify
}

build_package() {
  local goos="$1"
  local goarch="$2"
  local artifact="$APP_NAME-$goos-$goarch"
  local work="$OUT_DIR/.work/$artifact"

  rm -rf "$work"
  mkdir -p "$work"

  log "building backend for $goos/$goarch"
  (
    cd "$ROOT_DIR/backend"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags="-s -w" -o "$work/server" ./cmd/server
  )

  cp "$ROOT_DIR/backend/config.example.yaml" "$work/config.example.yaml"
  cp "$ROOT_DIR/install.sh" "$work/install.sh"
  cp -R "$ROOT_DIR/dist" "$work/dist"

  cat >"$work/README.txt" <<EOF
$APP_NAME $VERSION

This is a prebuilt release package.
Use install.sh in this package or from the repository to install it on a Linux server.
EOF

  chmod +x "$work/server"
  chmod +x "$work/install.sh"
  tar -C "$OUT_DIR/.work" -czf "$OUT_DIR/$artifact.tar.gz" "$artifact"
  log "wrote $OUT_DIR/$artifact.tar.gz"
}

main() {
  case "${1:-}" in
    -h|--help|help)
      usage
      exit 0
      ;;
    "")
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac

  need_cmd go
  need_cmd tar
  mkdir -p "$OUT_DIR/.work"
  build_frontend
  build_package linux amd64
  build_package linux arm64
  rm -rf "$OUT_DIR/.work"
  log "done"
}

main "$@"
