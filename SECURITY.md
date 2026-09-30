# Security Policy

## Supported Versions

| Version     | Supported          |
|-------------|--------------------|
| latest      | :white_check_mark: |
| < latest    | :x:                |

Lararium is pre-1.0 software: security fixes land on `main` and ship in
the next release. Before reporting, please confirm you are running the
latest release and reproduce against it.

## Reporting a Vulnerability

**Do not open a public issue for security problems.**

Report privately by email to **security@lararium.io** (a role mailbox —
never a personal account). Include:

- a description of the issue and the affected component
  (`hearthd`, `cell`, the PWA, or the pairing flow)
- steps to reproduce, with versions (`hearthd --version`, OS, kernel)
- any proof-of-concept material, scrubbed of personal data

What to expect:

- acknowledgment within **72 hours**
- a first severity/impact assessment within **7 days**
- a public advisory (GitHub Security Advisory) once a fix is released,
  with credit to the reporter unless you prefer anonymity

We follow coordinated disclosure: fixes ship before details become
public, and we will not take legal action against good-faith research.

## What we consider in scope

Anything that breaks the product's core promises: sandbox escape from a
cell, memory-file access across owners, pairing/authentication bypass,
privilege escalation on the host, or silent data exfiltration through a
provider or nuntius bridge.

## Out of scope

- physical/network-adjacent attacks requiring local unprivileged code execution on the same host as `hearthd` (it is single-owner software; see the [security model](docs/ARCHITECTURE.md))
- resource exhaustion from your own workloads
- vulnerabilities in third-party models or providers themselves
