# MCP-SPEC — the connector rail: external tool servers as Custos connectors

> Status: **Draft v0** — under review. Not approved; not frozen.
> Companion targets: CUSTOS-SPEC (amendments A1–A3 below), SURFACE-SPEC (none), ARCHITECTURE §"MCP is the connector standard" (already ratified; this spec implements it).
> Protocol baseline: MCP `2026-07-28` revision, negotiating down per server to `2025-03-26`+.

## 0. The problem

Custos (CUSTOS-SPEC) ships one connector: Gmail. ARCHITECTURE promises "MCP is
the connector standard": arbitrary external tool servers, each tool gated by
the same policy → ask-card → audit spine Gmail runs under. The gap is a
connector **rail**: a way to declare an MCP server in config, expose its tools
to cells under the frozen `Connector` contract (Name/Credential/Host/
HasTool/ValidateArgs/ReviewFields/Execute), and never let a third-party tool
definition, result, or argument smuggle secrets, policy holes, or unbounded
payloads through the custody envelope.

## 1. Scope (v1)

**In:** the MCP **client lane only** — Custos is a client; cells never speak
MCP directly.
- Transports: **stdio** (spawned child process) and **Streamable HTTP**
  (remote server). Legacy HTTP+SSE: rejected at config load (deprecated
  upstream with a fixed offramp; we do not adopt deprecated surfaces).
- Primitives: **tools** only (`tools/list`, `tools/call`). Prompts, resources,
  and the deprecated roots/sampling/logging surfaces are out.
- Protocol: conform to `2026-07-28` (stateless core, `server/discover`,
  cacheable list results); backward-negotiate via the vendored SDK to servers
  that only speak `2025-11-25` / `2025-06-18` / `2025-03-26`.
- Server-pushed `elicitation/create` / MRTR `input_required`: **rejected** in
  v1 (§8.4); card-backed elicitation is a reserved future amendment.
- The `io.modelcontextprotocol/tasks` extension: out (long calls hit the
  frozen `worker_exec_timeout` like any other).

**Out:** acting as an MCP **server** (Lararium exposing its own tools over
MCP); OAuth against remote MCP servers (v1 remote auth = bearer token from
the vault, §6); connector installation UI.

**Amendments this spec requires of frozen companions** (all to CUSTOS-SPEC;
each owns its override explicitly):
- **A1** — §5.1 (worker lane): adds `op: "list"` — the cell-facing connector
  tool catalog (today only `op: "call"` exists; nothing in the frozen text
  lets a cell discover connector tools). Response `{tools: [{connector,
  name, title, annotations}]}`, ordered by connector name, server order
  within connector. Read-only: no policy consult, no ask; cell token per
  existing lane rules. Cells MUST NOT render tool definitions from any
  other source.
- **A2** — §5.1a (worker error contract): the closed template set gains
  exactly one constant: code `upstream`, detail `protocol violation` (no
  status class, no server body — consistent with the no-body rule). Needed
  because MCP JSON-RPC-level failures and rejected server-initiated
  requests have no HTTP status to class (§8.3).
- **A3** — §11 (ctl CLI frozen shapes): adds `custos mcp status`,
  `custos mcp tools <name>`, `custos mcp refresh <name>` (§9), obeying §11's
  line-oriented list conventions (tab-separated, header-declared order).
- **A4** — config schema: `custos:` section gains `connectors.mcp[]` (§3).
  The loader is strict; these are the exact permitted keys.

Stated count: four. Reviewers: check attribution, not just count.

## 2. Principles (load-bearing)

1. **A tool server is untrusted input at every boundary.** Its `name`,
   `description`, `inputSchema`, annotations, review-worthy text, and results
   render into prompts and cards. Names pass the tool-name grammar (§4.2);
   everything human-readable passes the same scrubbing as upstream bodies
   (§5.1a details) before any card or log sees it.
2. **Credentials never transit the MCP channel.** The vault (§4) remains the
   only secret store; injection happens at the transport edge (§6) and every
   injected value registers with the detail-scrubber before the child starts.
