# Lararium Installation Guide

Everything you need to install Lararium, wire up model API keys, connectors,
and MCP servers — and the handful of things people usually get wrong. Deep
contracts live in the linked spec docs; this guide is the router.

---

## 1. Decide how to run it

| Method | Best for |
|---|---|
| **Docker image (recommended)** | Fastest start; amd64 + arm64; state in named volumes |
| **Docker Compose from source** | Editing the reference config / persona files alongside the repo checkout |
| **Bare host binary** | Native Linux/WSL2 with full sandbox; builds from Go source |

Requirements:

- **Model endpoint:** Lararium ships no default provider. Any OpenAI-compatible
  endpoint works (llama.cpp, Ollama, vLLM, LM Studio on your machine; or an
  API key for OpenRouter, OpenAI, Anthropic, Novita).
- **Sandbox (the "cage"):** needs Linux kernel features — systemd-nspawn,
  nftables, overlayfs, cgroup2. Full cage on native Linux and WSL2 (systemd
  enabled); the web door also works on Windows/macOS without the cage.
  Tested on x86-64 and arm64.
- **Source builds:** Go 1.26+.

---

## 2. Install — Docker image (recommended)

Tags are on the releases page; `<version>` below means a pinned tag like
`v1.0.0-beta3` (avoid `:latest` during beta).

1. Start the container. It publishes the web chat + API on your machine's
   loopback only:

   ```bash
   docker run -d --name lararium \
     -p 127.0.0.1:7717:7717 \
     -v lararium-hearth:/hearth \
     -v lararium-config:/etc/lararium \
     ghcr.io/lararium-app/hearthd:<version>
   ```

   First run seeds the persona home (`/hearth`) and the reference config
   (`/etc/lararium/lararium.yaml`) into the volumes.

