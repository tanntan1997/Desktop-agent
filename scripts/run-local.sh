#!/usr/bin/env bash
# Build and run the web server (device-hub) and the agent together on this
# machine for local testing. Ctrl+C stops both.
#
#   ./scripts/run-local.sh            # or: make run-local
#   PORT=9090 AGENT_PORT=7101 ./scripts/run-local.sh  # use other ports
#
# Overrides: PORT (server, default 8080), AGENT_PORT (agent's local API,
# default 7100), AGENT_TOKEN, API_TOKEN, LOG_LEVEL (info).
# The agent uses its normal config file (see README "Configuration").
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-8080}"
AGENT_PORT="${AGENT_PORT:-7100}"
AGENT_TOKEN="${AGENT_TOKEN:-local-agent-token}"
API_TOKEN="${API_TOKEN:-local-api-token}"
LOG_LEVEL="${LOG_LEVEL:-info}"
LOG_DIR="dist/logs"

for spec in "PORT:$PORT" "AGENT_PORT:$AGENT_PORT"; do
  name="${spec%%:*}" port="${spec#*:}"
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "Port $port ($name) is already in use by:" >&2
    lsof -nP -iTCP:"$port" -sTCP:LISTEN | awk 'NR>1 {print "  " $1 " (pid " $2 ")"}' | sort -u >&2
    echo "Stop that program, or pick another port, e.g.: $name=$((port + 1)) $0" >&2
    exit 1
  fi
done

echo "==> Building"
make --no-print-directory build build-server

mkdir -p "$LOG_DIR"
pids=()
cleanup() {
  trap - INT TERM EXIT
  echo
  echo "==> Stopping"
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup INT TERM EXIT

echo "==> Starting server on :$PORT (log: $LOG_DIR/hub.log)"
./dist/device-hub -listen "127.0.0.1:$PORT" -agent-token "$AGENT_TOKEN" \
  -api-token "$API_TOKEN" -log-level "$LOG_LEVEL" >"$LOG_DIR/hub.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  if ! kill -0 "${pids[0]}" 2>/dev/null; then
    echo "Server failed to start:" >&2; cat "$LOG_DIR/hub.log" >&2; exit 1
  fi
  sleep 0.1
done

echo "==> Starting agent (log: $LOG_DIR/agent.log)"
./dist/device-agent -server "ws://127.0.0.1:$PORT/agent" -server-token "$AGENT_TOKEN" \
  -listen "127.0.0.1:$AGENT_PORT" -log-level "$LOG_LEVEL" >"$LOG_DIR/agent.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 50); do
  grep -q "agent connected" "$LOG_DIR/hub.log" && break
  if ! kill -0 "${pids[1]}" 2>/dev/null; then
    echo "Agent failed to start:" >&2; cat "$LOG_DIR/agent.log" >&2; exit 1
  fi
  sleep 0.2
done
grep -q "agent connected" "$LOG_DIR/hub.log" || echo "warning: agent has not connected yet; check $LOG_DIR/agent.log" >&2

cat <<EOF

  Dashboard:  http://localhost:$PORT/?token=$API_TOKEN
              (or open http://localhost:$PORT and enter token: $API_TOKEN)

  Devices:    curl -H "Authorization: Bearer $API_TOKEN" localhost:$PORT/api/devices

  Press Ctrl+C to stop. Live logs below.

EOF

tail -n +1 -f "$LOG_DIR/hub.log" "$LOG_DIR/agent.log" &
pids+=($!)
wait "${pids[0]}" "${pids[1]}"
