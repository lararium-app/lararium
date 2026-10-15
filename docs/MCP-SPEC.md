# MCP-SPEC — the connector rail: external tool servers as Custos connectors

> Status: **Draft v0.3.1** — under review. Not approved; not frozen.
> Companion targets: CUSTOS-SPEC (amendments A1–A8 below), SURFACE-SPEC (none), ARCHITECTURE §"MCP is the connector standard" (already ratified; this spec implements it).
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
  (remote server). Legacy HTTP+SSE is not offered (`transport` is a closed
  enum `stdio|http` — the loader cannot probe what a url actually speaks,
  so an SSE-only server simply fails its first request and lands in
  backoff, reason `protocol`).
- Primitives: **tools** only (`tools/list`, `tools/call`). Prompts, resources,
  and the deprecated roots/sampling/logging surfaces are out.
- Protocol: conform to `2026-07-28` (stateless core — no `initialize`/
  `Mcp-Session-Id`; per-request client metadata in `_meta`; cacheable list
  results). Negotiation with legacy revisions (`2025-11-25`, `2025-06-18`,
  `2025-03-26`) — including their handshake machinery — is delegated to the
  vendored SDK and invisible above the transport layer.
- Server-initiated interaction (MRTR `input_required`, legacy
  elicitation/create): **rejected** in v1 (§8.4); card-backed elicitation is
  a reserved future amendment.
- The `io.modelcontextprotocol/tasks` extension: out (long calls hit the
  frozen execution budget like any other).

**Out:** acting as an MCP **server** (Lararium exposing its own tools over
MCP); OAuth against remote MCP servers (v1 remote auth = bearer token from
the vault, §6); connector installation UI.

**Amendments this spec requires of frozen companions** (all to CUSTOS-SPEC;
each owns its override explicitly):
- **A1** — §5.1 (worker lane): adds `op: "list"` — the cell-facing connector
  tool catalog (today only `op: "call"` exists; nothing in the frozen text
  lets a cell discover connector tools). Response
  `{tools: [{connector, name, title}]}`, ordered by connector name, server
  order within connector; `title` is server-provided display text,
  scrubbed (§2 P1). Read-only: no policy consult, no ask; cell token per
  existing lane rules. Cells MUST NOT render connector tool definitions
  from any other source.
- **A2** — §5.1a (worker error contract), three changes: (i) the closed
  detail-template set gains exactly one constant — code `upstream`, detail
  `protocol violation` (constant; no status class, no server body, per the
  no-body rule) — because MCP JSON-RPC-level failures and rejected
  server-initiated requests have no HTTP status to class (§8.3); (ii) the
  closed `denied` template set (frozen contents: exactly
  `denied: approval`) gains exactly `denied: credential unavailable`, for
  a credential removed between approval and execute (§8.3); (iii) the
  scrub scope gains **revoked-credential retention**: a credential removed
  from the vault remains in the scrubber's needle set until every in-flight
  worker call that could carry it has completed (in-memory tombstone,
  never persisted; §6). Without (iii) revocation blinds the scrubber
  mid-flight.
- **A3** — §3 (ctl.sock wire protocol) **and** §11 (ctl CLI): the socket
  verb set gains `MCP-STATUS <token>`, `MCP-TOOLS <token> <name>`,
  `MCP-REFRESH <token> <name>` (dash style per frozen verb law), and the
  `custos` CLI gains the matching `mcp status|tools|refresh` verbs (§9),
  obeying §11's line-oriented list conventions (tab-separated,
  header-declared order).
- **A4** — config schema: `custos:` gains `connectors.mcp[]` (§3). The
  loader is strict; these are the exact permitted keys.
- **A5** — §6.4 (card review fields): for **rail tools only**, when the
  tool's annotations carry `destructiveHint == true`, the review line is
  prefixed with the constant `⚠ destructive `. Prefix-only; the rest of the
  frozen review-field construction (declared schema fields, argument
  digest) is untouched.
