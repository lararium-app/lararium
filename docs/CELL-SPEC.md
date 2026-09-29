# CELL-SPEC — sandbox contract (`cell/1`)

Status: DRAFT v2 — redraft after architecture review (2026-09-29); supersedes
v1, whose G3a approval is void (fresh approval required — see §10).
Scope: Phase 3 of BUILD-PROCESS.md. Defines the **cell**: the per-user sandbox
container that executes tools. The daemon (hearthd) never executes tools in its
own process space; it dispatches them into a running cell.

Terminology: *cell* = one nspawn container + its overlay root + its network +
its cgroup. *Template* = the pristine cached rootfs that forms every cell's
lower layer. *Egress proxy* = the dumb pass-through forwarder (Phase 3) that
later becomes custos (Phase 4).

Changelog v1→v2 (review findings, all accepted):
1. §4 — in-cell default route + host DNAT(80,443)→proxy. v1's route-less
   design killed guest sockets client-side (`ENETUNREACH`) before host rules
   were ever consulted — the proxy door was unreachable and C7 unpassable.
2. §1 — user namespaces (`--private-users=pick`) mandated. Without them,
   in-cell root *is* host UID 0 and keeper's sudo is a containment breach.
3. §2 — OverlayFS replaces reflink copies (ext4 lacks reflink; `cp
   --reflink=auto` silently falls back to full copies).
4. §6 — boot vs exec split (`systemd-run --machine` execs into a running
   machine; it does not boot one).
Plus follow-ons: host **input**-chain drops (gateway-directed traffic never
traverses forward), proxy env injection, userns-aware bind ownership,
overlay-capability `doctor` probe, uid-map check in C10.

---

## 1. Isolation primitive

**systemd-nspawn** with **user namespaces**: `--private-users=pick`. In-cell
root maps to an unprivileged host subuid range — root-in-cell ≠ anything on
the host, by mechanism, not politeness. This is what makes keeper's in-cell
sudo (§2) harmless; without userns, sudo-in-cell would be host root and this
spec would be void.

Host prerequisites (checked by `cell doctor`, hard-fail with actionable
messages): Linux ≥ 5.x with cgroup2 (`stat -fc %T /sys/fs/cgroup` =
cgroup2fs), systemd ≥ 254, `systemd-nspawn` present, subuid/subgid ranges
allocated for the invoking user (`/etc/subuid`), `systemd-machined` reachable
(required by `cell run`, §6), and the cells directory on a filesystem that
supports overlayfs upperdirs (d_type + xattrs — `doctor` probes by mounting a
throwaway overlay). Anything missing → refuse to start a cell, never start a
half-caged one.

Registration note: an nspawn invoked from a raw ssh/sudo session cannot
register with machined (`--register=yes` is refused — hit during the
feasibility spike). `cell start` therefore boots the container as a transient
**systemd unit** (root-mode, or a delegated user-scope unit), never a bare
foreground nspawn — the unit registration is what `cell run` later targets.

## 2. Rootfs template + OverlayFS root

- Builder script (root, run once per host): debootstrap Ubuntu LTS minimal →
  install `bash curl python3 iputils-ping iproute2 ca-certificates sudo
  dnsutils` → create uid 1000 user `keeper` (in the `sudo` group **inside the
  cell only**; harmless under §1's userns mapping, and lets the agent install
  a package without host privileges) → machine-id zeroed → cached read-only
  at `/var/lib/lararium/template/<distro>-<ver>/`, recorded in
  `template.json` (distro, version, built-at, sha256 of the tree).
- **Cell root = OverlayFS**, never a copy:
  - `lowerdir` = template (read-only, shared by all cells),
  - `upperdir` = `cells/<id>/upper/` (per-cell writes),
  - `workdir` = `cells/<id>/work/` (overlayfs requires both on the same fs),
  - merged `cells/<id>/merged/` is what nspawn boots.
  - **create** = mkdir three dirs + mount (instantaneous, any filesystem);
    **destroy** = unmount + `rm -rf upper`;
    **snapshot** = freeze: unmount, rename `upper` → `snapshots/<ts>/upper`
    (new empty upper for the live cell; historical lowers kept for restore).
    Snapshots are recorded `unsupported` only if even rename fails — never
    faked.
  - `doctor` mounts a throwaway overlay in the cells dir to prove d_type +
    xattr support (NFS/vfat-backed homes fail here with an explicit message
    instead of a mystery at create time).
