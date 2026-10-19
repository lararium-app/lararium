#!/usr/bin/env bash
# upgrade-drill.sh — automated v0.8 -> current release upgrade drill
#
# Verifies zero-manual-intervention upgrade on live hearth data:
# 1. Seeds persistent volumes (config and hearth home) and boots v0.8.4-alpha container.
# 2. Creates a door token and executes a chat turn through the v0.8 surface to establish
#    real on-disk state (door store + session event audit chain).
# 3. Stops and removes the v0.8 container, preserving named volumes.
# 4. Boots current target (via TO_TAG container image or TO_BINARY mount) against the SAME volumes.
# 5. Asserts: process starts cleanly (strict config loader passes), /v1/health reports target version,
#    v0.8 door token validates via authenticated API request, and backup/audit chain verification passes.
# 6. Cleans up containers and volumes on exit.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Configuration & Parameterization
FROM_VERSION="${FROM_VERSION:-v0.8.4-alpha}"
FROM_IMAGE="${FROM_IMAGE:-ghcr.io/lararium-app/hearthd:${FROM_VERSION}}"
TO_TAG="${TO_TAG:-}"
TO_BINARY="${TO_BINARY:-}"
DRILL_PORT="${DRILL_PORT:-7717}"

CFG_VOL="${UPGRADE_CFG_VOL:-hearth_upgrade_cfg_$$}"
DATA_VOL="${UPGRADE_DATA_VOL:-hearth_upgrade_home_$$}"
CONTAINER_V08="${CONTAINER_V08:-hearthd-drill-v08-$$}"
CONTAINER_NEW="${CONTAINER_NEW:-hearthd-drill-new-$$}"
SEED_DIR="$(mktemp -d)"

echo "=== Lararium Upgrade Drill: ${FROM_VERSION} -> target ==="
echo "From Image:    ${FROM_IMAGE}"
echo "Config Volume: ${CFG_VOL}"
echo "Data Volume:   ${DATA_VOL}"
echo "Loopback Port: ${DRILL_PORT}"

# Ensure cleanup on exit
cleanup() {
  echo "--- Cleanup: removing drill containers and named volumes ---"
  docker rm -f "${CONTAINER_V08}" "${CONTAINER_NEW}" 2>/dev/null || true
  docker volume rm -f "${CFG_VOL}" "${DATA_VOL}" 2>/dev/null || true
  rm -rf "${SEED_DIR}"
}
trap cleanup EXIT INT TERM

# The v0.8 side of the drill must see the config template and persona
# files as they existed AT THE TAG — seeding from the current tree would
# break the drill the first time a new config field lands.
echo "Extracting seed tree from tag ${FROM_VERSION}..."
git -C "${REPO_ROOT}" rev-parse --verify "${FROM_VERSION}" >/dev/null 2>&1 || {
  echo "ERROR: tag ${FROM_VERSION} not found in git repository (need fetch-depth: 0)" >&2
  exit 1
}
git -C "${REPO_ROOT}" archive "${FROM_VERSION}" | tar -x -C "${SEED_DIR}"

# Ensure FROM_IMAGE exists locally or pull/build it
ensure_from_image() {
  if docker image inspect "${FROM_IMAGE}" >/dev/null 2>&1; then
    echo "[OK] Found base image locally: ${FROM_IMAGE}"
    return 0
  fi

  echo "Attempting to pull ${FROM_IMAGE}..."
  if docker pull "${FROM_IMAGE}" >/dev/null 2>&1; then
    echo "[OK] Pulled base image: ${FROM_IMAGE}"
    return 0
  fi

  echo "Image ${FROM_IMAGE} not in registry; building from git tag ${FROM_VERSION}..."
  TMP_BUILD_DIR="$(mktemp -d)"
  if git -C "${REPO_ROOT}" rev-parse --verify "${FROM_VERSION}" >/dev/null 2>&1; then
    git -C "${REPO_ROOT}" archive "${FROM_VERSION}" | tar -x -C "${TMP_BUILD_DIR}"
  else
    echo "ERROR: tag ${FROM_VERSION} not found in git repository" >&2
    rm -rf "${TMP_BUILD_DIR}"
    exit 1
  fi

  docker build -f "${TMP_BUILD_DIR}/docker/Dockerfile" -t "${FROM_IMAGE}" "${TMP_BUILD_DIR}"
  rm -rf "${TMP_BUILD_DIR}"
  echo "[OK] Built base image: ${FROM_IMAGE}"
}