- **A6** — §6.5 (Always verdict): for **rail tools**, an `always` verdict
  writes an exact `auto` rule on `<connector>/<tool>` as frozen — and the
  rail **MUST** revoke that auto rule (audited, never silently) when the
  tool's fingerprint changes (§8.2). Fingerprint: SHA-256 over RFC 8785
  (JCS) canonicalization of `{name, description, inputSchema,
  annotations}` — description text is covered, so a vendor editing
  prompt-visible prose invalidates standing approval.
- **A7** — §5.1 (worker lane, credential doctrine): a connector whose
  `Credential()` returns the constant `none` executes **without** the
  vault lookup — the lane skips `GetCredential`, passes the zero-value
  credential, and audits `credential: none`. Needed because rail
  connectors legitimately have no vault secret (stdio server with literal
  env; http without bearer); Gmail (returns `gmail`) is unaffected.
  Rail-internal `${vault:}` references (§3) are resolved by the rail
  itself at spawn/request, never through the lane lookup.
- **A8** — §8.1 (closed audit-kind set): extended with exactly
  `mcp_config_literal_env`, `mcp_tool_dropped`, `mcp_catalog_changed`,
  `mcp_result_truncated`, `mcp_always_revoked`. Rail calls are audited by
  the existing `worker_call_allowed`/`worker_call_denied` kinds — the rail
  adds **no** per-call kind of its own.

Stated count: eight. Reviewers: check attribution, not just count.

## 2. Principles (load-bearing)

1. **A tool server is untrusted input at every boundary.** Its `name`,
   `description`, `inputSchema`, annotations, titles, review-worthy text,
   and results render into prompts and cards. Names pass the tool-name
   grammar (§4.2); everything human-readable is HTML-escaped and scrubbed
   of injected credential values (§6) before any card, prompt, or log sees
   it.
2. **Credentials never transit the MCP channel.** The CUSTOS-SPEC vault is
   the only secret store; injection happens at the transport edge (§6) and
   every injected value stays under scrubber coverage for as long as it can
   still leak (§6 retention rule).
3. **Policy is per qualified tool, not per server.** `<name>/<tool>` is the
   exact-tier pattern the frozen §6 comparator already ranks above `<name>`
   (tier 1). Default is `ask`, always, for a never-seen tool.
4. **Cells stay protocol-illiterate.** MCP exists only inside custosd; the
   worker lane changes only by A1.
5. **Borrow & pin.** Vendored, version-pinned MCP SDK (§10), no runtime
   fetching, no CGo requirement.

## 3. Config (amendment A4)

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
          API_KEY: ${vault:fs-key}  #   vault form is doctrine; literals audited
        # --- http only:
        url: https://mcp.example.com/mcp
        bearer: ${vault:example-mcp-token}   # optional; Authorization header
        # --- shared:
        tool_allowlist: ["read_file", "search"]   # empty = all listed tools
        tool_denylist: []
        max_concurrent_calls: 4     # 1..16
        list_timeout: 30s           # spawn + first tools/list budget
