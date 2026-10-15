# MCP-SPEC — the connector rail: external tool servers as Custos connectors

> Status: **Draft v0.7** — under review. Not approved; not frozen.
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
  sanitized (§4.2) and scrubbed (§2 P1). Read-only: no policy consult, no
  ask; cell token per existing lane rules. **Lane position:** `op:"list"`
  passes the frozen dispatch gates in the same order as `call` — op
  recognized, cell token verified, locked gate — so while locked it is
  rejected at the locked gate exactly as `call` is; A1 adds no new
  locked-state behavior, and cells enumerate after unlocking. Successful
  lists audit nothing (read-only, like CARDS polling); list-path denials
  carry empty `connector`/`tool` fields, as connection-level records
  already do. Enumeration mechanism, declared here so it exists: an
  **optional** interface `ToolLister { ListTools() []ToolInfo }` with
  `ToolInfo{Connector, Name, Title string}` — exactly the three columns
  the list row renders; descriptions and schemas deliberately do not
  travel this path. (A distinct type, asserted — the frozen `Connector`
  interface itself gains no methods.) Every rail handle implements it
  (dynamic, serving the current catalog snapshot); `GmailConnector`
  implements it with its static tool table; WorkerServer's `op:"list"`
  answer = rail snapshots ∪ assertion results from non-rail connectors
  (connector order: names sorted). Connectors that don't implement it
  contribute nothing. First-party cells render connector tool definitions
  only from this response (normative for first-party cells; custo cannot
  constrain third-party guest internals — see §2 P1).
- **A2** — §5.1a (worker error contract), three changes: (i) the closed
  detail-template set gains exactly one constant — code `upstream`, detail
  `protocol violation` (constant; no status class, no server body, per the
  no-body rule) — because MCP JSON-RPC-level failures and rejected
  server-initiated requests have no HTTP status to class (§8.3); the
  frozen closed `worker_call_denied` **reason set (§8.1)** correspondingly
  gains exactly `upstream`, which is what `ws.auditDenied` records for
  `we.code` on this path; (ii) the
  closed `denied` template set (frozen contents: exactly
  `denied: approval`) gains exactly `denied: credential unavailable`, for
  a credential removed between approval and execute (§8.3); (iii) the
  scrub scope gains **revoked-credential retention**: needles are
  snapshotted **per call at injection time** and held by that call's
  context until the call completes — values injected into a call stay
  covered by the scrubber for that call's entire life even if the vault
  forgets them mid-flight. No vault→WorkerServer notification is
  required or introduced: the frozen `RevokeCredential` touches only the
  vault (its only hook is `CancelByCredential`), and this design needs
  none. In-memory only, never persisted; §6. Without (iii) revocation
  blinds the scrubber mid-flight, because
  `ws.scrubNeedles()` reads `LoadedCredentials()`, which
  `RevokeCredential` purges immediately.
- **A3** — §3 (ctl.sock wire protocol) **and** §11 (ctl CLI): the socket
  verb set gains `MCP-STATUS <token>`, `MCP-TOOLS <token> <name>`,
  `MCP-REFRESH <token> <name>` (dash style per frozen verb law), and the
  `custos` CLI gains the matching `mcp status|tools|refresh` verbs (§9),
  obeying §11's line-oriented list conventions (tab-separated,
  header-declared order). Per-verb deadline: the frozen
  `ctlCommandTimeout` (5 s, applied via `conn.SetDeadline` to every
  command) is overridden for `MCP-REFRESH` only, to that connector's
  `list_timeout` + 5 s grace — a server restart legitimately outlives the
  generic ctl deadline; all other verbs keep the frozen 5 s. The CLI
  `mcp refresh` passes the matching client-side timeout to `RoundTrip`
  (the frozen 5 s client default would otherwise abort the very request
  A3 extends on the server).
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
  prompt-visible prose invalidates standing approval. Implementation
  shape: frozen `PolicyEngine.RemoveRule` hardcodes `policy_written` and
  cannot carry A8's payload, so revocation goes through an unexported
  variant `removeRuleWithAudit(lane, pattern, actor, kind, tool, cred)`;
  `RemoveRule` itself and the `policy rm` CLI path are untouched.