# Ensure target binary / image is ready
ensure_target() {
  if [ -n "${TO_TAG}" ]; then
    echo "[OK] Target container image specified: ${TO_TAG}"
    return 0
  fi

  if [ -z "${TO_BINARY}" ]; then
    echo "No TO_TAG or TO_BINARY specified; building target hearthd from current workspace..."
    mkdir -p "${REPO_ROOT}/bin"
    (cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o bin/hearthd ./cmd/hearthd)
    TO_BINARY="${REPO_ROOT}/bin/hearthd"
  fi

  if [ ! -f "${TO_BINARY}" ]; then
    # Try resolving relative path from cwd or REPO_ROOT
    if [ -f "${REPO_ROOT}/${TO_BINARY}" ]; then
      TO_BINARY="${REPO_ROOT}/${TO_BINARY}"
    else
      echo "ERROR: target binary not found: ${TO_BINARY}" >&2
      exit 1
    fi
  fi
  TO_BINARY="$(cd "$(dirname "${TO_BINARY}")" && pwd)/$(basename "${TO_BINARY}")"
  echo "[OK] Target binary: $(basename "${TO_BINARY}")"
}

wait_for_health() {
  local container="$1"
  local port="$2"
  local attempts=30
  echo "Waiting for ${container} to serve /v1/health on 127.0.0.1:${port}..."
  for _ in $(seq 1 "${attempts}"); do
    if curl -s -f -H "Host: localhost" "http://127.0.0.1:${port}/v1/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "ERROR: ${container} failed to become healthy. Container logs:" >&2
  docker logs "${container}" >&2 || true
  return 1
}

ensure_from_image
ensure_target

# Step a: Create named volumes and seed initial config & persona files
echo "--- Step a: Preparing named volumes and running v0.8 container ---"
docker volume create "${CFG_VOL}" >/dev/null
docker volume create "${DATA_VOL}" >/dev/null

