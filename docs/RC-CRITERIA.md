# Path to 1.0.0 stable

Exit criteria for `v1.0.0`. Beta said "a stranger can install it, trust it,
and use it daily" (see `BETA-CRITERIA.md`, closed 2026-11-02). Stable says
**we stopped adding things and the machine proved it**. This file is the
contract in two acts: sections A–E are gates **for RC1**; the Stabilization
section is the gate **for 1.0.0**. A box is checked only with a linked
artifact (test, release note, drill transcript, soak log, or doc diff).

## Versioning plan

- **v1.0.0-beta9** — soak-verdict release: the proactivity soak's outcome
  and any fixes the soak surfaces. No new features.
- **v1.0.0-rc1** — feature freeze for daemon and Android client. Tagged
  when every box in A–E is checked with its artifact linked.
- **v1.0.0** — stable, per the Stabilization section. The Android client
  releases against the same freeze window on its own version line;
  whether the client bumps to `1.0.0` alongside the daemon is a PO call
  taken at RC1, not assumed here.

## A. Durability (the soak verdict)

- [ ] Proactivity soak completes on the dogfood hearth (started
      2026-11-04): ≥72 h of engine uptime, zero daemon restarts
      attributable to the scheduler, zero missed due-runs, zero duplicate
      notifications; `events.jsonl` archived and summarized in the beta9
      release notes.
- [ ] Any fix the soak surfaces is re-soaked ≥24 h before the beta9 tag —
      same evidence bar, no waiver.
- [ ] Seven consecutive days of daemon uptime on a live hearth at RC tip
      (systemd unit stats + journal as artifact; memory stays inside the
      cgroup cap).
- [ ] `go test -race ./...` at the RC tip, three consecutive runs, zero
      flakes (beta bar, unchanged).

## B. Security at the tag

- [ ] `govulncheck` clean at the RC tag — the CI gate has run on every PR
      since beta2; the box links the run at the tag tip.
- [ ] Hostile-install walkthrough re-executed on a fresh VM at RC tip
      (default loopback posture, Host allow-list, token rotation, door
      revocation); transcript refreshes
      `docs/WALKTHROUGH-HOSTILE-INSTALL.md`.
- [ ] Proactivity trust-boundary pass at RC tip: scheduled turns can fire
      tools with nobody watching; the mitigations that make that lawful —
      opt-in `fasti:` block, quiet hours, noise budget, custody cards for
      model-proposed jobs — verified by `tests/proactivity/run.sh` and the
      V-P spec suite (`scripts/spec-verify.sh`) passing at the tag.
- [ ] Neutrality + secret hygiene clean at the RC tag: the CI scan step in
      `.github/workflows/ci.yml` green at the tag tip, and
      `scripts/scan-secrets.sh` (including test fixtures, per the beta
      extension) run manually at the tag.
- [ ] DMARC: `rua` switched to `dmarc@lararium.io`, one full reporting
      cycle reviewed, then ratchet `p=none` → `p=quarantine`.

## C. Lifecycle (the promises the docs make)

- [ ] Upgrade drill on the real chain: **v1.0.0-beta8 → RC** on a live
      hearth with data, one command. `scripts/upgrade-drill.sh` already
      takes `FROM_VERSION`; the RC gate includes parameterizing
      `.github/workflows/upgrade.yml` (today pinned to `v0.8.4-alpha`) so
      CI exercises the beta8 chain, plus `docs/UPGRADE.md` matching
      reality.
- [ ] Backup → wipe → restore re-rehearsed on the 1.0 line (the beta
      rehearsal ran on v0.10.x): `docs/RECOVERY.md` and
      `docs/RUNBOOK-RECOVERY.md` re-executed and refreshed.
- [ ] Proactivity state rejoins a restored hearth **alive**: PENATUS is a
      whole-hearth capture, so the gate is the proof, not the promise — a
      restored hearth resumes its `fasti/state.json` schedules (no
      double-fire of due jobs, no lost jobs), with `fasti/events.jsonl`
      and `sessions/cron-*` covered by a BK test or a refreshed drill.
- [ ] `docs/UNINSTALL.md` residue list + revocation steps updated for the
      new state: `fasti/` and `sessions/cron-*` paths listed; nuntius
      state paths listed alongside the existing BotFather revocation step.
- [ ] Fresh-install smoke on **both** install paths at the RC tag:
      (a) published Docker image — anonymous pull, volume seeding, web UI
      200 on the loopback publish, foreign Host rejected 403,
      `/v1/health` reports the tag version; (b) bare host —
      `hearthd setup --defaults` on a clean box completes without config,
      web UI reachable, health reports the tag version.

## D. Client parity (Android)

- [ ] **T6 hardening shipped** (PO decision 2026-11-04: this gates 1.0):
      biometric gate on app open and on the scary verbs — connector
      add/edit, vault writes, approval resolves — and the pair token moved
      from plain storage into the Android keystore, surviving a backup
      restore only as ciphertext (android ROADMAP T6 exit).
- [ ] End-to-end pass against the RC daemon on a paired phone: pairing,
      approvals (push → one-tap resolve), chat with connector tools,
      connector add/edit from the Connectors screen, device rails.
- [ ] GitHub-flavor release APK built by CI and signed with the stable
      release key; the signing cert SHA-256 is pinned here and re-checked
      against the previous release at the tag.
- [ ] The client observes the same freeze window: no open P0/P1 client
      defects at the 1.0.0 tag.

## E. Launch surface

- [ ] Walkthrough **video** recorded from an RC build, frame-checked for
      neutrality, placed on the site and the README. Demo assets return
      only as the real thing; a still-image set stands in only on an
      explicit written PO waiver.
- [ ] README + site copy audit at RC: every claim verifiable by a stranger
      on a fresh install; vendor-neutral phrasing throughout.
- [ ] GitHub org + both repos current: descriptions, topics, and release
      notes a newcomer can follow.

## Stabilization → 1.0.0 (the launch itself)

- [ ] RC1 is 14 days old with zero open P0/P1 in either repo.
- [ ] Both install paths re-verified against the 1.0.0 tag binaries
      (same smoke as C, re-run at the stable tip).
- [ ] Launch email wave executed to the waitlist only, under formal PO
      release sign-off recorded on the 1.0.0 release ticket (no second
      sends to the original-wave addresses without it).
- [ ] 1.0.0 release notes link this file exactly as it stood at RC1.

## Non-goals for 1.0 (named so they don't creep in)

- Multi-user / multi-tenant — the hearth stays single-operator.
- Cloud-hosted relay of any kind — the hearth stays on the user's metal.
- Android roadmap tracks **T4** (assistant-role takeover) and **T5**
  ("watch the agent work" trace screen) stay on the roadmap: they are
  differentiators, not launch blockers. **T6** (biometric gate + keystore
  pair-token custody) is IN the freeze window — PO decision 2026-11-04,
  see gate D.
- Third-party connector marketplace or user-authored skills with tool
  access (prompt-injection surface).
- Voice I/O, cell TPROXY, PENATUS scheduled native backups — out of scope
  v1 in their specs.
