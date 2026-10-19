# Recovery runbook — backup → wipe → restore (executed drill)

Status: drilled 2026-10-19 on a real box using the Docker install path
(bind mounts), with a live daemon, seeded persona line, and a minted
token. Everything below is what actually happened, not a wish-book.

## Verdict

| Check | Result |
|---|---|
| `backup create` + `verify` on live daemon | OK (3.9 KB bundle, contains `config/lararium.yaml` + `tree/*` incl. `tokens.json`) |
| Bundle survives off-box copy | OK (sha-stable zip) |
| Full loss (both mount dirs emptied) then restore | **OK via the workaround below** — 10/10 tree entries, seeded line intact, config sha byte-identical (`e81022…`), pre-disaster token authenticates (`200`), daemon boots with zero fatal lines |
| `restore --replace /hearth` (documented primary) | **FAILS on any bind/volume mount root**: `swap rename-1 … device or resource busy`; the failed attempt also leaves `backup.lock`, `custos/`, `keys.lock` inside the wiped target |
| `restore --to /hearth` | **Unreachable** for the same layout: `--to requires a fresh root`, and a mount point always exists |

Tracked as [#119](https://github.com/lararium-app/lararium/issues/119)
(sibling of #110). Until it lands, use the verified procedure below — do
NOT run `--replace` against a mount root.

## Verified procedure (containerized installs)

Run one throwaway container against the (stopped) stack's mount dirs:

```bash
docker compose down                       # stop the daemon first

docker run --rm --entrypoint sh \
  -v "$PWD/hearth:/hearth" -v "$PWD/config:/etc/lararium" \
  -v /path/to/bundles:/b docker-hearthd:latest -c '
    set -e
    hearthd restore /b/bundle.zip --to /hearth/.restore   # fresh root INSIDE the mount
    cp -a /hearth/.restore/. /hearth/
    rm -rf /hearth/.restore
    hearthd backup extract-config /b/bundle.zip /etc/lararium/lararium.yaml'

docker compose up -d
```

Why `--to` inside the mount: `--to` demands a fresh root; a subdirectory
of the mount is fresh, the mount point never is. The `cp -a` is the only
step the daemon should be doing for us — see #119.

Bare-metal (non-container) installs: `restore --replace <dir> --yes`
works as documented, since the hearth dir is then renamable.

## Drill protocol (re-run before each release tag)

1. Live daemon with: a minted token, one distinguishing line appended to
   `MEMORY.md`, known `sha256` of `lararium.yaml`.
2. `backup create --out /tmp/x.zip` → `backup verify` → copy off-box.
3. Catastrophe: stop, empty both mount dirs (not delete the mount points).
4. Restore via the procedure above.
5. Prove, not assume: token → `200`; seeded line grep = 1; config sha
   matches pre-disaster; daemon log has no fatal lines; then mint-and-
   revoke one token to prove the write paths are alive too.

## Drill hygiene notes

- `hearth/custos` is container-uid-owned — wipe/restore needs to run
  through the container image, not the host shell.
- After any failed restore experiment, grep the target for scaffolding
  (`backup.lock`, `keys.lock`) before declaring state clean (#119).
