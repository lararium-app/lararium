# CELL-SPEC — sandbox contract (`cell/1`)

Status: DRAFT v1 — awaiting Gate G3 freeze (Product Owner approval before code lands).
Scope: Phase 3 of BUILD-PROCESS.md. Defines the **cell**: the per-user sandbox
container that executes tools. The daemon (hearthd) never executes tools in its
own process space; it dispatches them into a running cell.

Terminology: *cell* = one nspawn container + its rootfs + its network + its
cgroup. *Template* = the pristine cached rootfs every cell clones from.
*Egress proxy* = the dumb pass-through forwarder (Phase 3) that later becomes
custos (Phase 4).

---

## 1. Isolation primitive

**systemd-nspawn** (v2 or v10 unified-sandbox format — format choice pinned at
implementation, must be the same on both bench systems before G3).

Rationale: kernel-native, distro-shipped, proven at scale; matches the
architecture decision. gVisor/firecracker remain a hosting-provider option
later — out of scope here.

Host prerequisites (checked by `cell doctor`, hard-fail with actionable
messages): Linux ≥ 5.x with cgroup2 mounted (`stat -fc %T /sys/fs/cgroup` =
cgroup2fs), systemd ≥ 254, `systemd-nspawn` present, user delegation of a
sub-cgroup tree (`org.systemd.delegate.v1` on the user's slice) **or** root
mode. Anything missing → refuse to start a cell, never start a half-caged one.

## 2. Rootfs template

- Builder script (root, run once per host): debootstrap Ubuntu LTS minimal →
  install `bash curl python3 iputils-ping iproute2 ca-certificates sudo` →
  create uid 1000 user `keeper` (in the `sudo` group **inside the cell only**;
  root-in-cell ≠ root-on-host) → machine-id zeroed → cached at
  `/var/lib/lararium/template/<distro>-<ver>.raw` (ext4 image) or directory,
  builder's choice, recorded in a `template.json` manifest (distro, version,
  built-at, sha256).
- Cells are **clones, not copies, wherever cheap**: subvolume/btrfs snapshot if
  available; reflink copy (`cp --reflink=auto`) otherwise. A cell must be
  creatable in seconds, not minutes.
- The template contains no Lararium binaries and no secrets. Tools reach the
  cell via mounts + stdio (below), not a shipped agent daemon.

## 3. Filesystem layout

Per-cell host directory `cells/<id>/`: `cell.json` (identity, ports, created),
`root/` (the clone), `workspace/`, `log/`.

Mounts into the running cell:

| Host path | Cell path | Mode | Contents |
|---|---|---|---|
| `cells/<id>/workspace/` | `/workspace` | RW | the agent's working tree |
| `<hearth>` | `/hearth` | RW (v1; RO deferred to custos) | persona + memory tree |
| `cells/<id>/bin/` | `/opt/lararium` | RO, nodev | tool helpers staged by hearthd |

Everything else is the clone. `/home/keeper` exists but holds nothing
durable — durability lives in `/workspace` and `/hearth` only, so a cell can
be destroyed and rebuilt from the template at any time (crash doctrine, §6).

## 4. Network doctrine (the M2 cage)

**Doctrine: no route anywhere except the egress proxy. Fail closed.**

- Cell gets a private network namespace (nspawn `--network-veth`); the host
  end `v-cell-<id>` gets `10.91.<n>.1/28`; the cell gets `10.91.<n>.2`.
- The cell unit runs with **no default route** configured by us; instead host
  nftables (root, installed once by `cell net-install`) performs
  `dnat any → <proxy-addr>:<proxy-port>` for traffic from the cell subnet and
  `drop` everything else from that subnet (forward chain + explicit drop, no
  default accept anywhere). Host IP forwarding is enabled only for this subnet
  pair.
- Consequence: even the current **dumb pass-through proxy** is the only door,
  and Phase 4 swaps the knob (custos) without touching the cell.
