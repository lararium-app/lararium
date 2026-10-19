1|# Path to beta
2|
3|Exit criteria for `v1.0.0-beta1`. Alpha said "the features work"; beta must
4|say "a stranger can install it, trust it, and use it daily." This file is
5|the contract: when every box is checked with a linked artifact (test,
6|release note, doc, or audit transcript), we tag beta.
7|
8|## Versioning plan
9|
10|- **v0.9.x line** — MCP usable from the product, not just the CLI
11|  (MCP verbs over doors landed in v0.9.0-alpha; web-chat tool injection and
12|  connector cards completed the track in v0.10.0-alpha).
13|- **v0.9.x** — install + mobile polish, flake elimination, hardening.
14|- **v1.0.0-beta1** — first release whose install path, security posture,
15|  and recovery story are documented and rehearsed end to end.
16|
17|## A. Install & ops
18|
19|- [x] Setup wizard works on a fresh box with no config (#84, v0.8.4-alpha)
20|- [x] Official Docker image published; install is `docker run`/compose
21|      one-liner, no git clone required (#49 — verified 2026-10-19 against
22|      `ghcr.io/lararium-app/hearthd:v0.10.0-alpha`: anonymous pull, volume
23|      seeding, web UI 200 on the loopback publish, foreign Host rejected 403,
24|      `/v1/health` reports 0.10.0)
25|- [x] Upgrade drill: v0.8 → v1.0-beta on a live hearth with data, one
      command, verified by CI test (automated via `.github/workflows/upgrade.yml`
      and `scripts/upgrade-drill.sh`; documented in `docs/UPGRADE.md`)
- [x] Uninstall leaves an explicit, documented residue list (keys, hearth)
      — `docs/UNINSTALL.md`: full path table (config, hearth home, custos
      vault, backups, Docker volumes) plus the revocation steps `rm` cannot
      do (upstream OAuth, BotFather, door tokens)
38|
39|## B. Features at beta bar
40|
41|- [x] MCP: model can see and call connector tools from web chat (#83,
42|      PR #102, v0.10.0-alpha)
43|- [x] MCP: connector state + tools visible in the web UI (#83 — shipped as
44|      the connectors card in the Doors view, PR #103, v0.10.0-alpha)
45|- [ ] Approvals: phone can resolve a pending card from the notification
46|      (android-client slice 4)
47|- [ ] Gmail cell + backup/restore keep their green status (regression
48|      suite, no spec drift)
49|
50|## C. Security
51|
52|- [x] Hostile-install walkthrough on a fresh VM: default loopback posture,
53|      Host allow-list behavior, token rotation, door revocation — executed
54|      and transcribed, findings filed and closed — see
55|      `docs/WALKTHROUGH-HOSTILE-INSTALL.md` (fresh clone of the compose
56|      install path; finding #117 filed+closed)
57|- [x] Threat-model pass over the doors protocol now that it carries MCP
58|      frames (auth gate, snapshot races, slow-client DoS) — see
59|      `docs/THREATMODEL-DOORS.md` (F1–F4, all accepted/documented, no
60|      transport bypass found)
61|- [ ] `govulncheck` clean at the beta tag, and stays clean through the
62|      release week
63|- [x] Secret hygiene: neutrality scan extended to catch tokens/keys in
64|      test fixtures; zero findings at the tag (scripts/scan-secrets.sh)
65|
66|## D. Quality
67|
68|- [x] CI is boringly green: #81 (approval-cancel race) fixed at root (2662e07),
69|  no rerun-to-green in the final two weeks before the tag
70|- [ ] `go test -race ./...` at tag tip, three consecutive runs, zero flakes
71|- [x] Hot spots under the soft cap: `worker.go` and `ctl.go` split
72|  (behavior-free moves only, #93 / 4def5be)
73|- [x] Lock discipline: no network I/O under a held mutex on any daemon
74|      path (audit `startLocked` and friends; race detector + review)
75|      — audit transcript: `docs/AUDIT-LOCKS.md` (one bounded, documented
76|      waiver at MCP connector start; full `-race` suite green)
77|
78|## E. Documentation
79|
80|- [x] README install path matches the post-#49 reality (one screen) —
81|      verified against the live `v0.10.1-alpha` image: the `docker run`
82|      block is the whole install (8 lines, anonymous pull, volume seed),
83|      with the bare-host `setup --defaults` path as the second tab
84|      (#106); source/compose moved below as the developer path
85|- [x] Remote access guide: TLS proxy + `allowed_hosts`, tailnet example
86|      — `docs/REMOTE-ACCESS.md`: tailnet-first, Caddy/nginx TLS proxy with
87|      SSE-safe settings, SSH tunnel zero-config path, Host allow-list
88|      semantics, container bind law, bearer-token hygiene checklist
89|- [ ] First MCP server recipe exists and was followed by someone who did
90|      not write it
91|- [ ] Backup → wipe → restore rehearsed on a real box, written up
      (drilled twice 2026-10-19: `docs/RECOVERY.md` — named volumes on
      v0.10.1-alpha; `docs/RUNBOOK-RECOVERY.md` — bind mounts on built
      main; stays red until #119 lands — the documented primary path
      fails on containerized installs, both workarounds verified live)
104|
105|## Non-goals for beta (named so they don't creep in)
106|
107|- Multi-user / multi-tenant (single-operator product)
108|- Native mobile app beyond the Android approvals client
109|- Cloud-hosted relay of any kind — the hearth stays on the user's metal
110|- Anything the specs call out of scope v1 (SURFACE §v1 bans, NUNTIUS §11,
111|  CELL TPROXY, PENATUS scheduled native backups)