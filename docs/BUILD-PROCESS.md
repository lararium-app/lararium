# Lararium Build Process — v1 draft

Status: FOR REVIEW. Every phase gate below is a checkpoint where the Product Owner approves, questions, or redirects before work continues. This document is the contract for how Lararium gets built, by whom, with what verification, in what order.

---

## 0. How we work (the meta-process)

**Roles**

| Actor | Job |
|---|---|
| Product Owner | Approves specs, reviews demos at phase gates, owns all launch/announcement decisions. |
| Hermes | Architect, reviewer, release engineer. Writes specs and test suites, delegates drafting, verifies every diff against the spec, owns CI/CD and security review. |
| OpenCode (Qwen3.6-27B, local GPU drafter box) | Code drafter. Never merges anything. Known failure mode: fabricates APIs and module bodies — every file it writes is verified against claims before merge. |
| GitHub Actions CI | Independent truth. Tests run on clean runners; local-green-but-CI-red counts as red. |

**Every change follows the same pipeline:**

1. Spec section exists and is approved (no spec, no code).
2. Task brief written (files, interfaces, acceptance tests).
3. OpenCode drafts (or Hermes, for security-critical or gnarly code).
4. Hermes reviews the full diff line-by-line against brief — fabricated files/shims are rejected, not patched blind.
5. Tests green locally **and** in CI (property tests for security code, golden tests for prompt assembly).
6. Fresh-context reviewer pass before merge.
7. Merge to `main` → auto-deploy where applicable (site today; binaries later).

**Hard rules**

- Security-critical components (`custos`, `cell`, egress proxy): tests written FIRST, adversarial cases included; OpenCode may only fill implementations against fixed interfaces.
- No secrets in git, ever (`.gitignore` already guards `config/`); CI uses Actions secrets.
- Every phase ends in a runnable demo on the **test bench** — never a slide, not a screenshot. The bench is deliberately heterogeneous (multiple machines, configs, Linux distros); nothing may be hardcoded to one box. A phase is done when it runs on the bench *and* the CI matrix, not when it runs on any single machine.
- `main` stays releasable; anything experimental lives in a branch.

---

## Phase 0 — Groundwork ✅ DONE (Sept 27–28)

- Muse teardown: 1M-token trained compaction, file-based memory, per-user VM, connectors, credential surrogation, approvals, proactivity, channels.
- Name: **Lararium** (Roman household shrine). Domain `lararium.io` (Cloudflare). GitHub org `lararium-app`.
- `ARCHITECTURE.md` — six components, clean-room stack.
- Landing page live (Cloudflare Pages), waitlist live (Worker + KV, zero third parties).
- Classified as a "Decide/Learn" surface: manifesto, covenant, no feature-grid slop.

---

## Phase 1 — The penatus contract ⏱ ~1 week

**Why first:** penatus (memory/persona format) is the filesystem every other component reads and writes — hearthd's prompt assembly, the UI's memory editor, importers from ChatGPT/Muse, multi-device sync. Get it wrong and everything rewrites later; get it right and three tracks can run in parallel.

**Build (docs only, zero code):** `docs/penatus-spec.md`

