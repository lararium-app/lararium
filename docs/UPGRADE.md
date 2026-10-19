# Upgrading Lararium

Lararium guarantees zero-data-surgery upgrades: moving from an older release
(such as `v0.8.x`) to the current release (`v0.10.x` / `v1.0-beta`) requires only
updating the executable or container image. Your configuration, door tokens,
chat logs, audit chains, and persona state remain intact and backwards-compatible.

## One-Command Upgrade

### Path 1: Docker (Recommended)

When running via Docker Compose, upgrade in a single command:

```bash
docker compose pull && docker compose down && docker compose up -d
```

(During the alpha/beta phase, pin versions in `docker-compose.yml` — `:latest`
starts moving at the first stable release.)

If using standalone `docker run` with named volumes (`hearth_cfg` and `hearth_data`):

```bash
docker pull ghcr.io/lararium-app/hearthd:<version> && \
docker rm -f hearthd && \
docker run -d --name hearthd \
  -v hearth_cfg:/etc/lararium \
  -v hearth_data:/hearth \
  -e LARARIUM_CONFIG=/etc/lararium/lararium.yaml \
  -p 127.0.0.1:7717:7717 \
  ghcr.io/lararium-app/hearthd:<version> serve
```

### Path 2: Bare Host

When running `hearthd` directly on the host:

```bash
# Rebuild from release or source and restart the daemon
git pull && CGO_ENABLED=0 go build -o /usr/local/bin/hearthd ./cmd/hearthd && systemctl restart hearthd
```

Or when running manually under a process manager:

```bash
pkill hearthd && /usr/local/bin/hearthd -config /etc/lararium/lararium.yaml serve &
```

---

## What Is Preserved

Upgrades never rewrite, drop, or invalidate existing live state:

1. **Configuration (`/etc/lararium/lararium.yaml`)**:
   The strict config loader accepts earlier configurations without error. Additive fields (such as MCP connector definitions) default safely when absent.
2. **Persona & Memory (`/hearth/*.md`)**:
   `SOUL.md`, `IDENTITY.md`, `USER.md`, and `MEMORY.md` are untouched.
3. **Door Tokens (`/hearth/tokens.json`)**:
   All door tokens minted on earlier releases continue to validate immediately for authenticated API and web requests.
4. **Chat Sessions & Audit Chains (`/hearth/sessions/`)**:
   All conversation logs and sequence numbers in `events.jsonl` remain continuous and valid.
5. **Key Store (`/hearth/keys.json`)**:
   Provider keys and secrets remain in place.
6. **Search Indices (`/hearth/memory/index.db`)**:
   SQLite FTS5 indices upgrade cleanly or rebuild transparently on boot.

---

## Verification

After upgrading, verify that the daemon is healthy and your state is intact:

```bash
# 1. Verify health endpoint
curl -s -H "Host: localhost" http://127.0.0.1:7717/v1/health
# Response: {"ok":true,"version":"<target-version>"}

# 2. Verify door token authentication
curl -s -i -H "Host: localhost" -H "Authorization: Bearer <YOUR_DOOR_TOKEN>" http://127.0.0.1:7717/v1/sessions

# 3. Create and verify a backup bundle
hearthd backup create --out /tmp/post-upgrade-backup.zip
hearthd backup verify /tmp/post-upgrade-backup.zip
```

---

## Rollback Procedure

Because volumes and state files are preserved non-destructively:

- **Docker**: Pin the image tag back to the previous version (e.g. `ghcr.io/lararium-app/hearthd:v0.8.4-alpha`) and restart the container (`docker compose up -d`). Your named volumes (`hearth_cfg`, `hearth_data`) remain untouched.
- **Bare Host**: Replace `/usr/local/bin/hearthd` with the previous binary and restart the service.

---

## Automated CI Testing

The upgrade drill is automated as a continuous integration test:
- **CI Workflow**: `.github/workflows/upgrade.yml`
- **Drill Script**: `scripts/upgrade-drill.sh`

The drill boots a live `v0.8.4-alpha` container on named volumes, seeds tokens and chat turns, stops the container, boots the target binary against the same volumes, and asserts health, token validity, and audit integrity end-to-end.
