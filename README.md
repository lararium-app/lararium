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

Lararium is an open-source, self-hosted AI agent daemon (`hearthd`). It runs on
hardware you own, remembers everything in plain files you can read and edit,
points at model providers you choose, and reaches you on the screens you
already carry.

## Why Lararium

- **BYO model** — point at llama.cpp on your own silicon, or an API key for
  Ollama, vLLM, OpenRouter, OpenAI, Anthropic, LM Studio. No lock-in, no
  hidden routing, no account of ours in the loop.
- **Plain-file memory** — everything the agent knows lives in text files
  ([Penatus](docs/PENATUS-SPEC.md)). Export your whole life with one `tar`.
- **Vaulted keys** — credentials are sealed at rest in the custody vault and
  never shown to the model; the network layer injects them, policy-checked, on
  the way out ([CUSTOS-SPEC](docs/CUSTOS-SPEC.md)).
- **Kernel-grade sandbox** — agent work executes inside cells
  ([CELL-SPEC](docs/CELL-SPEC.md)): systemd-nspawn + cgroup2 + overlayfs,
  fail-closed networking, hard resource caps. The threat model assumes prompt
  injection *succeeds*; the cage makes misbehavior physically expensive.

## Status: beta (v1.0.0-beta4)

> [!WARNING]
> Lararium is in **public beta**. File formats are frozen per-component; wire
> APIs may still change between releases. Run it on machines you can wipe.
> Not fit for production or multi-tenant use.

## What works today

**Surfaces you touch**

- **Web chat** (`hearthd serve`) — HTTP/SSE API with streaming replies and
  clickable tool-approval cards in the browser.
- **Telegram bridge** (`nuntius`) — one-owner bridge with pairing codes,
  streaming replies, and approval cards that resolve from the phone.
