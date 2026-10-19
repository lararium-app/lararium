# Threat model: the doors protocol with MCP frames

Status: beta criterion C (docs/BETA-CRITERIA.md § C) — threat-model pass.
Date: 2026-10-19. Method: line-level read of the request path, the doors.sock
client, the MCP rail, and the SSE plumbing; every claim cites the code that
holds it. Findings are numbered F1–F4 with disposition.

## 1. Scope and trust boundaries

```
model ──tool_use──▶ worker ──▶ rail ──stdio/HTTP──▶ MCP connector (child proc)
                                      ▲
browser/CLI ──HTTP 127.0.0.1──▶ surface (HostGate→bodyLimit→cleanPath→BearerAuth)
                                      │ doors.sock (unix)
                                      ▼
                                   hearthd (custos door client, rail client)
```

Boundaries, strongest to weakest:

1. **Network → surface.** Default loopback bind; leaving loopback is loud
   (`WARNING: listening beyond loopback`, server.go:155) and requires an
   explicit `allowed_hosts` entry.
2. **Connector → daemon.** MCP connector child processes are *operator-
   installed*: semi-trusted. They may lie through their frames, but only
   inside their own tools' schemas and result channel. Frame parsing itself
   is the official pinned `modelcontextprotocol/go-sdk` (mcp_rail.go
   imports), not hand-rolled framing — CVE exposure rides go.sum + the
   govulncheck gate, not bespoke parsers.
3. **Attacker-controlled content → model.** Prompt injection via tool
   results is inherent to giving a model networked tools; the product-level
   mitigation is the custody/approval layer (side-effecting verbs need a
   card), which this document treats as out of scope except where the
   transport could bypass it (it cannot: tool calls are dispatched only by
   the worker, never by a connector).

Out of scope: compromised daemon binary, filesystem access to the data
directory (token store lives there — anyone who can read it already owns
the house), and the frozen CustosSpec itself.

## 2. Axis 1 — the auth gate

Path order (server.go:33–37): `HostGate → bodyLimit(1 MiB) → cleanPathGate →
BearerAuth → handler`.

- **HostGate** (gates.go:26) rejects any `Host:` that is not loopback or an
  `allowed_hosts` entry *before* any token is examined. This is the DNS-
  rebinding defense: a browser tricked into `Host: evil.example` dies here
  even though the TCP connection lands on loopback. Fail-closed: unknown
  host = 403.
- **bodyLimit** caps request bodies at 1 MiB before the handler allocates
  anything proportional — a big-body DoS never reaches JSON parsing.
- **BearerAuth** (gates.go:65) verifies via `TokenStore.Verify`
  (tokens.go:153): SHA-256 of the candidate compared with
  `subtle.ConstantTimeCompare` against every stored hash, **file re-read on
  every call**, so revocation is effective immediately with no cache to
  invalidate. Tokens are minted by `NewToken` (tokens.go:50) from
  `crypto/rand`, 32 chars over a uniform alphabet via `crypto/big` — ~190
  bits, brute force is not an avenue; there is deliberately no rate-limit
  complexity to hide behind.
- Plaintext tokens exist only in memory and are printed once by
  `token create` (spec §3); disk holds only hashes.
- **doors.sock is a unix socket** — no network exposure; bearer-token
  equivariance is file permission on the socket, unchanged by MCP frames.

**Finding F2 (Low, accepted):** `Verify` re-reads and re-parses the token
file on every authenticated request. An unauthenticated attacker past the
Host gate (i.e., only from an allowed host — practically: the LAN) can
amplify disk work, bounded by a page-cached, operator-sized JSON file.
Accepted for beta: correctness (instant revoke) is worth more than the I/O,
and HostGate already collapses the attack population. Revisit only if
remote use outgrows a home LAN.

## 3. Axis 2 — snapshot races

The MCP addition means three live views must stay coherent: connector
status, tool inventory, and custody cards — each read by the web UI while
the rail mutates them (connect, die, restart, refresh).

- **Attach is atomic by contract and in code.** `CustosRegistry.Attach`
  (custos_cards.go:78) mandates snapshot + subscribe under one lock,
  mirroring the frozen HELLO-replay pattern; `client.Attach`
  (custosdoor/client.go:419) implements it: `c.mu.Lock()`,
  `snapshotLocked()`, subscriber registration, unlock — no event can land
  between the snapshot and the subscription. Cancel/dropToDead is idempotent
  (sync.Once-style closed flag under subMu).
