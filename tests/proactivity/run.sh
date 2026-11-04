#!/usr/bin/env bash
# V-P exit suite for Fasti proactivity (docs/PROACTIVITY-SPEC.md §Exit criterion).
# Step 1: R1 / V-Pa clock gate — zero time.Now calls in internal/fasti non-test files.
# Step 2: Full V-P Go corpus (-race -count=1): internal/fasti, cmd/hearthd fasti,
#         and internal/loop wake tests unmodified (Vi coexistence).
# Step 3: End-to-end & wiring gap scenarios (V-Pc, V-Pf, V-Ph, Vi).
#
# Usage: bash tests/proactivity/run.sh
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PASS=0
FAIL=0
FAILED_LIST=""

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
bad()  { FAIL=$((FAIL + 1)); FAILED_LIST="$FAILED_LIST $1"; echo "FAIL $1"; }
check(){ local label="$1"; shift; local desc="$1"; shift; if "$@"; then ok "$label $desc"; else bad "$label $desc"; fi; }

# ---- Step 1: R1 / V-Pa clock gate (no time.Now in internal/fasti non-test) ---

step1_clock_gate() {
    local files=()
    while IFS= read -r f; do
        [[ -n "$f" ]] && files+=("$f")
    done < <(find "$ROOT_DIR/internal/fasti" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' | sort)
    if [[ ${#files[@]} -eq 0 ]]; then
        echo "no non-test .go files found in internal/fasti"
        return 1
    fi
    local offenders
    offenders=$(grep -n 'time\.Now' "${files[@]}" || true)
    if [[ -n "$offenders" ]]; then
        echo "time.Now found in internal/fasti non-test files:"
        echo "$offenders"
        return 1
    fi
    return 0
}

# ---- Step 2: V-P corpus (-race -count=1) -------------------------------------

step2_fasti_pkg() {
    (cd "$ROOT_DIR" && go test -race -count=1 ./internal/fasti/)
}

step2_hearthd_fasti() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run 'Fasti|F4c' ./cmd/hearthd/)
}

step2_loop_wake_unmodified() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run 'Schedule|Wake' ./internal/loop/)
}

# ---- Step 3: Gap scenarios (V-Pc, V-Pf, V-Ph, Vi) ----------------------------

step3_vpc_e2e() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run '^TestFastiVPc_DoorLawEndToEnd$' ./cmd/hearthd/)
}

step3_vpf_log_clean() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run '^TestFastiVPf_ProposalCardLogClean$' ./cmd/hearthd/)
}

step3_vph_literal_passthrough() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run '^TestFastiVPh_LiteralVaultRefPassthrough$' ./cmd/hearthd/)
}

step3_vi_boot_defaults() {
    (cd "$ROOT_DIR" && go test -race -count=1 -run '^TestFastiVi_BootDefaultsNoFastiBlock$' ./cmd/hearthd/)
}

# ---- run ---------------------------------------------------------------------

echo "=== running V-P proactivity exit suite ==="
check R1-clock "no time.Now in internal/fasti non-test files"          step1_clock_gate
check VP-fasti "internal/fasti full corpus (-race -count=1)"           step2_fasti_pkg
check VP-wire  "cmd/hearthd Fasti|F4c corpus (-race -count=1)"         step2_hearthd_fasti
check Vi-wake  "internal/loop wake tests unmodified (-race -count=1)"  step2_loop_wake_unmodified
check VPc-e2e  "hub turn + approval card + audit shape + 5-skip pause" step3_vpc_e2e
check VPf-log  "proposal card -> deny leaves daemon log needle-free"   step3_vpf_log_clean
check VPh-lit  "literal \${vault:MYTHING} passthrough unresolved"       step3_vph_literal_passthrough
check Vi-boot  "boot without fasti block applies D7/D11 defaults"      step3_vi_boot_defaults

echo "=== summary ==="
echo "PASS: $PASS  FAIL: $FAIL"
if [[ $FAIL -gt 0 ]]; then
    echo "failed:$FAILED_LIST"
    exit 1
fi
echo "ALL PASS"
echo
echo "V-P exit criterion mapping (docs/PROACTIVITY-SPEC.md §Exit criterion):"
echo "  V-Pa deterministic clock & cron    internal/fasti/vpa_test.go + R1-clock grep gate"
echo "  V-Pb catch-up coalescing & window  internal/fasti/vpb_test.go"
echo "  V-Pc door law & 5-skip self-pause  internal/fasti/r3_test.go + cmd/hearthd TestFastiVPc_DoorLawEndToEnd"
echo "  V-Pd quiet hours & urgent bypass   internal/fasti/quiet_test.go"
echo "  V-Pe noise budget & digest         internal/fasti/noise_test.go + noise_dedup_test.go"
echo "  V-Pf model proposal card & log     cmd/hearthd TestF4c_* + TestFastiVPf_ProposalCardLogClean"
echo "  V-Pg heartbeat & goals hygiene     internal/fasti/{heartbeat,goals,secret_needles}_test.go"
echo "  V-Ph edit re-approval & vault ref  internal/fasti/editjob_law_test.go + cmd/hearthd TestFastiVPh_LiteralVaultRefPassthrough"
echo "  Vi   wake coexistence & defaults   internal/loop/schedule_test.go + cmd/hearthd TestFastiVi_BootDefaultsNoFastiBlock"
exit 0
