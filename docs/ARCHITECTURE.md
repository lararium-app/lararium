# Lararium — Architecture & Build Plan

> Open-source, self-hosted, model-agnostic personal AI agent.
> **Lararium** is the house; **penates** are what it keeps. Your agent lives at your hearth, not ours.

Working name lock: `lararium.io` (owned 2026-09-28, Cloudflare Registrar). "Penatus" reserved for the memory/persona subsystem.

---

## 0. The founding decision: own the core

**We do not build on Hermes (or any existing agent product).** Hermes served as a working prototype for feature discovery — useful, throwaway as a dependency.

Why a clean core:

- **Update risk is real.** Any third-party agent runtime that owns the loop can reshape it in a release and take our features with it. A product whose headline promise is "this is yours" cannot have its heartbeat be someone else's changelog.
- **The core is small.** The genuinely differentiated part — session engine, context/compaction policy, memory lifecycle, approval flow, credential surrogation — is ~10–20K lines of disciplined code, not a decade of research. Everyone else's "secret sauce" is that same loop plus good models.
- **Borrow commodities, own decisions.** We build the *loop*; we consume *libraries*: Playwright/CDP (browser), MCP SDKs (tools), sqlite-vec (search), systemd-nspawn (isolation). Vendored, version-pinned, in-tree. "100% ours" = we own every decision path; library dependencies are pinned, audited, and replaceable.

Hermes/OpenCode stay as internal dogfooding tools during development — never a runtime dependency.

---

## 1. Principles