- **Rail state is lock-guarded with a codified order.** All rail views go
  through the `*Locked` accessors (`statusesLocked`, `listToolsLocked`)
  under `c.mu`; the refresh path takes `refreshMu → c.mu` in that order
  everywhere (the one inversion found in the G2 review was fixed and locked
  down in PR #101), and the D3 audit (docs/AUDIT-LOCKS.md) shows no network
  I/O under any held mutex.
- **Connector snapshot endpoint is a pure read.** `mcpConnectorsHandler`
  (custos_cards.go:283) grabs the registry pointer under `custosMu` and
  delegates to `ConnectorSnapshot()`; connector cards are observable
  inventory and deliberately *not* resolvable approvals (§6.4a) — a torn
  read at worst shows one-epoch-stale state, never fabricates an approval.
- **Tool dispatch snapshots the canonical→raw map** per call (rail CallTool),
  so a mid-call tool-list refresh cannot route to a stale raw name.

No finding: this axis was already the subject of PR #101 and stays clean
under line review.

## 4. Axis 3 — slow-client DoS

- **SSE event fan-out is bounded per client.** Each subscriber channel has
  cap 64 (custos_cards.go:226); on overflow the publisher *closes the
  stream* (custos_cards.go:239–240) so the client reconnects and resyncs
  from a fresh snapshot. A wedged browser tab costs one dead stream, never
  unbounded memory or a blocked publisher. Heartbeat pings detect dead
  transports.
- **Surface timeouts are deliberate.** `ReadHeaderTimeout: 10s` kills slow-
  header slowloris; `ReadTimeout/WriteTimeout: 0` are justified in-comment
  because streaming owns its own bounds, and every owning path does set
  them: doors.sock writes carry `SetWriteDeadline` (mcp_client.go:143 et
  al.), ack timeouts on commands, **35 s** connector-list reply budget
  (A3), **90 s** tool-call budget, worker exec timeout behind the rail.
- **Runaway generation is capped** per stream (`runawayCapBytes` in the
  turn stream) — an infinitely-generating model cannot out-run the socket
  forever.
- **MCP result shrinking**: oversized tool results are shrunk with an
  `mcp_result_truncated` audit record (mcp_rail.go:413) — a chatty or
  hostile connector cannot balloon a context window unboundedly, and the
  shrink is auditable.

**Finding F1 (Info, accepted):** no per-IP connection cap on the surface
(N SSE streams cost N goroutines + 64-event buffers ≈ tens of KB each).
Accepted for beta: the gate chain means the connect population is loopback
plus `allowed_hosts`; single-operator product. Revisit before any
internet-facing deployment story.

## 5. MCP-frame-specific notes

- **Schema-validated dispatch.** `ValidateArgs(st.InputSchema, args)` runs
  before any connector sees a call (mcp_rail.go:302) — the model cannot
  smuggle arguments outside the tool's advertised schema, and the connector
  receives only what it declared it accepts.
- **Connector namespacing.** Tools carry a canonical name; the
  `CanonToRaw` mapping means two connectors exposing identically-named raw
  tools cannot shadow each other.
- **Audit chain.** Connect, restart, call, and truncation events append to
  the hash-chained audit log — a misbehaving connector leaves an immutable
  trail, and the chain detects its own tampering.

**Finding F3 (Info, by design):** connector *output* flowing into model
context is a prompt-injection surface. This is the MCP threat model, not a
Lararium defect; the compensating control is the custody layer (approvals
for side effects) plus per-connector operator trust at install time. Documented here so the beta bar is explicit: **connectors are chosen by the
operator; nothing in the transport widens what a connector can request.**

**Finding F4 (Info, accepted):** `govulncheck` covers our modules weekly in
CI; `mcp-go`/`go-sdk` framing CVEs flow through it. Accepted: standard Go
supply-chain posture, re-checked at the tag (criterion C, box 3).

## 6. Disposition

| ID | Severity | Statement | Disposition |
|----|----------|-----------|-------------|
| F1 | Info | No per-IP connection cap (SSE streams) | Accepted for beta; revisit pre-internet-facing |
| F2 | Low | Token file re-read per auth request | Accepted; correctness over I/O on trusted LAN |
| F3 | Info | Connector output = injection surface | By design; custody layer compensates |
| F4 | Info | Framing CVEs delegated to pinned SDK | govulncheck gate covers |

No transport-level bypass of the auth gate, no snapshot race that can
fabricate state, and no unbounded-memory slow-client path were found on
the MCP-carrying doors protocol. Pass.
