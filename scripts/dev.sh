#!/usr/bin/env bash
# Start everything needed to click around the Fleet console.
#
# Two processes, one command, no infrastructure. The object store is a
# directory and the model repository is a synthetic one, so nothing is
# downloaded, nothing needs a GPU, and nothing reaches the network.
#
#   ./scripts/dev.sh          gateway on :8080, control plane on :8081
#   ./scripts/dev.sh --real   same, but pulls come from huggingface.co
#
# The console is embedded in the gateway binary at build time, so the first run
# needs `make web` (or npm --prefix web ci && npm --prefix web run build). The
# embedded placeholder page tells you when that has not happened yet.

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

GATEWAY_PORT=${GATEWAY_PORT:-8080}
APISERVER_PORT=${APISERVER_PORT:-8081}
DATA_DIR=${DATA_DIR:-$ROOT/.dev}

HUB_FLAG=--dev
if [ "${1:-}" = "--real" ]; then
  HUB_FLAG=
  echo "==> pulling from huggingface.co; gated repositories need a --hub-token"
fi

if ! command -v go >/dev/null 2>&1; then
  echo "go is not on PATH" >&2
  exit 1
fi

mkdir -p "$DATA_DIR"

# The dist directory must exist for the embed directive to compile, even when
# it is only the placeholder.
mkdir -p core/internal/gateway/webui/dist
if [ ! -f core/internal/gateway/webui/dist/index.html ]; then
  echo "==> console not built; building it now (first run only)"
  if command -v npm >/dev/null 2>&1; then
    [ -d web/node_modules ] || npm --prefix web ci
    npm --prefix web run build
    cp -r web/dist/. core/internal/gateway/webui/dist/
  else
    echo "    npm is not on PATH. The console will serve a placeholder page," >&2
    echo "    and the Go build still works. Install Node to get the real UI." >&2
  fi
fi

check_ports() {
  for p in "$GATEWAY_PORT" "$APISERVER_PORT"; do
    if command -v lsof >/dev/null 2>&1 && lsof -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "port $p is already in use; stop that process or set $p to another port" >&2
      return 1
    fi
  done
  return 0
}

cleanup() {
  [ -n "${APISERVER_PID:-}" ] && kill "$APISERVER_PID" 2>/dev/null || true
  [ -n "${GATEWAY_PID:-}" ] && kill "$GATEWAY_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

check_ports

echo "==> control plane on :$APISERVER_PORT (data in $DATA_DIR)"
FLEET_DATA_DIR="$DATA_DIR" go -C core run ./cmd/fleet-apiserver \
  --listen "127.0.0.1:$APISERVER_PORT" \
  --data "$DATA_DIR" \
  $HUB_FLAG &
APISERVER_PID=$!

# Wait for the control plane rather than sleeping a guessed interval: the
# console polls it on load, and a request that arrives first shows an error the
# user would read as a bug.
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$APISERVER_PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$APISERVER_PID" 2>/dev/null; then
    echo "control plane exited during startup" >&2
    exit 1
  fi
  sleep 0.2
done

echo "==> gateway on :$GATEWAY_PORT (demo engine, no GPU needed)"
go -C core run ./cmd/fleet-gateway \
  --listen "127.0.0.1:$GATEWAY_PORT" \
  --demo &
GATEWAY_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$GATEWAY_PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    echo "gateway exited during startup" >&2
    exit 1
  fi
  sleep 0.2
done

cat <<EOF

  Console    http://127.0.0.1:$GATEWAY_PORT
  Gateway    http://127.0.0.1:$GATEWAY_PORT/v1
  Control    http://127.0.0.1:$APISERVER_PORT/api/v1

  What works here:
    Playground  a stub engine answers, so completions stream with no GPU
    Fleet       endpoints, TTFT, token usage, edition capabilities
    Models      pull a synthetic repository, watch bytes land in $DATA_DIR
    Cluster     stays empty until an operator reports in, which is the
                honest state of a laptop with no GPUs

  Ctrl-C stops both.

EOF

# Either process dying takes the other down. Left alone, a gateway that lost a
# port race exits instantly while the control plane keeps running, and the
# script then sits on `wait` with a console URL that answers nothing.
while :; do
  if ! kill -0 "$APISERVER_PID" 2>/dev/null; then
    echo "==> control plane exited; stopping the gateway" >&2
    break
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    echo "==> gateway exited; stopping the control plane" >&2
    break
  fi
  sleep 1
done