```

Rules:
- `name` participates in tool naming (§4.2) and policy patterns; it MUST NOT
  contain `/` and MUST NOT collide with a built-in connector name.
- Loader rejects (named reason on stderr, exit non-zero): unknown keys
  (existing strictness); duplicate names; `transport` other than
  `stdio|http`; missing per-transport required keys; `command` not an
  absolute existing executable; `url` scheme `http` unless host is
  loopback/RFC1918 (mirrors the doors local-host doctrine; `https` always
  permitted); `max_concurrent_calls` outside 1..16.
- Literal `env` values are accepted but audited once per connector start
  (`mcp_config_literal_env`, warning kind) — config is readable by anyone
  with repo access; `${vault:}` is the doctrine.
- `${vault:X}` / `${env:NAME}` resolve lazily at spawn/request. A missing
  vault entry fails the connector to `disabled` state with reason
  `credential_unavailable` (§7) — never start a child with an unsubstituted
  `${...}` placeholder; the resolved value never renders anywhere.
- `tool_allowlist` / `tool_denylist` match the MCP tool name; deny wins.
- Config is not hot-reloaded; changes apply at daemon restart.
  `custos mcp refresh` re-reads *tool lists* from live servers, not config.

## 4. Tool rail mechanics

### 4.1 Registration
At custosd start, for each config entry the rail builds a connector handle
and registers it with the WorkerServer through the existing
`RegisterConnector`. Registration is unconditional — it does not wait for
the child to boot. A connector whose first `tools/list` has not succeeded
has an empty catalog: calls dispatch past `unknown_connector` and settle
at `unknown_tool` (`unknown tool`, frozen). Successful first list
transitions the handle `starting → ready` (§7).

### 4.2 Naming (qualified exact)
- Worker-lane connector name: the config `name`.
- Tool name exposed to cells via `op:"list"` (A1) and addressed by
  `op:"call"`: the MCP tool name verbatim after grammar check
  `^[A-Za-z0-9_-]{1,64}$`. A tool failing the grammar is **dropped at list
  time**, audited once per catalog generation (`mcp_tool_dropped`), and
  absent from cards and prompts.
- Policy patterns address `<name>` (bare = whole connector) and
  `<name>/<tool>` (exact outranks bare — frozen tier law).
- The custody card's `tool` field carries `<name>/<tool>`, matching the
  frozen §6 example shape (`gmail/send`).

### 4.3 Catalog snapshots (2026-07-28 cacheability)
- The catalog is an **immutable snapshot** (tool set + schemas + per-tool
  fingerprints per A6). Refresh builds a new snapshot and swaps it via
  `atomic.Pointer` — every reader sees a whole snapshot, never a mix (no
  torn reads; race-detector test V10). Pinning does **not** survive the
  frozen interface boundary (ValidateArgs and Execute are disjoint calls,
  the ask-hold can span minutes), so **Execute revalidates against the
  then-current snapshot**: if the tool vanished → frozen `unknown_tool`; if
  args now violate the current schema's required/type rules → frozen
  `bad_args`. Fail closed, no exec, no card change (I11; V10).
  The §11 invariant is therefore "no torn reads + fail-closed on
  between-call churn", not per-call pinning.
- First list at registration; thereafter honor `ttlMs`/`cacheScope` from
  the server response. `op:"list"` serves snapshots from cache; when a
  connector's cache has expired, that connector refreshes inline bounded by
  `list_timeout`, while cached connectors' entries are emitted without
  waiting. `custos mcp refresh <name>` forces immediate re-list.
- Order: server order verbatim (list results are spec'd deterministic;
  re-sorting churns consumer prompt caches).
- A refresh that removes a tool takes effect at the next snapshot; in-flight
  calls to it complete. A fingerprint change is audited
  (`mcp_catalog_changed`: names added/removed + fingerprint-changed list)
  and triggers A6 revocation for exactly those tools' auto rules (§8.2).

### 4.4 ValidateArgs (shallow, deliberate)
Worker lane runs before policy/ask. v1 validates **structurally, not
semantically**: args unmarshal to a JSON object; every entry in the tool's
`inputSchema.required` is present; top-level properties present are
type-compatible with `inputSchema.properties` (by JSON kind; `enum`
membership when declared). Nested validation is the server's job.
Violation → frozen `bad_args` (`invalid arguments`). A tool whose schema is
absent or malformed accepts any object here (the server is authoritative);
same revalidation applies at Execute (§4.3).

### 4.5 Execute and result budget
`tools/call` with the decoded args. The worker result is
`{"content": [ …blocks… ], "structuredContent": …, "isError": …}`.
- **Budget: 768 KiB measured on the FINAL serialized bytes** (all block
  types, base64 and JSON escape expansion included), under the frozen 1 MiB
  lane line limit (`workerMaxLineBytes = 1 << 20`). Enforcement is a
  converge loop, not a single branch: serialize → if over budget, replace
  the largest remaining non-text block with `"[content omitted by
  custos]"` while any non-text block exceeds the headroom text truncation
  cannot recover → then truncate `text` blocks (largest first, keeping
  room for the appended `"[truncated by custos]"` block) and **re-serialize
  and re-measure**, repeating until serialized size ≤ budget; escaping
  expansion (\\n, \\uXXXX) is therefore always measured, never predicted.
  One `mcp_result_truncated` audit per call listing which paths ran. An
  empty result fits, so the loop terminates. The serialized output can
  never exceed the budget.