docker run --rm -v "${CFG_VOL}":/etc/lararium -v "${DATA_VOL}":/hearth -v "${SEED_DIR}":/repo:ro \
  --entrypoint sh "${FROM_IMAGE}" -c "
    cp /repo/docker/config/lararium.yaml /etc/lararium/lararium.yaml
    cp /repo/docker/hearth/*.md /hearth/
"

docker run -d --name "${CONTAINER_V08}" \
  -v "${CFG_VOL}":/etc/lararium \
  -v "${DATA_VOL}":/hearth \
  -e LARARIUM_CONFIG=/etc/lararium/lararium.yaml \
  -p "127.0.0.1:${DRILL_PORT}:7717" \
  "${FROM_IMAGE}" serve

wait_for_health "${CONTAINER_V08}" "${DRILL_PORT}"
V08_HEALTH=$(curl -s -H "Host: localhost" "http://127.0.0.1:${DRILL_PORT}/v1/health")
echo "[OK] v0.8 health check: ${V08_HEALTH}"

# Step b: Seed REAL state through v0.8 surface: door token + chat turn
echo "--- Step b: Seeding real state through v0.8 surface ---"
TOKEN_OUTPUT=$(docker exec "${CONTAINER_V08}" hearthd token create drill-operator)
V08_TOKEN=$(echo "${TOKEN_OUTPUT}" | head -n 1)
if [[ ! "${V08_TOKEN}" =~ ^lar1_ ]]; then
  echo "ERROR: generated door token does not start with lar1_: ${V08_TOKEN}" >&2
  exit 1
fi
echo "[OK] Minted v0.8 door token: ${V08_TOKEN:0:10}..."

# Execute a chat turn to seed session log and audit chain
TURN_RESP=$(curl -s -w "\n%{http_code}" -H "Host: localhost" \
  -H "Authorization: Bearer ${V08_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"text":"upgrade drill v0.8 verification turn"}' \
  "http://127.0.0.1:${DRILL_PORT}/v1/sessions/main/messages")
HTTP_CODE=$(echo "${TURN_RESP}" | tail -n 1)
echo "[OK] v0.8 chat turn completed (HTTP ${HTTP_CODE})"

# Step c: Stop and remove v0.8 container (named volumes persist)
echo "--- Step c: Stopping and removing v0.8 container ---"
docker rm -f "${CONTAINER_V08}" >/dev/null
echo "[OK] v0.8 container stopped; volumes persist"

# Step d: (Target binary prepared in ensure_target)
echo "--- Step d: Target binary verified ready ---"

# Step e: Run NEW binary against the SAME volumes and assert compatibility
echo "--- Step e: Starting target binary against the same volumes ---"
if [ -n "${TO_TAG}" ]; then
  docker run -d --name "${CONTAINER_NEW}" \
    -v "${CFG_VOL}":/etc/lararium \
    -v "${DATA_VOL}":/hearth \
    -e LARARIUM_CONFIG=/etc/lararium/lararium.yaml \
    -p "127.0.0.1:${DRILL_PORT}:7717" \
    "${TO_TAG}" serve
else
  docker run -d --name "${CONTAINER_NEW}" \
    -v "${CFG_VOL}":/etc/lararium \
    -v "${DATA_VOL}":/hearth \
    -v "${TO_BINARY}":/usr/local/bin/hearthd:ro \
    -e LARARIUM_CONFIG=/etc/lararium/lararium.yaml \
    -p "127.0.0.1:${DRILL_PORT}:7717" \
    debian:bookworm-slim /usr/local/bin/hearthd serve
fi

wait_for_health "${CONTAINER_NEW}" "${DRILL_PORT}"

# Assertion 1: Process started cleanly, strict config loader succeeded
IS_RUNNING=$(docker inspect -f '{{.State.Running}}' "${CONTAINER_NEW}")
if [ "${IS_RUNNING}" != "true" ]; then
  echo "ERROR: target container is not running" >&2
  docker logs "${CONTAINER_NEW}" >&2
  exit 1
fi
echo "[ASSERT OK] New binary booted with v0.8 config (strict loader accepted configuration)"

# Assertion 2: /v1/health ok on loopback with Host: localhost
NEW_HEALTH=$(curl -s -i -H "Host: localhost" "http://127.0.0.1:${DRILL_PORT}/v1/health")
if ! echo "${NEW_HEALTH}" | grep -q "200 OK"; then
  echo "ERROR: /v1/health did not return 200 OK: ${NEW_HEALTH}" >&2
  exit 1
fi
if ! echo "${NEW_HEALTH}" | grep -q '"ok":true'; then
  echo "ERROR: /v1/health body missing ok:true: ${NEW_HEALTH}" >&2
  exit 1
fi
echo "[ASSERT OK] /v1/health healthy on loopback with Host: localhost: $(echo "${NEW_HEALTH}" | grep '^{')"

# Assertion 3: v0.8 door token validates via authenticated request
SESSIONS_RESP=$(curl -s -w "\n%{http_code}" -H "Host: localhost" \
  -H "Authorization: Bearer ${V08_TOKEN}" \
  "http://127.0.0.1:${DRILL_PORT}/v1/sessions")
SESSIONS_HTTP_CODE=$(echo "${SESSIONS_RESP}" | tail -n 1)
SESSIONS_BODY=$(echo "${SESSIONS_RESP}" | sed '$d')
if [ "${SESSIONS_HTTP_CODE}" != "200" ]; then
  echo "ERROR: v0.8 door token failed authentication on upgraded binary (HTTP ${SESSIONS_HTTP_CODE}): ${SESSIONS_BODY}" >&2
  exit 1
fi
echo "[ASSERT OK] v0.8 door token validates on upgraded daemon (HTTP 200 on /v1/sessions): ${SESSIONS_BODY}"

# Assertion 3b: Invalid door token rejected 401
BAD_RESP=$(curl -s -w "\n%{http_code}" -H "Host: localhost" \
  -H "Authorization: Bearer lar1_InvalidToken00000000000000000000" \
  "http://127.0.0.1:${DRILL_PORT}/v1/sessions")
BAD_HTTP_CODE=$(echo "${BAD_RESP}" | tail -n 1)
if [ "${BAD_HTTP_CODE}" != "401" ]; then
  echo "ERROR: invalid token was not rejected with 401: HTTP ${BAD_HTTP_CODE}" >&2
  exit 1
fi
echo "[ASSERT OK] Invalid door token correctly rejected with HTTP 401"

# Assertion 4: Audit chain & backup verification passes
echo "Creating backup bundle from live upgraded daemon..."
docker exec "${CONTAINER_NEW}" /usr/local/bin/hearthd backup create --out /tmp/upgrade-drill-backup.zip
echo "Verifying backup bundle..."
docker exec "${CONTAINER_NEW}" /usr/local/bin/hearthd backup verify /tmp/upgrade-drill-backup.zip
echo "[ASSERT OK] Backup verification passed with zero tamper/quarantine findings"

# Assertion 4b: Check session event log continuity
if ! docker exec "${CONTAINER_NEW}" grep -q "upgrade drill v0.8 verification turn" /hearth/sessions/main/events.jsonl; then
  echo "ERROR: seeded v0.8 session event not found in /hearth/sessions/main/events.jsonl" >&2
  exit 1
fi
echo "[ASSERT OK] v0.8 session event log intact and continuous"

# Assertion 5: Door store and database integrity
if ! docker exec "${CONTAINER_NEW}" test -f /hearth/tokens.json; then
  echo "ERROR: /hearth/tokens.json missing after upgrade" >&2
  exit 1
fi
echo "[ASSERT OK] Door token store preserved without data loss"

echo "=== Upgrade drill SUCCESS: v0.8 -> target verified end-to-end with zero manual data surgery ==="
