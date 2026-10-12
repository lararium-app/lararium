#!/usr/bin/env bash
# V17: Policy rules surface routes (docs/SURFACE-SPEC.md §4, §9 V17).
# Checks GET/PUT/DELETE /v1/policy/rules with unlocked, locked, and corrupt fixtures.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${ROOT_DIR}/hearthd.vsuite"
WORK="$(mktemp -d)"
FAKE_PID=""
DAEMON_PID=""
PASS=0
FAIL=0
FAILED_LIST=""

cleanup() {
    [[ -n "$DAEMON_PID" ]] && kill "$DAEMON_PID" 2>/dev/null
    [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null
    wait 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
bad()  { FAIL=$((FAIL + 1)); FAILED_LIST="$FAILED_LIST $1"; echo "FAIL $1"; }
check(){ local label="$1"; shift; local desc="$1"; shift; if "$@"; then ok "$label $desc"; else bad "$label $desc"; fi; }

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# Build hearthd and custos binaries if needed
if [[ ! -x "$BIN" ]]; then
    (cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/hearthd) || { echo "build hearthd failed"; exit 1; }
fi
CUSTOS_BIN="$WORK/custos"
(cd "$ROOT_DIR" && go build -o "$CUSTOS_BIN" ./cmd/custos) || { echo "build custos failed"; exit 1; }

# Start fake LLM
FAKE_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("",0));print(s.getsockname()[1]);s.close()')
python3 "$ROOT_DIR/tests/surface/fakellm.py" "$FAKE_PORT" &
FAKE_PID=$!
sleep 0.5

PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("",0));print(s.getsockname()[1]);s.close()')
LISTEN="127.0.0.1:${PORT}"

stop_daemon() {
    if [[ -n "$DAEMON_PID" ]]; then
        kill "$DAEMON_PID" 2>/dev/null
        wait "$DAEMON_PID" 2>/dev/null
        DAEMON_PID=""
    fi
}

start_daemon() {
    "$BIN" --config "$WORK/lararium.yaml" serve > "$WORK/daemon.log" 2>&1 &
    DAEMON_PID=$!
    local i
    for ((i = 0; i < 40; i++)); do
        curl -sf -H "Host: localhost" "http://${LISTEN}/v1/health" > /dev/null 2>&1 && return 0
        sleep 0.25
    done
    echo "daemon did not become healthy; log:"; cat "$WORK/daemon.log"
    return 1
}

setup_persona() {
    mkdir -p "$WORK/hearth/sessions/main"
    printf -- '---\nname: SOUL\ntemplate: minimal\n---\nTest soul.\n' > "$WORK/hearth/SOUL.md"
    printf -- '---\nname: IDENTITY\ntemplate: minimal\n---\nTest identity.\n' > "$WORK/hearth/IDENTITY.md"
    printf -- '---\nname: USER\ntemplate: minimal\n---\nTest user.\n' > "$WORK/hearth/USER.md"
}

write_config() {
    local keyfile="$1"
    cat > "$WORK/lararium.yaml" <<EOF
hearth:
  home: $WORK/hearth
models:
  default: [fake/test]
  compact: [fake/test]
providers:
  - name: fake
    base_url: http://127.0.0.1:${FAKE_PORT}/v1/plain
    api_key: dummy
serve:
  listen: ${LISTEN}
  allowed_hosts: ["localhost", "127.0.0.1"]
  approval_timeout: 5m
  turn_timeout: 60s
custos:
  unlock_keyfile: "${keyfile}"
EOF
}

# --- 1. Unlocked vault tests ---
KEYFILE="$WORK/keyfile"
printf "test-v17-passphrase\n" > "$KEYFILE"
chmod 0600 "$KEYFILE"

setup_persona
"$CUSTOS_BIN" --hearth "$WORK/hearth" --keyfile "$KEYFILE" init > /dev/null 2>&1 || {
    echo "custos init failed"
    exit 1
}

write_config "$KEYFILE"
TOKEN=$("$BIN" --config "$WORK/lararium.yaml" token create v17 | head -1)

start_daemon || exit 1

AUTH=(-H "Authorization: Bearer $TOKEN")

# V17.1: No bearer -> 401
check V17 "no bearer -> 401" test "$(code "http://$LISTEN/v1/policy/rules")" = "401"

# V17.2: Fresh GET []-not-null + egress_strict false
v17_fresh_get() {
    local body code_val
    code_val=$(code "${AUTH[@]}" "http://$LISTEN/v1/policy/rules")
    [[ "$code_val" == "200" ]] || return 1
    body=$(curl -s "${AUTH[@]}" "http://$LISTEN/v1/policy/rules")
    echo "$body" | grep -q '"rules":\[\]' || return 1
    echo "$body" | grep -qv '"rules":null' || return 1
    echo "$body" | grep -q '"egress_strict":false' || return 1
    return 0
}
check V17 "fresh GET []-not-null + egress_strict false" v17_fresh_get