- **Scrub coverage = every string leaf of the delivered result** — text
  block bodies AND `structuredContent` string leaves — against **that
  connector's own injected credential values** before delivery (O(1)
  per-request scope, the bearer-lane analogue; whole-vault scans stay on
  `detail` per §5.1a's scope split). Base64 blocks (image/audio/resource
  blobs) are opaque to substring scrubbing, so each is additionally
  checked against the needles' base64 encodings at all three alignments;
  a hit replaces the whole block (no mid-stream truncation of encoded
  data) — this is what stops a stdio child, which legitimately holds the
  injected secret in its env, from smuggling it back through a result
  blob. Custos never dereferences resource links or fetches blob URIs.
- `isError: true` is data, not a custody failure: the flag and (scrubbed,
  capped) content travel as the result. The frozen no-body rule governs
  error `detail` construction; a processed tool error is a result and the
  lane's existing `worker_call_allowed` audit covers it — the rail emits
  nothing extra (A8). §8.3 governs custody/transport failures.
- Concurrency: per-connector counting semaphore, `max_concurrent_calls`;
  an over-limit call waits for a slot, bounded by the frozen execution
  budget (`DefaultWorkerExecTimeout`, 60 s default) → `timeout` on expiry.

## 5. Review line (the card)

Per frozen §6.4 the review line carries **declared schema fields**; MCP
servers declare no Lararium review fields, so the rail declares them on the
tool's behalf, Gmail-style:
- Up to 3 top-level scalar args (string/number/boolean), in
  `key=value, key=value` form, each value truncated at 96 runes, scrubbed
  (§2 P1, §6); total ≤512 runes; unparseable args → `"?"` (frozen Gmail
  fallback shape). Structured args are named only: `arg=…`.
- **A5 prefix**: `⚠ destructive ` when `destructiveHint == true`.
- Annotations, title, and annotation-absence live outside the card, in
  `custos mcp tools` flags (§9) — the card shape stays frozen.

## 6. Credentials and the transport edge

- **stdio:** env injection at spawn. The child's environment contains
  exactly: declared `env` entries (resolved), plus a frozen safe base —
  `PATH` = `/usr/local/bin:/usr/bin:/bin:` + `dirname(command)`
  (interpreter shebangs like `#!/usr/bin/env node` resolve without
  inheriting the daemon's PATH), `HOME` (connector-private dir under the
  custos root), `TMPDIR` (same). Nothing else — no `*_KEY`/`*_TOKEN`/
  `*_SECRET` from the daemon's own environment.
  The connector-private dir is `<hearth>/connectors/<name>/` (0700) — a
  **sibling** of the C2 state root, so connector scratch and stderr files
  never join (or violate) the `<hearth>/custos/` inventory.
- **http:** custos injects exactly one header, `Authorization` with the
  bearer value. Protocol headers the SDK sends (`Mcp-Method`, `Mcp-Name`,
  `MCP-Protocol-Version`) are the SDK's contract, not custos additions.
- **Scrub retention (the leak window, amendment A2(iii)):** every injected
  value registers with the scrubber (scope: that connector for result
  bodies, §4.5; whole-set for `detail` per §5.1a) at injection and
  **stays registered until every in-flight call that could carry it has
  completed** — revocation removes the *source*, never early scrub
  coverage. The frozen scrubber derives needles from loaded vault
  credentials, which revocation purges immediately; A2(iii)'s in-memory
  tombstone is what makes this sentence implementable.