- Persona tree: `SOUL.md`, `IDENTITY.md`, `MEMORY.md`, `USER.md`, `HEARTBEAT.md` — field rules, size budgets, injection order.
- Recall tree: `memory/` (people, projects, daily notes) + memory index format.
- Session transcript JSONL schema (append-only, per-message provenance, deletion semantics — Muse's per-message delete requires tombstones).
- Compaction event format: what gets written when the model summarizes (summary node, dropped-range pointers, restorable transcript refs).
- Import/export contract: `.zip` bundle, forward-compat rules, version field + migration policy.

**Gate G1 — PRODUCT OWNER.** Spec reviewed and approved before any Phase 2 code lands.

---

## Phase 2 — hearthd core ⏱ 3–4 weeks

**Build:**

- Go monorepo (`hearthd`, shared `proto`/types package).
- Session loop: load persona tree → assemble system prompt → stream model → tool calls → transcript append → compaction trigger.
- Model router: one interface → OpenAI-compatible APIs (OpenRouter, OpenAI), llama.cpp server, Ollama. Fallback chains, per-task model pins. Detection, not assumption: the router probes capabilities (context length, vision, tool support) instead of trusting host-specific config.
- Tool runtime: exec (sandboxed later), file read/write (workspace-scoped), HTTP fetch, browser (vendored Playwright).
- Config: single `lararium.yaml`, model-agnostic.

**Milestone M1 — first fire:** chat with Lararium running against a local llama.cpp or Ollama endpoint on the test bench, and against one cloud API. Memory files persist across restarts. `/compact` works. Runs in `docker compose up` form (no sandbox yet — trust boundary is explicitly "not yet"). Hardware-agnostic: CI matrix covers x86_64 + ARM, and the install script detects, it doesn't assume.

**Verification:** golden-file tests on prompt assembly (any drift fails CI); loop test with mock model; M1 demo running on at least two distinct bench systems (e.g. one x86_64 + one ARM, or discrete GPU + iGPU).

**Gate G2:** M1 demo + code review of the router interface.

### Approval log

| Gate | Status | Date | Notes |
|---|---|---|---|
| G1 (penatus spec) | ✅ Approved | 2026-09-28 | Product Owner, chat record |
| G2 (M1 demo + router review) | ✅ Approved | 2026-09-29 | Demo vs OpenRouter cloud model: chat, tool loop, memory persist across restart, `/compact`, failover chain. Carrier bugs found+fixed at gate: llama.cpp-only capability probe (0-token window on OpenAI-compatible hosts); silent 200-with-error-body accepted as completion. Carried to M1.5: `docker compose up` form + second bench system verification. |

**M1.5 progress (2026-09-29):** CI gate live (`build` + `vet` + `test -race`, first run green on the commit that added it); `docker compose up` form landed and smoke-tested (distroless image, env-only keys, persona bind mount; one live chat turn through the container REPL). Open M1.5 items: second bench system demo (a bench) + CI matrix x86_64/ARM.

---

## Phase 3 — cell (sandbox) ⏱ 2–3 weeks, parallel with late Phase 2

**Build:**

- systemd-nspawn profile generator (Ubuntu LTS rootfs template, built once, cached).
- Isolation: network namespace with **no route** except to the egress proxy; cgroup caps (default 2 CPU / 8 GB, configurable); read-only mounts + writable workspace only.
- Lifecycle: start/stop/snapshot CLI; crash containment tests.

**Milestone M2 — cage proof:** from inside a running cell, `curl`/`ping`/anything else fails everywhere on the network **except** through the (still dumb, pass-through) proxy. Written test suite, not vibes.

**Gate G3:** cage-proof suite reviewed and passing on at least two bench systems (nspawn behavior differs subtly across distros/kernel versions — that's exactly what the matrix catches).

---

## Phase 4 — custos (vault + surrogation + approvals) ⏱ 4 weeks — the security spine

**Build:**

- Credential vault: age-encrypted at rest, unlocked per-boot or per-session.
- **Surrogation:** connectors receive surrogate tokens from the vault broker; the real secret only exists in the egress authority, which swaps it in at the network boundary. The agent process — and therefore any prompt injection inside it — never holds a real credential.
- Egress proxy: MITM-capable (own CA, explicitly trusted by the cell only), path/host allow-lists, credential injector, rate limits.
- Permissions: per-connector and per-site rules (ask / always-ask / allow / deny); approval cards delivered through nuntius/PWA with one-tap approve/deny; append-only action log.

**Milestone M3 — Gmail-can't-leak:** red-team suite where hostile email content attempts (a) direct exfiltration via HTTP, (b) surrogate replay from outside the proxy, (c) social-engineering the agent into "just forward this," (d) DNS/alternate-channel exfil. All attempts must fail or require an approval card. This suite is permanent CI, run on every commit to these packages.

**Gate G4 — PRODUCT OWNER, careful one:** review the threat model doc + red-team results before custodianship of any real account.

---

## Phase 5 — penatus implementation ⏱ 2–3 weeks

**Build:**

- Three-tier memory per spec: hot window / warm markdown / cold transcripts.
- Compactor v1: prompted summarizer (model-configurable), writing spec-conformant compaction events; trained compaction is post-1.0.
- Memory index + recall tool (sqlite-vec over embeddings, local model optional).

**Milestone M4 — memory that survives:** benchmark suite — 30-day simulated life, >90% recall on probe questions across compaction boundaries; memory editor in the PWA reads/writes the tree.

**Gate G5:** benchmark numbers reviewed.

---

## Phase 6 — fasti (proactivity) + nuntius (channels) ⏱ 2–3 weeks

- fasti: crons, goals, `HEARTBEAT.md` loop, morning briefings, price-drop watchers.
- nuntius: Telegram first (easiest API), WhatsApp second, Signal third; approval cards as native buttons on each channel.

**Milestone M5:** Lararium messages you first — scheduled briefing on your phone at 07:00, action taken only after your tap.

---

## Phase 7 — connectors (OAuth farm) ⏱ 4 weeks, parallelizable across contributors

- OAuth loopback flow → tokens custody in custos → each connector is an MCP server (open standard, no lock-in).
- First ten (draft, awaiting your priority): Gmail, Google Calendar, Drive, GitHub, Notion, Home Assistant, Spotify, News/RSS, IMAP/SMTP, Brave search.
- Custom-connector generator: point it at an OpenAPI spec, get a working MCP connector + permission profile.

**Milestone M6:** connect Gmail; agent reads today's schedule, proposes a reply, approval card, sends. Real credentials, agent never sees them.

---

## Phase 8 — native apps + PWA ⏱ Android track starts in parallel from Phase 3

**Decision:** native apps sooner, Android first. **gos-ai is MIT-licensed (verified), so a "bits and pieces" strategy is legally clean — and it's the right strategy technically too:**

- **New app from scratch** for the product itself: gos-ai's architecture is a direct-LLM client (Gemini replacement: prompt in → tokens out). Lararium's client is a different animal — pairs to a home server, streams sessions, renders approval cards as actionable UI, handles offline/reconnect against *your* infrastructure. Retrofitting that onto gos-ai would cost more than rebuilding.
- **Port from gos-ai where it's already battle-tested:** voice input pipeline (STT wiring), image capture/analysis glue, notification/foreground-service plumbing, permission-request UX — the Android-isms we've already debugged against real ROMs.
- **PWA stays** as the universal companion (desktop, iOS before native iOS, and the "install nothing" crowd).
- iOS: post-alpha; reassess Kotlin Multiplatform vs native Swift then.

**Build:**

- Android: Kotlin + Compose; pairing with hearthd via QR/one-time token; session streaming; approvals inbox (push-delivered, one-tap); persona/memory viewer/editor; action log.
- Shared: PWA gains the same approval surfaces via Web Push + nuntius fallbacks.

**Milestone M7 — pocket demo:** from a phone on the bus — chat with the agent working in the cell at home, receive an approval card, tap it, done. Runs on at least two Android versions/OEM skins on the bench.

---

## Phase 9 — packaging + public alpha ⏱ 2–3 weeks

- `curl install.sh` / `docker compose up` on consumer mini-PC / small-server class hardware (CI install matrix: multiple distros, x86_64 + ARM); setup wizard (model choice: local or API key); backup/restore of the penatus bundle.
- Threat-model doc, security README, `SECURITY.md`.
- Repo goes public (AGPLv3), landing page gets GitHub button, waitlist emailed in waves.

**Milestone M8 — fire lit in public.** **Alpha gate (Product Owner's call):** core stable *and* Android app installable-and-pairable — public alpha is announced only when both hold.

---

## Timeline

Full-time: ~5–6 months to M8. Evening/weekend pace: ~8–9. Phases 3+5 and 6+7 can overlap once the contracts (Phase 1) are frozen.

## Decisions log (resolved)

- **1. Language core: Go + Rust at eBPF edges** ✅
- **2. Database: SQLite** ✅ (revisit only if multi-tenant hosting ever ships)
- **3. Frontend: native apps sooner — Android first, from Phase 3 in parallel; PWA as universal companion; iOS post-alpha** ✅
- **5. License: AGPLv3 core + MIT client libs** ✅
- **6. Visibility: public alpha when core is stable AND Android app ready** ✅ (see Phase 9 alpha gate)
- **7. Benchmark: multiple systems × multiple configs (test bench + CI matrix); zero hardware-specific hardcoding — detection, not assumption** ✅
- **8. Compaction v1: prompted summarizer; trained compaction post-1.0** ✅

**Still open:**

- **4. Connector priority:** first-ten draft stands (Gmail, Google Calendar, Drive, GitHub, Notion, Home Assistant, Spotify, News/RSS, IMAP/SMTP, Brave) — revise with the Product Owner's daily-driver list before Phase 7.

*Next action: Phase 3 — cell (sandbox) per BUILD-PROCESS.md, parallel with M1.5 leftovers (docker compose form, second bench system, CI matrix).*