- **A7** — §4 (vault) + §5.1 (worker lane, credential doctrine): a rail
  connector's `Credential()` returns its config `name` — the **identity
  slot**: the same string as the policy pattern namespace (§6.2) and the
  card credential field, so frozen pattern matching
  (`ParsedCredentialPattern.MatchesWorkerCall`, which compares rule name
  against `Credential()`) keeps working verbatim. **custosd MUST ensure a
  vault slot of that name exists when the rail starts — once per daemon
  process lifecycle, on the first successful unlock, before rail
  connectors may leave `starting` — creating it with an empty value when
  absent** (idempotent). **Latched:** the ensure runs exactly once per
  process (or re-runs only via explicit `custos mcp refresh`), so
  deleting the identity slot to disable a connector **survives any
  number of later lock/unlock cycles** — the unlock handler must not
  resurrect a deliberately deleted slot. Ensuring at *config load* is
  impossible and not claimed: a standard passphrase boot starts locked
  with the vault sealed, and while locked **no rail behavior is ever
  reached** — the frozen worker's locked gate answers `locked` /
  `custody locked` before any connector or tool lookup (frozen dispatch
  law, quoted verbatim in A1; the rail claims nothing about — and can
  claim nothing about — the locked error code). A connector registered
  while locked simply sits `starting` with an empty catalog. An
  empty-valued slot contributes no scrub needle. Consequences:
  the frozen `GetCredential(conn.Credential())` never spuriously fails
  once unlocked. **Secrets are separate from the identity slot:**
  `${vault:<cred-id>}` references (§3) name their own secrets (e.g.
  `fs-key`), and policy patterns **never** bind secret names —
  `fs-key deny` matches nothing by design; the policy unit is
  `<connector>/<tool>` against the identity slot. This splits revocation
  into two honest kill-switches (§6): deleting the **identity slot** hits
  the frozen gate (`credential_gone`, all tools of that server);
  deleting a **referenced secret** is enforced at the injection edge —
  the rail resolves `${vault:}` references against the live vault at
  spawn (stdio) / per request (http), caches nothing, and a missing
  target fails that call `denied: credential unavailable` (A2(ii)). The
  frozen gate cannot see referenced-secret revocation and the spec does
  not claim it can. Gmail (returns `gmail`) is unaffected; lane dispatch
  code is untouched — the amendment changes vault *contents*, which §4
  owns. (An earlier draft — return the constant `none` and skip the
  lookup — is **rejected**: it decoupled the policy namespace from the
  credential dimension, which would have made `filesystem deny` and every
  `filesystem/<tool> auto` rule dead code, and written `none/<tool>`
  rules on `always` verdicts.)
- **A8** — §8.1 (closed audit-kind set): extended with exactly
  `mcp_config_literal_env`, `mcp_tool_dropped`, `mcp_catalog_changed`,
  `mcp_result_truncated`, `mcp_always_revoked`, `mcp_refresh`. In these
  payloads `cred` is the **config connector name** (control-plane
  identity), not the vault identity slot — so catalog audits carry
  attribution even while the vault is sealed. Rail calls need no extra
  per-call audit beyond the frozen `worker_call_allowed`/
  `worker_call_denied`; the sole exception is `mcp_result_truncated`,
  which exists only on calls that actually shrank a response (§4.5).
  Payloads ride the frozen
  `AuditRecord` field set only (`cred`, `host`, `tool`, `names`):
  `mcp_config_literal_env` → `cred` (connector name) + `host` +
  `names=[env-key]` (value never
  recorded); `mcp_tool_dropped` → `cred` (connector name) +
  `names=[raw-name]`; `mcp_catalog_changed` → `cred` (connector name) +
  `names=["+added"…, "-removed"…, "~fingerprint"…]` (qualified tool
  names); `mcp_refresh` → `cred` (one emission per successful
  `MCP-REFRESH`, §6 trust posture); `mcp_result_truncated` → `cred` + `tool` (qualified
  `<connector>/<tool>`) + `host` (per §4.2) +
  `names=["blocks_collapsed"|"text_truncated"|"structuredContent_dropped"]`;
  `mcp_always_revoked` → `tool` + `cred`. No new audit fields exist.

Stated count: eight. Reviewers: check attribution, not just count.

## 2. Principles (load-bearing)

