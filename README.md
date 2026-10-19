<div align="center">

# Lararium

*/ lārārium / — the household shrine where the protective spirits were kept*

**A personal AI agent that lives at your hearth and rides in your pocket.**
Your machine, your models, your keys.

[![CI](https://github.com/lararium-app/lararium/actions/workflows/ci.yml/badge.svg)](https://github.com/lararium-app/lararium/actions/workflows/ci.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)

[Website](https://lararium.io) · [Docs](docs/) · [Security](SECURITY.md) · [Contributing](CONTRIBUTING.md)

*`hearthd serve` — the browser door. One command, one token, one real conversation.*

</div>

---

Lararium is an open-source, self-hosted AI agent daemon (`hearthd`). It runs on hardware you own,
remembers everything in plain files you can read and edit, points at model providers you choose,
and reaches you on the screens you already carry.

## The covenant

- **Your fire** — bring your own llama.cpp on your own silicon, or an API key for any major
  provider. No lock-in, no hidden routing.
- **Your memory** — everything the agent knows lives in plain text files
  ([Penatus](docs/PENATUS-SPEC.md)). Export your whole life with one `tar`.
- **Your keys** — credentials are vaulted and never shown to the model; the network layer injects
  them, policy-checked, on the way out. *(Planned.)*
- **Sandboxed work** — agent work executes inside cells ([CELL-SPEC](docs/CELL-SPEC.md)):
  systemd-nspawn + cgroup2 + overlayfs, fail-closed networking, hard resource caps,
  snapshot/rollback. The threat model assumes prompt injection *succeeds*; the cage makes
  misbehavior physically expensive below the model.

## Status: public preview (v0.10.1-alpha)

> [!WARNING]
> Lararium is **pre-1.0 alpha**. The file formats are specified and frozen
> per-component; the wire APIs may still change between releases. Run it on
> machines you can wipe. Not fit for production or multi-tenant use.

## What works today

- **`hearthd` REPL** — append-only event log, context compaction, and memory tools.
- **Model routing** — fallback chains across any OpenAI-compatible endpoint: llama.cpp, Ollama,
  vLLM, OpenRouter, OpenAI, Anthropic-compat.
- **Penatus memory** — plain-file memory, fully portable.
- **`hearthd serve`** — HTTP/SSE API plus built-in web chat with streaming and clickable
  tool approvals.
- **Telegram (`nuntius`)** — one-owner bridge with pairing codes, streaming replies, and approval
  cards that resolve from the phone.
- **Cell sandbox** — proven by a C1–C12 containment suite on five hosts across two architectures.
- **Credential vault (`custos`)** — `custosd` holds OAuth credentials sealed at rest; a Gmail
  connector sends through a policy gate (auto / ask / deny) over an isolated worker lane the agent
  can never read from, with an append-only audit chain. Verified live end-to-end: consent →
  sealed refresh token → policy-checked send → audit line.
- **Standalone approval door** — `custos approvals` lists pending cards and answers them Allow-once
  or Deny from the owner's terminal, so `ask` verdicts are decidable with no `hearthd` running.
- **Card fan-out** — `hearthd` connects to `custosd` as a door client, so one custody approval
  card renders live on the web surface and on Telegram at once. Answering from either surface
  settles the parked request and every sibling render, all recorded in the same audit chain
  (`via` names the answering surface). Verified live end-to-end on real Telegram.
  See [CUSTOS-SPEC.md](docs/CUSTOS-SPEC.md).

## What's coming

Other messaging channels (Signal, …), voice I/O, and `custosd` in the Docker quick start are not
built yet. [ARCHITECTURE.md](docs/ARCHITECTURE.md) maps the whole shape.

## Platforms

| Platform | Daemon | Sandbox |
|---|---|---|
| Linux (native) | ✅ | ✅ full cage — the reference target |
| WSL2 (Windows 11) | ✅ | ✅ full cage — enable systemd, one package line |
| Windows / macOS native | ✅ compiles clean | ❌ cage needs a Linux kernel (or hypervisor backend, planned) — Docker door meanwhile |

The cage is built on kernel features (`systemd-nspawn`, nftables, overlayfs, cgroup2), not
distro-specific glue. It has run green on x86-64 and arm64, three distro families, ext4 and btrfs,
bare metal and WSL2.

On Windows, point it at WSL2: set `[boot] systemd=true` in `/etc/wsl.conf`, install
`uidmap systemd-container nftables acl debootstrap`, and the cell stack behaves exactly as on
bare metal.

## Quick start

**Bring your own model.** Lararium talks to any OpenAI-compatible endpoint — a llama.cpp / Ollama
/ vLLM / LM Studio server on your own machine, or a hosted key from OpenRouter, OpenAI,
Anthropic, &c. There is no default provider and no account of ours in the loop.

### Run it (Docker pull)

```bash
docker run -d --name lararium \
  -p 127.0.0.1:7717:7717 \
  -v lararium-hearth:/hearth \
  -v lararium-config:/etc/lararium \
  ghcr.io/lararium-app/hearthd:<version>
```

First run seeds the persona home and reference config into those volumes, and opens the setup
wizard if you attach a TTY (`docker run -it …` once) while no provider is configured; image
tags and the version/arch matrix live on the [releases page](https://github.com/lararium-app/lararium/releases).

### Run it (from source)

The supported way to run the alpha is Docker Compose:

```bash
git clone https://github.com/lararium-app/lararium.git
cd lararium/docker
docker compose up --build
```

The reference config points at a model server on **your** machine
(`http://host.docker.internal:8080/v1`) — serve any model there with llama.cpp (or point the
reference config at yourself) and it just works, key-free. To use a hosted endpoint, uncomment
one provider block in `docker/config/lararium.yaml`, export its key, and pass it through:

```bash
OPENAI_API_KEY=*** docker compose up --build
```

Keys come from your environment only — nothing secret is baked into the image or committed to
this repo.

Compose serves the web chat on `http://127.0.0.1:7717`. Open a door to it:

```bash
docker compose exec hearthd hearthd token create me
```

The command prints a ready URL to open in a browser (shown once).

### Bare host (no Docker)

```bash
go build -o hearthd ./cmd/hearthd
./hearthd setup --defaults
./hearthd setup
./hearthd serve
```
This creates `lararium.yaml` in the working directory and seeds the persona home at `~/.lararium` with `SOUL.md`, `IDENTITY.md`, and `USER.md` templates.

### Choosing a provider

Pick the model endpoint at any time with the setup wizard — hosted keys
(OpenRouter, OpenAI, Anthropic, &c.), a custom OpenAI-compatible base URL, or scan for a
local server:

```bash
docker compose exec -it hearthd hearthd setup
```

It writes to `config/lararium.yaml` and is safe to re-run. The image entrypoint launches the
same wizard on a first run when the container is attached to a terminal; `docker compose up`
does not allocate one, so on a fresh compose install the command above is how you configure
the provider.

### Reaching the chat from another machine

By default the door is published on `127.0.0.1:7717` **only** — it is not reachable from
other devices on purpose. To expose it on your tailnet or LAN, change the port mapping in
`docker-compose.yml` from `127.0.0.1:7717:7717` to `<host-ip>:7717:7717` (or drop the
`127.0.0.1:` prefix for all interfaces, not recommended outside a tailnet), then **tell the
Host gate the address you'll use**. The gate rejects any request whose `Host` header isn't
loopback or in the allow-list — a DNS-rebinding defense — and returns `403 {"error":"forbidden"}`.
Add that address to `serve.allowed_hosts` in `config/lararium.yaml`:

```yaml
serve:
  listen: "0.0.0.0:7717"
  allowed_hosts: ["localhost", "127.0.0.1", "[::1]", "100.121.44.7"]  # + your reach-in address
```

Then `docker compose restart` and open `http://<host-ip>:7717/#<token>`. The scheme is
plain `http` (TLS is your reverse proxy's job); a ready URL from `token create` prints with
`http://`. If you still see `{"error":"forbidden"}`, the `Host` you're typing isn't in
`allowed_hosts` yet.

Build from source with Go 1.26+ (`go build ./cmd/hearthd && go test
./...`); run `./hearthd` for the REPL or `./hearthd serve` for the web
door. Native Linux/WSL2 installs additionally get the `cell` sandbox —
see [CELL-SPEC.md](docs/CELL-SPEC.md) for the host requirements and
`cell doctor` to check yours.

### Connecting an MCP server

MCP is Lararium's connector standard: any [Model Context Protocol](https://modelcontextprotocol.io) server — local command or remote URL — becomes tools your agent can reach, and every one of those tools passes the same policy gate (auto / ask / deny) as everything else. Declare servers under the `custos:` block (custosd reads them; hearthd ignores them, so one shared `lararium.yaml` can serve both daemons):

```yaml
custos:
  connectors:
    mcp:
      - name: filesystem          # stdio: custos spawns and supervises it
        transport: stdio
        command: /usr/local/bin/mcp-server-filesystem  # absolute path, validated at load
        args: ["/srv/docs"]
        env:
          LOG_LEVEL: "warn"       # literal values are audit-warned; prefer ${env:NAME}
      - name: search
        transport: http           # custos connects; nothing is spawned
        url: "http://10.0.0.12:8322/mcp"   # plain http only on LAN/tailnet hosts; use https in the clear
        bearer: "${vault:search-key}"   # or ${env:SEARCH_KEY}; resolved lazily at
                                        # connect time — the value renders nowhere
        tool_allowlist: ["web_search"]  # optional; denylist wins over allowlist
        list_timeout: 30s
```

Secrets only ever appear as references (`${env:NAME}`, `${vault:ID}`). Resolved values join the scrub set: they are stripped from tool results, error lines, and audit records — which carry key *names*, never values.

Connectors start when the vault unlocks and supervise themselves: a flapping server retries with doubling backoff, an unknown server never wedges the daemon, and revoking a credential kills the connector's whole process group before the socket replies. Watch and drive it from the CLI:

```console
$ custos mcp status
name         state    tools  restarts  reason
filesystem   ready    14     0         ok
search       backoff  0      2         transport
$ custos mcp tools filesystem
$ custos mcp refresh search
```

A standing approval on a connector tool is bound to the tool's definition fingerprint: if the server edits the tool's schema or description, the approval is revoked (audited) and the next call asks again. Full contract: [`docs/MCP-SPEC.md`](docs/MCP-SPEC.md).

## Documentation

| Document | What it freezes |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Component map, data flow, design principles |
| [PENATUS-SPEC.md](docs/PENATUS-SPEC.md) | The on-disk memory formats |
| [CELL-SPEC.md](docs/CELL-SPEC.md) | The sandbox contract (nspawn + cgroup2 + nftables) |
| [SURFACE-SPEC.md](docs/SURFACE-SPEC.md) | The HTTP/SSE API and web chat security model |
| [UPGRADE.md](docs/UPGRADE.md) | One-command upgrade and rollback procedures |

## What Lararium is *not*

- **Not a cloud service.** There is no hosted backend; if a repo or site claims to be
  "official Lararium cloud", it isn't us.
- **Not a multi-tenant platform.** One household, one owner. The security model is
  single-trust-domain by design.
- **Not a model zoo.** Lararium orchestrates inference; it does not ship, train, or endorse models.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and feature proposals go through the
[issue templates](https://github.com/lararium-app/lararium/issues/new/choose).
By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Lararium is licensed under the **GNU AGPL-3.0-only** — see [LICENSE](LICENSE).

In short: you may run, study, modify, and share Lararium freely, including commercially *as
self-hosted software*. If you run a modified version as a network service, the AGPL requires you
to offer its source to your users.

**Commercial licensing:** entities that want to embed Lararium in closed-source products or
appliances may obtain a proprietary license from the project. Self-hosting, personal use, and
open-source contributions are not affected — this clause exists to keep the project funded,
not to gate the community.

© 2026 The Lararium authors.
