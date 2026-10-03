#!/usr/bin/env bash
# V-suite for the HTTP/SSE surface (docs/SURFACE-SPEC.md §9).
# Mechanical checks V1-V5b, V7, V11, V13, V15c run here with curl + grep.
# Stateful live checks V6-V6c, V8-V10, V12, V14, V15, V15b, V15d live in
# internal/surface Go tests (httptest fakes); the mapping prints at the end.
#
# Usage: bash tests/surface/run.sh
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

# ---- build ------------------------------------------------------------------

echo "=== building hearthd ==="
(cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/hearthd) || { echo "build failed"; exit 1; }

echo "=== starting fake LLM ==="
FAKE_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("",0));print(s.getsockname()[1]);s.close()')
python3 "$ROOT_DIR/tests/surface/fakellm.py" "$FAKE_PORT" &
FAKE_PID=$!
sleep 0.5

# ---- per-daemon fixtures ----------------------------------------------------

stop_daemon() {
    if [[ -n "$DAEMON_PID" ]]; then
        kill "$DAEMON_PID" 2>/dev/null
        wait "$DAEMON_PID" 2>/dev/null
        DAEMON_PID=""
    fi
}

# fresh <listen> <allowed_hosts-json> [turn_timeout] — persona, config, token
fresh() {
    local listen="$1" hosts="$2" turnto="${3:-60s}"
    stop_daemon
    rm -rf "$WORK/hearth"
    mkdir -p "$WORK/hearth/sessions/main"
    printf -- '---\nname: SOUL\ntemplate: minimal\n---\nTest soul.\n' > "$WORK/hearth/SOUL.md"
    printf -- '---\nname: IDENTITY\ntemplate: minimal\n---\nTest identity.\n' > "$WORK/hearth/IDENTITY.md"
    printf -- '---\nname: USER\ntemplate: minimal\n---\nTest user.\n' > "$WORK/hearth/USER.md"
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
  listen: ${listen}
  allowed_hosts: ${hosts}
  approval_timeout: 5m
  turn_timeout: ${turnto}
EOF
    TOKEN=$("$BIN" --config "$WORK/lararium.yaml" token create vsuite | head -1)
}

LISTEN=""

