#!/usr/bin/env bash
set -euo pipefail

# Manager script placed alongside main.go
# Usage:
#   ./server.sh build|start|stop|restart|status|run
#
# Env vars:
#   GITHUB_TOKEN (required), GITHUB_OWNER (required), GITHUB_REPO (required)
#   TIMEZONE (default: Asia/Shanghai), TITLE_PREFIX (default: 服务端个人日报)
#   SLACK_WEBHOOK_URL (optional), RUN_LOG_FILE (optional)
#   GOOS/GOARCH for build (default linux/amd64)

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="$BASE_DIR"
BIN_DIR="$BASE_DIR/bin"
LOG_DIR="$BASE_DIR/logs"
PID_FILE="$BASE_DIR/app.pid"
BIN_PATH="$BIN_DIR/daily_report"

# Load .env from the same directory if present
if [[ -f "$BASE_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$BASE_DIR/.env"
  set +a
fi

mkdir -p "$BIN_DIR" "$LOG_DIR"

cmd_build() {
  : "${GOOS:=linux}"
  : "${GOARCH:=amd64}"
  export CGO_ENABLED=0
  echo "Building ($GOOS/$GOARCH) -> $BIN_PATH"
  ( cd "$APP_DIR" && GOOS="$GOOS" GOARCH="$GOARCH" go build -o "$BIN_PATH" )
  echo "Build completed."
}

is_running() {
  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    if [[ -n "${pid}" ]] && kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
  fi
  return 1
}

cmd_start() {
  if is_running; then
    echo "Already running with PID $(cat "$PID_FILE")."
    return 0
  fi
  if [[ ! -x "$BIN_PATH" ]]; then
    echo "Binary not found, building first..."
    cmd_build
  fi
  : "${TIMEZONE:=Asia/Shanghai}"
  : "${TITLE_PREFIX:=服务端个人日报}"
  export TIMEZONE TITLE_PREFIX SLACK_WEBHOOK_URL RUN_LOG_FILE || true
  echo "Starting... logs -> $LOG_DIR/app.log"
  nohup "$BIN_PATH" >>"$LOG_DIR/app.log" 2>&1 &
  echo $! > "$PID_FILE"
  echo "Started PID $(cat "$PID_FILE")."
}

cmd_stop() {
  if is_running; then
    local pid
    pid="$(cat "$PID_FILE")"
    echo "Stopping PID $pid ..."
    kill "$pid" || true
    for i in {1..10}; do
      if kill -0 "$pid" 2>/dev/null; then
        sleep 0.3
      else
        break
      fi
    done
    if kill -0 "$pid" 2>/dev/null; then
      echo "Force killing PID $pid"
      kill -9 "$pid" || true
    fi
    rm -f "$PID_FILE"
    echo "Stopped."
  else
    echo "Not running."
  fi
}

cmd_status() {
  if is_running; then
    echo "Running (PID $(cat "$PID_FILE"))"
  else
    echo "Not running"
  fi
}

cmd_restart() {
  cmd_stop || true
  cmd_start
}

cmd_run() {
  : "${TIMEZONE:=Asia/Shanghai}"
  : "${TITLE_PREFIX:=服务端个人日报}"
  export TIMEZONE TITLE_PREFIX SLACK_WEBHOOK_URL RUN_LOG_FILE || true
  if [[ ! -x "$BIN_PATH" ]]; then
    echo "Binary not found, building first..."
    cmd_build
  fi
  echo "Running in foreground..."
  exec "$BIN_PATH"
}

case "${1:-}" in
  build)   cmd_build ;;
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  run)     cmd_run ;;
  *)
    echo "Usage: $0 {build|start|stop|restart|status|run}"
    exit 1
    ;;
esac


