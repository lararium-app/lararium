# MCP-SPEC — the connector rail: external tool servers as Custos connectors

> Status: **Draft v0.2** — under review. Not approved; not frozen.
> Companion targets: CUSTOS-SPEC (amendments A1–A7 below), SURFACE-SPEC (none), ARCHITECTURE §"MCP is the connector standard" (already ratified; this spec implements it).
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
- **A2** — §5.1a (worker error contract): the closed detail-template set
  gains exactly one constant: code `upstream`, detail `protocol violation`
  (constant — no status class, no server body, consistent with the no-body
  rule). Needed because MCP JSON-RPC-level failures and rejected
  server-initiated requests have no HTTP status to class (§8.3).
- **A3** — §11 (ctl CLI frozen shapes): adds `custos mcp status`,
  `custos mcp tools <name>`, `custos mcp refresh <name>` (§9), obeying §11's
  line-oriented list conventions (tab-separated, header-declared order).
- **A4** — config schema: `custos:` gains `connectors.mcp[]` (§3). The
  loader is strict; these are the exact permitted keys.
- **A5** — §6.4 (card review fields): for **rail tools only**, when the
  tool's annotations carry `destructiveHint == true`, the review line is
  prefixed with the constant `⚠ destructive `. Prefix-only; the rest of the
  frozen review-field construction (declared schema fields, argument
  digest) is untouched.
- **A6** — §6.5 (Always verdict): for **rail tools**, an `always` verdict
  writes an exact `auto` rule on `<connector>/<tool>` as frozen — and the
  rail MAY revoke that auto rule (audited, never silently) when the tool's
  schema hash changes (§8.2).
- **A7** — §6 (comparator semantics): for **rail tools**, an `auto` rule
  resolves as `ask` when the tool's `inputSchema` is absent or malformed
  (§8.2 R3). Downgrade happens inside the policy consult, so the frozen
  dispatch order and `auto`'s meaning for every non-rail pattern are
  unchanged.

Stated count: seven. Reviewers: check attribution, not just count.

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
- The catalog is an **immutable snapshot** (tool set + schemas + schema
  hashes). Refresh builds a new snapshot and swaps it via `atomic.Pointer`;
  readers and in-flight calls pin the snapshot they started with — no torn
  reads (I11; race-detector test V10).
- First list at registration; thereafter honor `ttlMs`/`cacheScope` from
  the server response. `op:"list"` serves snapshots from cache; when a
  connector's cache has expired, that connector refreshes inline bounded by
  `list_timeout`, while cached connectors' entries are emitted without
  waiting. `custos mcp refresh <name>` forces immediate re-list.
- Order: server order verbatim (list results are spec'd deterministic;
  re-sorting churns consumer prompt caches).
- A refresh that removes a tool takes effect at the next snapshot; in-flight
  calls to it complete. A schema-hash change is audited
  (`mcp_catalog_changed`: names added/removed + hash-changed list) and
  triggers A6 revocation for exactly those tools' auto rules (§8.2).

### 4.4 ValidateArgs (shallow, deliberate)
Worker lane runs before policy/ask. v1 validates **structurally, not
semantically**: args unmarshal to a JSON object; every entry in the tool's
`inputSchema.required` is present; top-level properties present are
type-compatible with `inputSchema.properties` (by JSON kind; `enum`
membership when declared). Nested validation is the server's job.
Violation → frozen `bad_args` (`invalid arguments`). A tool whose schema is
absent or malformed accepts any object here (the server is authoritative)
— the schema problem is handled at policy (R3/A7), not by rejecting calls.

### 4.5 Execute and result budget
`tools/call` with the decoded args. The worker result is
`{"content": [ …blocks… ], "structuredContent": …, "isError": …}`.
- **Budget: 768 KiB against the fully serialized result** (all block types,
  base64 included), under the frozen 1 MiB lane line limit
  (`workerMaxLineBytes = 1 << 20`). Enforcement order: truncate `text`
  blocks at `768 KiB − marker length` and append the block
  `"[truncated by custos]"`; if non-text blocks alone exceed the budget,
  replace them (largest first) with `"[content omitted by custos]"`.
  Either path audits `mcp_result_truncated`. The serialized output can
  never exceed the budget.
