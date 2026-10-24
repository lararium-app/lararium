# Connecting Your First MCP Server

This walkthrough connects a Model Context Protocol (MCP) server to Lararium over standard I/O, verifies execution from the command line, and exposes the tools to model chat under the custody policy gate. Completing the steps takes approximately 5 minutes once Node.js is installed.

## Prerequisites

- Node.js and `npx` installed on the host.
- **This is a bare-host walkthrough.** The Docker quick-start ships `hearthd`
  only — `custosd` is not part of that image and does not run in that stack,
  so MCP connectors cannot work there. Build the binaries per README →
  Quick start ("Bare host"). (Running `custosd` in its own container is
  possible: `command` binaries must then exist *inside that* image — prefer
  the HTTP transport or a custom image with Node.js installed.)

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
```

Initialize the custody vault and start the daemons. Below, `$REPO` is your Lararium checkout; build the `custos`, `custosd`, and `hearthd` binaries into `$REPO/bin/` and keep `lararium.yaml` at `$REPO/lararium.yaml`:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml init
$ $REPO/bin/custosd -config $REPO/lararium.yaml serve
$ $REPO/bin/hearthd -config $REPO/lararium.yaml serve
```

`custosd` and `hearthd` are independent daemons; neither launches the other.

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
… (14 tools in total; 10 rows omitted)
```

Execute a tool call round-trip using arguments defined in the tool inputSchema:

```console
$ $REPO/bin/custos -config $REPO/lararium.yaml mcp call everything echo '{"message":"Hello from Lararium"}'
{"content":[{"text":"Echo: Hello from Lararium","type":"text"}],"structuredContent":null}
```

## Use it in chat

To route chat turns to an LLM provider, configure the `models:` and `providers:` blocks described in [README.md](../README.md#quick-start).

When `hearthd` connects to `custosd` over `doors_sock`, tools from active connectors join the chat turn registry automatically. Model-facing tool names follow the pattern:

```
mcp__<connector>__<tool>
```

For the `everything` connector, the `echo` and `get-sum` tools appear as `mcp__everything__echo` and `mcp__everything__get-sum`.

Per [MCP-SPEC.md](MCP-SPEC.md), every unseen rail tool defaults to policy verdict `ask`. Annotation flags reported by servers (such as `read_only`) do not grant automatic approval. When the model selects a rail tool, the web interface displays an approval card showing the tool name and arguments. Selecting `Always` writes an `auto` rule bound to the tool schema fingerprint. If a server refresh alters the tool description or schema, the fingerprint changes, the rule is revoked, and subsequent invocations revert to `ask`.

In the Doors view of the web UI, the "MCP connectors" card shows connector name `everything`, status pill `ready`, and total discovered tool count.

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
        tool_allowlist: ["echo", "get-sum"]
        max_concurrent_calls: 4
        env:
          SERVICE_KEY: ${env:EXTERNAL_SERVICE_KEY}
```

- `tool_allowlist`: Limits tools exposed to the model. Unlisted tools are excluded from registration.
- `env`: Reference host environment variables via `${env:NAME}` or stored credentials via `${vault:ID}`. Literal values in `env` trigger boot audit warnings.
- `max_concurrent_calls`: Bounds simultaneous tool executions between 1 and 16 (default 4).
- Configuration reload: `lararium.yaml` changes take effect on daemon restart. `custos mcp refresh <name>` re-reads tool catalogs from running servers without reloading daemon configuration.

## When it does not start

If a connector fails to reach `ready` state, inspect `custos mcp status` and match the reported reason:

| State | Reason | Cause | Remedy |
|---|---|---|---|
| `backoff` | `spawn_failed` | Command binary missing, unexecutable, or failed on launch | Verify `command` is an absolute path to an executable file accessible to the process |
| `backoff` | `list_timeout` | `tools/list` exceeded deadline during initial startup | Increase `list_timeout` in `lararium.yaml` to accommodate package download or process startup |
| `disabled` | `credential_unavailable` | Referenced `${vault:...}` credential not found or vault is locked | Unlock the vault via `custos unlock` or configure `custos.unlock_keyfile` |
| `disabled` | `name_occupied` | Connector name matches a credential name in the vault or a reserved namespace | Rename the connector in `lararium.yaml` to an unreserved identifier |

Running `custos mcp status` immediately after startup while `npx` performs an initial cold download can fail with `read response: EOF`. During initial startup, the connector holds its initialization lock across `tools/list`, which can outlast the 5-second CLI socket deadline. Allow the download to finish before querying status.
