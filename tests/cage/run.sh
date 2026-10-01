#!/usr/bin/env bash
# Cage-proof suite per docs/CELL-SPEC.md §8: C1–C12, exit 0 only if
# every check passes. Checks C9–C12 live in c09..c12 files sourced
# here (check_cNN). Run as root on a systemd host.
#
#   LARARIUM_CELL_ROOT=/some/abs/root ./tests/cage/run.sh
set -u
if [ "$(id -u)" -ne 0 ]; then echo "error: run as root (drives nftables + systemd)" >&2; exit 1; fi
cd "$(dirname "$0")" || exit 1

BIN="${CELL_BIN:-cell}"
ROOT="${LARARIUM_CELL_ROOT:?set LARARIUM_CELL_ROOT to an absolute scratch root}"
CFG="$ROOT/cage.yaml"
ID=cage-$-
PROXY_PORT="${PROXY_PORT:-3128}"
GW=10.91.0.1
CAGE_FAIL=0
LOGH="$ROOT/cage-report-$(date +%Y%m%dT%H%M%S).txt"

# Report header (spec §8: versions recorded so any user can reproduce).
{
  printf '== Lararium cage suite ==\n'
  printf 'kernel:  %s\n' "$(uname -r)"
  printf 'systemd: %s\n' "$(systemctl --version | head -1)"
  printf 'nspawn:  %s\n' "$(systemd-nspawn --version | head -1)"
  printf 'nft:     %s\n' "$(nft --version)"
  printf 'date:    %s\n\n' "$(date -Is)"
} | tee "$LOGH"

