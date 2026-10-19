# Hostile-install walkthrough (beta C1) — executed transcript

Status: executed 2026-10-19. Findings filed (#117) and closed with this PR.

## Environment

* Fresh `git clone` of `main` at `53d7f06` (no dev state), Docker Compose
  path from README ("Run it (from source)").
* Host: Debian 13, Docker 26.1.5. Image built from the clean clone
  (`docker compose up --build -d`), daemon started detached (documented
  defaults; no provider configured — web/API surface still serves).
* Vantages: (a) host loopback, (b) a throwaway `alpine` container attached
  to the serving network ("LAN neighbor"), (c) header spoofing via curl.

Every response line below is verbatim from the run (HTTP codes via
`curl -w %{http_code}`; bodies where printed).

## 1. Default posture

* Published per README: `127.0.0.1:7717:7717` — host exposure loopback-only.
* Reference config binds `0.0.0.0:7717` **inside** the container (required
  for port publishing; the Host gate carries identity), and the daemon
  correctly logs: `WARNING: listening beyond loopback on 0.0.0.0:7717`.
  On a fresh compose install that warning reads like an alarm → filed as
  [#117], closed here by documenting it in `docker/docker-compose.yml`.
* `GET http://127.0.0.1:7717/v1/sessions` (no credentials) → **401**. The
  surface answers but never serves unauthenticated.

## 2. Host allow-list (DNS-rebinding defense)

| Probe | Host header | Result |
|---|---|---|
| W2.1 | `127.0.0.1:7717` | 401 (host gate passes; auth gate answers) |
| W2.2 | `evil.example` | **403 forbidden** |
| W2.3 | `203.0.113.7:7717` (numeric, non-loopback) | **403 forbidden** |
| W2.4 | `localhost:7717` | 401 (gate passes) |

Positive control W6: after adding `evil.example` to
`serve.allowed_hosts` and restarting — `Host: evil.example` → **401**
(gate passes), while `Host: notlisted.example` still → **403**. The list
works in both directions; default is fail-closed.

## 3. LAN-neighbor vantage (W4b)

From a fresh alpine container on the serving network:

```
curl http://hearthd:7717/v1/sessions
  Host: hearthd:7717            → 403 {"error":"forbidden"}      (host gate)
  Host: 127.0.0.1:7717 (spoofed) → 401 {"error":"unauthorized"}  (auth gate)
```

Interpretation: a network neighbor can open the socket (container bind),
but the Host gate rejects its natural requests, and header spoofing only
downgrades to the token wall — never to data. Two independent gates, both
fail-closed.

## 4. Token rotation & door revocation (W3)

Doors in Lararium **are** bearer tokens (`hearthd token create` = "open a
door"), so rotation/revoke covers both criteria lines:

```
token minted: lar1_uGWvH…                     (crypto/rand, printed once)
use-before-revoke HTTP: 200
hearthd token revoke walk-rot → revoked "walk-rot" (effective on the daemon's next request)
use-AFTER-revoke HTTP (no restart): 401
bogus token (right shape, wrong bytes): 401
```

Revocation is effective on the very next request — `Verify` re-reads the
hash store per request (docs/THREATMODEL-DOORS.md §2), no restart, no
cache window.

## 5. Hostile-request spot checks

| Probe | Input | Result |
|---|---|---|
| W5.1 | 2 MB random body POST | **413** (bodyLimit before any parsing) |
| W5.2 | `/v1/..%2f..%2fetc/passwd` | **404** (cleanPathGate + mux) |

## 6. Findings

| ID | Severity | Statement | Disposition |
|---|---|---|---|
| [#117](https://github.com/lararium-app/lararium/issues/117) | Info (DX) | Fresh compose start logs the by-design "listening beyond loopback" WARNING with no nearby explanation | Closed: comment added to `docker/docker-compose.yml` by this PR |

No security defects found: posture, gates, rotation, and revocation all
behaved to spec from three vantages. Walkthrough **passes**.

## Repro notes (harness pitfalls, for the next walker)

* Probe wrapper caveat: `exec -T` inside an `eval`-wrapped string hits the
  shell builtin, not docker — run compose exec probes directly (the W3
  battery above was re-run that way).
* An attacker container must sit **on the serving bridge** (`docker_default`,
  target by service name `hearthd`); connecting it to a second bridge and
  dialing the first bridge's IP yields `000` (no route), not a verdict.