1. **A tool server is untrusted input at every boundary.** Its `name`,
   `description`, `inputSchema`, annotations, titles, review-worthy text,
   and results render into prompts and cards. Names pass the tool-name
   grammar (§4.2); everything human-readable is scrubbed of injected
   credential values (§6) before any card, prompt, or log sees it, and
   reaches an HTML surface only through that surface's own existing
   escaping path (SURFACE-SPEC's rule; the rail adds no second escaping
   step, and never escapes into plaintext channels — ctl output, card
   `key=value` review fields — where escaping would corrupt data).
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
  `op:"call"`: the MCP tool name **normalized into the frozen policy
  grammar** `^[a-z0-9][a-z0-9_-]{0,63}$` (`keyNameRe` in `policy.go`,
  which also rejects uppercase outright). Raw MCP names are commonly
  camelCase (`readFile`) or underscore-prefixed (`_debug`) — those would
  pass ingestion but make `storeAlwaysRule`'s `AddCredentialRule(
  "<name>/<tool>")` fail `ErrInvalidPattern`, killing the `always`
  verdict and CLI rules for ordinary servers. Normalization is
  deterministic: lowercase → replace each character outside
  `[a-z0-9_-]` with `_` → while the first character is not `[a-z0-9]`,
  prefix `t-` → truncate to 64 → on collision within the connector's
  snapshot, append `-2`, `-3`, … (base truncated to fit). **Anything
  that survives normalization is name-lossless:** the snapshot keeps
  the canonical → raw wire-name mapping (in-process only); `op:"call"`
  addresses the
  canonical name and the rail maps back to the raw name on the MCP wire.
  Cards, audit records, and policy patterns always show the canonical
  name, so every stored pattern is grammar-valid by construction (V16l).
  A raw name that normalizes to the empty string (name `""`) is dropped
  at list time, audited once per catalog generation
  (`mcp_tool_dropped`), the only drop case.
- Policy patterns address `<name>` (bare = whole connector) and
  `<name>/<tool>` (exact outranks bare — frozen tier law).
- The custody card's `tool` field carries `<name>/<tool>`, matching the
  frozen §6 example shape (`gmail/send`).
- `Connector.Host()` for rail handles (frozen obligatory method; cards
  build `dest = host + ":" + tool`): **http** connector → URL hostname,
  same parse as Gmail's `Host()`; **stdio** connector → the connector
  `name` (a local child has no network host; a literal like
  `localhost` would collapse every local server into one indistinguishable
  card destination). So destinations read `filesystem:read_file`,
  `api.github.com:create_issue`.
- **Title/description sanitization at catalog ingestion** (server strings
  have no name grammar to hide behind): `title` and `description` render
  as CR/LF/TAB → single space, other C0/C1 control characters removed,
  whitespace runs collapsed, then truncate to 120 runes with `…`
  appended, then credential-scrub (§6). Only the sanitized form is ever
  stored or displayed, so no server string can forge a line-oriented ctl
  frame, split a TSV column, or smuggle prompt markdown that survives to
  cards. custo never dereferences server-supplied URIs.

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
  the server response. `notifications/tools/list_changed` (present since
  the `2025-03-26` floor) is **accepted and treated as a hint only**: it
  schedules the same debounced async refresh as TTL expiry and never
  mutates a snapshot inline (a server must not be able to change the
  catalog between two of its own `tools/list` answers through a side
  channel). `op:"list"` **never blocks on the network**: it
  answers the current snapshots in the one frozen NDJSON response frame.
  Expired connectors get an **async**, debounced (one-per-connector
  in-flight) refresh whose result appears in later lists. A connector
  whose TTL expired with no usable snapshot simply **contributes no
  rows** — list rows are exactly `ToolInfo` triples (A1); there is no
  `state` column to render, and connector state is observable only via
  `MCP-STATUS` (§9), never smuggled into the tool array. The
  synchronous, wait-for-result path is `custos mcp refresh <name>` alone
  (A3's per-verb deadline; exit 0 = catalog re-listed).
- Order: server order verbatim, **pinned per snapshot** — the protocol
  guarantees no cross-response determinism, so each snapshot freezes the
  arrival order and serving stays byte-stable until the next snapshot
  (re-sorting churns consumer prompt caches).
- A refresh that removes a tool takes effect at the next snapshot.
  "In-flight" means precisely: a call whose `tools/call` has **already been
  dispatched to the server** — it completes, because killing it mid-transport
  hides outcomes rather than preventing them. A call that has **not** reached
  dispatch — parked on an ask card, queued on the semaphore, or between
  ValidateArgs and Execute — is governed by the revalidation above and fails
  closed with `unknown_tool`. There is no third category (I11). A
  fingerprint change is audited
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
`tools/call` with the decoded args. **Closed result parsing:** the rail
parses the MCP result into the declared members only — `content`,
`structuredContent`, `isError` — and re-serializes from that struct;
unknown top-level members (`_meta` and any extension field) are
**discarded at parse and never forwarded**. Inside `content`, only the
recognized block types `text | image | audio | resource_link | resource`
survive parse; any other block shape is discarded too. That closed set
is what step 2's "non-text block" quantifies over. The worker result is
`{"content": [ …blocks… ], "structuredContent": …, "isError": …}`.
- **Budget: 768 KiB measured on the FINAL serialized bytes** (all block
  types, base64 and JSON escape expansion included), under the frozen 1 MiB
  lane line limit (`workerMaxLineBytes = 1 << 20`). Enforcement is a
  converge loop, not a single branch. **Progress rule:** every iteration
  must strictly shrink the serialized bytes; when no step among 2–3 makes
  strict progress, jump to step 4:
  1. serialize; if ≤ budget, done;
  2. replace the largest remaining non-text `content` block with `"[content omitted by custos]"` — only if that strictly shrinks the whole;
  3. truncate the largest `text` block body, appending the inline suffix `" [truncated by custos]"` (budgeting for the suffix itself) — only if strictly shrinkable; repeat 2–3 largest-first;
  4. **no-progress floor:** drop `structuredContent` to `null` if present, and replace the entire `content` array with one text block `"[content collapsed by custos]"`. This catches every deadlock the round-4/5 reviews found: framing-only overflow (thousands of `{type,text:""}` blocks), a `structuredContent`-only breach, and (with closed parsing above) extension-field payloads such as a multi-megabyte `_meta` — nothing outside the declared struct survives parsing, so the floor's serialization is a true upper bound;
  5. re-serialize and re-measure after each action. Step 4's state is
     one collapsed text block with `structuredContent: null` — a fixed,
     tiny serialization well under budget — so at most one further
     iteration follows it — termination is structural and no server
     payload, of any block count, can wedge the worker thread (V16c).
     Escaping expansion (\\n, \\uXXXX) is measured, never predicted. The
     audit records A8's token names (`blocks_collapsed`,
     `text_truncated`, `structuredContent_dropped`) — the display strings
     in steps 2–4 are content, not audit vocabulary. One
     `mcp_result_truncated` audit per call that fired any step (never on
     a result already within budget). `op:"list"` responses honor the
     same budget: drop tail entries until ≤ budget, then append **one
     marker row that is a valid `ToolInfo` triple** —
     `{connector: "custos", name: "truncated", title: "[truncated by
     custos — N more tools]"}` — so typed unmarshaling of the tools
     array never breaks (a raw string or a grammar-invalid name would
     fail `json.Unmarshal` into `[]ToolInfo`). `custos` is therefore a
     **reserved connector name**: a config entry with `name: custos` is
     a loader error. The serialized output can never exceed the budget.
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
  **Decode-or-replace rule:** every base64 block is additionally
  decoded and its **decoded payload** substring-matched against the
  needles; a hit — or a base64-encoding alignment hit — replaces the
  whole block. Decoding after re-encode closes the non-canonical
  variants (alternative alphabets, dropped/permuted padding) that an
  alignments-only check misses. Deliberately fragmented or transformed
  (e.g. reversed, XOR-wrapped) smugglings are out of scope v1 and
  honestly so: the child only ever holds what injection gave it, so the
  scrubber bounds echo noise, and the injection edge bounds exposure
  (V16b probes a non-canonical re-encode).
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
  credentials, which revocation purges immediately; A2(iii)'s per-call
  needle snapshot is what makes this sentence implementable — the frozen
  `RevokeCredential` notifies nothing, and this design needs none of it.