3. **Policy is per qualified tool, not per server.** `<server>/<tool>` is the
   exact-tier pattern the policy comparator already ranks above `<server>`
   (CUSTOS §6). Default is `ask`, always, for a never-seen tool.
4. **Cells stay protocol-illiterate.** MCP exists only inside custosd; the
   worker lane (op `call`) is unchanged.
5. **Borrow & pin.** Vendored, version-pinned MCP SDK (see §10), no runtime
   fetching, no CGo requirement.

## 3. Config (amendment A3)

```yaml
custos:
  connectors:
    mcp:
      - name: filesystem            # [a-z][a-z0-9_-]{0,31}; unique; may not
                                    # collide with a built-in connector name
                                    # (currently: gmail)
        transport: stdio            # stdio | http
        # --- stdio only:
        command: /usr/local/bin/mcp-filesystem
        args: ["--root", "/srv/books"]
        env:                        # KEY: ${vault:<cred-id>} | ${env:NAME} | literal
          API_KEY: ${vault:fs-key}  #   vault form only; no bare literals for secrets
        # --- http only:
        url: https://mcp.example.com/mcp
        bearer: ${vault:example-mcp-token}   # optional; sent as Authorization header
        # --- shared:
        tool_allowlist: ["read_file", "search"]   # empty = all listed tools
        tool_denylist: []
        max_concurrent_calls: 4     # 1..16
        init_timeout: 30s
```

Rules:
- `name` participates in tool naming (§4.2) and policy patterns; it MUST NOT
  contain `/` and MUST NOT collide with a built-in connector name.
- Loader rejects unknown keys (existing strictness), duplicate names,
  `transport` other than `stdio|http`, missing per-transport required keys,
  `command` not an absolute existing executable path, `url` without scheme
  `https` (or `http` with host loopback/RFC1918 only — mirrors doors'
  local-host doctrine), and any `env` value that is a bare secret-looking
  literal when a `${vault:}` reference is available — policy: literals are
  permitted but audited `mcp_config_literal_env` (warning), because config is
  read by every `cat lararium.yaml`; vault references are the doctrine.
- `${vault:X}` resolves lazily at spawn/request; missing X fails the
  connector to `credential_unavailable` state (§7), never starts a child
  with an unsubstituted placeholder, never renders the reference target's
  value anywhere.
- `tool_allowlist`/`tool_denylist` match the MCP tool name; deny wins.
- Changes apply on daemon restart (config is not hot-reloaded; `mcp-refresh`
  re-reads *tool lists* from live servers, not config).

## 4. Tool rail mechanics

### 4.1 Registration
At custosd start, for each config entry: build connector, start lifecycle
(§7), and on first successful `tools/list` register it with the WorkerServer
via the existing `RegisterConnector`. A connector that has never produced a
tool list is registered but reports no tools (calls → `unknown_tool`).

### 4.2 Naming (qualified exact)
- Worker-lane connector name: the config `name`.
- Tool name exposed to cells via `op:"list"` (A1): the MCP tool name verbatim
  after grammar check: `^[A-Za-z0-9_-]{1,64}$`. A tool failing the grammar is
  **dropped at list time**, audited once per connector generation
  (`mcp_tool_dropped`), and absent from cards and prompts.