# start — launch the daemon on $LISTEN, wait for health
start() {
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

# ---- V1: loopback bind; non-loopback bind logs warning ----------------------

v1() {
    LISTEN="127.0.0.1:17717"
    fresh "127.0.0.1:17717" "[]"
    start || return 1
    ss -ltn | grep -q "127.0.0.1:17717" || return 1
    ss -ltn | grep -q "0.0.0.0:17717" && return 1
    stop_daemon
    LISTEN="127.0.0.1:17718"
    fresh "0.0.0.0:17718" '["hearth.example.com"]'
    start || return 1
    grep -q "listening beyond loopback" "$WORK/daemon.log" || return 1
    return 0
}

# ---- V2: auth matrix ---------------------------------------------------------

v2() {
    LISTEN="127.0.0.1:17719"
    fresh "127.0.0.1:17719" "[]"
    start || return 1
    [[ $(code "http://$LISTEN/v1/sessions") == 401 ]] || return 1
    [[ $(code -H "Authorization: Bearer ***" "http://$LISTEN/v1/sessions") == 401 ]] || return 1
    [[ $(code "http://$LISTEN/v1/health") == 200 ]] || return 1
    [[ $(code -H "Host: evil.example" "http://$LISTEN/v1/health") == 403 ]] || return 1
    [[ $(code -H "Authorization: Bearer $TOKEN" "http://$LISTEN/v1/sessions") == 200 ]] || return 1
    return 0
}

# ---- V3: exact headers on / and both assets ---------------------------------

v3() {
    LISTEN="127.0.0.1:17720"
    fresh "127.0.0.1:17720" "[]"
    start || return 1
    local h
    for path in / /app.js /app.css; do
        h=$(curl -s -D - -o /dev/null "http://$LISTEN$path")
        echo "$h" | tr -d '\r' | grep -qi "^content-security-policy: default-src 'self'; style-src 'self'; frame-ancestors 'none'$" || return 1
        echo "$h" | tr -d '\r' | grep -qi "^x-content-type-options: nosniff$" || return 1
    done
    echo "$h" | grep -qi "set-cookie:" && return 1
    echo "$h" | grep -qi "access-control-" && return 1
    return 0
}

# ---- V4: id fuzz -> 404 ------------------------------------------------------

v4() {
    LISTEN="127.0.0.1:17721"
    fresh "127.0.0.1:17721" "[]"
    start || return 1
    local auth=(-H "Authorization: Bearer $TOKEN" --path-as-is --max-redirs 0)
    local bad_ids=(".." "..%2f" "../.." "s_../.." "s_short" "S_UPPER" "a")
    local id
    for id in "${bad_ids[@]}"; do
        # dot-segments may 301 to the cleaned path (net/http mux); the
        # cleaned path must still be no session. --max-redirs 0 turns any
        # redirect into a failure, so only a direct 404 passes.
        [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/$id/events") == 404 ]] || return 1
        [[ $(code "${auth[@]}" -X POST -H 'Content-Type: application/json' \
             -d '{"text":"hi"}' "http://$LISTEN/v1/sessions/$id/messages") == 404 ]] || return 1
    done
    # malformed approval id
    [[ $(code "${auth[@]}" -X POST -H 'Content-Type: application/json' \
         -d '{"decision":"allow"}' "http://$LISTEN/v1/sessions/main/approvals/bad") == 404 ]] || return 1
    return 0
}

# ---- V5: token plaintext once, file 0600, no plaintext on disk, no CORS ------

v5() {
    LISTEN="127.0.0.1:17722"
    fresh "127.0.0.1:17722" "[]"
    start || return 1
    local out tok
    out=$("$BIN" --config "$WORK/lararium.yaml" token create v5label)
    tok=$(echo "$out" | head -1)
    echo "$tok" | grep -qE '^lar1_[A-Za-z0-9]{32,}$' || return 1
    # the ready URL line carries the token only in its fragment
    echo "$out" | tail -1 | grep -q "Open: http://.*#$tok$" || return 1
    # tokens.json stores no plaintext
    grep -q "$tok" "$WORK/hearth/tokens.json" && return 1
    [[ $(stat -c %a "$WORK/hearth/tokens.json") == 600 ]] || return 1
    # daemon log never carries a token
    grep -q "lar1_" "$WORK/daemon.log" && return 1
    # a full turn's SSE frames carry no token either
    curl -s -N --max-time 10 -H "Authorization: Bearer $TOKEN" \
        -H 'Content-Type: application/json' -d '{"text":"hi"}' \
        "http://$LISTEN/v1/sessions/main/messages" > "$WORK/turn.sse"
    grep -q "lar1_" "$WORK/turn.sse" && return 1
    # no CORS headers on API responses
    local h
    h=$(curl -s -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "http://$LISTEN/v1/sessions")
    echo "$h" | grep -qi "access-control-" && return 1
    return 0
}

# ---- V5b: revoke round-trip ---------------------------------------------------

v5b() {
    LISTEN="127.0.0.1:17723"
    fresh "127.0.0.1:17723" "[]"
    start || return 1
    local tok
    tok=$("$BIN" --config "$WORK/lararium.yaml" token create revoke-me | head -1)
    [[ $(code -H "Authorization: Bearer $tok" "http://$LISTEN/v1/sessions") == 200 ]] || return 1
    "$BIN" --config "$WORK/lararium.yaml" token revoke revoke-me > /dev/null || return 1
    [[ $(code -H "Authorization: Bearer $tok" "http://$LISTEN/v1/sessions") == 401 ]] || return 1
    return 0
}

# ---- V7: 1 MiB body cap -> 413 -------------------------------------------------

v7() {
    LISTEN="127.0.0.1:17724"
    fresh "127.0.0.1:17724" "[]"
    start || return 1
    { printf '{"text":"'; head -c 1100000 /dev/zero | tr '\0' 'a'; printf '"}'; } > "$WORK/body.json"
    [[ $(code -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
         --data-binary "@$WORK/body.json" "http://$LISTEN/v1/sessions/main/messages") == 413 ]] || return 1
    return 0
}

# ---- V11: page source greps ------------------------------------------------------

v11() {
    local w="$ROOT_DIR/web"
    grep -qE 'https?://' "$w/index.html" "$w/app.js" "$w/app.css" && return 1
    # protocol-relative URLs only ever appear as string literals; comments
    # legitimately contain //, so anchor to a quote or backtick
    grep -qE '["'"'"'`]//' "$w/app.js" && return 1
    # fetch() with a literal argument must use a relative literal;
    # variable args are checked at their call sites (all relative)
    grep -E "fetch\\(['\"\`]" "$w/app.js" | grep -vqE "fetch\\(['\"\`]/" && return 1
    grep -q "localStorage" "$w/app.js" && return 1
    grep -qE '\.innerHTML[[:space:]]*=' "$w/app.js" && return 1
    grep -qE 'console\.[a-z]+\([^)]*token' "$w/app.js" && return 1
    grep -q "replaceState" "$w/app.js" || return 1
    grep -q "sessionStorage" "$w/app.js" || return 1
    return 0
}

# ---- V13: Host gate with config ----------------------------------------------------

v13() {
    LISTEN="127.0.0.1:17725"
    fresh "127.0.0.1:17725" "[]"
    start || return 1
    [[ $(code -H "Host: localhost:17725" "http://$LISTEN/v1/health") == 200 ]] || return 1
    stop_daemon

    # non-loopback bind with empty allowed_hosts -> startup refused
    fresh "0.0.0.0:17726" "[]"
    "$BIN" --config "$WORK/lararium.yaml" serve > "$WORK/daemon.log" 2>&1
    local rc=$?
    [[ $rc != 0 ]] || return 1
    grep -qi "allowed_hosts" "$WORK/daemon.log" || return 1

    LISTEN="127.0.0.1:17726"
    fresh "0.0.0.0:17726" '["hearth.example.com"]'
    start || return 1
    [[ $(code -H "Host: hearth.example.com" "http://127.0.0.1:17726/v1/health") == 200 ]] || return 1
    [[ $(code -H "Host: evil.example" "http://127.0.0.1:17726/v1/health") == 403 ]] || return 1
    return 0
}

# ---- V15c: events clamp + 400s ------------------------------------------------------

v15c() {
    LISTEN="127.0.0.1:17727"
    fresh "127.0.0.1:17727" "[]"
    # seed 1005 events into the main log
    python3 - "$WORK/hearth/sessions/main/events.jsonl" <<'PYEOF'
import json, sys, datetime
ts = datetime.datetime.now(datetime.timezone.utc).isoformat()
with open(sys.argv[1], "w") as f:
    for i in range(1, 1006):
        f.write(json.dumps({"t": "msg", "ts": ts, "seq": i,
                            "fields": {"role": "user", "text": "seed %d" % i}}) + "\n")
PYEOF
    start || return 1
    local auth=(-H "Authorization: Bearer $TOKEN")
    local body n
    body=$(curl -s "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?limit=5000")
    n=$(echo "$body" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["events"]))' 2>/dev/null)
    [[ "$n" == "1000" ]] || return 1
    [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?limit=0") == 400 ]] || return 1
    [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?limit=-1") == 400 ]] || return 1
    [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?limit=abc") == 400 ]] || return 1
    [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?after_seq=-1") == 400 ]] || return 1
    [[ $(code "${auth[@]}" "http://$LISTEN/v1/sessions/main/events?after_seq=abc") == 400 ]] || return 1
    return 0
}

# ---- run ------------------------------------------------------------------------------

echo "=== running V-suite ==="
check V1   "loopback bind + non-loopback warning"   v1
check V2   "auth matrix 401/401/200/403/200"        v2
check V3   "exact CSP + nosniff on page and assets" v3
check V4   "id fuzz -> 404"                         v4
check V5   "token once, 0600, no plaintext, no CORS" v5
check V5b  "revoke round-trip"                      v5b
check V7   "1.1 MiB body -> 413"                    v7
check V11  "page source greps"                      v11
check V13  "Host gate config matrix"                v13
check V15c "events clamp + 400s"                    v15c

echo "=== summary ==="
echo "PASS: $PASS  FAIL: $FAIL"
if [[ $FAIL -gt 0 ]]; then
    echo "failed:$FAILED_LIST"
    exit 1
fi
echo "ALL PASS"
echo
echo "stateful live checks live in Go tests (httptest fakes):"
echo "  V6/V6b/V6c approvals+turns tests   V8/V9/V14/V15/V15b turns tests"
echo "  V10 audit schema loop tests         V12 shutdown approvals tests"
echo "  V15d id scope + heartbeat           turns tests"
exit 0