# V17.3: PUT demo/echo ask -> 200, GET shows source web always false
v17_put_credential() {
    local code_val body
    code_val=$(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
        -d '{"lane":"credential","pattern":"demo/echo","verdict":"ask"}' \
        "http://$LISTEN/v1/policy/rules")
    [[ "$code_val" == "200" ]] || return 1
    body=$(curl -s "${AUTH[@]}" "http://$LISTEN/v1/policy/rules")
    echo "$body" | grep -q '"pattern":"demo/echo"' || return 1
    echo "$body" | grep -q '"source":"web"' || return 1
    echo "$body" | grep -q '"always":false' || return 1
    return 0
}
check V17 "PUT demo/echo ask -> 200, GET shows source web always false" v17_put_credential

# V17.4: dominating-pattern PUT returns notes array
v17_dominating_put() {
    local body
    body=$(curl -s -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
        -d '{"lane":"credential","pattern":"demo","verdict":"auto"}' \
        "http://$LISTEN/v1/policy/rules")
    echo "$body" | grep -q '"ok":true' || return 1
    echo "$body" | grep -q '"notes":\[' || return 1
    echo "$body" | grep -q 'overridden by' || return 1
    return 0
}
check V17 "dominating-pattern PUT returns notes array" v17_dominating_put

# V17.5: PUT invalid verdict -> 400
check V17 "invalid verdict 400" test "$(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
    -d '{"lane":"credential","pattern":"demo/test","verdict":"badverdict"}' \
    "http://$LISTEN/v1/policy/rules")" = "400"

# V17.6: PUT empty pattern -> 400
check V17 "empty pattern 400" test "$(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
    -d '{"lane":"credential","pattern":"","verdict":"auto"}' \
    "http://$LISTEN/v1/policy/rules")" = "400"

# V17.7: egress lane PUT -> 200, row lane egress
v17_egress_put() {
    local code_val body
    code_val=$(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
        -d '{"lane":"egress","pattern":"proxy.test/*","verdict":"auto"}' \
        "http://$LISTEN/v1/policy/rules")
    [[ "$code_val" == "200" ]] || return 1
    body=$(curl -s "${AUTH[@]}" "http://$LISTEN/v1/policy/rules")
    echo "$body" | grep -q '"lane":"egress"' || return 1
    return 0
}
check V17 "egress lane PUT" v17_egress_put

# V17.8: DELETE removed true then false
v17_delete_cycle() {
    local body1 body2
    body1=$(curl -s -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/demo%2fecho")
    echo "$body1" | grep -q '"removed":true' || return 1
    body2=$(curl -s -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/demo%2fecho")
    echo "$body2" | grep -q '"removed":false' || return 1
    return 0
}
check V17 "DELETE removed true then false" v17_delete_cycle

# V17.9: control-char pattern (curl --path-as-is with %00 / over-256 chars) -> 404
v17_pattern_gate_404() {
    [[ $(code --path-as-is -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/%00") == "404" ]] || return 1
    local over256
    over256=$(python3 -c 'print("a"*257)')
    [[ $(code -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/${over256}") == "404" ]] || return 1
    return 0
}
check V17 "control-char or over-256 pattern -> 404" v17_pattern_gate_404

# V17.10: audit tail contains actor web
v17_audit_tail() {
    local audit_files
    audit_files=$(find "$WORK/hearth/custos/audit" -name "*.jsonl" 2>/dev/null)
    [[ -n "$audit_files" ]] || return 1
    grep -q '"actor":"web"' $audit_files || return 1
    return 0
}
check V17 "audit tail contains actor web" v17_audit_tail

stop_daemon

# --- 2. Locked vault tests (all three verbs -> 423) ---
write_config ""
start_daemon || exit 1

v17_locked_verbs() {
    [[ $(code "${AUTH[@]}" "http://$LISTEN/v1/policy/rules") == "423" ]] || return 1
    [[ $(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
        -d '{"lane":"credential","pattern":"locked/test","verdict":"ask"}' \
        "http://$LISTEN/v1/policy/rules") == "423" ]] || return 1
    [[ $(code -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/demo") == "423" ]] || return 1
    return 0
}
check V17 "locked vault all three verbs -> 423" v17_locked_verbs

stop_daemon

# --- 3. Corrupt posture fixture (all three verbs -> 503) ---
# Corrupt policy.json on disk
printf '{"version":1,"credential_actions":{"bad-syntax-!!":{"verdict":"auto"}}}\n' > "$WORK/hearth/custos/policy.json"
write_config "$KEYFILE"
start_daemon || exit 1

v17_corrupt_verbs() {
    [[ $(code "${AUTH[@]}" "http://$LISTEN/v1/policy/rules") == "503" ]] || return 1
    [[ $(code -X PUT "${AUTH[@]}" -H "Content-Type: application/json" \
        -d '{"lane":"credential","pattern":"corrupt/test","verdict":"ask"}' \
        "http://$LISTEN/v1/policy/rules") == "503" ]] || return 1
    [[ $(code -X DELETE "${AUTH[@]}" "http://$LISTEN/v1/policy/rules/demo") == "503" ]] || return 1
    return 0
}
check V17 "corrupt fixture all three verbs -> 503" v17_corrupt_verbs

stop_daemon

echo "PASS: $PASS  FAIL: $FAIL"
if [[ $FAIL -gt 0 ]]; then
    echo "failed:$FAILED_LIST"
    exit 1
fi
echo "ALL PASS"
exit 0