2. Point it at a model. If no provider is configured, a detached start
   prints guidance and exits 2 rather than hang. Run the setup wizard once
   with a TTY (same image, same volumes, so the wizard's output persists):

   ```bash
   docker run -it --rm \
     -v lararium-hearth:/hearth \
     -v lararium-config:/etc/lararium \
     ghcr.io/lararium-app/hearthd:<version> setup
   ```

   Skip this if you run a local OpenAI-compatible server: the seeded
   reference config already points at `http://host.docker.internal:8080/v1`
   (serve a model there and it just works, key-free).

3. Start (or restart) the container:

   ```bash
   docker start lararium
   ```

4. Create a door token — this prints a ready URL, **shown once**:

   ```bash
   docker exec lararium hearthd token create me
   ```

5. Open the printed URL (`http://127.0.0.1:7717/#<token>`) in a browser on
   the same machine. Verify the daemon answers:

   ```bash
   curl -s -H "Host: localhost" http://127.0.0.1:7717/v1/health
   ```

   Expected: `{"ok":true,"version":"<version>"}`.

**Where state lives:** `/hearth` (persona files, sessions, `keys.json`,
door tokens, vault) and `/etc/lararium` (`lararium.yaml`) — the named
volumes above. Back them up with `hearthd backup create`
([RECOVERY.md](RECOVERY.md)). `custosd` is **not** part of the Docker quick
start yet; use a bare-host install for connectors/MCP for now.

**Note:** inside the container the daemon binds `0.0.0.0` and logs
"listening beyond loopback" at startup. That is expected — the container has
its own network namespace, and host exposure stays loopback-only via the
`-p 127.0.0.1:7717:7717` mapping.

---

## 3. Install — Docker Compose from source

1. Clone, hand the mounted directories to the daemon's user, and start:

   ```bash
   git clone https://github.com/lararium-app/lararium.git
   cd lararium/docker
   sudo chown -R "$(id -u):$(id -g)" config hearth   # the daemon writes only here
   docker compose up --build
   ```

   The container runs unprivileged with a read-only rootfs (set
   `LARARIUM_UID` to override the uid). Attached to a TTY, an unconfigured
   container runs the setup wizard first; detached starts (`-d`) use the
   bundled defaults.

2. For hosted keys, run the wizard explicitly:

   ```bash
   docker compose exec -it hearthd hearthd setup
   ```

3. Create a door token:

   ```bash
   docker compose exec hearthd hearthd token create me
   ```

4. Open `http://127.0.0.1:7717/#<token>`.

State lives in the checkout: `docker/config/` is mounted at
`/etc/lararium` and `docker/hearth/` at `/hearth`, so config and persona
files stay editable on the host. Provider keys come from your shell
environment (`.env`-style pass-through is wired for `OPENROUTER_API_KEY`,
`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `NOVITA_API_KEY`); nothing secret is
baked into the image.

To prefer a hosted endpoint: uncomment a provider block in
`docker/config/lararium.yaml`, then pass the key when starting:
`OPENAI_API_KEY=*** docker compose up --build`.

---

## 4. Install — bare host from source

1. Build the daemon (Go 1.26+):

   ```bash
   go build -o hearthd ./cmd/hearthd
   ```

2. Run the interactive setup wizard (writes providers + a starter
   `lararium.yaml`, and can seed the persona home):

   ```bash
   ./hearthd setup
   ```

3. Start the web door:

   ```bash
   ./hearthd serve
   ```

4. In another terminal, mint your door token (shown once) and open the
   printed URL:

   ```bash
   ./hearthd token create me
   ```

5. Optional: the terminal REPL — `./hearthd` with no subcommand.

6. Optional but recommended on Linux/WSL2 — check the sandbox host:

   ```bash
   go build -o cell ./cmd/cell
   ./cell doctor
   ```

To reach the door from another device (phone pairing included), follow
[REMOTE-ACCESS.md](REMOTE-ACCESS.md) — by default the daemon binds
`127.0.0.1` and refuses non-localhost `Host` headers.

---

## 5. Model API keys — add, change, remove

Keys can live in **three places**. Precedence is fixed:

1. **Environment variable** (`api_key_env: NAME` in the provider block) — wins.
2. **`keys.json`** under the hearth home — managed by `hearthd keys`.
3. **Literal `api_key:` in the config file** — last resort; the loader
   warns you off it.

A provider entry looks like this in `lararium.yaml` (keys in the config are
`name`, `base_url`, `api_key`, `api_key_env`):

```yaml
providers:
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
```

### Add a key

```bash
hearthd keys set <provider>
```

The prompt reads the key with hidden input (or one piped stdin line in
scripts — the value never touches argv, logs, or output). With a running
daemon the change applies immediately; without one you get
"no daemon running — will apply at next start" and the write still
succeeded. In Docker:
`docker exec -it lararium hearthd keys set <provider>`.

### Change a key

Run `hearthd keys set <provider>` again — overwriting an existing name is
always allowed:

```bash
hearthd keys set <provider>
```

Or remove first, then set:

```bash
hearthd keys rm <provider>
hearthd keys set <provider>
```

Caveat: if an environment variable also provides this provider's key, the
env var **shadows** `keys.json` — `hearthd keys list` warns
"keys.json entry shadowed by api_key_env". Unset the env var (or fix it) or
your edits won't take effect.

### Remove a key

```bash
hearthd keys rm <provider>
```

Reports `not set` if the name was absent.

### Verify

```bash
hearthd keys list
```

One line per provider: name, masked key (`••••` + last 4 chars only — the
value itself never prints), and the winning source (`env`, `keys.json`,
`config`, or `missing`). Warnings flag shadowed entries and literal config
keys. Note: there is no `hearthd models`/`providers` subcommand in beta —
`keys list` is the status view.

**Storage facts** (from source): keys live in `<hearth.home>/keys.json`,
mode `0600`, serialized under a `flock` lockfile shared with the daemon;
a socket ping reloads a running daemon. The store caps at 64 providers; a
full store refuses new names *before* prompting you to type a secret.

---

## 6. Connectors — custody vault credentials (e.g. Gmail)

`custos` is the CLI and `custosd` the daemon for the custody vault: OAuth
credentials sealed at rest, policy gate, append-only audit chain. They read
the same `lararium.yaml` (the `custos:` block). `custosd` and `hearthd` are
independent daemons; neither launches the other.

Build them alongside `hearthd`:

```bash
go build -o custos ./cmd/custos
go build -o custosd ./cmd/custosd
```

### Set up

1. Initialize the vault — sets the **passphrase** (hidden input; it is what
   seals everything in the vault):

   ```bash
   custos -config ./lararium.yaml init
   ```

2. Start the daemon (the vault **starts locked on every boot**):

   ```bash
   custosd -config ./lararium.yaml serve
   ```

3. Unlock it (connectors wait for this — while locked, `${vault:…}`
   references resolve to `credential_unavailable` and connectors stay
   `disabled`):

   ```bash
   custos -config ./lararium.yaml unlock
   ```

4. Add a credential via OAuth (currently Gmail). The client secret is
   prompted with hidden input, never a flag; the consent URL is printed;
   on success an `oauth2` credential is stored:

   ```bash
   custos -config ./lararium.yaml login gmail --client-id <id>
   ```

   Headless host? Add `--manual` to paste the redirect URL/code back.
   `--cred <name>` stores it under a custom credential name.

5. Check vault state:

   ```bash
   custos -config ./lararium.yaml status
   ```

### Gate what agents may do

Policy rules carry verdicts `auto`, `ask`, or `deny`:

```bash
custos -config ./lararium.yaml policy add <pattern> <verdict>
custos -config ./lararium.yaml policy list
custos -config ./lararium.yaml policy rm [<lane>] <pattern>
custos -config ./lararium.yaml policy reset
```

`policy add` takes `[<lane>] <pattern> <verdict>` — the lane
(`credential`/`egress`) is inferred from the pattern when omitted.

When a rule says `ask`, a card appears in the web UI (and on Telegram /
the companion app); from the CLI:

```bash
custos -config ./lararium.yaml approvals list
custos -config ./lararium.yaml approvals watch
custos -config ./lararium.yaml approvals approve <id>
custos -config ./lararium.yaml approvals deny <id>
```

`approvals watch` is interactive: `a <id>`, `d <id>`, `q` to quit. Held
requests park for `custos.ask_hold_timeout` (default 330s), then time out.
Answering on any surface settles it everywhere.

### Revoke and rotate

```bash
custos -config ./lararium.yaml revoke <credential-name>
custos -config ./lararium.yaml change-passphrase
```

`change-passphrase` prompts for the current then the new passphrase and
works while the daemon runs. Revoking a connector credential also kills its
supervised process group.

### Lock, snapshots, audit

```bash
custos -config ./lararium.yaml lock
custos -config ./lararium.yaml snapshots list
custos -config ./lararium.yaml restore <gen> --yes
custos -config ./lararium.yaml audit verify
```

`custosd` itself answers `status` and `shutdown`
(`custosd [serve|status|shutdown]`). For a hands-free unlock at boot, set
`custos.unlock_keyfile: <path>` — but note keyfile mode is *always
unlocked*, and `custos lock` refuses in that mode.

Full contract: [CUSTOS-SPEC.md](CUSTOS-SPEC.md).

---

## 7. MCP servers — add, change, remove

Any Model Context Protocol server becomes policy-gated tools. Declare
servers under `custos.connectors.mcp` in `lararium.yaml` — `custosd` reads
them, `hearthd` ignores them, so one shared file works for both:

```yaml
custos:
  connectors:
    mcp:
      - name: filesystem          # stdio: custos spawns and supervises it
        transport: stdio
        command: /usr/local/bin/mcp-server-filesystem
        args: ["/srv/docs"]
        env:
          LOG_LEVEL: "warn"       # literal values are audit-warned; prefer ${env:NAME}
      - name: search
        transport: http           # custos connects; nothing is spawned
        url: "http://mcp.example.com:8322/mcp"
        bearer: "${vault:search-key}"   # or ${env:SEARCH_KEY}; resolved lazily
        tool_allowlist: ["web_search"]  # optional; denylist wins over allowlist
        list_timeout: 30s
```

Fields per server (all from the config struct): `name`, `transport`
(`stdio` | `http`), `command`, `args`, `env`, `url`, `bearer`,
`tool_allowlist`, `tool_denylist`, `max_concurrent_calls` (1–16, default
4), `list_timeout`.

**Secrets only as references:** `${env:NAME}` or `${vault:ID}`. Literal
values in `env` trigger boot audit warnings; resolved values are stripped
from tool results, error lines, and audit records.

### Add

1. Add the block above to `lararium.yaml` (for `npx`-based servers, give
   the absolute path to the binary and set `list_timeout: 60s` so the first
   download fits).
2. Restart `custosd` — config changes apply on daemon restart:

   ```bash
   custosd -config ./lararium.yaml serve
   ```

3. Unlock the vault first if any `${vault:…}` reference is used (§6), then
   verify the rail:

   ```bash
   custos -config ./lararium.yaml mcp status
   custos -config ./lararium.yaml mcp tools <name>
   custos -config ./lararium.yaml mcp call <name> <tool> '<json-args>'
   ```

   Example: `custos -config ./lararium.yaml mcp call everything echo '{"message":"Hello from Lararium"}'`.

4. In chat, tools appear as `mcp__<connector>__<tool>` and default to
   policy verdict `ask` — every unseen rail tool asks, regardless of any
   `read_only` annotation the server reports.

### Change

Edit the block, restart `custosd`, re-check `mcp status`. If a running
server's tool schema changes (or you edit `tool_allowlist`), standing
"Always" approvals are bound to the tool's definition fingerprint — the
fingerprint changes, the approval is revoked, and the next call asks again.
To re-read a tool catalog from a *running* server without a config reload:

```bash
custos -config ./lararium.yaml mcp refresh <name>
```

### Remove

Delete the server's block from `lararium.yaml` and restart `custosd`.

### Trust an entire connector (optional)

To stop approval cards for one connector — all of its current *and future*
tools run without asking — mint a connector-wide rule:

```bash
custos -config ./lararium.yaml mcp trust <name>
```

This writes a credential-lane rule `<name>/* = auto`. Undo it with:

```bash
custos -config ./lararium.yaml mcp distrust <name>
```

Individual per-tool Always approvals still work when you want tighter
scopes; `distrust` removes only the connector-wide rule, leaving those in
place. Denylist rules always win over a connector-wide auto.

### When it does not start

Match the `reason` column of `custos mcp status`:

| State | Reason | Remedy |
|---|---|---|
| `backoff` | `spawn_failed` | `command` must be an absolute path to an executable the daemon can run |
| `backoff` | `list_timeout` | Raise `list_timeout` (cold `npx` downloads are slow) |
| `disabled` | `credential_unavailable` | `custos unlock`, or configure `custos.unlock_keyfile` |
| `disabled` | `name_occupied` | Rename the connector (collides with a vault credential name) |

Docker note: `custosd` spawns stdio children *inside* the container, so the
`command` binary must exist in the image — prefer the `http` transport on
Docker. Full contract: [MCP-SPEC.md](MCP-SPEC.md); walkthrough:
[MCP-SERVER-RECIPE.md](MCP-SERVER-RECIPE.md).

---

## 8. Tips & gotchas

1. **Four different secrets, don't conflate them.** Door token
   (`lar1_…`): bearer credential for the web door, printed **once** at
   creation and stored under the hearth home (`tokens.json`). Model API
   key: in `keys.json`/env, replaceable anytime (§5). Vault passphrase:
   chosen at `custos init`, seals credentials; rotate with
   `custos change-passphrase`. Vault unlock: a runtime state, not a
   secret — you re-enter the passphrase after every restart.
2. **Door tokens are shown once.** Lost it? There is no reveal command:
   revoke the old label with `hearthd token revoke <label>` and mint a
   replacement with `hearthd token create <label>`. Rotate the same way
   whenever a client is lost.
3. **403 when opening from another device?** That is the Host gate, not a
   crash. Non-loopback binds refuse to start without
   `serve.allowed_hosts`, and every request's `Host` header must appear in
   it — add the hostname/IP you actually type in the browser (with port if
   present). See [REMOTE-ACCESS.md](REMOTE-ACCESS.md).
4. **The vault relocks on every daemon restart** (and after crashes).
   Connectors stay `disabled` with `credential_unavailable` until
   `custos unlock` runs again.
5. **Env vars shadow `keys.json`.** If `api_key_env: FOO` is set in config
   *and* FOO is exported, editing keys via `hearthd keys set` changes a
   copy that never gets used. `hearthd keys list` prints the winning source
   and warns on shadows — read that column before debugging anything else.
6. **`hearthd keys set` works while the daemon is down.** The write is the
   contract; a running daemon reloads via the control socket, a stopped one
   picks it up at next start. Both cases are success.
7. **Approval cards time out.** Web-turn approvals hold
   `serve.approval_timeout` (default 5m); vault-side `ask` requests hold
   `custos.ask_hold_timeout` (default 330s). If your answer arrives later,
   the request already expired — answer from the phone faster, or widen the
   timeout in `lararium.yaml`.
8. **MCP config changes need a daemon restart.** `lararium.yaml` is read at
   startup; `custos mcp refresh <name>` only re-reads tool catalogs from
   running servers. Editing an `env` value does nothing until restart.
9. **`custos mcp status` can print `read response: EOF` right after startup**
   while `npx` is doing its first cold download. Wait for the download to
   finish, then ask again.
10. **Docker quick start has no `custosd`** — for vault-gated connectors
    today, run bare-host (§6–7).
11. **Phone/pairing requires reachability.** A door token is useless if the
    phone can't reach the server. Default bind is `127.0.0.1` — put both on
    the same LAN or tailnet per [REMOTE-ACCESS.md](REMOTE-ACCESS.md).
12. **Backup before upgrading.** `hearthd backup create --out <file>` then
    `hearthd backup verify <file>`; the bundle contains material that
    unlocks your agent — store it encrypted, off the machine. Upgrade and
    rollback: [UPGRADE.md](UPGRADE.md); restore drill:
    [RECOVERY.md](RECOVERY.md).
13. **Telegram pairing lives outside the door token.**
    `hearthd pair create` prints the pairing code (then send it to the
    bot); `hearthd pair status` lists owners. It does not need a daemon
    restart to apply.
14. **Literal keys in `lararium.yaml` are loudly discouraged.** The config
    loader warns ("prefer 'hearthd keys set …'") and MCP `env` literals get
    audit-warned. Use `api_key_env` + `hearthd keys set`, `${env:NAME}`,
    `${vault:ID}`.

---

## 9. Where to go next

- Upgrades & rollback: [UPGRADE.md](UPGRADE.md)
- Backup & restore drill: [RECOVERY.md](RECOVERY.md)
- Remote access (tailnet, TLS proxy, SSH tunnel):
  [REMOTE-ACCESS.md](REMOTE-ACCESS.md)
- MCP walkthrough: [MCP-SERVER-RECIPE.md](MCP-SERVER-RECIPE.md)
- Contracts: [CUSTOS-SPEC.md](CUSTOS-SPEC.md) ·
  [MCP-SPEC.md](MCP-SPEC.md) · [SURFACE-SPEC.md](SURFACE-SPEC.md) ·
  [CELL-SPEC.md](CELL-SPEC.md) · [KEYS-SPEC.md](KEYS-SPEC.md) ·
  [ARCHITECTURE.md](ARCHITECTURE.md)

---

*Every command in this guide was checked against the CLI usage strings and
sources in this repo (`cmd/hearthd`, `cmd/custos`, `cmd/custosd`, `cmd/cell`,
`docker/`, `internal/custos`, `internal/surface`, `internal/keystore`), and
every config key against its `yaml:"…"` struct tag. If something here
disagrees with the binaries you ran, the guide is wrong — open an issue.*