1. **Model-agnostic by construction.** Every call goes through a router speaking OpenAI-completions/Chat-Completions + Anthropic Messages + OpenRouter. Local llama.cpp/Ollama/vLLM are first-class providers, not a fallback-of-last-resort mode. No feature may depend on a vendor-specific capability without a documented degradation path.
2. **The file layer is the product's memory; the model window is scratch.** Durable state is human-readable markdown + JSONL, per-user, exportable in one `tar`. (This is exactly how Muse actually works; the trained-compaction is the model's problem, the file layer is ours.)
3. **Single node, boring infra.** SQLite + filesystem + containers. Postgres only if someone ships a multi-tenant host. The whole stack must `docker compose up` on a $20/mo box or a home NUC.
4. **Trust no transcript.** Assume prompt injection succeeds at the prompt level; enforcement happens below the model: kernel-level egress control, scoped credentials, human approval gates. (Muse's own security post is the design brief.)
5. **Never break users.** SemVer + pinned releases + migration test suites + frozen file formats. The discipline we demanded from upstream is the discipline we ship.

---

## 2. Component map

| Component | One-liner | Language |
|---|---|---|
| **hearthd** | Daemon: session engine, model router, tool dispatch, supervisor | Go |
| **cell** | Per-user sandbox container (the agent's "computer") | OCI image |
| **penatus** | Persona + memory + compaction subsystem | Go lib |
| **custos** | Credential vault, approval engine, egress broker | Rust |
| **fasti** | Scheduler: crons, goals, heartbeats | Go (in hearthd) |
| **nuntius** | Channel bridge: WhatsApp/Telegram/Signal/SMS | Go |
| **app** | Web PWA → native shells | TypeScript/React |

### 2.1 hearthd — the core loop
- **Session = append-only event log** (JSONL, fsync'd). Everything else (UI, compaction, replay, branch/side-chat) is a projection of the log. Rewind/branch become trivial.
- Model router: per-session model + per-task profiles (cheap chat model for the feed, frontier model for agentic turns, local model for private turns). Fallback chains config'd per deployment.
- Tool dispatch: MCP-native. Tools run **inside the cell**, never in the daemon's process space.

### 2.2 The cell (sandbox)
- systemd-nspawn (Muse uses the same primitive; it is proven at scale for exactly this) or gVisor/firecracker for hostile-tenant hosting providers.
- Image ships: bash, python, git, chromium via CDP, a browser-automation helper, the user home tree: `agent/` (soul/identity/memory — the **penatus** layer), `workspace/`, `sessions/`.
- Root-in-cell ≠ root-on-host. Filtered syscalls, no io_uring, its own veth — only route to anywhere is custos.

### 2.3 penatus — memory & context (answers "what happens when main chat grows forever")
Three tiers, mirroring (not copying) Muse's proven design:
1. **Hot**: working window, auto-compacted by harness policy (token-budget trigger, structured summary that preserves tasks/decidents/entity refs; compaction is an event in the session log, restorable, auditable).
2. **Warm**: `MEMORY.md` + `memory/` tree (people/, projects/, daily notes). The agent is instructed to persist anything that must survive compaction *before* compaction. Search = BM25 + sqlite-vec hybrid, no external vector DB.
3. **Cold**: full session transcripts + workspace files on disk. The agent greps/opens them via tools.
- Persona files are **user-editable, versioned, documented formats** (our answer to SOUL.md/IDENTITY.md — but spec'd openly so other clients can implement them).
- Importers: ChatGPT/Muse/Other exports → penatus format (Muse itself ships this feature; parity table-stakes).

### 2.4 custos — the security envelope (the moat, cloned from first principles)
Three separations, each kernel-enforced:
1. **Vault**: credentials encrypted at rest (age/age-compatible; user KMS optional). The agent process *never* holds real secrets.
2. **Surrogation**: agent receives surrogate tokens; custos swaps real credentials at the network boundary, per-request, after policy passes. Prompt injection cannot exfiltrate what the agent never has.
3. **Approval authority (the Vigil policy)**: every connector action + every network egress evaluates user policy: auto-allow (clean, narrow), ask, deny. Approval cards surface in app/nuntius. Append-only audit log (hash-chained).
   - "Tainted egress" lite, phase 2+: tool processes that read user data lose auto-allow eligibility (eBPF attribution later; v1 does coarse per-session taint in the proxy).
- Egress: forward proxy is the *only* network path from cells (DNS + L4 + L7 checks, SSRF guard: re-resolve and pin).

### 2.5 fasti — proactivity
- Cron (5-field + intervals, same ergonomics as Muse Code's /loop), goals (multi-turn, checkpointed, requirement-checked before "done"), heartbeat (periodic wake with HEARTBEAT.md checklist → morning briefings, price-watch, inbox triage). Quiet-hours + noise-budget policy so it doesn't become a notification spammer.

### 2.6 Connectors
- **MCP is the connector standard.** Built-ins ship as typed workers executed by custos (outside the cell, with explicit per-worker credential allowlists).
- v1 built-ins: Gmail/Calendar/Drive, IMAP/SMTP, contacts, local files, web/browser, local shell, Lararium API. Everything else by API key.
- **Custom connector builder**: user pastes an OpenAPI spec URL or MCP server URL → agent writes the connector, sandboxed test call, credential into vault. (Muse's most-loved feature; fully ours to build since MCP+OpenAPI are public standards.)
- Directory/community connectors: signed manifest repo, install = verify signature + policy default-deny.
- OAuth honesty: Google/MS365 app verification is a real tax (weeks, review) — budget it once, ship a BYO-client path immediately for developers.

### 2.7 Surfaces
- **PWA first** (main chat, side chats, approvals, action log, feeds, settings). Installable, push via web-push.
- Native iOS/Android shells (Capacitor) when device APIs (SMS, contacts, health) need them — not before.
- **nuntius**: Telegram first (instant bot API), WhatsApp second (Meta Cloud API — Ironic, but it's where users are). Same agent, same approvals, remote control of the house from any channel.
- API tokens: the full agent surface is a documented HTTP/SSE API; "CLI" is just a client, and so is any third-party UI.

### 2.8 Wallet — explicitly last
Virtual cards = regulated money movement (issuer + BIN partner + KYC). Phase 5; design hook now (custos already brokers per-request authorization — a "purchase" is one more policy-gated action type).

---

## 3. Distribution & ops

- `curl -fsSL get.lararium.io | sh` → systemd-user service + onboarding wizard (model: point at llama.cpp / pick cloud + key) + Tailscale-style remote access recipe (or first-party relay with E2E encryption — build minimal, prefer Tailscale).
- `docker compose up` = the reference deployment (app + hearthd + custos + nuntius).
- Updates: pinned versions, signed, non-destructive migrations with dry-run + snapshot-before-migrate. Update channel opt-in (stable/fast). **Breaking change policy: file formats and APIs get a deprecation window ≥ 2 minor versions.**

## 4. Licensing

- **Core (hearthd, penatus, custos, fasti, nuntius): AGPL-3.0** — fork-protection for a solo project; unambiguous "open source" signal to the audience.
- **App/clients + connector SDK: MIT** — maximize adoption.
- No CLA unless we ever need dual-license leverage.

## 5. Build-vs-borrow (the 100%-ours ledger)

**Build (this is the company):** hearthd loop + session log; penatus formats + compaction policy; custos vault/surrogation/approvals/egress; fasti; PWA; connector-builder agent; packaging/updater.
**Borrow & pin (libraries, vendored):** systemd-nspawn, Playwright/CDP, MCP SDKs, sqlite + FTS5 + sqlite-vec, React/Capacitor, web-push, age encryption, tailscale libs.
**Consume (user's choice, not ours):** models (llama.cpp, Ollama, OpenRouter, Anthropic, Google…), OAuth providers, WhatsApp Cloud API.

## 6. Roadmap (solo dev + AI coding agents; honest sizing)

- **P0 — Shrine & sign (1–2 wk):** monorepo, CI, `lararium.io` landing page, ADR log, license headers. *(org: `lararium-app`, repo `lararium`)*
- **P1 — Hearth core (4–8 wk):** session engine + router + sandbox + penatus (files + compaction) + web chat with main/side chats. Milestone: we run our lives on it internally.
- **P2 — Custos (3–6 wk):** vault, surrogation proxy, approval cards, audit log. First public alpha. Milestone: an agent with Gmail access physically cannot leak the token.
- **P3 — Connectors (4–8 wk):** 6–10 built-ins + MCP builder + custom-connector flow.
- **P4 — Surfaces (4–6 wk):** PWA polish, Telegram, native shells, Android SMS/contacts/calls parity.
- **P5 — Proactivity + import + polish (ongoing):** goals/fasti depth, heartbeats, importers, public directory, `1.0`.
- Wallet: post-1.0, partnerships-first.

**Cut line (v1):** image generation and voice calls are out. Text + browser + connectors + memory + proactivity is the Muse core; everything else rides the MCP train post-launch.

## 7. Risks (named, not hidden)

1. **Scope gravity** — Muse has a funded org; we have discipline. The cut line above is a load-bearing document.
2. **OAuth review timelines** — start Google verification paperwork in P1, not P3.
3. **Solo-bus factor** — AGPL + radical docs + dogfooding are the mitigation; it's also why every subsystem gets its spec in `/docs` before code lands.
4. **Model drift** — some models behave worse under our harness; the router's eval suite (golden agentic tasks per model) runs in CI against every provider bump.

---

*The shrine keeps the penates. Lararium keeps your agent — its memory, its credentials, its fire. Bring your own model; keep your own keys.*