cleanup() {
  "$BIN" --config "$CFG" destroy "$ID" --yes >/dev/null 2>&1 || true
  "$BIN" --config "$CFG" net-remove >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$ROOT"
cat > "$CFG" <<EOF
schema_version: 1
hearth: $ROOT/hearth
cell:
  memory_mb: 512
  cpu_quota: "100"
  tasks_max: 256
EOF

# shellcheck source=lib.sh
. ./lib.sh
exec > >(tee -a "$LOGH") 2>&1

"$BIN" --config "$CFG" net-install || { echo "FATAL: net-install"; exit 1; }
"$BIN" --config "$CFG" create "$ID" >/dev/null || { echo "FATAL: create"; exit 1; }
"$BIN" --config "$CFG" start "$ID" >/dev/null || { echo "FATAL: start"; exit 1; }
derive_gw
settle || bad "guest never reached the proxy door within 15s"

run_check() { # run_check <name> <fn>
  printf -- '-- %s\n' "$1"
  CAGE_MARK=$CAGE_FAIL
  "$2"
  [ "$CAGE_MARK" -eq "$CAGE_FAIL" ] && ok "$1 passed"
}

run_c1() {
  cellrun -- env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
    curl -m 8 -s -o /dev/null "http://93.184.216.34/" \
    && bad "C1: direct HTTP egress succeeded" || ok "C1: direct HTTP blocked"
}
run_c2() {
  cellrun -- env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
    curl -m 8 -s -o /dev/null "https://93.184.216.34/" \
    && bad "C2: direct HTTPS egress succeeded" || ok "C2: direct HTTPS blocked"
}
run_c3() {
  cellrun -- ping -c 2 -W 3 1.1.1.1 >/dev/null 2>&1 \
    && bad "C3: ICMP egress succeeded" || ok "C3: ICMP blocked"
}
run_c4() {
  cellrun -- timeout 6 bash -c "</dev/tcp/93.184.216.34/81" \
    && bad "C4: raw TCP :81 succeeded" || ok "C4: raw TCP odd port blocked"
}
run_c5() {
  cellrun -- env -u http_proxy -u https_proxy \
    dig +time=3 +tries=1 @8.8.8.8 example.com >/dev/null 2>&1 \
    && bad "C5: external DNS succeeded" || ok "C5: external DNS blocked"
}
run_c6() {
  local p refused=0
  for p in 22 8080; do
    if cellrun -- timeout 3 bash -c "</dev/tcp/$GW/$p" 2>/dev/null; then
      bad "C6: gateway :$p accepted (escape)"
    else
      refused=$((refused+1))
    fi
  done
  [ "$refused" -eq 2 ] && ok "C6: gateway :22/:8080 refused"
  if cellrun -- timeout 3 bash -c "</dev/tcp/$GW/$PROXY_PORT" 2>/dev/null; then
    ok "C6: proxy door connects (it is the door)"
  else
    bad "C6: proxy door refused — the door must accept"
  fi
}
run_c7() {
  local before after code
  before=$(wc -l < "$ROOT/proxy/access.log" 2>/dev/null || echo 0)
  code=$(cellrun -- curl -s -o /dev/null -w '%{http_code}' --max-time 20 \
    http://example.com/ 2>/dev/null)
  case "$code" in 200|301|308) : ;; *) bad "C7: coop fetch got '$code'"; return ;; esac
  sleep 1
  after=$(wc -l < "$ROOT/proxy/access.log" 2>/dev/null || echo 0)
  [ "$after" -gt "$before" ] \
    && ok "C7: coop fetch $code + access.log grew (packet took the door)" \
    || bad "C7: fetch ok but no access.log line — went through a hole?"
}
run_c7b() {
  local code
  code=$(cellrun -- env -u http_proxy -u https_proxy \
    curl -s -o /dev/null -w '%{http_code}' --max-time 8 \
    -x "" "http://$GW:$PROXY_PORT/" 2>/dev/null; echo "$?")
  # Relative-form request to the proxy without proxy env: proxy must
  # reject non-proxy-form requests (400/405), never fetch (200).
  case "$code" in
    200) bad "C7b: bypass fetch returned 200" ;;
    *)   ok "C7b: bypass refused ($code)" ;;
  esac
  run_c1; run_c5
}
run_c8() {
  fresh_cell --no-proxy || { bad "C8: no-proxy cell setup"; return; }
  local code
  code=$(cellrun -- curl -s -o /dev/null -w '%{http_code}' --max-time 12 \
    http://example.com/ 2>/dev/null)
  [ "$code" = 200 ] && bad "C8: fetch worked with NO proxy" || ok "C8: no-proxy fetch failed ($code)"
  run_c1; run_c2; run_c3; run_c4; run_c5
  # C6-inversion: for a no-proxy cell even the door is closed.
  if cellrun -- timeout 3 bash -c "</dev/tcp/$GW/$PROXY_PORT" 2>/dev/null; then
    bad "C8: proxy door open in no-proxy cell"
  else
    ok "C8: no-proxy cell has no door (every port refused)"
  fi
}

run_check "C1 direct HTTP" run_c1
run_check "C2 direct HTTPS" run_c2
run_check "C3 ICMP" run_c3
run_check "C4 raw TCP odd port" run_c4
run_check "C5 external DNS" run_c5
run_check "C6 host escape" run_c6
run_check "C7 proxy path" run_c7
run_check "C7b bypass refused" run_c7b

# Fresh proxy cell for the C9–C12 group (C8 left a no-proxy cell).
fresh_cell || { echo "FATAL: recreate for C9 group"; exit 1; }
settle

run_group() { # run_group <file> <fn> <label>
  if [ ! -f "./$1" ]; then
    printf -- '-- %s\n' "$3"
    bad "$3: $1 not drafted"
    return
  fi
  # shellcheck disable=SC1090
  . "./$1"
  run_check "$3" "$2"
}

run_group c09_crash.sh check_c09 "C9 crash containment"
run_group c10_uidmap.sh check_c10 "C10 uid map + binds"
run_group c11_memcap.sh check_c11 "C11 cgroup cap"
run_group c12_rebuild.sh check_c12 "C12 destroy/rebuild"

printf '\n== report: %s ==\n' "$LOGH"
if [ "$CAGE_FAIL" -ne 0 ]; then
  echo "CAGE: FAIL"
  exit 1
fi
echo "CAGE: ALL PASS"
