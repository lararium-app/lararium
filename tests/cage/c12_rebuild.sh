# C12 — destroy/rebuild speed (spec §8): destroy + create + start +
# one `cell run` echo in under 60 seconds, on the same cell id.
# shellcheck shell=bash

check_c12() {
  local t0 out
  t0=$(date +%s)

  "$BIN" --config "$CFG" destroy "$ID" --yes >/dev/null 2>&1 \
    || { bad "C12: destroy failed"; return; }
  "$BIN" --config "$CFG" create "$ID" >/dev/null 2>&1 \
    || { bad "C12: create after destroy failed"; return; }
  "$BIN" --config "$CFG" start "$ID" >/dev/null 2>&1 \
    || { bad "C12: start after rebuild failed"; return; }

  out=$(cellrun -- echo rebuilt 2>/dev/null)
  local dt elapsed
  dt=$(date +%s)
  elapsed=$((dt - t0))

  if [ "$out" != "rebuilt" ]; then
    bad "C12: post-rebuild echo got '$out'"
    return
  fi
  if [ "$elapsed" -lt 60 ]; then
    ok "C12: destroy+create+start+run in ${elapsed}s (< 60s)"
  else
    bad "C12: rebuild took ${elapsed}s (>= 60s)"
  fi

  # Re-established cell must still be caged: door reachable, C1-style
  # direct fetch must fail (cheap re-run of the C1 probe).
  derive_gw
  if settle; then
    if cellrun -- env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
        curl -m 8 -s -o /dev/null "http://93.184.216.34/" 2>/dev/null; then
      bad "C12: rebuilt cell has direct HTTP egress (cage lost)"
    else
      ok "C12: rebuilt cell still caged"
    fi
  else
    bad "C12: rebuilt cell never reached the proxy door"
  fi
}
