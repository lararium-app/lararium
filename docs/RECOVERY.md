# Backup & recovery

Lararium state is designed to be backed up as files and restored as files.
This guide is the rehearsed procedure — every command below was executed
against `ghcr.io/lararium-app/hearthd:v0.10.1-alpha` with real volumes,
including a full wipe of both volumes before restoring.

## Back up

```sh
hearthd backup create --out ~/backups/hearth-$(date -u +%Y%m%dT%H%M%SZ).lararium-backup
hearthd backup verify <that-file>
```

The bundle is a single file containing the hearth home (sessions, Penatus
memory, audit chain, door tokens, vault references) and a copy of the config.
`verify` lists the bundle contents and flags the embedded derived secrets —
**treat the bundle as your disk**: it contains material that unlocks your
agent. Store it encrypted, off the machine.

Bundles are point-in-time: anything written after `backup create` is not in
them. `backup list <dir>` inventories bundles; timestamps in filenames are
your restore menu.

## Restore onto a wiped machine

Two commands, in order — the hearth home and the config are restored
separately because they may live on different disks:

```sh
# 1. Hearth home into a fresh (non-existent) root:
hearthd restore <bundle> --to ~/.lararium

# 2. Config out of the bundle:
hearthd backup extract-config <bundle> ~/.config/lararium/lararium.yaml
#    (or wherever you keep it: ./lararium.yaml, /etc/lararium/lararium.yaml)

hearthd serve   # and open the door URL, or reuse your saved door token
```

Notes from the rehearsal:

- `--to` creates a fresh root; restoring **over an existing home** uses
  `--replace <dir> --yes` (explicit confirmation required — it refuses both
  flags in `--to` mode, and says so).
- Door tokens survive: the restored `tokens.json` is byte-identical, so
  previously issued URLs and `token create` clients keep working.
- If you wipe and restore on a brand-new box, restore keys (`hearthd keys`,
  see KEYS-SPEC) before trusting vault contents — the vault is sealed to
  this machine's key material.

## Docker containers

Same commands, same bundle, run against the volumes:

```sh
# back up (a cron entry on the host is the boring version):
docker run --rm --entrypoint /usr/local/bin/hearthd \
  -e HOME=/hearth -v lararium-hearth:/hearth -v lararium-config:/etc/lararium \
  -v "$PWD:/backup" ghcr.io/lararium-app/hearthd:<version> \
  backup create --config /etc/lararium/lararium.yaml --out /backup/hearth.lararium-backup

# after total volume loss:
docker volume rm lararium-hearth lararium-config        # the wipe
docker run --rm --entrypoint /usr/local/bin/hearthd \
  -e HOME=/hearth -v lararium-hearth:/hearth -v lararium-config:/etc/lararium \
  -v "$PWD:/backup" ghcr.io/lararium-app/hearthd:<version> \
  restore /backup/hearth.lararium-backup --to /hearth/.lararium
docker run --rm --entrypoint /usr/local/bin/hearthd \
  -v lararium-config:/etc/lararium -v "$PWD:/backup" \
  ghcr.io/lararium-app/hearthd:<version> \
  backup extract-config /backup/hearth.lararium-backup /etc/lararium/lararium.yaml
```

Two container-specific quirks found during the rehearsal (tracked in
lararium-app/lararium#110): set `HOME` (and optionally `HEARTH_HOME`)
consistently for every one-shot `hearthd` command, and if you scaffolded
with `setup --defaults` inside the image, flip the bind to the container
interface once — the scaffold writes `127.0.0.1:7717`, which inside a
container is unreachable through a published port:

```sh
docker run --rm --entrypoint sh -v lararium-config:/etc/lararium \
  ghcr.io/lararium-app/hearthd:<version> \
  -c "sed -i s/127.0.0.1:7717/0.0.0.0:7717/ /etc/lararium/lararium.yaml"
```

`allowed_hosts` in the scaffold already accepts `localhost`/`127.0.0.1`, so
the Host gate keeps working with the published port.

## Drills worth running before you rely on it

1. Back up, restore onto a scratch directory, `grep` for something only you
   wrote (a SOUL.md line works well).
2. Re-issue a request with an old door token — it must still open the door.
3. `hearthd backup verify` your oldest bundle; a bundle that has never been
   verified is a wish, not a backup.
