#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# != 1 ]]; then
  echo "Usage: scripts/prepare-frontend.sh NEW_OUTPUT_DIRECTORY" >&2
  exit 2
fi
output="$(realpath -m -- "$1")"
archive="${FRONTEND_ARCHIVE:-}"
if [[ -n "$archive" ]]; then archive="$(realpath -- "$archive")"; fi
if [[ -z "$archive" && -f "$ROOT_DIR/frontend-bundle/frontend.tar.gz" ]]; then
  archive="$ROOT_DIR/frontend-bundle/frontend.tar.gz"
fi
args=(--lock "$ROOT_DIR/frontend.lock.json" --output "$output")
if [[ -n "$archive" ]]; then args+=(--archive "$archive"); fi
cd "$ROOT_DIR/backend"
go run ./cmd/frontend-package "${args[@]}"
