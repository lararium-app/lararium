# Recovery runbook — backup → wipe → restore (executed drill)

Status: drilled 2026-10-19 on a real box using the Docker install path
(bind mounts), with a live daemon, seeded persona line, and a minted
token; re-drilled after the #119 fix landed. Everything below is what
actually happened, not a wish-book.

## Verdict

| Check | Result |
|---|---|
| `backup create` + `verify` on live daemon | OK (3.9 KB bundle, contains `config/lararium.yaml` + `tree/*` incl. `tokens.json`) |
| Bundle survives off-box copy | OK (sha-stable zip) |
| Full loss (both mount dirs emptied) then restore | **OK** — 10/10 tree entries, seeded line intact, config sha byte-identical (`e81022…`), pre-disaster token authenticates (`200`), daemon boots with zero fatal lines |
| `restore --replace /hearth` (documented primary) | **OK in place** — mount roots cannot be renamed (`EBUSY`), so the daemon detects that and swaps by copy: tree complete (incl. nested files), modes preserved, no marker/staging residue, safety bundle kept. Re-drilled 2026-10-19 on the fixed binary. |
| Interrupted copy (crash between clear and copy) | **OK** — recovered by design: the copy-mode marker makes the next `restore`/daemon recovery replay the copy from staging (unit-tested both rows) |

History: before the fix, `--replace` on a bind/volume mount root failed
with `swap rename-1 … device or resource busy` — tracked and closed as
[#119](https://github.com/lararium-app/lararium/issues/119). Pre-fix
binaries (≤ v0.10.1-alpha) still need the historical workaround at the
bottom of this runbook.

## Verified procedure (containerized installs, current binary)

Run one throwaway container against the (stopped) stack's mount dirs:

```bash
docker compose down                       # stop the daemon first

docker run --rm --entrypoint sh \
  -v "$PWD/hearth:/hearth" -v "$PWD/config:/etc/lararium" \
  -v /path/to/bundles:/b docker-hearthd:latest -c '
    hearthd restore /b/bundle.zip --replace /hearth --yes &&
    hearthd backup extract-config /b/bundle.zip /etc/lararium/lararium.yaml'

docker compose up -d
```

The copy swap is non-atomic by necessity (a mount root cannot be
detached from inside the mount): old contents are preserved as
`/hearth.pre-restore-<ts>.lararium-backup` before anything is touched.
If the process dies mid-copy, re-run the same command — the marker
replays the copy from the surviving staging tree.

Bare-metal (non-container) installs keep the atomic rename swap:
`restore --replace <dir> --yes`.

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
- After any *failed* restore experiment, grep the target for scaffolding
  (`backup.lock`, `keys.lock`) before declaring state clean — the lock
  probe runs before the swap is guaranteed.
- A successful in-place copy leaves `hearth.lock`, `keys.lock` and a
  fresh `custos/` in the restored root; those are the daemon's own
  session files, not residue.

## Historical workaround (pre-fix binaries ≤ v0.10.1-alpha)

Before #119 was fixed, containerized restores had to stage a fresh root
*inside* the mount and copy:

```bash
docker run --rm --entrypoint sh \
  -v "$PWD/hearth:/hearth" -v "$PWD/config:/etc/lararium" \
  -v /path/to/bundles:/b docker-hearthd:latest -c '
    set -e
    hearthd restore /b/bundle.zip --to /hearth/.restore
    cp -a /hearth/.restore/. /hearth/
    rm -rf /hearth/.restore
    hearthd backup extract-config /b/bundle.zip /etc/lararium/lararium.yaml'
```

Named-volume images had a third variant: restore into the daemon's own
fresh default (`$HOME/.lararium` under `HOME=/hearth`) — see
`docs/RECOVERY.md`. All paths converge on the plain `--replace` flow
above with a current binary.