- `text` block bodies are scrubbed against **that connector's own injected
  credential values** before delivery (O(1) per-request scope, the
  bearer-lane analogue; whole-vault scans stay on `detail` per §5.1a's
  scope split). Custos never dereferences resource links or fetches blob
  URIs.
- `isError: true` is data, not a custody failure: the flag and (scrubbed,
  capped) content travel as the result. The frozen no-body rule governs
  error `detail` construction; a processed tool error is a result. Audit
  records it with `ok=false`, reason `is_error`. §8.3 governs
  custody/transport failures.
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
- **http:** custos injects exactly one header, `Authorization` with the
  bearer value. Protocol headers the SDK sends (`Mcp-Method`, `Mcp-Name`,
  `MCP-Protocol-Version`) are the SDK's contract, not custos additions.
- **Scrub retention (the leak window):** every injected value registers
  with the scrubber (scope: that connector for result bodies, §4.5;
  whole-set for `detail` per §5.1a) at injection and **stays registered
  until every in-flight call that could carry it has completed** —
  revocation removes the *source*, never early scrub coverage.
- **Revocation:** on credential revoke (frozen KEYS/CUSTOS path) the
  connector transitions `disabled`, reason `credential_unavailable`; for
  stdio the child process **group** is killed (SIGTERM, 5 s grace, SIGKILL)
  so the value dies in memory and stops being readable via
  `/proc/<pid>/environ`; new calls fail the frozen
  `denied: credential unavailable` path. Re-enable by `custos mcp refresh`
  after the credential is restored.
- Child **stderr** (diagnostics) is captured to a rotating file under the
  custos root — 1 MiB per file, 3 files max — after scrubbing injected
  values; never forwarded into worker responses or logs.

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
- **R3** (A7): matching `auto` rule + absent/malformed `inputSchema` →
  resolves as `ask` (downgrade inside the policy consult; audited once per
  tool per schema state as `mcp_auto_downgraded`).
- **Always grants** (A6): `always` on a rail card writes the exact auto
  rule on `<name>/<tool>` (frozen §6.5 shape). A catalog refresh that
  changes that tool's schema hash **revokes** the auto rule (audit
  `mcp_always_revoked`); sibling tools unaffected. A revoked rule is gone,
  not demoted — the PO re-approves deliberately against the new schema.

### 8.3 Error mapping (frozen §5.1a templates + the one A2 constant)
| Failure | code | detail |
|---|---|---|
| child dead / conn lost / http connect fail | `transport` | `connection failed before response` (frozen) |
| http 4xx / 5xx at transport level | `upstream` | `upstream returned 4xx` / `5xx` (frozen) |
| execution or list deadline | `timeout` | `operation deadline exceeded` (frozen) |
| JSON-RPC error response; protocol violation; MRTR/elicitation present | `upstream` | `protocol violation` (**A2**) |
| credential missing at call | `denied` | `denied: credential unavailable` (frozen) |

`tools/call` with `isError: true` is NOT in this table — see §4.5 (it is a
result). Rail audit kind `mcp_call` records `{connector, tool, ok, reason}`
with `reason ∈ ok | is_error | transport | timeout | protocol | auth`
(closed set declared here).

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
and answers these verbs on the existing custosd control socket. The
`Connector` interface and WorkerServer gain **no methods**; state, tool
enumeration, and refresh are rail-module functions over its own registry.

```
custos mcp status          → header: name<TAB>state<TAB>tools<TAB>restarts<TAB>reason
custos mcp tools <name>    → header: name<TAB>flags<TAB>title
custos mcp refresh <name>  → exit 0 refreshed; exit 1 + stderr reason
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
  schema-hash change revokes it (audit); siblings unaffected.
- **I6** Auto downgrade (A7/R3): auto rule + malformed schema → ask +
  `mcp_auto_downgraded` audit; valid schema restores auto behavior.
- **I7** Serialized cap: result with huge text and huge image serializes
  ≤ 768 KiB in every mix, marker/placeholder present, audit recorded.
- **I8** Process group: SIGKILL path on a hanging child also kills a probe
  grandchild.
- **I9** http host gate: public `http://` url rejected at load;
  loopback/RFC1918 accepted; `https` accepted.
