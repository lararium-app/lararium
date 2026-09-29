# Task Brief T4 — `internal/cell` + `cmd/cell`: template, storage, lifecycle

Spec: docs/CELL-SPEC.md (**FROZEN** — §1, §2, §5, §6 govern; where this brief
and the spec conflict, the spec wins; where YOU disagree with the spec, STOP
and report, do not improvise).
Module root: github.com/lararium-app/lararium. Go 1.24.
Allowed dependencies: stdlib + `gopkg.in/yaml.v3` (already in go.mod) only.
Do NOT touch anything outside `internal/cell/`, `cmd/cell/`, and
`tests/cellunit/` (your unit tests). **No network code — the cage (§4) is
T5.** T4 boots with `--private-network`: the guest simply cannot reach
anything, which is safe *interim* isolation while T5 replaces it with the
routed + host-nft design. Do NOT treat a private netns as the product's
security answer — spec §4 says the cage is the host's answer to every
packet; T4 just ships no path at all until that exists.

## What you are building

A `cell` CLI that manages per-user sandbox containers via systemd-nspawn:

```
cell build-template <dir>       # root: debootstrap noble minbase + bake
cell create <id>                # overlay dirs + seed + cell.json
cell start <id>                 # boot nspawn in transient unit (private net)
cell run <id> [--cwd P] [--timeout S] -- <cmd>   # exec via machined
cell stop <id>                  # unit stop + unmount merged
cell status [id]                # one line per cell
cell snapshot <id>              # upper -> snapshots/<ts>/, fresh upper
cell destroy <id>               # stop + unmount + rm -rf
cell doctor                     # environment gates (§"doctor" below)
```

## EXACT DESIGN (do not deviate)

### Layout (host paths, under `<root>`, default `/var/lib/lararium` unless
`$LARARIUM_CELL_ROOT` overrides — needed so tests run without root)

```
<root>/templates/noble/            # RO lower (built once)
<root>/cells/<id>/upper/           # per-cell RW upper layer
<root>/cells/<id>/work/            # overlayfs work dir (same fs as upper)
<root>/cells/<id>/merged/          # overlay mount point (nspawn's -D)
<root>/cells/<id>/workspace/       # bind-mounted to /workspace in guest
<root>/cells/<id>/cell.json        # id, subuid_base, veth state (T5 fields
                                   # present but unused now), created ts
<root>/cells/<id>/snapshots/<ts>/  # historical uppers
```

`cell.json`: `struct { ID string; SubUIDBase int; Created time.Time;
SubUIDCount int }` — written atomically (temp + rename). Unknown extra
fields on read are tolerated; missing file = error `cell not found`.

### internal/cell/ files

1. `cell.go` — state model: load/save cell.json, list cells, path helpers.
   Every path under `<root>` is validated (`filepath.Clean` + prefix check) —
   a cell id containing `..` or `/` is rejected at create with
   `ErrInvalidID`.
2. `template.go` — `BuildTemplate(dir string) error`:
   - refuse unless euid 0 and `debootstrap` exists;
   - `debootstrap --variant=minbase noble <dir>/noble` (mirror from
     `$LARARIUM_DEBIAN_MIRROR` if set, default Ubuntu archive);
   - bake (spec §2, exact): `debootstrap --variant=minbase` then
     `chroot`-install `bash curl python3 iputils-ping iproute2
     ca-certificates sudo systemd-networkd`; create uid 1000 user `keeper`
     **in the `sudo` group** (spec pins this; userns is what makes it
     harmless — do NOT "harden" it away); `/workspace` dir; enable
     `systemd-networkd.service`; mask `NetworkManager` if present; machine-id
     removed (`rm -f etc/machine-id; : > etc/machine-id`); apt-cleanup.
     Nothing beyond that list (no build tools, no extra daemons).
3. `overlay.go` — mount/unmount via `/bin/mount -t overlay` (mount(2) direct
   is fine too; `mount` binary keeps error messages honest):
   `lowerdir=templates/noble,upperdir=cells/<id>/upper,workdir=cells/<id>/work`.
   Ownership: nspawn's `--private-users-ownership=auto` (spec §1 — idmapped
   mounts, no recursive chown; **not** `map`, not chown fallback).
   `Create` = mkdir 3 dirs → seed `upper/` (spec: seed BEFORE mount) → mount.
   `Destroy` = **plain `umount` only**, retry ≤ 3 with 1 s spacing; if still
   busy → error `cell busy: <pids from fuser -m>`, **never delete**, caller
   decides (stale-handle holders included). NO `umount -l`: lazy unmount
   vanishes from `/proc/mounts` while the kernel keeps the fs alive, so the
   safety check passes and `rm -rf` guts a live filesystem. After successful
   unmount, re-check `/proc/mounts` AND `<root>` prefix before any delete.
