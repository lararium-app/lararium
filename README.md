<div align="center">

# Lararium

*/ lārārium / — the household shrine where the protective spirits were kept*

**A personal AI agent that lives at your hearth and rides in your pocket.**
Your machine, your models, your keys.

[![CI](https://github.com/lararium-app/lararium/actions/workflows/ci.yml/badge.svg)](https://github.com/lararium-app/lararium/actions/workflows/ci.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)

[Website](https://lararium.io) · [Docs](docs/) · [Security](SECURITY.md) · [Contributing](CONTRIBUTING.md)

</div>

---

Lararium is an open-source, self-hosted AI agent daemon (`hearthd`). It
runs on hardware you own — a box under the stairs, a NAS, a used mini-PC —
remembers everything in plain files you can read and own, points at model
providers you choose, and reaches you on the screens you already carry.

## The covenant

- **Your fire** — bring your own llama.cpp on your own silicon, or an API
  key for any major provider. No lock-in, no hidden routing.
- **Your memory** — everything the agent knows lives in plain text files
  ([Penatus](docs/PENATUS-SPEC.md)). Export your whole life with one `tar`.
- **Your keys** — credentials are vaulted and never shown to the model;
  the network layer injects them, policy-checked, on the way out.
- **Sandboxed work** — long-running agent work executes inside cells
  ([CELL-SPEC](docs/CELL-SPEC.md)): systemd-nspawn + cgroup2 + overlayfs,
  fail-closed networking by default.

## Status: public preview (v0.1.0-alpha)

> [!WARNING]
> Lararium is **pre-1.0 alpha**. The file formats are specified and frozen
> per-component; the wire APIs may still change between releases. Run it on
> machines you can wipe. Not fit for production or multi-tenant use.

**Working today:**

- **`hearthd`** — the agent daemon: interactive REPL session loop with an
  append-only, crash-safe event log; context compaction so sessions
  outlive the context window; tools for memory (`remember`, `forget`,
  `memory_search`) and time (`clock`).
- **Model routing** — any OpenAI-compatible endpoint (llama.cpp,
  OpenRouter, …) and Anthropic Messages; per-session model selection,
  fallback chains, streaming, capability probing.
- **Penatus memory** — persona files + `MEMORY.md` + a searchable memory
  tree, all plain text with a frozen on-disk spec. Your agent's whole mind
  is files you own.
- **Cell sandbox** — `cell` runs agent work inside systemd-nspawn +
  cgroup2 + overlayfs with hard memory/CPU/task caps, uid-mapped binds,
  snapshots, and crash recovery. Networking is fail-closed: a cell has no
  route anywhere except through an auditing egress proxy (CONNECT + HTTP,
  SSRF re-resolve, host-address floor). Template bake → create → run →
  destroy in under a minute.

**Not built yet:** messaging channels and a web UI (the session layer is
channel-ready; the REPL is the only front door today), the credential
vault (`custos`), and the HTTP API server. See
[ARCHITECTURE.md](docs/ARCHITECTURE.md) for the whole shape and what each
component will do.

## How the sandbox protects you

The threat model assumes prompt injection *succeeds* — a hostile web page
or email convinces the agent to misbehave. Lararium's answer is to make
misbehavior physically expensive below the model:

- **No network except the door.** Each cell gets a dedicated veth pair in
  its own netns; an nftables cage drops direct HTTP/HTTPS/DNS/ICMP/raw
  TCP, refuses the host's own addresses (SSRF floor), and logs every
  bypass attempt. The only exit is the local proxy, which re-resolves
  hostnames per request and enforces per-request budgets.
- **Hard resource caps.** `memory.max` with swap disabled, CPU quota,
  task limits, OOM containment (`OOMPolicy=continue`) — a runaway cell
  dies inside its own cgroup and the host never notices.
- **Filesystem containment.** overlayfs workspaces, uid-mapped binds so
  in-cell root is a mapped unprivileged uid on the host, and per-cell
  snapshots you can roll back.
- **Proven, not claimed.** A C1–C12 containment suite asserts each
  property positively (escape attempts, crash containment, uid mapping,
  memory caps with random fill so zero-page tricks can't false-pass).
  Green on four hosts spanning two architectures, three distro families,
  ext4/btrfs, and all three iptables/nftables host shapes.

Found a way around any of this? [SECURITY.md](SECURITY.md) — we take it
seriously and credit reporters.

## Quick start

The supported way to run the alpha is Docker Compose:

```bash
git clone https://github.com/lararium-app/lararium.git
cd lararium/docker
OPENROUTER_API_KEY=*** docker compose up --build
```

Keys come from your environment only — nothing secret is baked into the
image or committed to this repo. Edit `docker/config/lararium.yaml` to
point at your own model server instead (any OpenAI-compatible endpoint
works, e.g. llama.cpp).

Build from source with Go 1.24+:

```bash
go build ./cmd/hearthd && go test ./...
```

## Documentation

| Document | What it freezes |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Component map, data flow, design principles |
| [PENATUS-SPEC.md](docs/PENATUS-SPEC.md) | The on-disk memory formats |
| [CELL-SPEC.md](docs/CELL-SPEC.md) | The sandbox contract (nspawn + cgroup2 + nftables) |

## What Lararium is *not*

- **Not a cloud service.** There is no hosted backend; if a repo or site
  claims to be "official Lararium cloud", it isn't us.
- **Not a multi-tenant platform.** One household, one owner. The security
  model is single-trust-domain by design.
- **Not a model zoo.** Lararium orchestrates inference; it does not ship,
  train, or endorse models.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and feature
proposals go through the [issue templates](../../issues/new/choose).
By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Lararium is licensed under the **GNU AGPL-3.0-only** — see [LICENSE](LICENSE).

In short: you may run, study, modify, and share Lararium freely, including
commercially *as self-hosted software*. If you run a modified version as a
network service, the AGPL requires you to offer its source to your users.

**Commercial licensing:** entities that want to embed Lararium in
closed-source products or appliances may obtain a proprietary license from
the project. Self-hosting, personal use, and open-source contributions are
not affected — this clause exists to keep the project funded, not to gate
the community.

© 2026 The Lararium authors.