- **Revocation:** on credential revoke (frozen KEYS/CUSTOS path) the
  connector transitions `disabled`, reason `credential_unavailable`; for
  **stdio** the child process **group** is killed immediately (SIGTERM, 5 s
  grace, SIGKILL) — the value dies in memory and stops being readable via
  `/proc/<pid>/environ`; in-flight stdio calls settle `transport`
  (`connection failed before response`) per §7. For **http** in-flight
  requests complete and their bodies are scrubbed against the tombstoned
  value (A2(iii)). New calls fail the A2(ii) `denied: credential
  unavailable` path. Re-enable by `custos mcp refresh` after the
  credential is restored.
- **Trust posture (stated plainly):** the stdio child runs at custosd's
  uid — no v1 sandboxing. Installing a server binary is a root-adjacent PO
  act, the same trust class as installing an npm/pip package on the host.
  Env composition, PATH rules, and scrubbing narrow what leaks *through
  the custody envelope*; they do not confine a hostile binary running as
  the daemon user. uid-drop/namespaced execution is roadmap (§13). Every
  MCP host shipping today has this same property; pretending otherwise is
  the actual vulnerability.
- Child **stderr** (diagnostics) is captured to a rotating file under
  `<hearth>/connectors/<name>/` — 1 MiB per file, 3 files max — after
  scrubbing injected values; never forwarded into worker responses or
  logs.

## 7. Lifecycle (stdio unless noted)

States (closed): `starting | ready | backoff | disabled`.
- Start: spawn (stdio) and first `tools/list` within `list_timeout`
  (default 30 s; covers child readiness — the `2026-07-28` core needs no
  handshake; SDK-negotiated legacy handshakes live inside this budget).
  Failure → `backoff`, reason `spawn_failed` or `list_timeout`.
- Backoff: 1 s doubling to a 5 min cap; max 5 restarts per rolling hour,
  then `disabled` (reason `protocol` for repeated protocol faults, else
  last reason) until `custos mcp refresh` or daemon restart.
- Crash with calls in flight: those calls → `transport`
  (`connection failed before response`). Custos never retries (the
  at-least-once decision belongs to the caller's turn).
- Shutdown / revocation: kill the child's own process **group** (spawn
  uses `Setpgid`), SIGTERM → 5 s → SIGKILL; no orphan grandchildren (I8).
- http: same state machine minus spawn. Three consecutive transport
  failures → `backoff`; 401/403 → `disabled` with reason `auth_failed`
  (never hammer an auth server).

## 8. Policy, ask, and the frozen error contract

### 8.1 Path
Rail calls travel the frozen dispatch order (locked → unknown_connector →
unknown_tool → policy → ask → execute). The rail supplies the same seven
interface methods Gmail does; nothing else in the order changes.

### 8.2 Ask rules (rail policy semantics)
- **R1**: no matching policy rule → `ask`. Default for every unseen rail
  tool.
- **R2**: absent annotation flags mean *not read-only* — annotations are
  server self-reports and never auto-approve anything. (They surface in
  `custos mcp tools` flags and, for destructive, as the A5 card prefix.)
- **R3**: the policy engine is frozen deterministic no-I/O and never
  consults tool schemas; rail tools carry no schema-based policy override
  in v1. Defense against schema/prose churn lives entirely in Execute-time
  revalidation (§4.3) and A6 revocation.
- **Always grants** (A6): `always` on a rail card writes the exact auto
  rule on `<name>/<tool>` (frozen §6.5 shape). A catalog refresh that
  changes that tool's fingerprint **MUST revoke** the auto rule (audit
  `mcp_always_revoked`); sibling tools unaffected. A revoked rule is gone,
  not demoted — the PO re-approves deliberately against the new shape.