- Policy patterns address `<name>` (bare = whole connector) and
  `<name>/<tool>` (exact outranks bare — already the comparator's tier law).

### 4.3 Catalog refresh (2026-07-28 cacheability)
- First list at registration; thereafter honor `ttlMs`/`cacheScope` from the
  server response: cache until expiry. `op:"list"` (A1) is served from cache;
  when a connector's cache has expired, that connector's entry refreshes
  inline, bounded by `init_timeout`, and other connectors' cached entries do
  not wait on it. `custos mcp refresh <name>` (§9) forces immediate re-list.
  Deterministic order: preserve server order verbatim (list results are
  spec'd deterministic; re-sorting churns consumer prompt caches).
- A refresh that **removes** a tool takes effect at the next catalog read;
  in-flight calls to it complete. A refresh that changes a tool's schema is
  audited (`mcp_catalog_changed`, diff of names + schema hash).

### 4.4 ValidateArgs (shallow, deliberate)
Worker lane runs before policy/ask. v1 validates **structurally, not
semantically**: args unmarshal to a JSON object; every entry in the tool's
`inputSchema.required` is present; top-level properties present are type-
compatible with `inputSchema.properties` (string/number/integer/boolean/
object/array by kind; `enum` membership when declared top-level). Nested
validation is the server's job. Schema absent/malformed → accept (server
authoritative) but force `ask` regardless of policy (§8.2 rule R3).
Violation → frozen `bad_args`.

### 4.5 Execute
`tools/call` with the decoded args; MCP result is serialized as:
`{"content": [ …blocks… ], "structuredContent": …, "isError": …}` with each
`text` block's body scrubbed (§2.1). The serialized result is capped at
**768 KiB** by custosd itself (below the 1 MiB lane line budget, so clients
never face an over-long response): on overflow, text blocks truncate at the
cap with a final block `"[truncated by custos]"`, and audit records
`mcp_result_truncated`. Non-text blocks pass through verbatim; custos never
dereferences resource links or fetches blob URIs. `isError: true` is data,
not a custody failure: it passes through in the result (§8.3 governs
custody/transport failures only).

## 5. Review fields (the card)

`ReviewFields(tool, args)` renders, in this order, all within 512 runes:
1. `⚠ destructive` when annotation `destructiveHint == true`;
   `read-only` when `readOnlyHint == true`; `open-world` when
   `openWorldHint == true` (absent flags render nothing, but see R2).
2. Tool `title` when non-empty (escaped, scrubbed).
3. `server/tool` — always.
4. Scalar summary: up to 3 top-level scalar args (string/number/bool), first
   96 runes each, scrubbed; long/structured args shown as `arg=…(N bytes)`.
Cards thus name what an unattended server says it will do — but the PO sees
the qualified name regardless, which is the trusted part.

## 6. Credentials

- **stdio:** env injection at spawn. Only these reach the child's
  environment: declared `env` entries (resolved), plus a frozen safe base —
  `PATH` = `/usr/local/bin:/usr/bin:/bin:` + `dirname(command)` (interpreter
  shebangs like `#!/usr/bin/env node` resolve without inheriting the
  daemon's PATH), `HOME` (connector-private dir under the custos root),
  `TMPDIR` (same). Everything else, including
  `*_KEY`/`*_TOKEN`/`*_SECRET` from the daemon's own environment, is withheld.
- **http:** `bearer` → `Authorization: header verbatim`; nothing else added.
- Every resolved value registers with the §5.1a detail scrubber for the
  connector's lifetime and is removed at reconfig/credential revoke (revoke →
  in-flight calls finish; new calls → `credential_unavailable` →
  `denied: credential unavailable` per frozen templates).
- Values never appear in config, cards, audit payloads, logs, or error
  details. Child **stderr** (diagnostics) is captured to a rotating file
  under the custos root **after scrubbing** injected values; never forwarded
  into worker responses.

## 7. Lifecycle (stdio unless noted)

States: `starting → ready → backoff → disabled`; http connectors share the
same states minus spawn. `mcp-status` prints them.
- Spawn: init handshake budget `init_timeout` (default 30 s); failure →
  backoff. Backoff: 1 s doubling to 5 min cap, max 5 restarts per rolling
  hour, then `disabled` until `mcp-refresh` or restart.
- Crash while calls in flight: those calls → `transport` (
  `connection failed before response`). No retry by custos (at-least-once is
  the caller's business; the cell's turn may already be auditing).
- Shutdown: `ctx` cancel → SIGTERM, 5 s grace, SIGKILL. Child group killed
  (spawn uses its own process group; no orphan grandchildren).
- http: state derived from last request outcome — 3 consecutive transport
  failures → `backoff` (same schedule); auth 401/403 → `disabled` with
  reason `auth_failed` (no hammering an auth server).

## 8. Policy, ask, and the frozen error enum

### 8.1 Path
MCP calls travel the existing frozen dispatch order (locked →
unknown_connector → unknown_tool → policy → ask → execute). Nothing about
the order changes; the connector only supplies the same seven methods Gmail
does.

### 8.2 Ask rules (policy defaults, enforced in code)
- **R1**: default action for a tool with no matching policy rule is `ask`.
- **R2**: absent annotation flags count as "not read-only": an `allow` policy
  on an un-annotated tool is permitted (PO override) but the card still marks
  it `unannotated`.
- **R3**: malformed/absent `inputSchema` forces `ask` even under an `allow`
  rule (defense against schema-dependent prompt drift; audited).
- `always`-granted verdicts (per existing verdict flow) bind to
  `<name>/<tool>` exactly — never to the whole server — and expire on catalog
  change: a schema-hash change of that tool revokes its `always` grant
  (audited `mcp_always_revoked`).

### 8.3 Error mapping (frozen §5.1a codes; one template added by A2)
| MCP/transport failure | code | detail |
|---|---|---|
| child dead / conn lost / http connect fail | `transport` | `connection failed before response` (frozen) |
| http 4xx/5xx transport-level failure | `upstream` | `upstream returned 4xx` / `5xx` (frozen) |
| init or call deadline | `timeout` | frozen timeout template |
| JSON-RPC error response; protocol violation; server-initiated request (MRTR/elicitation) present | `upstream` | `protocol violation` (**A2**, constant — no server text, honoring the no-body rule) |
| credential missing at call | `denied` | `denied: credential unavailable` (frozen) |

`tools/call` results carrying `isError: true` are NOT worker errors: the
call was processed; the flag and (scrubbed, capped) content travel as the
result, so the cell's model sees the server's own failure message while
custody failure semantics stay template-pure. Audit records `mcp_call` with
`ok=false` plus a closed reason (`is_error`, `transport`, `timeout`,
`protocol`, `auth`) — audit reasons are a closed set declared here.

### 8.4 Elicitation / MRTR
Server returns `resultType: "input_required"` (or any legacy
elicitation/create request) → custos responds per the protocol's rejection
path and the worker call fails `upstream` / `protocol violation` (A2).
Audit reason `protocol`. No card is raised (v1).

## 9. ctl verbs (amendment A2, frozen shapes)

```
custos mcp status          → header `name\tstate\ttools\trestarts\treason`;
                             one line per connector
custos mcp tools <name>    → header `name\tflags\ttitle`; one line per tool
custos mcp refresh <name>  → exit 0 on refreshed; exit 1 + stderr reason
```
Line-oriented per §11 (tab-separated, header-declared order, no decoration;
V-tests assert headers + one round-trip parse). No secret material. `state ∈
starting|ready|backoff|disabled` (§7); `reason` closed set: `ok`,
`auth_failed`, `init_timeout`, `spawn_failed`, `protocol`,
`credential_unavailable`.

## 10. Dependencies

- `github.com/modelcontextprotocol/go-sdk` v1.8.0 (Tier 1 SDK; speaks
  2026-07-28, negotiates legacy revisions; MIT/Apache-2.0 dual-transition
  license — compatible with AGPL-3.0-only + dual-license posture; vendored,
  pinned). Fallback if migration friction: `mark3labs/mcp-go` v1.3.0 (MIT).
  Pin chosen at transcription; whichever pins gets a `go build` +
  `-race` proof in the first brief.
- No new deps for ValidateArgs (stdlib `encoding/json` structural checks).

## 11. Security invariants (each maps to a test)

- **I1** No env leak: spawned child's environment contains only §6's base
  (PATH composed exactly as specified) + declared entries (test inspects
  child's `/proc/<pid>/environ` via a probe child).
- **I2** Scrub coverage: a sentinel secret injected via `${vault:}` never
  appears in: worker responses, cards, audit files, logs, child stderr file
  (probe child echoes it into stderr and stdout).
- **I3** Grammar gate: tool named `bad/name`, `x`.`, 200-char name, empty
  name — all dropped at list time, never registered, audit emitted once.
- **I4** Default ask: fresh catalog tool with no policy rule → dispatch
  parks an ask; `deny` on bare `<name>` covers every tool of the connector
  including tools added by a later refresh.
- **I5** `always` scoping: verdict `always` on `<name>/<tool>` binds exactly
  that tool; catalog schema-hash change revokes it; sibling tools unaffected.
- **I6** No hot-path secret in config: loader accepts `${vault:X}` only for
  existing X at use time (spawn/request), never at parse time (a credential
  revoked then re-added never blocks start).
- **I7** Truncation: result over cap → response ≤ workerMaxLineBytes with
  truncation marker; audit line present.
- **I8** Process group: SIGKILL the group of a hanging child; probe
  grandchild dies with it.
- **I9** http host gate: `url` with public `http://` scheme rejected at load;
  loopback/RFC1918 permitted; `Host()` feeds existing doors policy.
- **I10** Revoke mid-flight: credential revoked between ask-approval and
  execute → `denied: credential unavailable` (existing frozen path), child
  (stdio) restarted with scrubbed env on next spawn (value gone).

## 12. Verification suite (numbered)

- **V1–V3** config loader matrix: legal examples (§3) load; every rule in §3
  rejects with the named reason; strict-unknown-key refusal.
- **V4** fake stdio server (fixture Go program, `//go:build tools`, or
  testdata script): register → tools/list → 3 tools; drop cases per I3.
- **V5** call round-trip via worker lane `op:"call"` with cell token:
  allow-policy tool executes; args arrive verbatim; response shape §4.5.
- **V6** ask flow: default park, approve once → executes, deny →
  `denied: approval`.
- **V7–V9** error matrix per §8.3: kill child mid-call; force timeouts;
  JSON-RPC error responses.
- **V10** elicitation rejection string exactness (8.4).
- **V11** catalog refresh: tool added/removable between sessions; `always`
  revocation I5 live-matrix (schema change only for the granted tool).
- **V12** http transport against in-process `httptest` server: bearer
  present/absent, 401 → disabled/auth_failed, transport-flip → backoff.
- **V13** `op:"list"` (A1): shape exact, ordering law holds, expired-cache
  connector refreshes inline bounded while cached connectors don't wait;
  grammar-gated tools (§4.2) absent.
- **V14** invariants I1–I10 each have the named test (some fold into V1–V13;
  the mapping table is the reviewer's checklist).
- Live acceptance (non-CI): filesystem MCP server (official reference) on
  the dev box + one remote demo server; PO-visible cards from web UI;
  kill -9 churn run.

## 13. Roadmap (non-normative)

Card-backed elicitation (MRTR `input_required` → ask-card round-trip — the
protocol's design and our approval model were built for each other); Tasks
extension for long calls; OAuth (CIMD) for remote servers; prompts-as-persona
surface; MCP-server-mode export of Lararium's own tools; connector install
flow (ARCHITECTURE's builder).

## 14. Open questions — for the PO

1. **Truncation vs refusal for over-cap results.** Recommend truncate+marker
   (cells' blobs machinery truncates identically); refusal adds a new error
   code against enum discipline.
2. **`auto`/`allow` default for `readOnlyHint == true` tools.** Recommend:
   **no** — keep default `ask` (R1); annotation is a server's self-report.
   A PO who trusts a server sets one `allow` rule.
3. **Literal env values in config.** Recommend: permit but audit-warn
   (draft §3), hard-ban deferred until vault generic-secret UX covers CLI
   apps well enough to make vault-only frictionless.
4. **Both transports in the first release, or stdio first?** Recommend
   stdio first (brief 1), http as brief 2 on the same connector abstraction;
   both stay in this one spec so the contract never forks.

## Changelog

- v0 (draft): initial text.