- The template contains no Lararium binaries and no secrets. Tools reach the
  cell via mounts + stdio (§7), not a shipped agent daemon.

## 3. Filesystem layout

Per-cell host directory `cells/<id>/`: `cell.json` (identity, subnet index,
proxy target, limits, created), `upper/` + `work/` + `merged/`,
`snapshots/`, `workspace/`, `log/`.

Mounts into the running cell:

| Host path | Cell path | Mode | Contents |
|---|---|---|---|
| `cells/<id>/workspace/` | `/workspace` | RW | the agent's working tree |
| `<hearth>` | `/hearth` | RW (v1; RO deferred to custos) | persona + memory tree |
| `cells/<id>/bin/` | `/opt/lararium` | RO, nodev | tool helpers staged by hearthd |

**Userns-aware binds:** with `--private-users=pick`, bind-mounted host dirs
appear under the cell's uid map. `workspace/` and `<hearth>` are owned by the
host user, so cell.json records an explicit idmapping (source owner uid ↔
in-cell keeper) — via `--uid-map` entries — so the cell can actually read and
write its durable state. Implementation must prove it with a test: create a
file as keeper in-cell → read it host-side → owner is the recorded mapped
uid, content readable. A cell that boots but cannot write `/workspace` is a
failed cell.

Everything else is the overlay. `/home/keeper` lives in the upper layer —
nothing there is durable (crash doctrine, §6); durability lives only in
`/workspace` and `/hearth`.

## 4. Network doctrine (the M2 cage)

**Doctrine: no egress anywhere except through the egress proxy. Fail closed.**
Enforcement is host-side and packet-level; guest cooperation (env vars) is
plumbing, never the boundary.

Topology: nspawn `--network-veth`; host end `v-cell-<id>` addressed
`10.91.<n>.1/28`, cell gets static `10.91.<n>.2` (generated `.network` file
or nspawn network args).

**In-cell:** default route via `10.91.<n>.1`. The guest must *think* it has a
normal network — a route-less guest fails at socket creation (`ENETUNREACH`)
before any host rule sees a packet, which would break the DNAT'd proxy path
too. The cage is the host's answer to every packet, not the guest's routing
table.

**Host nftables** (root, installed once by `cell net-install`; table
`lararium`, family `inet`; atomic `nft -f` replace keeps it idempotent across
cell churn):
- **forward** chain, `iif v-cell-*`: `tcp dport { 80, 443 }` → `dnat` to the
  configured proxy `ip:port`; **drop everything else** from `iif v-cell-*`
  (any proto, any port — kills ICMP, DNS, raw TCP).
- **input** chain, `iif v-cell-*`: drop all except established/related and
  DNAT'd proxy traffic. Traffic aimed at the gateway address itself
  traverses *input*, not forward — without this rule `10.91.<n>.1` is a free
  window into the host (check C6).
- No `masquerade` needed for DNAT'd flows (conntrack hairpins the answers);
  there is deliberately no rule that forwards non-DNAT'd cell traffic, so no
  path exists to masquerade onto.
- **Fail-closed:** no proxy configured → install the drops with *no* DNAT
  rule → total isolation. The drop rules are unconditional; DNAT is the only
  hole, opened per proxy config.
- Broadcast/multicast/LLMNR: dropped by the same chains. Loopback in-cell is
  free (loopback is not the boundary).

Env plumbing for the dumb L7 proxy: every cell gets
`HTTP_PROXY/HTTPS_PROXY=http://10.91.<n>.1:<port>` (+ lowercase twins,
`NO_PROXY=localhost,127.0.0.1`). curl et al. then speak absolute-form to the
gateway; DNAT still catches anything that ignored the env and aims 80/443
out. Everything else — DNS included — dies at the drop rules; in Phase 3 the
agent resolves nothing itself (name-based fetching happens at proxy level via
absolute URLs).

Known-and-accepted: DNAT forcing is transparent-proxy territory; real L7
policy, DNS re-resolution, SSRF pinning, and CONNECT filtering are custos
(Phase 4). Phase 3 proves the **cage**, not the policy.

## 5. Resource limits

Applied as properties on the transient unit that *boots* the cell (§6):
`MemoryMax` default 8G, `CPUQuota` default 200%, `TasksMax` default 512
(`[cell] memory_max / cpu_quota` in lararium.yaml, per-cell override in
cell.json). systemd enforces; our code only asks. `IOWeight` left default.