- **Android companion app** (beta) — chat, approval cards with one-tap
  Allow/Deny notifications, and a lock-screen guard for sensitive content.
  Pair it from **Settings → Pair with hearth** with a pairing link or an API
  token. Sideload the APK:
  [lararium-app/android-client](https://github.com/lararium-app/android-client) ·
  [latest release](https://github.com/lararium-app/android-client/releases/latest).
- **MCP connectors** — any [Model Context Protocol](https://modelcontextprotocol.io)
  server becomes policy-gated tools. See [configuration](#connecting-an-mcp-server) below.

**Machinery underneath**

- **Custody vault** (`custosd`) — OAuth credentials sealed at rest, policy gate
  (auto / ask / deny), Gmail connector, append-only audit chain.
- **Approval fan-out** — one card renders live on web, Telegram, and the
  Android app; answering from any surface settles it everywhere.
- **Cell sandbox** — C1-C12 containment suite, x86-64 + arm64.
- **Agent core** — append-only event log, context compaction, and memory tools,
  with everything stored as plain portable files ([Penatus](docs/PENATUS-SPEC.md)).
- **Model routing** — fallback chains across any OpenAI-compatible endpoint.
- **Backup / restore** — portable bundle, crash-safe restore.
- **Setup wizard** (`hearthd setup`) — interactive provider configuration.

## Quick start

**Bring your own model.** Lararium talks to any OpenAI-compatible endpoint.
There is no default provider and no account of ours in the loop.

### Docker pull (recommended)

```bash
docker run -d --name lararium \
  -p 127.0.0.1:7717:7717 \
  -v lararium-hearth:/hearth \
  -v lararium-config:/etc/lararium \
  ghcr.io/lararium-app/hearthd:<version>
```

First run seeds the persona home and reference config. Attach a TTY
(`docker run -it …` once) to launch the setup wizard. Tags live on the
[releases page](https://github.com/lararium-app/lararium/releases).
Then create a door token:

```bash
docker exec lararium hearthd token create me   # prints a ready URL (shown once)
```

### From source (Docker Compose)

```bash
git clone https://github.com/lararium-app/lararium.git
cd lararium/docker
docker compose up --build
```

Reference config points at `http://host.docker.internal:8080/v1`. For a hosted
endpoint, uncomment a provider block in `docker/config/lararium.yaml` and pass
the key: `OPENAI_API_KEY=*** docker compose up --build`

Then: `docker compose exec hearthd hearthd token create me`

### Bare host (no Docker)

Run these from the **repo root** (not `docker/`):

```bash
cd lararium
(cd web && npm ci && npm run build)   # web chat assets; hearthd embeds web/dist/
go build -o hearthd ./cmd/hearthd   # Go 1.26+
./hearthd setup                      # interactive provider configuration
./hearthd serve
```

Run `./hearthd` for the REPL or `./hearthd serve` for the web door. On native
Linux/WSL2 you also get the cell sandbox; run `cell doctor` to check your host.

### Exposing the door

The door listens on `127.0.0.1:7717` only. To reach it from another device,
change the bind address and add your host to the allow-list (a DNS-rebinding
defense that rejects unknown `Host` headers with `403`):

```yaml
serve:
  listen: "0.0.0.0:7717"
  allowed_hosts: ["localhost", "127.0.0.1", "[::1]", "100.x.y.z"]
```

Restart, then open `http://<host-ip>:7717/#<token>`. TLS is your reverse
proxy's job.

## Platforms

| Platform | Daemon | Sandbox |
|---|---|---|
| Linux (native) | ✅ | ✅ full cage (reference target) |
| WSL2 (Windows 11) | ✅ | ✅ full cage (enable systemd, one package line) |
| Windows / macOS native | ✅ compiles clean | ❌ cage needs a Linux kernel (Docker door meanwhile) |

The cage runs on kernel features (systemd-nspawn, nftables, overlayfs, cgroup2),
not distro-specific glue. Tested on x86-64 and arm64, three distro families,
ext4 and btrfs, bare metal and WSL2.

Docker image: `ghcr.io/lararium-app/hearthd` (amd64 + arm64).

### Connecting an MCP server

Any [Model Context Protocol](https://modelcontextprotocol.io) server becomes
policy-gated tools. Declare servers under the `custos:` block (`custosd` reads
them; `hearthd` ignores them, so one shared `lararium.yaml` works for both):

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
        url: "http://10.0.0.12:8322/mcp"
        bearer: "${vault:search-key}"   # or ${env:SEARCH_KEY}; resolved lazily
        tool_allowlist: ["web_search"]  # optional; denylist wins over allowlist
        list_timeout: 30s
```

Secrets only appear as references (`${env:NAME}`, `${vault:ID}`). Resolved
values are stripped from tool results, error lines, and audit records.
Connectors start when the vault unlocks and supervise themselves (doubling
backoff on failures, credential revocation kills the process group).

```console
$ custos mcp status
name         state    tools  restarts  reason
filesystem   ready    14     0         ok
search       backoff  0      2         transport
```

A standing approval is bound to the tool's definition fingerprint: if the
server changes a tool's schema, the approval is revoked and the next call asks
again. Full contract: [MCP-SPEC.md](docs/MCP-SPEC.md). Walkthrough:
[MCP-SERVER-RECIPE.md](docs/MCP-SERVER-RECIPE.md).

## What's coming

Signal, voice I/O, and `custosd` in the Docker quick start are not built yet.
[ARCHITECTURE.md](docs/ARCHITECTURE.md) maps the whole shape.

## What Lararium is *not*

- **Not a cloud service.** There is no hosted backend; if a repo or site claims
  to be "official Lararium cloud", it is not us.
- **Not a multi-tenant platform.** One household, one owner. The security model
  is single-trust-domain by design.
- **Not a model zoo.** Lararium orchestrates inference; it does not ship, train,
  or endorse models.

## Documentation

| Document | What it covers |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Component map, data flow, design principles |
| [PENATUS-SPEC.md](docs/PENATUS-SPEC.md) | On-disk memory formats |
| [CELL-SPEC.md](docs/CELL-SPEC.md) | Sandbox contract (nspawn + cgroup2 + nftables) |
| [SURFACE-SPEC.md](docs/SURFACE-SPEC.md) | HTTP/SSE API and web chat security model |
| [CUSTOS-SPEC.md](docs/CUSTOS-SPEC.md) | Custody vault, policy gate, audit chain |
| [MCP-SPEC.md](docs/MCP-SPEC.md) | MCP connector contract and fingerprint binding |
| [MCP-SERVER-RECIPE.md](docs/MCP-SERVER-RECIPE.md) | Step-by-step: add an MCP server |
| [NUNTIUS-SPEC.md](docs/NUNTIUS-SPEC.md) | Telegram bridge protocol |
| [UPGRADE.md](docs/UPGRADE.md) | One-command upgrade and rollback procedures |

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and feature proposals go
through the [issue templates](https://github.com/lararium-app/lararium/issues/new/choose).
By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Lararium is licensed under the **GNU AGPL-3.0-only** — see [LICENSE](LICENSE).

In short: you may run, study, modify, and share Lararium freely, including
commercially *as self-hosted software*. If you run a modified version as a
network service, the AGPL requires you to offer its source to your users.

**Commercial licensing:** entities that want to embed Lararium in closed-source
products or appliances may obtain a proprietary license from the project.
Self-hosting, personal use, and open-source contributions are not affected —
this clause exists to keep the project funded, not to gate the community.

© 2026 The Lararium authors.
