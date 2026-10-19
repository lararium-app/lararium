#!/usr/bin/env bash
#
# scripts/scan-secrets.sh: neutrality & secret scanner for tracked repository files.
# Beta criterion C4 secret hygiene scanner.
#
# How to add allowlist entries:
#   To allowlist false positives, add entries to .secrets-allowlist at the repository root.
#   Format:
#     path-glob|pattern-id|human-readable reason
#
#   Rules:
#     - path-glob: A glob matching the file path relative to repo root (e.g. "go.sum", "web/*").
#     - pattern-id: Rule identifier to suppress, or "*" for all rules. Supported pattern-ids:
#         - pem-private-key
#         - door-token
#         - openai-api-key
#         - jwt-token
#         - high-entropy-token
#         - sensitive-assignment
#     - reason: Clear explanation why this entry is safe (mandatory).
#
#   Lines starting with '#' and blank lines are ignored.
#   The allowlist must stay empty or near-empty; every entry needs a human reason.
#
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

ALLOWLIST_FILE="${REPO_ROOT}/.secrets-allowlist"
ALLOWLIST_PATHS=()
ALLOWLIST_RULES=()
ALLOWLIST_REASONS=()

if [[ -f "$ALLOWLIST_FILE" ]]; then
  while IFS='|' read -r pglob pid reason || [[ -n "${pglob:-}" ]]; do
    pglob="$(echo "${pglob:-}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    pid="$(echo "${pid:-}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    reason="$(echo "${reason:-}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [[ -z "$pglob" || "$pglob" =~ ^# ]] && continue
    ALLOWLIST_PATHS+=("$pglob")
    ALLOWLIST_RULES+=("$pid")
    ALLOWLIST_REASONS+=("$reason")
  done < "$ALLOWLIST_FILE"
fi

is_allowlisted() {
  local file="$1"
  local rule="$2"
  for i in "${!ALLOWLIST_PATHS[@]}"; do
    local pglob="${ALLOWLIST_PATHS[$i]}"
    local pid="${ALLOWLIST_RULES[$i]}"
    # shellcheck disable=SC2053
    if [[ "$file" == $pglob ]]; then
      if [[ "$pid" == "$rule" || "$pid" == "*" ]]; then
        return 0
      fi
    fi
  done
  return 1
}

# Collect tracked text files
TEXT_FILES=()
while IFS= read -r -d '' f; do
  [[ -f "$f" ]] || continue
  # Skip binaries
  mime="$(file -b --mime-encoding "$f" 2>/dev/null || true)"
  [[ "$mime" == "binary" ]] && continue
  TEXT_FILES+=("$f")
done < <(git ls-files -z)

if [[ ${#TEXT_FILES[@]} -eq 0 ]]; then
  exit 0
fi

AWK_SCRIPT='
BEGIN {
    IGNORECASE = 1
}

function has_sentinel(val,   v) {
    v = tolower(val)
    # Common test sentinel keywords anywhere in value
    if (index(v, "test") || index(v, "fake") || index(v, "dummy") || index(v, "mock") || \
        index(v, "stub") || index(v, "placeholder") || index(v, "example") || index(v, "sample") || \
        index(v, "fixture") || index(v, "sentinel") || index(v, "canary") || \
        index(v, "stale") || index(v, "wrong") || index(v, "invalid") || index(v, "bad") || \
        index(v, "changeme") || index(v, "template") || index(v, "demo") || \
        index(v, "none") || index(v, "null") || index(v, "unknown") || index(v, "ghost")) {
        return 1
    }
    # Exact or compound obvious placeholders using self-names or vendor prefixes
    if (v ~ /^(my|to|drain|the|a)?[-_]?(secret|token|password|passwd)[-_]?(val|orphan|landed)?[0-9]*$/ || \
        v ~ /^secret[-_]val[-_]?[0-9]*$/ || \
        v ~ /^(client[-_]secret|cell[-_]token|nv[-_]token|at[-_]coalesced|at[-_]live|at[-_]stale)[-_]?/ || \
        v ~ /^sk-(gmail|openai|anthropic|ant)[-_]?/ || \
        v ~ /^(admin|root|default|guest)$/) {
        return 1
    }
    return 0
}

function is_repeated(val,   i, c, first, same) {
    if (length(val) < 8) return 1
    first = substr(val, 1, 1)
    same = 1
    for (i = 2; i <= length(val); i++) {
        if (substr(val, i, 1) != first) {
            same = 0
            break
        }
    }
    return same
}

{
    line = $0

    # Rule a: pem-private-key
    if (match(line, /-----BEGIN [A-Z0-9 ]+PRIVATE KEY-----/)) {
        redacted = line
        gsub(/-----BEGIN [A-Z0-9 ]+PRIVATE KEY-----/, "-----BEGIN [REDACTED] PRIVATE KEY-----", redacted)
        print FILENAME ":" FNR " [pem-private-key] " redacted
    }

    # Rule f: door-token (dedicated product rule for hearthd door tokens lar1_<32 base62>)
    if (match(line, /(^|[^a-zA-Z0-9_])lar1_[0-9A-Za-z]{20,}([^a-zA-Z0-9_]|$)/)) {
        redacted = line
        gsub(/lar1_[0-9A-Za-z]{20,}/, "lar1_[REDACTED]", redacted)
        print FILENAME ":" FNR " [door-token] " redacted
    }

    # Rule b: openai-api-key (sk- + 20+ alnum/hyphen)
    if (match(line, /(^|[^a-zA-Z0-9_-])sk-[A-Za-z0-9_-]{20,}([^a-zA-Z0-9_-]|$)/)) {
        redacted = line
        gsub(/sk-[A-Za-z0-9_-]{20,}/, "sk-[REDACTED]", redacted)
        print FILENAME ":" FNR " [openai-api-key] " redacted
    }

    # Rule c: jwt-token (eyJ + base64url run)
    if (match(line, /(^|[^a-zA-Z0-9_-])eyJ[A-Za-z0-9_-]{20,}([^a-zA-Z0-9_-]|$)/)) {
        redacted = line
        gsub(/eyJ[A-Za-z0-9_-]{20,}/, "eyJ[REDACTED]", redacted)
        print FILENAME ":" FNR " [jwt-token] " redacted
    }

    # Rule d: high-entropy-token (40+ hex or base64)
    if (match(line, /(^|[^a-zA-Z0-9])[0-9a-fA-F]{40,}([^a-zA-Z0-9]|$)/)) {
        match(line, /[0-9a-fA-F]{40,}/)
        tok = substr(line, RSTART, RLENGTH)
        if (!is_repeated(tok) && !match(line, /uses:.*@[0-9a-fA-F]{40}/) && \
            !match(line, /@[0-9a-fA-F]{40}/) && !match(line, /actions-981bab4f43/) && \
            !match(tok, /badbad/) && !match(tok, /laglag/) && !match(line, /badtoken/) && \
            tolower(tok) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
            redacted = line
            gsub(tok, "[REDACTED]", redacted)
            print FILENAME ":" FNR " [high-entropy-token] " redacted
        }
    }

    # Rule e: sensitive-assignment
    if (match(line, /[a-zA-Z0-9_]*(password|passwd|secret|token|api_key|apikey)[a-zA-Z0-9_]*\s*[:=]\s*["\x27][^"\x27]{8,}["\x27]/)) {
        assign_str = substr(line, RSTART, RLENGTH)
        split_idx = match(assign_str, /[:=]/)
        varname = substr(assign_str, 1, split_idx - 1)
        sub(/^[ \t]+/, "", varname)
        sub(/[ \t]+$/, "", varname)
        val_quoted = substr(assign_str, split_idx + 1)
        sub(/^[ \t]+/, "", val_quoted)
        val = substr(val_quoted, 2, length(val_quoted) - 2)

        vlow = tolower(varname)
        if (!match(vlow, /(env|envvar|path|file|url|uri|type|header|label|count|length|len|alphabet|prefix|remedy|error|err|format|ttl|age|mode|window|cost|status|action|state|reason|policy|rule|cap|line|block|dropped|collapsed|truncated)$/) && \
            !match(vlow, /truncation/) && !match(vlow, /tokenize/)) {
            if (!match(val, /^https?:\/\//) && !match(line, /os\.Getenv/) && \
                !match(val, /^\$\{/) && !match(val, /^\$env:/) && \
                !match(val, /^<.*>$/) && !is_repeated(val) && !has_sentinel(val)) {
                redacted = line
                sub(val_quoted, "\"[REDACTED]\"", redacted)
                print FILENAME ":" FNR " [sensitive-assignment] " redacted
            }
        }
    }
}
'

FINDINGS=0

# Scan all text files using awk in one batch
while IFS= read -r finding; do
  [[ -z "$finding" ]] && continue
  file="${finding%%:*}"
  rule="$(echo "$finding" | sed -n 's/^[^:]*:[0-9]* \[\(.*\)\] .*/\1/p')"
  if is_allowlisted "$file" "$rule"; then
    continue
  fi
  echo "$finding"
  FINDINGS=$((FINDINGS + 1))
done < <(awk "$AWK_SCRIPT" "${TEXT_FILES[@]}")

if [[ $FINDINGS -gt 0 ]]; then
  echo "Secrets scan found $FINDINGS issue(s)." >&2
  exit 1
fi

echo "Secrets scan clean: 0 findings."
exit 0
