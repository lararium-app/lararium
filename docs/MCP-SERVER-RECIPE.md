# Connecting Your First MCP Server

This walkthrough connects a Model Context Protocol (MCP) server to Lararium over standard I/O, verifies execution from the command line, and exposes the tools to model chat under the custody policy gate. Completing the steps takes roughly 15 minutes on a fresh machine: the Go build and the first `npx` package download each add a few minutes over the interaction itself.

## Prerequisites

- Node.js and `npx` installed on the host.
- **This is a bare-host walkthrough.** The Docker quick-start ships `hearthd`
  only — `custosd` is not part of that image and does not run in that stack,
  so MCP connectors cannot work there. (Running `custosd` in its own container
  is possible: `command` binaries must then exist *inside that* image — prefer
  the HTTP transport or a custom image with Node.js installed.)
- **Build all three binaries** (`$REPO` is your Lararium checkout; Go 1.26+
  — with the default `GOTOOLCHAIN=auto`, an older Go installs 1.26 itself on
  first build).
  Build the web bundle first — `hearthd` embeds `web/dist/` at compile time,
  and git ships only a stub of it:

  ```bash
  cd "$REPO"
  (cd web && npm ci && npm run build)   # web chat + approval-card assets
  go build -o bin/custos  ./cmd/custos
  go build -o bin/custosd ./cmd/custosd
  go build -o bin/hearthd ./cmd/hearthd
  ```

  Skipping the npm step yields a `hearthd` whose served page references a
  404ing script: the door answers, but chat and approval cards never appear.

## Declare it

Declare the connector under `custos.connectors.mcp` in `lararium.yaml`. When using `npx`, specify the absolute path to the binary and configure `list_timeout: 60s` so `npx` has sufficient time to download packages on first launch.

```yaml
hearth:
  home: /ABS/PATH/hearth

serve:
  listen: "127.0.0.1:7717"
  allowed_hosts: ["localhost", "127.0.0.1", "[::1]"]

custos:
  doors_sock: /ABS/PATH/hearth/custos/doors.sock
  door_token: /ABS/PATH/hearth/custos/door.token
  unlock_keyfile: /ABS/PATH/unlock.key
  connectors:
    mcp:
      - name: everything
        transport: stdio
        command: /ABS/PATH/npx
        args: ["-y", "@modelcontextprotocol/server-everything"]
        list_timeout: 60s

models:
  default:
    - local/your-model-id
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
```

Keep the `models:` and `providers:` blocks in place even if you have not
chosen a server yet: `setup` and `serve` both refuse a config whose
`models.default` chain is empty, so a connector-only config cannot start.
You swap in real values in “Use it in chat”.

Initialize the custody vault and start the daemons. `$REPO` is your Lararium
checkout; keep `lararium.yaml` at `$REPO/lararium.yaml` (binaries were built
into `$REPO/bin/` above). The `unlock_keyfile` above must exist *before*
`init` — it is the vault passphrase read once at spawn for headless unlock;
create it 0600:

```bash
umask 077
head -c 32 /dev/urandom | base64 > /ABS/PATH/unlock.key
```

Then initialize and start. `hearthd setup --defaults` scaffolds the persona
files (`SOUL.md` and friends) that `hearthd serve` refuses to boot without;
it leaves an existing `lararium.yaml` untouched:

```console
$ $REPO/bin/custos  -config $REPO/lararium.yaml init
$ $REPO/bin/hearthd -config $REPO/lararium.yaml setup --defaults
$ $REPO/bin/custosd -config $REPO/lararium.yaml serve   # no banner; use `custos mcp status`
$ $REPO/bin/hearthd -config $REPO/lararium.yaml serve   # prints "web chat + API on …"
```

`custosd` and `hearthd` are independent daemons; neither launches the other.
Run both in the background (or two terminals) so you can keep querying them.

## Verify the rail

Verify connector health, list discovered tools, and execute a tool directly using the `custos` CLI.