- If `cell.json` names no proxy: the dnat rule targets nothing and the drop
  rule catches everything → total isolation, still fail-closed.
- mDNS/LLMNR/broadcast: dropped by the same chain. The loopback inside the
  cell is free.

Known-and-accepted: dnat-based forcing is transparent-proxy territory; real
L7 policy, DNS re-resolution, and SSRF pinning are custos (Phase 4). Phase 3
proves the **cage**, not the policy.

## 5. Resource limits

Applied via the transient systemd scope/unit properties (so systemd enforces,
not our code): `MemoryMax` default 8G, `CPUQuota` default 200%
(`[cell] memory_max / cpu_quota` in lararium.yaml, per-cell override in
cell.json). IO weight optional, unweighted default. PidsMax default 512.

## 6. Lifecycle

CLI (Go, `cmd/cell`): `build-template` (root), `create <id>`, `start <id>`,
`stop <id>`, `status [id]`, `run <id> -- <cmd>` (exec inside running cell via
`machinectl shell`-style `systemd-run --machine`), `snapshot <id> [--name]`,
`destroy <id>`, `doctor`.

- start = `systemd-run --machine=<id>` scope as the invoking user (or root
  mode); status/stop are idempotent.
- snapshot = metadata + best-effort btrfs reflink snapshot of `root/`; when
  the FS can't snapshot, record `unsupported` in cell.json — never fake it.
- **Crash doctrine:** kill the scope (SIGKILL) → workspace/hearth survive on
  host, root/ is disposable, `start` again yields a working cell. The unit has
  `Restart=no`; nothing auto-restarts.

## 7. Tool bridge (v1)

`cell run <id> -- <cmd>` semantics: stdin/stdout pipes, exit code propagated,
fresh mount namespace view, cwd `/workspace`, timeout enforced host-side
(SIGKILL to the scope's exec tree). hearthd's tool dispatcher will call this
interface; no network is involved in the bridge itself. Full daemon wiring is
Phase 3 tail work after M2 (the cage proof uses `cell run` directly).

## 8. Cage-proof suite (the G3 evidence)

Scripted suite `tests/cage/` — run on a bench host, writes a pass/fail report
with kernel + systemd versions; **exit 0 only if every check passes.**

| # | Check | Pass condition |
|---|---|---|
| C1 | direct HTTP from cell | `curl -m 8 http://example.com` fails |
| C2 | direct HTTPS | `curl -m 8 https://…` fails |
| C3 | ICMP | `ping -c 2 -W 3 1.1.1.1` fails |
| C4 | raw TCP to arbitrary port | `timeout 6 bash -c '</dev/tcp/93.184.216.34/81'` fails |
| C5 | DNS to external resolver | dig/nslookup-style A query fails |
| C6 | host escape on veth | cell cannot reach `10.91.<n>.1` |
| C7 | proxy path works | through dumb proxy: HTTP 200 from example.com |
| C8 | no-proxy fail-closed | cell.json without proxy → even proxy-destined traffic fails |
| C9 | crash containment | SIGKILL cell → host unaffected, restart works, workspace intact |
| C10 | env leak | cell hostname/machine-id ≠ host's; no `/home/<host-user>` visible |
| C11 | cgroup cap | `memory.max` in cell == configured cap |
| C12 | destroy/rebuild | destroy + create + start from template < 60 s |

## 9. Non-goals (Phase 3)

No MITM/TLS interception, no credential surrogation, no approval policy
(Phase 4 custos). No container image registry distribution (OCI packaging is
M1.5-adjacent polish). No hostile-multi-tenant hardening (unprivileged user
namespaces under review; single-operator home install is the target).

## 10. Verification bar

`go vet ./... && go test ./... -count=1` green; cage suite green on **two
bench systems** with the report files attached to the PR; `cell doctor`
output included for each. Gate G3 = Product Owner review of this spec
(before code) + suite review + two-bench green.