- **Revocation (mechanism, not aspiration):** the frozen `RevokeCredential`
  touches vault + hub cancel only, and neither may change. The kill path
  is therefore a **composition obligation**: the credential-delete verbs
  are served at daemon level — the process that owns both vault and rail
  — and that serving code MUST, synchronously after a successful vault
  delete, notify the rail: `NotifyCredentialGone(name)` for every rail
  connector whose identity slot **or** `${vault:<name>}` reference names
  the deleted credential. No vault API gains a hook (A2(iii) stands —
  WorkerServer is never notified); the observer lives where ownership
  already exists. On notification the rail, in order:
  **(a)** marks the connector `disabled`, reason `credential_unavailable`
  — the *normative* kill, because the frozen gate order guarantees no
  later dispatch reaches `Execute`; **(b)** for **stdio**, kills the
  child process **group** (SIGTERM, 5 s grace, SIGKILL; §7 spawn uses
  `Setpgid`) so the value stops being readable via `/proc/<pid>/environ`
  and cannot be echoed after the fact; **(c)** holds every needle it
  injected for that credential until the in-flight calls complete —
  their snapshots (A2(iii)) were taken pre-delete and keep scrubbing the
  only remaining echo window. In-flight stdio calls settle `transport`
  (`connection failed before response`) per §7; in-flight http requests
  complete and their bodies scrub against their own snapshots.
  Two kill-switches, honestly separated (A7): deleting the **identity
  slot** also trips the frozen gate itself (every tool answers
  `credential_gone` even if the rail notification were ever missed —
  belt and suspenders); deleting a **referenced secret** relies on (a) +
  injection-edge resolution (the rail caches no values, so a later spawn
  or request resolves the missing reference and fails `denied: credential
  unavailable`, A2(ii)); the frozen gate cannot see that second case and
  no claim is made that it can. A delete that somehow arrives with no
  notification wired is a bug pinned by V16f (process-group probe), not
  an unclaimed path. Re-enable by `custos mcp refresh` after the
  credential is restored (refresh re-ensures the identity slot — the
  latch of A7 makes refresh the only re-ensure path after a deliberate
  delete).
  **Trust posture of catalog mutation:** `ctl.token` is the frozen
  control-socket credential — its holder can already write policy rules,
  revoke credentials, and restart the daemon, so `MCP-REFRESH` inherits
  operator trust and the rail does not treat it as a distinct adversary.
  What refresh cannot do is launder trust: every successful refresh
  emits `mcp_refresh` (A8), and refreshed catalogs are still untrusted
  data — §4.2 sanitization and A6 fingerprint revocation apply to every
  new snapshot, so a hostile refresh can flip an `auto` tool away from a
  grant but never silently keep one.
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
  uses `Setpgid`), SIGTERM → 5 s → SIGKILL; no orphan grandchildren (I7).
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
result, audited by the lane's `worker_call_allowed`). The rail adds no
error-path audit kind of its own (A8; its one result-side exception,
`mcp_result_truncated`, is not an error path — §4.5).