Check connector status:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml mcp status
name        state    tools  restarts  reason
everything  ready    14     1         ok
```

List tools registered by the connector:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml mcp tools everything
name                            flags       title                       inputSchema
echo                            read_only   Echo Tool                   {"$schema":"http://json-schema.org/draft-07/schema#","properties":{"message":{"description":"Message to echo","type":"string"}},"required":["message"],"type":"object"}
get-sum                         read_only   Get Sum Tool                {"$schema":"http://json-schema.org/draft-07/schema#","properties":{"a":{"description":"First number","type":"number"},"b":{"description":"Second number","type":"number"}},"required":["a","b"],"type":"object"}
gzip-file-as-resource           open_world  GZip File as Resource Tool  {"$schema":"http://json-schema.org/draft-07/schema#","properties":{"data":{"default":"https://raw.githubusercontent.com/modelcontextprotocol/servers/refs/heads/main/README.md","description":"URL or data URI of the file content to compress","format":"uri","type":"string"},"name":{"default":"README.md.gz","description":"Name of the output file","type":"string"},"outputType":{"default":"resourceLink","description":"How the resulting gzipped file should be returned. 'resourceLink' returns a link to a resource that can be read later, 'resource' returns a full resource object.","enum":["resourceLink","resource"],"type":"string"}},"type":"object"}
toggle-simulated-logging        none        Toggle Simulated Logging    {"$schema":"http://json-schema.org/draft-07/schema#","properties":{},"type":"object"}
… (14 tools in total; middle rows omitted here, output is alphabetical)
```

