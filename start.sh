#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export HOME="${HOME:-/root}"
export XDG_CACHE_HOME="${XDG_CACHE_HOME:-$HOME/.cache}"
export GOCACHE="${GOCACHE:-/tmp/91/go-build}"

FRONTEND_HOST="${FRONTEND_HOST:-0.0.0.0}"
FRONTEND_PORT="${FRONTEND_PORT:-9191}"
FRONTEND_MODE="${FRONTEND_MODE:-preview}"
BACKEND_PORT="${BACKEND_PORT:-9192}"
export BACKEND_PORT
DEV_CONFIG="$ROOT_DIR/backend/config.dev.yaml"
LOG_DIR="${LOG_DIR:-/tmp/91-dev}"

FRONTEND_LOG="$LOG_DIR/frontend.log"
BACKEND_LOG="$LOG_DIR/backend.log"

usage() {
  cat <<EOF
Usage: ./start.sh [--restart|--stop|--status]

Local development launcher (use deploy.sh for systemd deployment).
Backend configuration: $DEV_CONFIG
New development data: $ROOT_DIR/backend/data-dev
BACKEND_PORT sets both the backend listener and the Vite proxy target.

Environment overrides:
  FRONTEND_HOST=$FRONTEND_HOST
  FRONTEND_PORT=$FRONTEND_PORT
  FRONTEND_MODE=$FRONTEND_MODE  # preview (default, no HMR) or dev
  BACKEND_PORT=$BACKEND_PORT
  LOG_DIR=$LOG_DIR

Logs:
  frontend: $FRONTEND_LOG
  backend:  $BACKEND_LOG
EOF
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

pids_on_port() {
  local port="$1"
  ss -H -ltnp "sport = :$port" 2>/dev/null \
    | sed -nE 's/.*pid=([0-9]+).*/\1/p' \
    | sort -u
}

owns_dev_process() {
  local pid="$1"
  [[ -r "/proc/$pid/environ" ]] &&
    tr '\0' '\n' <"/proc/$pid/environ" | grep -Fx "VIDEO_DEV_ROOT=$ROOT_DIR" >/dev/null
}

check_port_owner() {
  local name="$1" port="$2" pid
  for pid in $(pids_on_port "$port"); do
    if ! owns_dev_process "$pid"; then
      echo "$name port $port is occupied by another service (pid: $pid); choose a different port" >&2
      return 1
    fi
  done
}

validate_ports() {
  local port
  for port in "$FRONTEND_PORT" "$BACKEND_PORT"; do
    if [[ ! "$port" =~ ^[1-9][0-9]{0,4}$ ]] || (( port > 65535 )); then
      echo "ports must be integers between 1 and 65535" >&2
      return 1
    fi
  done
  if [[ "$FRONTEND_PORT" == "$BACKEND_PORT" ]]; then
    echo "frontend and backend must use different ports" >&2
    return 1
  fi
}

print_port_status() {
  local name="$1"
  local port="$2"
  local pids
  pids="$(pids_on_port "$port" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
  if [[ -n "$pids" ]]; then
    if check_port_owner "$name" "$port"; then
      echo "$name listening on port $port (pid: $pids)"
    fi
  else
    echo "$name not listening on port $port"
  fi
}

stop_port() {
  local name="$1"
  local port="$2"
  local pids
  check_port_owner "$name" "$port"
  pids="$(pids_on_port "$port" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
  if [[ -z "$pids" ]]; then
    echo "$name is not running on port $port"
    return
  fi

  echo "stopping $name on port $port (pid: $pids)"
  kill $pids 2>/dev/null || true

  for _ in $(seq 1 20); do
    if [[ -z "$(pids_on_port "$port")" ]]; then
      return
    fi
    sleep 0.2
  done

  echo "$name did not stop gracefully; sending SIGKILL"
  kill -9 $pids 2>/dev/null || true
}

wait_for_port() {
  local name="$1"
  local port="$2"
  for _ in $(seq 1 60); do
    if [[ -n "$(pids_on_port "$port")" ]]; then
      check_port_owner "$name" "$port"
      print_port_status "$name" "$port"
      return 0
    fi
    sleep 0.5
  done
  echo "$name did not start on port $port. Check logs in $LOG_DIR" >&2
  return 1
}

start_backend() {
  check_port_owner "backend" "$BACKEND_PORT"
  if [[ -n "$(pids_on_port "$BACKEND_PORT")" ]]; then
    print_port_status "backend" "$BACKEND_PORT"
    return
  fi

  need_cmd go
  need_cmd node
  mkdir -p "$LOG_DIR" "$GOCACHE"
  node "$ROOT_DIR/scripts/prepare-dev-config.mjs" "$ROOT_DIR/backend/config.example.yaml" "$DEV_CONFIG" "$BACKEND_PORT"
  echo "starting backend on 127.0.0.1:$BACKEND_PORT"
  (
    cd "$ROOT_DIR/backend"
    export VIDEO_CONFIG="$DEV_CONFIG" VIDEO_DEV_ROOT="$ROOT_DIR"
    setsid nohup go run ./cmd/server >>"$BACKEND_LOG" 2>&1 </dev/null &
  )
  wait_for_port "backend" "$BACKEND_PORT"
}

start_frontend() {
  check_port_owner "frontend" "$FRONTEND_PORT"
  if [[ -n "$(pids_on_port "$FRONTEND_PORT")" ]]; then
    print_port_status "frontend" "$FRONTEND_PORT"
    return
  fi

  need_cmd npm
  mkdir -p "$LOG_DIR"
  if [[ "$FRONTEND_MODE" == "dev" ]]; then
    echo "starting frontend dev server on $FRONTEND_HOST:$FRONTEND_PORT"
    (
      cd "$ROOT_DIR"
      export VIDEO_DEV_ROOT="$ROOT_DIR"
      setsid nohup npm run dev -- --host "$FRONTEND_HOST" --port "$FRONTEND_PORT" >>"$FRONTEND_LOG" 2>&1 </dev/null &
    )
  else
    echo "building frontend for preview mode"
    (
      cd "$ROOT_DIR"
      npm run build >>"$FRONTEND_LOG" 2>&1
    )
    echo "starting frontend preview server on $FRONTEND_HOST:$FRONTEND_PORT"
    (
      cd "$ROOT_DIR"
      export VIDEO_DEV_ROOT="$ROOT_DIR"
      setsid nohup npm run preview -- --host "$FRONTEND_HOST" --port "$FRONTEND_PORT" >>"$FRONTEND_LOG" 2>&1 </dev/null &
    )
  fi
  wait_for_port "frontend" "$FRONTEND_PORT"
}

main() {
  local action="${1:-start}"

  case "$action" in
    start)
      need_cmd ss
      validate_ports
      check_port_owner "frontend" "$FRONTEND_PORT"
      check_port_owner "backend" "$BACKEND_PORT"
      start_backend
      start_frontend
      echo
      echo "ready:"
      echo "  frontend: http://127.0.0.1:$FRONTEND_PORT/"
      echo "  backend:  http://127.0.0.1:$BACKEND_PORT/"
      ;;
    --restart|restart)
      need_cmd ss
      validate_ports
      check_port_owner "frontend" "$FRONTEND_PORT"
      check_port_owner "backend" "$BACKEND_PORT"
      stop_port "frontend" "$FRONTEND_PORT"
      stop_port "backend" "$BACKEND_PORT"
      start_backend
      start_frontend
      echo
      echo "restarted:"
      echo "  frontend: http://127.0.0.1:$FRONTEND_PORT/"
      echo "  backend:  http://127.0.0.1:$BACKEND_PORT/"
      ;;
    --stop|stop)
      need_cmd ss
      validate_ports
      check_port_owner "frontend" "$FRONTEND_PORT"
      check_port_owner "backend" "$BACKEND_PORT"
      stop_port "frontend" "$FRONTEND_PORT"
      stop_port "backend" "$BACKEND_PORT"
      ;;
    --status|status)
      need_cmd ss
      validate_ports
      print_port_status "frontend" "$FRONTEND_PORT"
      print_port_status "backend" "$BACKEND_PORT"
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