## 6. Lifecycle — boot vs exec (two operations, two commands)

- **`cell start <id>` boots the container.** Runs `systemd-nspawn
  --machine=<id>` with the template's `/sbin/init` inside a transient systemd
  unit (`systemd-run --unit=lararium-cell-<id>`, root-mode or delegated
  user-scope) carrying the §5 limits. Unit props: `Restart=no`,
  `StopKillMode=mixed`. Healthy = unit active **and** machine registered with
  machined.
- **`cell run <id> [--cwd P] [--timeout S] -- <cmd>` execs inside a booted
  cell.** Via machined (`systemd-run --machine=<id> …` or the
  `MachineStartUnit`-free direct bus call). `--machine` **never boots**: if
  the machine isn't registered, `run` errors `cell not running` — no implicit
  start. stdin/stdout/stderr piped, exit code propagated, default cwd
  `/workspace`, host-enforced timeout kills just the exec scope, not the
  cell.
- Other CLI: `build-template` (root), `create`, `stop` (stop boot unit +
  unmount merged), `status [id]`, `snapshot`, `destroy` (stop + unmount +
  `rm -rf`), `doctor`. create/start/stop are idempotent.

**Crash doctrine:** `kill -9` the boot unit's main process → kernel tears
down the netns + cgroup; `workspace/` and `<hearth>` survive on the host;
`upper/` is disposable (possibly dirty — plain destroy/create from template).
`start` again yields a working cell. Nothing auto-restarts.

## 7. Tool bridge (v1)

hearthd's tool dispatcher calls the §6 `run` semantics: exec in a booted
cell, pipes + exit code, cwd `/workspace`, timeout enforced host-side. No
network involved in the bridge. Full daemon wiring is Phase 3 tail work after
M2 — the cage suite drives `cell run` directly.

## 8. Cage-proof suite (the G3b evidence)

Scripted suite `tests/cage/` — run on a bench host, writes a pass/fail report
(kernel, systemd, nspawn versions included); **exit 0 only if every check
passes.** Each check runs in a freshly created cell unless stated.

| # | Check | Pass condition |
|---|---|---|
| C1 | direct HTTP | in-cell `curl -m 8 http://93.184.216.34` (proxy env unset, literal IP) **fails** |
| C2 | direct HTTPS | `curl -m 8 https://93.184.216.34` fails |
| C3 | ICMP | `ping -c 2 -W 3 1.1.1.1` fails |
| C4 | raw TCP odd port | `timeout 6 bash -c '</dev/tcp/93.184.216.34/81'` fails |
| C5 | external DNS | direct A query to 8.8.8.8 fails |
| C6 | host escape | gateway `10.91.<n>.1` unreachable on every port **except the proxy port** (that one must connect — it is the door); e.g. port 22/8080 on gateway refused |
| C7 | proxy path works | with default cell proxy env: HTTP fetch of example.com returns 200 **and** the dumb proxy logs the request (log line required — proves the packet took the door, not a hole) |
| C8 | no-proxy fail-closed | cell created with no proxy → C7-style fetch fails **and** C1–C6 still fail; no path exists that bypasses |
| C9 | crash containment | SIGKILL boot unit → host unaffected, `start` works again, workspace intact |
| C10 | env leak + uid map | cell hostname/machine-id ≠ host's; in-cell `id -u` for root reports 0 **while host-side `ps -o uid= -p <nspawn-pid>` shows an unprivileged uid**; file created by keeper in `/workspace`, read host-side, has the recorded mapped owner |
| C11 | cgroup cap | in-cell `memory.max` == configured cap |
| C12 | destroy/rebuild | destroy + create + start + one `cell run` echo < 60 s |

## 9. Non-goals (Phase 3)

No MITM/TLS interception, no credential surrogation, no approval policy
(Phase 4 custos). No container image registry distribution. No hostile
multi-tenant hardening beyond userns mapping (single-operator home install is
the target; gVisor/firecracker for hostile hosting stays a future axis).

## 10. Verification bar

`go vet ./... && go test ./... -count=1` green; cage suite green on **two
bench systems** with report files attached to the PR; `cell doctor` output
included for each. **Gate G3a = Product Owner approval of this v2 text
before code lands** (v1's approval is void). **Gate G3b = suite review +
two-bench green.**