### 8.3 Error mapping (frozen §5.1a templates + the one A2 constant)
| Failure | code | detail |
|---|---|---|
| child dead / conn lost / http connect fail | `transport` | `connection failed before response` (frozen) |
| http 4xx / 5xx at transport level | `upstream` | `upstream returned 4xx` / `5xx` (frozen) |
| execution or list deadline | `timeout` | `operation deadline exceeded` (frozen) |
| JSON-RPC error response; protocol violation; MRTR/elicitation present | `upstream` | `protocol violation` (**A2**) |
| credential missing at call | `denied` | `denied: credential unavailable` (A2(ii)) |

`tools/call` with `isError: true` is NOT in this table — see §4.5 (it is a
result, audited by the lane's `worker_call_allowed`). The rail emits no
per-call audit kind of its own (A8).

### 8.4 Elicitation / MRTR
Under `2026-07-28` a server that needs user input returns
`resultType: "input_required"` **in the call result**. The rail inspects
every call result; on `input_required` (or any inbound server-initiated
request on legacy revisions, which the SDK surfaces) it performs no
follow-up exchange and the worker call fails
`upstream` / `protocol violation`. Audit reason `protocol`. No card is
raised (v1).

## 9. ctl verbs (amendment A3)

Mechanism: the rail keeps its own handle registry (built from §3 config)
and answers these verbs on the existing custosd control socket (amendment
A3; frozen frame law — `<VERB> <ctl.token> [args...]`, line-oriented
ASCII, `ERR <reason>` on failure). The `Connector` interface gains **no**
methods; state, tool enumeration, and refresh are rail-module functions
over its own registry.

```
socket: MCP-STATUS  <token>          → header: name<TAB>state<TAB>tools<TAB>restarts<TAB>reason
socket: MCP-TOOLS   <token> <name>   → header: name<TAB>flags<TAB>title
socket: MCP-REFRESH <token> <name>   → OK | ERR <reason>
CLI:    custos mcp status | tools <name> | refresh <name>   (same tables;
        refresh exit 0 = refreshed, 1 + stderr reason)
```
Line-oriented per §11 (tab-separated, header-declared order, no
decoration). No secret material ever. Closed sets: `state` per §7;
`reason ∈ ok | auth_failed | spawn_failed | list_timeout | protocol |
credential_unavailable`; `flags` = comma-joined subset of
`read_only | destructive | open_world` (annotation-derived), or `none`
when no hint is set.

## 10. Dependencies and frozen constants

- `github.com/modelcontextprotocol/go-sdk` (Tier 1; speaks `2026-07-28`,
  negotiates legacy revisions; MIT/Apache-2.0 — compatible with the
  AGPL-3.0-only + dual-license posture). Vendored; exact version pinned at
  transcription with a `go build` + `-race` proof in the first brief.
  Fallback if migration friction: `mark3labs/mcp-go` (MIT), same pin rule.
- Frozen constants this spec cites (verified in tree):
  `workerMaxLineBytes = 1 << 20` (internal/custos/worker.go),
  `DefaultWorkerExecTimeout = 60s` (internal/custos/config.go), frozen
  detail-template block (internal/custos/worker.go §5.1a const set).

## 11. Security invariants (each maps to a test in §12)

- **I1** No env leak: spawned child's environment = exactly §6's base
  (PATH composed as specified) + declared entries; probe child reads
  `/proc/self/environ`.
- **I2** Scrub coverage: a sentinel secret injected via `${vault:}` never
  appears in worker responses, cards, audit files, logs, or the stderr
  capture file (probe child echoes it to stdout and stderr).
- **I3** Grammar gate: `bad/name`, `x`.`, empty, and 200-char tool names
  dropped at list time, never in `op:"list"`, audit emitted once per
  generation.
- **I4** Default ask: fresh rail tool, no rule → dispatch parks an ask;
  `deny` on bare `<name>` covers tools added by later refreshes.
- **I5** Always scoping (A6): `always` binds exactly `<name>/<tool>`;
  fingerprint change (including description-only edits, per the JCS
  fingerprint) MUST revoke it (audit); siblings unaffected.
- **I6** Serialized cap: the converge loop keeps FINAL serialized bytes
  ≤ 768 KiB in every mix (text-only, image-only, mixed, escape-heavy
  text), marker/placeholder present, audit recorded once per call.
- **I7** Process group: SIGKILL path on a hanging child also kills a
  probe grandchild.
- **I8** http host gate: public `http://` url rejected at load;
  loopback/RFC1918 accepted; `https` accepted.
- **I9** Revoke, stdio: credential revoked between approval and execute →
  child group dead; in-flight call settles `transport: connection failed
  before response`; new calls `denied: credential unavailable` (A2(ii)).
- **I10** Revoke, http (A2(iii) tombstone): credential revoked while an
  http call is in flight → that call completes; a response echoing the
  old value arrives scrubbed; after drain the connector is `disabled` and
  no new call can carry the value.
- **I11** Catalog churn is fail-closed: concurrent refresh + calls under
  `-race`; no torn reads (whole-snapshot visibility); tool removed
  between ValidateArgs and Execute → `unknown_tool`; schema tightened
  mid-hold so stored args now violate it → `bad_args`; neither executes.
- **I12** Placeholder safety: a `${vault:missing}` config never spawns a
  child containing the literal `${...}` text; state `disabled`, reason
  `credential_unavailable`.
- **I13** Credential-free rail (A7): connector with `Credential() ==
  none` (literal-env stdio server, bearer-less http) executes — no
  `credential_gone` denial; audit `credential: none`; Gmail path
  unchanged.

## 12. Verification suite (numbered)

- **V1** config loader — every legal shape in §3 loads.
- **V2** loader refusals — each §3 rule rejects with its named reason
  (table-driven, one subtest per rule); includes I8 host-gate cases and
  `transport: sse` rejection (legacy SSE is not a transport choice).
- **V3** fake stdio server fixture: register, first list, 3 tools exposed
  in `op:"list"` shape/order (A1), Gmail's static ToolLister entries also
  present; grammar drops (I3).
- **V4** call round-trip via `op:"call"` with a cell token: policy-`auto`
  tool executes; args arrive verbatim; result shape §4.5.
- **V5** ask flow: default park; approve-once → executes; deny →
  `denied: approval`.
- **V6** R1/I4 defaults and bare-name deny coverage.
- **V7** error matrix per §8.3 (kill mid-call; force deadlines; JSON-RPC
  error; HTTP 404/500 via fixture) — exact code+detail per row.
- **V8** `isError: true` passes through as a result, not an error code,
  audited once by `worker_call_allowed` — rail adds nothing (A8).
- **V9** MRTR `input_required` result → `upstream` / `protocol violation`,
  audit `protocol` (§8.4).
- **V10** catalog churn: removal between calls → `unknown_tool`;
  tightened schema across the ask-hold → `bad_args`; fingerprint-change
  audit + A6 revocation matrix (description-only edit included); `-race`
  stress (I11, I5).
- **V11** http transport against in-process fixture: bearer present/
  absent, 401 → disabled `auth_failed`, transport-flip → backoff.
- **V12** allow/deny filtering: allowlist restricts, deny wins, filtered
  tools absent from `op:"list"` and uncallable.
- **V13** concurrency: `max_concurrent_calls=2`, 4 parallel calls — two
  run, two queue, queue expiry → `timeout` (§4.5).
- **V14** stderr rotation: >1 MiB of stderr churn → ≤3 files under
  `<hearth>/connectors/<name>/`, oldest dropped, sentinel scrubbed (§6).
- **V15** ctl verbs: socket frames `MCP-STATUS`/`MCP-TOOLS`/`MCP-REFRESH`
  with token framing incl. bad-token `ERR`; exact headers, tab-separated
  lines, CLI exit codes, unknown `<name>` (§9).
- **V16** invariant probes, one named test each:
  V16a I1 env-exact probe; V16b I2 sentinel-echo probe (all surfaces —
  text, structuredContent leaf, stderr file, and base64 image-block echo
  caught at all three alignments per §4.5);
  V16c I6 cap matrix (text-only / image-only / mixed 500+500 KiB /
  escape-heavy 768 KiB text); V16d I7 process-group probe; V16e I8
  loopback acceptance probe; V16f I9 stdio revoke probe; V16g I10 http
  tombstone-echo probe; V16h I12 placeholder probe; V16i I13
  credential-free probe.
- Invariant map (reviewer checklist): I1→V16a, I2→V16b, I3→V3, I4→V6,
  I5→V10, I6→V16c, I7→V16d, I8→V2+V16e, I9→V16f, I10→V16g, I11→V10,
  I12→V16h, I13→V16i.
- Live acceptance (non-CI): the official filesystem MCP server on the dev
  box; one remote Streamable-HTTP demo server; cards rendered from the
  web UI; kill -9 churn soak.

## 13. Roadmap (non-normative)

Card-backed elicitation (MRTR `input_required` → ask-card round-trip — the
protocol's design and our approval model were built for each other);
Tasks-extension long calls; OAuth/CIMD for remote servers; uid-drop or
namespaced execution for stdio children; schema-aware policy consult (a
candidate CUSTOS amendment giving the frozen engine schema facts as pure
input, replacing the v1 non-override R3);
prompts-as-persona surface; MCP-server-mode export of Lararium's own
tools; connector install flow (ARCHITECTURE's builder).

## 14. Open questions — for the PO

1. **Truncation vs refusal for over-cap results.** Recommend truncate +
   marker (draft's behavior); refusal needs another frozen code.
2. **Auto-approve `readOnlyHint == true` tools by default.** Recommend
   **no** (R1 holds): annotations are self-reports; a PO who trusts a
   server writes one `auto` rule.
3. **Literal env values.** Recommend: accept + audit-warn (draft §3);
   hard-ban only once vault UX covers CLI-app friction.
4. **Both transports in v1 text, stdio first in builds.** Draft keeps one
   contract so it can never fork; first implementation brief lands stdio,
   second lands http on the same rail.

## Changelog

- v0.3.1: independent round-1 reviewer's late findings — two still live:
  `denied: credential unavailable` was mis-labeled frozen (§5.1a's only
  denied template is `denied: approval`) → A2(ii) owns it; scrub scope
  extended from text blocks to all result string leaves + three-alignment
  base64 needle check on blobs. Others already superseded by v0.2/v0.3.
- v0.3: round-2 absorption — credential-free connectors amended in (A7);
  audit kinds amended in (A8) and the duplicate per-call rail audit
  removed; A2(ii) tombstone makes scrub-retention implementable; A6 MUST +
  JCS fingerprint incl. description; A3 re-attributed to §3+§11 with
  wire verbs; snapshot claim corrected to fail-closed Execute revalidation;
  cap rewritten as measure-converge loop on serialized bytes; R3 withdrawn
  (frozen engine is no-I/O — roadmap candidate); I9/I10 split stdio/http;
  connector dirs out of the C2 root; SSE claim made honestly
  un-probeable; stdio same-uid trust posture stated plainly.
- v0.2: first-review absorption — A5/A6/A7 made explicit amendments
  (review-field prefix, always-revoke, auto-downgrade); `allow` renamed to
  frozen `auto`; review line rebuilt on the frozen §6.4 declared-fields
  shape; catalog snapshots + atomic swap (race fix); serialized-result cap
  incl. non-text blocks; scrub retention vs revocation leak window closed;
  revocation kills the child group; `init_timeout` → `list_timeout`
  (stateless-core honesty); MRTR result-inspection; ctl verbs under the
  custos namespace with closed column sets; concurrency + stderr rotation
  mechanisms and tests; invariant↔test map declared.
- v0.1: self-review (op:list discovery A1; protocol-violation template A2;
  isError-is-data; concrete caps; PATH composition).
- v0: initial text.