### 8.4 Elicitation / MRTR
Under `2026-07-28` a server that needs user input returns
`resultType: "input_required"` **in the call result**. The rail inspects
every call result; on `input_required` (or any inbound server-initiated
request on legacy revisions, which the SDK surfaces) it performs no
follow-up exchange and the worker call fails
`upstream` / `protocol violation`, audited with reason `upstream`
(A2(i); `protocol` names only rail-internal state in §9, never an audit
reason). No card is raised (v1).

## 9. ctl verbs (amendment A3)

Mechanism: the rail keeps its own handle registry (built from §3 config)
and answers these verbs on the existing custosd control socket (amendment
A3; frozen frame law — `<VERB> <ctl.token> [args...]`, line-oriented
ASCII, `ERR <reason>` on failure). The `Connector` interface gains **no**
methods; state, tool enumeration, and refresh are rail-module functions
over its own registry.

```
socket: MCP-STATUS  <token>          → OK <json>  [{"name","state","tools","restarts","reason"}…]
socket: MCP-TOOLS   <token> <name>   → OK <json>  [{"name","flags","title"}…]
socket: MCP-REFRESH <token> <name>   → OK | ERR <reason>
CLI:    custos mcp status | tools <name> | refresh <name>   (renders the
        JSON as tables; refresh exit 0 = refreshed, 1 + stderr reason)
```
Socket responses are the frozen **single-line `OK <json>` / `ERR <reason>`
frames** `CtlClient.RoundTrip` mandates (one line, `OK `-prefixed — raw
TSV headers on the wire would die in its `unexpected response` branch);
the CLI renders them into §11-style line-oriented tables (tab-separated,
header-declared order, no decoration) — line-oriented governs **stdout**,
not the socket. No secret material ever. Closed sets: `state` per §7;
`reason ∈ ok | auth_failed | spawn_failed | list_timeout | protocol |
`credential_unavailable`; `flags` = comma-joined subset of
`read_only | destructive | open_world` (annotation-derived) in the fixed
canonical order `read_only,destructive,open_world` — set members omitted
when absent, so output is byte-stable — or `none` when no hint is set.
`idempotentHint` is deliberately **not** in the flags enum (no v1
consumer); it still rides the §4.3 per-tool fingerprint, so A6 revocation
covers changes to it.

## 10. Dependencies and frozen constants

- `github.com/modelcontextprotocol/go-sdk` (Tier 1; speaks `2026-07-28`,
  negotiates legacy revisions). License: **Apache-2.0**, with a small
  residue of MIT-licensed contributions under the project's in-progress
  relicensing (LICENSE notice checked at pin time — both are compatible
  with the AGPL-3.0-only + dual-license posture, as with any Apache-2.0
  Go dependency). Vendored; exact version pinned at transcription with
  a `go build` + `-race` proof **and a recorded copy of the pinned
  LICENSE header** in the first brief. Fallback if migration friction:
  `mark3labs/mcp-go` (MIT), same pin + license-record rule.
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
- **I3** Canonical naming (normalization, not drops): raw `readFile` →
  `readfile`, `_debug` → `t-_debug`, `notion.search` → `notion_search`;
  every canonical name matches `keyNameRe` (`^[a-z0-9][a-z0-9_-]{0,63}$`),
  an `always` verdict on any of them stores without `ErrInvalidPattern`
  (V16l); collisions get `-2` suffixes; only a name normalizing to empty
  drops at list time, audited once per generation.
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
- **I9** Revoke, stdio (§6 mechanism): credential revoked between
  approval and execute → delete-verb's `NotifyCredentialGone` lands →
  connector `disabled` and child group dead; in-flight call settles
  `transport: connection failed before response`; new calls
  `credential_unavailable` at the gate; identity-slot delete additionally
  answers `credential_gone` (frozen gate), and the A7 latch keeps a
  deleted slot absent across a lock/unlock cycle (no resurrection without
  `custos mcp refresh`).
- **I10** Revoke, http (A2(iii) call-local needle snapshot): credential
  revoked while an http call is in flight → that call completes; a
  response echoing the old value arrives scrubbed (the call's own needle
  snapshot survives vault purge); after drain the connector is `disabled`
  and no new call can carry the value.
- **I11** Catalog churn is fail-closed: concurrent refresh + calls under
  `-race`; no torn reads (whole-snapshot visibility); tool removed
  between ValidateArgs and **dispatch** → `unknown_tool`; tool removed
  after dispatch → that call completes; schema tightened mid-hold so
  stored args now violate it → `bad_args`; nothing pre-dispatch executes.
- **I12** Placeholder safety: a `${vault:missing}` config never spawns a
  child containing the literal `${...}` text; state `disabled`, reason
  `credential_unavailable`.
- **I13** Identity slot (A7): rail start after the first unlock ensures
  its empty vault slot **once per process** (locked boot: rail never
  touched — dispatch dies at the frozen locked gate `locked` before any
  connector lookup, both `op:"call"` and `op:"list"`; no vault write is
  ever attempted while sealed); rules keyed to the connector name match
  (`filesystem deny` denies every rail tool of `filesystem`; an `always`
  verdict on `filesystem/read_file` writes exactly
  `filesystem/read_file auto`, never `none/…` or a cred-less rule);
  deleting the slot makes all its tools answer `credential_gone` — and
  the latch survives lock/unlock: still `credential_gone` after `custos
  lock && custos unlock` — until `custos mcp refresh` re-ensures it;
  deleting a **referenced** secret (`${vault:fs-key}` target) disables
  the connector per §6 (frozen gate untouched); cards show `filesystem`
  in the credential field.

## 12. Verification suite (numbered)

- **V1** config loader — every legal shape in §3 loads.
- **V2** loader refusals — each §3 rule rejects with its named reason
  (table-driven, one subtest per rule); includes I8 host-gate cases and
  `transport: sse` rejection (legacy SSE is not a transport choice).
- **V3** fake stdio server fixture: register, first list, 3 tools exposed
  in `op:"list"` shape/order (A1), Gmail's static ToolLister entries also
  present; raw camelCase/underscore names arrive canonicalized (I3);
  while locked, `op:"list"` is rejected at
  the frozen locked gate exactly as `call` is (A1 lane position).
- **V4** call round-trip via `op:"call"` with a cell token: policy-`auto`
  tool executes; args arrive verbatim; result shape §4.5. Includes the
  namespace probes: `filesystem deny` denies; `filesystem/read_file auto`
  allows while a sibling still asks; card cred field = `filesystem` (A7).
- **V5** ask flow: default park; approve-once → executes; deny →
  `denied: approval`.
- **V6** R1/I4 defaults and bare-name deny coverage.
- **V7** error matrix per §8.3 (kill mid-call; force deadlines; JSON-RPC
  error; HTTP 404/500 via fixture) — exact code+detail per row.
- **V8** `isError: true` passes through as a result, not an error code,
  audited once by `worker_call_allowed` — rail adds nothing (A8).
- **V9** MRTR `input_required` result → `upstream` / `protocol violation`,
  audit reason `upstream` — the record validates against the frozen
  §8.1 reason enum with the A2(i) addition (§8.4).
- **V10** catalog churn: removal between ValidateArgs and dispatch →
  `unknown_tool`; removal after dispatch → in-flight call completes
  (§4.3 two-category rule);
  tightened schema across the ask-hold → `bad_args`; fingerprint-change
  audit + A6 revocation matrix (description-only edit included), and the
  revocation audit line's kind is `mcp_always_revoked` — never
  `policy_written` (`removeRuleWithAudit`, A6/A8); `-race`
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
  with token framing incl. bad-token `ERR`; responses asserted **through
  `CtlClient.RoundTrip`** — single-line `OK <json>` / `ERR <reason>`
  frames, its `unexpected response` branch never fires; CLI tables, exit
  codes, unknown `<name>` (the unknown-`<name>` probe applies to
  `MCP-TOOLS`/`MCP-REFRESH`; `MCP-STATUS` is no-arg — it lists all
  connectors — and has no name subtest); **slow-server fixture**: a 6 s
  stdio server completes `MCP-REFRESH` under the per-verb deadline (A3,
  client-side timeout included)
  while a generic verb on the same socket still times out at 5 s (§9).
- **V16** invariant probes, one named test each:
  V16a I1 env-exact probe; V16b I2 sentinel-echo probe (all surfaces —
  text, structuredContent leaf, stderr file, base64 image-block echo
  caught at all three alignments per §4.5, **plus a non-canonically
  re-encoded base64 echo caught by the decode-or-replace rule**);
  V16j title-injection fixture — a tool `title` carrying CR/LF/TAB,
  control characters, and a sentinel renders sanitized (single-line,
  ≤120 runes, scrubbed) in `op:"list"`, `MCP-TOOLS`, and cards (§4.2);
  V16k catalog overflow — a 10k-tool server with 5 KB titles answers
  `op:"list"` within the serialized budget, tail-dropped with the
  `custos/truncated` marker row, and the response unmarshals cleanly
  into `[]ToolInfo` (§4.5);
  V16c I6 cap matrix (text-only / image-only / mixed 500+500 KiB /
  escape-heavy 768 KiB text / **35k empty-text-blocks framing bomb →
  collapse floor, bounded time** / **2 MiB structuredContent → dropped**
  / **1 MiB `_meta` + empty content → dropped at parse, floor never
  breached, loop terminates**);
  V16d I7 process-group probe; V16e I8
  loopback acceptance probe; V16f I9 stdio revoke probe (notify→disable,
  group death, **latch across lock/unlock**); V16g I10 http
  snapshot-echo probe; V16h I12 placeholder probe; V16i I13
  identity-slot probe (both kill-switches, locked boot answered by the
  frozen locked gate before any rail code, latch);
  V16l I3 normalization fixture — `readFile`, `_debug`, `notion.search`,
  a 200-char name, and two raw names colliding after lowercase-fold all
  get canonical `keyNameRe` names, `op:"call"` round-trips the raw wire
  name, and `always` on each stores an exact rule with no
  `ErrInvalidPattern`.
- Invariant map (reviewer checklist): I1→V16a, I2→V16b+V16j, I3→V3+V16l,
  I4→V6,
  I5→V10, I6→V16c+V16k, I7→V16d, I8→V2+V16e, I9→V16f, I10→V16g, I11→V10,
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

- v0.7: round-5 delegated-lane absorption (late second opinion on v0.5;
  the AGY-lane items of that round shipped as v0.6) — `mcp_refresh`
  audit kind + catalog-mutation trust posture declared (§6: ctl.token =
  operator trust; refresh audited, catalogs still untrusted via A6);
  base64 smuggling closed past alignment checks with a decode-or-replace
  rule + non-canonical V16b probe (claim honestly scoped: exact-needle
  re-encodings, not a transform oracle); `tools/list_changed` declared
  hint-only (schedules debounced refresh, never mutates a snapshot
  inline); per-snapshot order pinning stated (protocol guarantees no
  cross-response order); closed content-block set named for the cap
  loop's step 2; `MCP-STATUS` no-arg semantics vs V15 unknown-name probe
  reconciled; `idempotentHint` stated out of the flags enum (fingerprint
  still covers it); V10 asserts the revocation audit kind is
  `mcp_always_revoked`, never `policy_written`; A8 `cred` declared
  config-identity (vault-independent); §10 SDK license corrected to
  **Apache-2.0 with MIT residue** (LICENSE file verified upstream) and
  the pin rule now records the license header.
- v0.6: round-5 absorption — tool names **normalized into the frozen
  `keyNameRe` grammar** (lowercase/map/prefix/truncate + collision
  suffixes, snapshot keeps the canonical→raw wire mapping; camelCase
  servers no longer poison `always` rules with `ErrInvalidPattern`; drop
  only for names normalizing to empty); revocation kill-switch given a
  **mechanism** — composition-root `NotifyCredentialGone` on the
  credential-delete verbs (vault API unchanged), disable-before-kill,
  needles held through in-flight drain; A7 ensure **latched once per
  process** (a deliberately deleted identity slot survives lock/unlock
  until `custos mcp refresh`) and the locked-boot claim corrected to the
  frozen truth (locked gate answers before any rail code); `tools/call`
  result parsing made **closed** (unknown members like `_meta` discarded
  at parse — cap floor is a true serialization bound); `op:"list"`
  truncation marker made a valid `ToolInfo` row with `custos` reserved
  as a connector name; async-refresh row myth deleted (rows are pure
  triples; state lives in `MCP-STATUS`); audit payload and flag-column
  field orders pinned; §6 notification prose and §7 cross-refs fixed.
- v0.5: round-4 absorption — ctl verbs moved to the frozen **single-line
  `OK <json>` socket frame** (raw TSV headers would die in
  `CtlClient.RoundTrip`'s `unexpected response` branch; line-oriented
  governs CLI stdout only); A7 identity-slot ensuring moved from boot
  (impossible: locked passphrase boot — deadlock) to first unlock,
  `starting` + `credential_unavailable` while locked, and revocation
  doctrine split into two honest kill-switches (identity slot → frozen
  `credential_gone`; referenced secret → injection-edge
  `denied: credential unavailable`, with the frozen gate's blindness
  stated, plus: policy never binds secret names); A2(iii) re-specified as
  per-call needle snapshots (no vault→WorkerServer hook exists or is
  needed; the frozen `RevokeCredential` notifies nothing); cap loop given
  a strict-progress rule with a reachable no-progress floor (fixes the
  structuredContent-only deadlock) and A8 audit tokens replacing display
  strings; A1 declares op:list's lane position (frozen locked gate, no
  new locked behavior, empty connector/tool fields on list-path records);
  §4.3 op:list made single-frame (snapshot-serve, async debounced
  refresh — the old inline-refresh sentence could not fit one NDJSON
  response); server `title`/`description` sanitization (protocol
  injection via CR/LF/TAB in `MCP-TOOLS`, unbounded catalog overflow)
  with V16j/V16k; A6 revocation gets an `removeRuleWithAudit` variant
  (frozen `RemoveRule` hardcodes `policy_written`); A8 payloads carry
  connector attribution and the per-call-audit contradiction resolved;
  A3 extended to the CLI-side per-verb timeout; `ToolInfo` declared.
- v0.4: round-3 absorption — **A7 rewritten as the identity slot**
  (`Credential()` returns the connector name; custosd ensures an
  empty-valued vault slot at load): the earlier `none`-skip draft
  decoupled the §6.2 policy namespace from the credential dimension
  (`filesystem deny` dead, `always` wrote `none/<tool>`); **A1 declares
  the ToolLister mechanism** that §12/V3 had been citing as a phantom;
  converge loop given terminal collapse/drop steps (framing-only overflow
  and structuredContent were infinite-loop/bypass paths); in-flight
  defined as past-dispatch, closing the §4.3/I11 contradiction; §8.4 audit
  reason `protocol` → `upstream` with A2(i) extending the frozen §8.1
  reason set; A2(iii) attribution sharpened (WorkerServer needle overlay,
  not vault retention); A3 overrides `ctlCommandTimeout` per-verb for
  MCP-REFRESH; A8 payloads mapped onto frozen `AuditRecord` fields;
  `Host()` defined per transport; Principle 1 escaping claim corrected to
  the surface's own path; guest-facing MUST NOT down-scoped.
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
