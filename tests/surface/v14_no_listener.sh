#!/usr/bin/env bash
# V14 smoke hook: hearthd serve with nuntius enabled opens zero listening sockets beyond the configured bind.
# (Not wired into CI workflows — release gate does that later).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${ROOT_DIR}/hearthd.v14"
WORK="$(mktemp -d)"
DAEMON_PID=""

cleanup() {
    if [[ -n "$DAEMON_PID" ]]; then
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
    rm -f "$BIN"
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "=== building hearthd ==="
(cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/hearthd)

# Find a free port on loopback
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
LISTEN="127.0.0.1:${PORT}"

# Setup hearth home
mkdir -p "$WORK/hearth/sessions/main"
printf -- '---\nname: SOUL\ntemplate: minimal\n---\nTest soul.\n' > "$WORK/hearth/SOUL.md"
printf -- '---\nname: IDENTITY\ntemplate: minimal\n---\nTest identity.\n' > "$WORK/hearth/IDENTITY.md"
printf -- '---\nname: USER\ntemplate: minimal\n---\nTest user.\n' > "$WORK/hearth/USER.md"

# 1. Start hearthd serve with minimal config (no nuntius)
cat > "$WORK/lararium_no_nuntius.yaml" <<EOF
hearth:
  home: $WORK/hearth
models:
  default: [p/m]
providers:
  - name: p
    base_url: http://127.0.0.1:9/v1
    api_key: dummy
serve:
  listen: ${LISTEN}
  allowed_hosts: ["localhost", "127.0.0.1"]
  approval_timeout: 5m
  turn_timeout: 60s
EOF

echo "=== starting hearthd without nuntius ==="
"$BIN" --config "$WORK/lararium_no_nuntius.yaml" serve > "$WORK/daemon1.log" 2>&1 &
DAEMON_PID=$!

# Wait for healthy
HEALTHY=0
for ((i = 0; i < 40; i++)); do
    if curl -sf "http://${LISTEN}/v1/health" > /dev/null 2>&1; then
        HEALTHY=1
        break
    fi
    sleep 0.25
done
if [[ "$HEALTHY" -ne 1 ]]; then
    echo "daemon 1 failed to become healthy. Log:"
    cat "$WORK/daemon1.log"
    exit 1
fi

# Capture listening sockets for DAEMON_PID
ss -ltnp | grep "pid=${DAEMON_PID}," | awk '{print $4}' | sort > "$WORK/ss_no_nuntius.txt"

kill "$DAEMON_PID"
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""
sleep 0.5

# 2. Start hearthd serve with nuntius enabled + dormant (bad token env)
cat > "$WORK/lararium_with_nuntius.yaml" <<EOF
hearth:
  home: $WORK/hearth
models:
  default: [p/m]
providers:
  - name: p
    base_url: http://127.0.0.1:9/v1
    api_key: dummy
serve:
  listen: ${LISTEN}
  allowed_hosts: ["localhost", "127.0.0.1"]
  approval_timeout: 5m
  turn_timeout: 60s
nuntius:
  enabled: true
  bot_token_env: NONEXISTENT_BAD_TOKEN_ENV_V14
EOF

echo "=== starting hearthd with nuntius enabled (dormant) ==="
"$BIN" --config "$WORK/lararium_with_nuntius.yaml" serve > "$WORK/daemon2.log" 2>&1 &
DAEMON_PID=$!

# Wait for healthy
HEALTHY=0
for ((i = 0; i < 40; i++)); do
    if curl -sf "http://${LISTEN}/v1/health" > /dev/null 2>&1; then
        HEALTHY=1
        break
    fi
    sleep 0.25
done
if [[ "$HEALTHY" -ne 1 ]]; then
    echo "daemon 2 failed to become healthy. Log:"
    cat "$WORK/daemon2.log"
    exit 1
fi

# Capture listening sockets for DAEMON_PID
ss -ltnp | grep "pid=${DAEMON_PID}," | awk '{print $4}' | sort > "$WORK/ss_with_nuntius.txt"

kill "$DAEMON_PID"
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""

echo "=== comparing listening sockets ==="
echo "Sockets without nuntius:"
cat "$WORK/ss_no_nuntius.txt"
echo "Sockets with nuntius:"
cat "$WORK/ss_with_nuntius.txt"

# Assert diff is empty
diff -u "$WORK/ss_no_nuntius.txt" "$WORK/ss_with_nuntius.txt"

# Also assert that the only listening socket is ${LISTEN}
if [[ "$(cat "$WORK/ss_with_nuntius.txt")" != "${LISTEN}" ]]; then
    echo "Unexpected listening socket in ss_with_nuntius: got $(cat "$WORK/ss_with_nuntius.txt"), want ${LISTEN}"
    exit 1
fi

echo "PASS: V14 no listener diff empty, zero extra listening sockets"
