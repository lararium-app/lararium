# Uninstalling Lararium

Lararium writes no data outside the handful of paths below, and it installs
no system services unless you wrote the unit file yourself. Uninstalling is
therefore two commands and, optionally, a key decision.

## What Lararium leaves on a machine

| Path | Created by | Contains |
|---|---|---|
| `./lararium.yaml` (or `$LARARIUM_CONFIG`, or `/etc/lararium/lararium.yaml`) | `hearthd setup` | Your config: model providers, listen address, `allowed_hosts`. May reference env-var names for API keys — never the keys themselves. |
| `<config dir>/.setup-done` | `hearthd setup` | Zero-logic completion marker. |
| `<config dir>/personas/` | `hearthd setup` | Your persona template files. Plain markdown; copy anywhere. |
| `~/.lararium/` (hearth home, or `$HEARTH_HOME` / `hearth.home` in config) | `hearthd serve` | All state: sessions, Penatus memory files, audit chain, door store (tokens, MCP grants), Nuntius pairings. |
| `<hearth home>/custos/` | `custosd` / `custos` | The credential vault (sealed at rest) and the `ctl.sock` control socket. |
| `hearth-<timestamp>.lararium-backup` | `hearthd backup` | Portable backup bundles, wherever you told them to land. |
| Docker image + named volumes | `docker run` / compose | `ghcr.io/lararium-app/hearthd` plus the two volumes you mounted (config + hearth). |

## Bare-host uninstall

```sh
systemctl --user stop lararium   # if you installed a unit; otherwise Ctrl-C the foreground hearthd
rm -rf ~/.lararium      # ALL state: memory, audit, vault — irretrievable without a backup
rm -f  lararium.yaml    # or $LARARIUM_CONFIG / /etc/lararium/lararium.yaml
```

Delete the config **after** the hearth home if you still want to read your
memory exports — `hearthd backup` needs the config to run.

## Docker uninstall

```sh
docker rm -f lararium
docker volume rm lararium-hearth lararium-config   # names from the README quickstart
docker rmi ghcr.io/lararium-app/hearthd
```

Compose users: `docker compose down -v` in the directory with
`docker-compose.yml` removes the containers and their volumes in one step.

## Revoking credentials (the part `rm` cannot do)

- **Door tokens** are stored hashed inside the hearth home and die with it;
  any token already handed to a client simply stops working.
- **Vaulted OAuth credentials** (e.g. Gmail refresh tokens) are sealed in
  `<hearth home>/custos/`; deleting the directory destroys the local copy,
  but the credential itself stays valid upstream until you revoke it at the
  provider (Google account → Security → Third-party access).
- **Telegram (nuntius)**: revoke the bot token at @BotFather if you want the
  bridge dead everywhere, not just on this machine.
- **API keys** live only in your environment or secrets manager; Lararium
  never copies them to disk.

## Reinstalling from scratch

`hearthd setup --defaults` (or the Docker one-liner in the README) against
fresh volumes gives a clean install; nothing outside the table above is read
at startup, so there is no hidden global state.