4. `boot.go` — `Start(id string, lim Limits) error`:
   - transient unit via
     `systemd-run --unit=lararium-cell-<id> --keep-unit --property=...`
     wrapping `systemd-nspawn --machine=<id> --register=yes --keep-unit
     --private-users=pick --private-users-ownership=auto --private-network
     -D <root>/cells/<id>/merged`
     (+ `--bind=<root>/cells/<id>/workspace:/workspace`,
     `--bind=<hearth>:/hearth`,
     `--bind=<root>/cells/<id>/bin:/opt/lararium:ro,nodev` — spec §3 table;
     create `bin/` empty at cell create)
     (T5 will drop `--private-network` and add veth wiring — isolate that
     into one function `nspawnArgs(cell) []string` with a TODO);
   - limits from `[cell]` config or overrides: `MemoryMax` 8G, `CPUQuota`
     200%, `TasksMax` 512 → `--property=MemoryMax=...` etc; **plus spec §6
     props: `--property=Restart=no --property=KillMode=mixed`** — mandatory,
     crash containment depends on them;
   - healthy first, then map: wait for healthy (bounded 30 s), THEN read the
     picked uid block from the **in-guest init's** `/proc/<pid>/uid_map`
     (walk the unit cgroup's process tree — the nspawn supervisor itself is
     unmapped host root and is NOT the subject; the mapped descendant is)
     and record the first line's lower offset in cell.json
     (`SubUIDBase`); if the map is `0 0 4294967295` (no userns) → **error,
     cell considered failed to start** (unit stopped, status reports why);
   - healthy = `systemctl is-active lararium-cell-<id>` AND machine
     registered (`machinectl show <id>` succeeds). Both checked, both
     reported.
   - **`--register=no` is never used in production code paths** (only docs
     mention it for ssh-session experiments).
5. `exec.go` — `Run(id string, opts RunOpts) (ExitCode, error)`:
   refuses if machine not registered (`cell not running`, no implicit
   start); `machinectl shell` is NOT used (allocates a tty); use
   `systemd-run --machine=<id> --wait --pipe --property=...
   -- /bin/sh -c <cmd>` OR the machined varlink API — **pick systemd-run,
   it is simpler and exists** ; `--timeout` → `--timeout-sec=` property +
   kill the scope on expiry; cwd default `/workspace`; stdio passthrough;
   exit code propagated verbatim (exec failures ≠ cell failures: distinguish
   126/127 from transport errors).
6. `doctor.go` — checks, each prints OK/FAIL/UNSUPPORTED + one-line why:
   root, systemd-nspawn present, `/run/host` (container nesting), cgroup2
   rw at `/sys/fs/cgroup`, user delegation of >=65536 subuids
   (`/etc/subuid` has an entry or root), overlay mount probe in a temp dir,
   userns nspawn probe: boot a **throwaway** cell from the built template
   (or from an empty dir if no template) with
   `--private-users=pick --private-users-ownership=auto --ephemeral
   --register=no -D <dir> /bin/sh -c 'id -u'` — wait, `--register=no` IS
   allowed here, doctor runs from odd sessions, and this is a probe, not
   production: expected `1000`-mapped output proving pick works; kernel
   >= 5.19. Exit code: nonzero if any OK/FAIL gate fails; UNSUPPORTED is
   allowed only for checks the platform genuinely lacks (report, don't
   fail, on e.g. ARM debootstrap quirks).
7. `config.go` — `LoadConfig(path)`: lararium.yaml `[cell]` section →
   Limits; defaults exactly spec §5.

`cmd/cell/main.go` — flag/std-parsing only; all logic in internal/cell.
Subcommand names exactly as listed above. Errors to stderr, exit 1; usage
exit 2.

### Behavior rules (unit tests in tests/cellunit/, root-gated parts behind
`testing.Short()` skip + a `TestMain` env check — CI runners are not root)

- create → cell.json exists, dirs exist, overlay in `/proc/mounts`;
  destroy → none of them, `rm -rf` never ran on a live mount (asserted via
  mount table before delete).
- create with id `..`, `a/b`, empty → `ErrInvalidID`.
- start without template → clear error mentioning `build-template`.
- start with fake/mock nspawn (inject exec function — design for it: all
  subprocess calls go through one `runner` interface field so tests can
  fake systemd entirely) → correct argv includes `--keep-unit`,
  `--private-users=pick`, `--private-users-ownership=auto` for nspawn AND
  `--keep-unit` for systemd-run; properties carry the limits.
- Run against unregistered machine → error string `cell not running`,
  exit code path never invokes the runner.
- snapshot: upper renamed to `snapshots/<UTC ts>/upper`, fresh empty upper
  remounted; cell.json untouched.
- status: three cells (running per fake, stopped, fresh) → three correct
  lines.

### Acceptance gate (paste real output before finishing)

```
go vet ./... && go test ./tests/cellunit/ -count=1 && gofmt -l internal/cell cmd/cell
```

All green, gofmt empty. This is T4's bar, NOT gate G3b: the cage suite
(`tests/cage/`, two benches, doctor output attached) belongs to T5 and will
be the G3b evidence for the whole feature — T4 merges only behind green unit
tests PLUS the live smoke below run by the maintainer on both benches. Then
ONE live smoke on the dev box (you have it via me, not yourself — write
`SMOKE.md` under tests/cellunit/ with the exact
commands for me to run: build-template, create demo, start, run echo,
uid-map record check, stop, destroy, doctor) and I run them before merge.

### Anti-fabrication clause

Every command name, flag, and property above was verified against the real
systemd 257+/nspawn manpages at spec-freeze time. If a flag you wrote is
rejected at runtime, that is a bug in YOUR code, not license to invent a
replacement flag: re-read `man 1 systemd-nspawn`, `man 5 systemd.resource-control`,
`man 1 systemd-run` and fix the call. Report any flag you could not verify
instead of guessing.