- **I10** Revoke: credential revoked between approval and execute → frozen
  `denied: credential unavailable`; stdio child group dead; scrubber keeps
  the old value until drained (I12).
- **I11** Catalog race: concurrent refresh + calls under `-race`; in-flight
  call uses its pinned snapshot's schema for the whole call.
- **I12** Retention window: after revoke, an in-flight call whose server
  echoes the old secret still arrives scrubbed; post-drain the connector is
  `disabled` and no new call can carry the value.
- **I13** Placeholder safety: a `${vault:missing}` config never spawns a
  child containing the literal `${...}` text; state `disabled`, reason
  `credential_unavailable`.

## 12. Verification suite (numbered)

- **V1** config loader — every legal shape in §3 loads.
- **V2** loader refusals — each §3 rule rejects with its named reason
  (table-driven, one subtest per rule); includes I9 host-gate cases.
- **V3** fake stdio server fixture: register, first list, 3 tools exposed
  in `op:"list"` shape/order (A1); grammar drops (I3).
- **V4** call round-trip via `op:"call"` with a cell token: policy-`auto`
  tool executes; args arrive verbatim; result shape §4.5.
- **V5** ask flow: default park; approve-once → executes; deny →
  `denied: approval`.
- **V6** R1/I4 defaults and bare-name deny coverage.
- **V7** error matrix per §8.3 (kill mid-call; force deadlines; JSON-RPC
  error; HTTP 404/500 via fixture) — exact code+detail per row.
- **V8** `isError: true` passes through as a result, not an error code
  (§4.5/§8.3 boundary), with `mcp_call(ok=false, is_error)` audit.
- **V9** MRTR `input_required` result → `upstream` / `protocol violation`,
  audit `protocol` (§8.4).
- **V10** catalog snapshots: removal, schema-change audit, A6 revocation
  matrix, and `-race` stress (I11).
- **V11** http transport against in-process fixture: bearer present/
  absent, 401 → disabled `auth_failed`, transport-flip → backoff.
- **V12** allow/deny filtering: allowlist restricts, deny wins, filtered
  tools absent from `op:"list"` and uncallable.
- **V13** A7 downgrade round-trip (I6).
- **V14** concurrency: `max_concurrent_calls=2`, 4 parallel calls — two
  run, two queue, queue expiry → `timeout` (§4.5).
- **V15** stderr rotation: >1 MiB of stderr churn → ≤3 files, oldest
  dropped, sentinel scrubbed (§6).
- **V16** ctl verbs: exact headers, tab-separated lines, exit codes for
  all three verbs incl. unknown `<name>` (§9).
- **V17** invariant probes, one named test each:
  V17a I1 env-exact probe; V17b I2 sentinel-echo probe (all surfaces);
  V17c I7 cap matrix (text-only / image-only / mixed); V17d I8 process
  group probe; V17e I9 loopback acceptance probe; V17f I10 revoke
  mid-flight probe; V17g I12 retention-window probe; V17h I13
  placeholder probe.
- Invariant map (reviewer checklist): I1→V17a, I2→V17b, I3→V3, I4→V6,
  I5→V10, I6→V13, I7→V17c, I8→V17d, I9→V2+V17e, I10→V17f, I11→V10,
  I12→V17g, I13→V17h.
- Live acceptance (non-CI): the official filesystem MCP server on the dev
  box; one remote Streamable-HTTP demo server; cards rendered from the
  web UI; kill -9 churn soak.

## 13. Roadmap (non-normative)

Card-backed elicitation (MRTR `input_required` → ask-card round-trip — the
protocol's design and our approval model were built for each other);
Tasks-extension long calls; OAuth/CIMD for remote servers;
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
