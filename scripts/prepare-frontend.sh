#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# != 1 ]]; then
  echo "Usage: scripts/prepare-frontend.sh NEW_OUTPUT_DIRECTORY" >&2
  exit 2
fi
output="$(realpath -m -- "$1")"
# Offline builds provide <name>.tar.gz for every frontend in frontend.lock.json.
archives="${FRONTEND_ARCHIVES:-}"
if [[ -n "$archives" ]]; then archives="$(realpath -- "$archives")"; fi
if [[ -z "$archives" ]] && compgen -G "$ROOT_DIR/frontend-bundle/*.tar.gz" >/dev/null; then
  archives="$ROOT_DIR/frontend-bundle"
fi
args=(--lock "$ROOT_DIR/frontend.lock.json" --output "$output")
if [[ -n "$archives" ]]; then args+=(--archives "$archives"); fi
cd "$ROOT_DIR/backend"
go run ./cmd/frontend-package "${args[@]}"