Rail calls run under the same custody policy as chat turns, and every unseen
tool defaults to `ask`. The CLI carries no approval prompt and surfaces no
approvable card: a gated call waits out a fixed 60-second dispatch hold (it
does not read `ask_hold_timeout` — tracked as
[#175](https://github.com/lararium-app/lararium/issues/175)), then
auto-denies and the command closes with `read response: EOF`. Approve the
connector first — `trust` writes one
connector-wide `auto` rule (`everything/*`), undone by
`custos mcp distrust everything`:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml mcp trust everything
```

The rule covers every lane: with `trust` in place, chat turns skip the
approval card for this connector’s tools too. The round-trip then succeeds:

Execute a tool call round-trip using arguments defined in the tool inputSchema:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml mcp call everything echo '{"message":"Hello from Lararium"}'
{"content":[{"text":"Echo: Hello from Lararium","type":"text"}],"structuredContent":null}
```

## Use it in chat

Chat turns route through the `models:` and `providers:` blocks you declared
in “Declare it” — replace the placeholders with your inference server’s
values, then **restart both daemons**: configuration is read only at
startup, and without the restart turns fail with an opaque `turn failed`
even though the provider itself answers fine (`curl <base_url>/models`).
A local OpenAI-compatible server on the **same machine** (loopback
`base_url`) needs no key. A provider on another host (for example a
llama.cpp box on your LAN) still needs a key value on record — loopback
endpoints are the only keyless exception. Servers like llama.cpp ignore the
header, so any placeholder works; `keys set` prompts for it in your
terminal:

```console
$ $REPO/bin/hearthd -config $REPO/lararium.yaml keys set local
```

For hosted endpoints, use a real key: add `api_key_env: OPENROUTER_API_KEY`
(or similar) to the provider block and export the variable; see the
comments in `docker/config/lararium.yaml` for the full menu.

When `hearthd serve` connects to `custosd` over `doors_sock`, tools from
active connectors join the chat turn registry automatically — this happens
on the **web chat** surface. The local REPL (`bin/hearthd` with no
subcommand) runs without the door, so rail tools never appear there.
Model-facing tool names follow the pattern:

```
mcp__<connector>__<tool>
```

For the `everything` connector, the `echo` and `get-sum` tools appear as `mcp__everything__echo` and `mcp__everything__get-sum`.

Open the URL printed by `hearthd token create <label>` (it carries the
token) and
start a chat — a fresh install has none, so use the sidebar’s new-chat
button (the API equivalent is `POST /v1/sessions`, see
[SURFACE-SPEC.md](SURFACE-SPEC.md)). Ask the model to “call
mcp__everything__echo with the message hello”.

If the reply comes back empty, the provider failed or was slow — a cold
model slot loads on first request. Confirm the server answers
(`curl <base_url>/models`) and send the turn again.

Per [MCP-SPEC.md](MCP-SPEC.md), every unseen rail tool defaults to policy verdict `ask`. Annotation flags reported by servers (such as `read_only`) do not grant automatic approval. When the model selects a rail tool that no rule auto-approves, the web interface displays an approval card naming the tool and destination; the arguments sit behind `Edit Parameters`. **Current-build caveat:** the pending call behind a card is killed 60 s after the tool call, and card resolutions have been observed to arrive too late to count (`Allow once` and the resolve API block, then the call denies at the 60 s mark — [#175](https://github.com/lararium-app/lararium/issues/175)). If `echo` comes back `denied`, that is this bug, not your setup: the working path for rail tools today is the `trust` rule above. Selecting `Always` is designed to write an `auto` rule bound to the tool schema fingerprint, so later chat turns skip the card for that tool. If a server refresh alters the tool description or schema, the fingerprint changes, the rule is revoked, and subsequent invocations revert to `ask`. A connector made fully `trust`ed earlier shows no cards at all — `custos mcp distrust` restores the per-call flow.

In the Doors view of the web UI, the "MCP connectors" card shows connector name `everything`, status pill `ready`, and total discovered tool count. The chat, its approval cards, and the HTTP API behind them (token-opened door URLs, streaming, resolving cards remotely) are specified in [SURFACE-SPEC.md](SURFACE-SPEC.md).

## Tighten it

Restrict tool visibility, secure environment variables, and manage concurrency:

```yaml
custos:
  connectors:
    mcp:
      - name: everything
        transport: stdio
        command: /ABS/PATH/npx
        args: ["-y", "@modelcontextprotocol/server-everything"]
        list_timeout: 60s
        tool_allowlist: ["echo", "get-sum"]
        max_concurrent_calls: 4
        env:
          SERVICE_KEY: ${env:EXTERNAL_SERVICE_KEY}
```

- `tool_allowlist`: Limits tools exposed to the model. Unlisted tools are excluded from registration.
- `env`: Reference host environment variables via `${env:NAME}` or stored credentials via `${vault:ID}`. Literal values in `env` trigger boot audit warnings. Export any `${env:NAME}` variable *before* `custosd` starts — an unset one lands the connector `disabled` with `credential_unavailable`.
- `max_concurrent_calls`: Bounds simultaneous tool executions between 1 and 16 (default 4).
- Configuration reload: `lararium.yaml` changes take effect on daemon restart. `custos mcp refresh <name>` re-reads tool catalogs from running servers without reloading daemon configuration.

## When it does not start

If a connector fails to reach `ready` state, inspect `custos mcp status` and match the reported reason:

| State | Reason | Cause | Remedy |
|---|---|---|---|
| (daemon exits at boot) | config error `bad_command` | `command` names a nonexistent or non-executable path — custosd refuses to start at all | Fix the path in `lararium.yaml`; a bad binary is treated as a config fault, not retried |
| `backoff`, then `disabled` after the restart budget | `spawn_failed` | Binary exists but the process fails on launch | Verify `command` is an absolute path to a runnable program; check its args and stderr |
| `backoff` | `list_timeout` | `tools/list` exceeded deadline during initial startup | Increase `list_timeout` in `lararium.yaml` to accommodate package download or process startup |
| Web Doors view says “No MCP connectors configured” despite a correct `custos.connectors` block | door down, not config | That view reflects the live hearthd↔custosd door, not the file — it empties whenever custosd is unreachable | Confirm custosd is up (its log ends at `listening unix=`), then reload the page |
| `disabled` | `credential_unavailable` | A referenced `${env:NAME}` was unset when the daemon started, or a `${vault:...}` credential was not found or the vault is locked | Export the variable or unlock the vault via `custos unlock` (or configure `custos.unlock_keyfile`), then restart the daemon |
| `disabled` | `name_occupied` | Connector name matches a credential name in the vault or a reserved namespace | Rename the connector in `lararium.yaml` to an unreserved identifier |

`custos mcp status` or `mcp call` failing with `read response: EOF` has two common causes: (1) the connector holds its initialization lock across `tools/list` while `npx` performs a cold package download, which can outlast the 5-second CLI socket deadline — let the download finish before querying; (2) the call hit the `ask` gate — the CLI has no way to approve, so it waits out the 60-second dispatch hold and closes; run the `trust` step above (approval cards from chat turns cannot be resolved in time in the current build, [#175](https://github.com/lararium-app/lararium/issues/175)).
