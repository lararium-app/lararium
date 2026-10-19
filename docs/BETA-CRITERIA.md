# Path to beta

Exit criteria for `v1.0.0-beta1`. Alpha said "the features work"; beta must
say "a stranger can install it, trust it, and use it daily." This file is
the contract: when every box is checked with a linked artifact (test,
release note, doc, or audit transcript), we tag beta.

## Versioning plan

- **v0.9.x line** — MCP usable from the product, not just the CLI
  (MCP verbs over doors landed in v0.9.0-alpha; web-chat tool injection and
  connector cards completed the track in v0.10.0-alpha).
- **v0.9.x** — install + mobile polish, flake elimination, hardening.
- **v1.0.0-beta1** — first release whose install path, security posture,
  and recovery story are documented and rehearsed end to end.

## A. Install & ops

- [x] Setup wizard works on a fresh box with no config (#84, v0.8.4-alpha)
- [x] Official Docker image published; install is `docker run`/compose
      one-liner, no git clone required (#49 — verified 2026-10-19 against
      `ghcr.io/lararium-app/hearthd:v0.10.0-alpha`: anonymous pull, volume
      seeding, web UI 200 on the loopback publish, foreign Host rejected 403,
      `/v1/health` reports 0.10.0)
- [x] Upgrade drill: v0.8 → v1.0-beta on a live hearth with data, one
      command, verified by CI test (automated via `.github/workflows/upgrade.yml`
      and `scripts/upgrade-drill.sh`; documented in `docs/UPGRADE.md`)
- [ ] Uninstall leaves an explicit, documented residue list (keys, hearth)

## B. Features at beta bar

- [x] MCP: model can see and call connector tools from web chat (#83,
      PR #102, v0.10.0-alpha)
- [x] MCP: connector state + tools visible in the web UI (#83 — shipped as
      the connectors card in the Doors view, PR #103, v0.10.0-alpha)
- [ ] Approvals: phone can resolve a pending card from the notification
      (android-client slice 4)
- [ ] Gmail cell + backup/restore keep their green status (regression
      suite, no spec drift)

## C. Security

- [ ] Hostile-install walkthrough on a fresh VM: default loopback posture,
      Host allow-list behavior, token rotation, door revocation — executed
      and transcribed, findings filed and closed
- [ ] Threat-model pass over the doors protocol now that it carries MCP
      frames (auth gate, snapshot races, slow-client DoS)
- [ ] `govulncheck` clean at the beta tag, and stays clean through the
      release week
- [x] Secret hygiene: neutrality scan extended to catch tokens/keys in
      test fixtures; zero findings at the tag (scripts/scan-secrets.sh)

## D. Quality

- [x] CI is boringly green: #81 (approval-cancel race) fixed at root (2662e07),
  no rerun-to-green in the final two weeks before the tag
- [ ] `go test -race ./...` at tag tip, three consecutive runs, zero flakes
- [x] Hot spots under the soft cap: `worker.go` and `ctl.go` split
  (behavior-free moves only, #93 / 4def5be)
- [x] Lock discipline: no network I/O under a held mutex on any daemon
      path (audit `startLocked` and friends; race detector + review)
      — audit transcript: `docs/AUDIT-LOCKS.md` (one bounded, documented
      waiver at MCP connector start; full `-race` suite green)

## E. Documentation

- [ ] README install path matches the post-#49 reality (one screen)
- [ ] Remote access guide: TLS proxy + `allowed_hosts`, tailnet example
- [ ] First MCP server recipe exists and was followed by someone who did
      not write it
- [ ] Backup → wipe → restore rehearsed on a real box, written up

## Non-goals for beta (named so they don't creep in)

- Multi-user / multi-tenant (single-operator product)
- Native mobile app beyond the Android approvals client
- Cloud-hosted relay of any kind — the hearth stays on the user's metal
- Anything the specs call out of scope v1 (SURFACE §v1 bans, NUNTIUS §11,
  CELL TPROXY, PENATUS scheduled native backups)
