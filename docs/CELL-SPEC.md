# CELL-SPEC — sandbox contract (`cell/1`)

Status: **FROZEN — approved 2026-09-29.** Changes require a version bump
and re-approval. Defines the **cell**: the per-user sandbox
container that executes tools. The daemon (hearthd) never executes tools in its
own process space; it dispatches them into a running cell.

Terminology: *cell* = one nspawn container + its overlay root + its network +
its cgroup. *Template* = the pristine cached rootfs that forms every cell's
lower layer. *Egress proxy* = the dumb pass-through forwarder (Phase 3) that
later becomes custos (Phase 4).


## 1. Isolation primitive

**systemd-nspawn** with **user namespaces**, pinned to:
`--private-users=pick --private-users-ownership=auto`

- First boot uses `pick`; the picked base is recorded in `cell.json`
  and **pinned** (explicit `--private-users=<base>`) on every later
  boot (erratum 2026-09-30, live-probed: re-`pick` allocates a fresh
  base per boot, orphaning overlay upper/work ownership and breaking
  keeper writes). Manual `--uid-map` remains forbidden.

- `pick` allocates a free subuid/subgid block per cell → in-cell root maps to
  an unprivileged host uid; root-in-cell ≠ anything on the host, by
  mechanism. This is what makes keeper's in-cell sudo (§2) harmless.
- `ownership=auto` uses **idmapped mounts** for the tree: no recursive chown,
  and critically no copy-up of the read-only template (chown mode through an
  overlay would materialize the whole OS into `upper/` at first boot).
  Manual `--uid-map` is **forbidden** — it disables `pick` automation and
  reopens the chown problem. Bind sources (§3) ride the same mechanism:
  kernel idmapped binds (≥ 5.15).
- Kernel floor **5.19** — idmapped mounts over **overlayfs** (the `merged/`
  path) require 5.19, not the 5.15 floor plain idmapped mounts do. Verified by
  live probe, not version
  strings: `doctor` spawns a throwaway cell and asserts a
  `/workspace` write lands owned by the mapped uid (same assertion as C10c).

Host prerequisites (`cell doctor`, hard-fail with actionable messages):
cgroup2 (`stat -fc %T /sys/fs/cgroup` = cgroup2fs), systemd ≥ 254,
`systemd-nspawn` present, subuid/subgid ranges for the invoking user
(`/etc/subuid`), `systemd-machined` reachable (required by `cell run`, §6),
cells dir on a filesystem supporting overlayfs upperdirs (d_type + xattrs —
throwaway-overlay mount probe), idmapped-bind probe above. Anything missing →
refuse to start a cell, never start a half-caged one.

Registration note: nspawn from a raw ssh/sudo session cannot register with
machined (`--register=yes` refused — hit in the spike). `cell start` boots
via a transient **systemd unit** (root-mode or delegated user-scope), never a
bare foreground spawn — the unit is what `cell run` targets.

## 2. Rootfs template + OverlayFS root

- Builder script (root, once per host): debootstrap Ubuntu LTS minimal →
  install `bash curl python3 iputils-ping iproute2 ca-certificates sudo
  systemd systemd-sysv dbus` (erratum 2026-09-29/30, live-probed on noble:
  there is no `systemd-networkd` package — networkd ships inside `systemd`;
  `systemd-sysv` provides `/sbin/init` (PID 1) without which nspawn cannot
  boot the template; `dbus` provides the in-guest machine bus that
  `systemd-run --machine=` (§6 exec) requires, enabled at boot)
  → create uid 1000 user `keeper` (in the `sudo` group
  **inside the cell only**; harmless under §1, lets the agent install a
  package without host privileges) → **enable `systemd-networkd.service`
  inside the template** (minbase doesn't ship it enabled; the guest has no
  other way to configure its interface, §4) → machine-id zeroed → cached
  read-only at `/var/lib/lararium/template/<distro>-<ver>/` with manifest
  `template.json` (distro, version, built-at, sha256 of the tree).
  - **Host-portability of the bake (erratum E5, 2026-10-01, live-probed
    on an Arch-family host, kernel 7.2.8):** (a) every `chroot`
    invocation during the bake must pin the GUEST-canonical PATH
    (`env PATH=/usr/local/sbin:...` inside the chroot) — `chroot`
    resolves argv[0] through the inherited HOST PATH, and on a
    usrmerged noble guest `useradd`/friends live under `/usr/sbin`,
    invisible to a host PATH that omits it (bake died: keeper-user
    `exit status 127`); (b) the read-only seal must NOT chmod
    symlinks — `os.Chmod` follows them, so `/dev/fd → /proc/self/fd`
    reaches the HOST procfs (EPERM on Arch kernels; a wrong-file
    mutation hazard wherever procfs is permissive) and Linux ignores
    symlink permission bits anyway; (c) `debootstrap` requires `dpkg`
    for host-architecture detection: Arch's patched debootstrap falls
    back to a `pacman-conf` mapping that rejects plain `x86_64`
    ("Unknown architecture"), so the bake checks for `dpkg` upfront
    and fails with the remedy (`pacman -S dpkg`) instead.
- **Cell root = OverlayFS**, never a copy:
  - `lowerdir` = template (RO, shared by all cells),
  - `upperdir` = `cells/<id>/upper/`, `workdir` = `cells/<id>/work/` (same fs),
  - merged `cells/<id>/merged/` is what nspawn boots; the overlay is mounted
    host-side (root) **before** the idmapped view is applied (§1 ordering —
    spike must verify this boot path first).
  - **create** = mkdir three dirs → **seed `upper/` from the host while it is
    still unmounted** (guest `.network` drop-in, §4) → overlay mount
    (instantaneous, any filesystem; mutating `upper/` behind a live mount is
    off-limits — undefined-behavior territory);
    **destroy** = unmount + `rm -rf upper` + remove the cell dir
    (erratum 2026-09-30, live-probed: deleting only `upper/` leaked
    815M of snapshots and let `create <same-id>` silently re-inherit
    stale workspace/snapshots state; §6's destroy contract is
    stop + unmount + `rm -rf`, and the crash doctrine scopes
    `workspace/` survival to `kill -9`, not explicit destroy);
    **snapshot** = unmount, rename `upper` → `snapshots/<ts>/upper`, fresh
    upper (historical uppers kept for restore); record `unsupported` only if
    rename itself fails — never faked.
  - `doctor` throwaway-overlay probe proves d_type + xattr (NFS/vfat-backed
    cells dirs fail loudly here, not mysteriously at create).
- Template contains no Lararium binaries and no secrets. Tools reach the cell
  via mounts + stdio (§7), not a shipped agent daemon.

## 3. Filesystem layout

Per-cell host dir `cells/<id>/`: `cell.json` (identity, subnet index, proxy
target, limits, created), `upper/` + `work/` + `merged/`, `snapshots/`,
`workspace/`, `log/`.

Mounts into the running cell:

| Host path | Cell path | Mode | Contents |
|---|---|---|---|
| `cells/<id>/workspace/` | `/workspace` | RW | the agent's working tree |
| `<hearth>` | `/hearth` | RW (v1; RO deferred to custos) | persona + memory tree |
| `cells/<id>/bin/` | `/opt/lararium` | RO, nodev | tool helpers staged by hearthd |

**Bind ownership under userns:** handled by §1's
`ownership=auto` + kernel idmapped binds — no manual uid math in v1. The
contract is behavioral, not mechanical: keeper creates a file under
`/workspace` in-cell → readable via the operator path, owner on disk = the
cell's mapped uid. A cell that boots but cannot write `/workspace` is a
failed cell (C10c gates it).

Everything else is the overlay. `/home/keeper` lives in `upper/` — nothing
there is durable; durability lives only in `/workspace` and `<hearth>`
(crash doctrine, §6).

## 4. Network doctrine (the M2 cage)

**Doctrine: no egress anywhere except through the egress proxy. Fail closed.**
Enforcement is host-side and packet-level; guest cooperation (env vars) is
plumbing, never the boundary.

Topology: nspawn `--network-veth`; cell end `host0`. The veth pair is
created **at spawn** (nspawn's own work; the host end gets nspawn's
`ve-<machine>`-family name, *not* a name we choose) — so **`cell start`
addresses the host end after spawn, resolving the real interface by
mechanism, never by guessing:** read the peer index from inside the
guest netns with netlink — `nsenter --net=/proc/<leader>/ns/net ip -o
link show dev host0` prints `host0@ifM`, the host-namespace ifindex of
our end (globally unique). NOTE: `cat /sys/class/net/host0/iflink`
under `nsenter --net` CANNOT work — sysfs stays the host's mount when
only the netns switches (live-proven 2026-10-01); a bare veth on the
host exposes `iflink` only (no `lower_*` symlink unless enslaved to a
bridge). Then `ip addr
replace 10.91.<n>.1/28 dev <resolved>` + `ip link set ... up`, bounded retry
until the peer appears. `net.sysctl` forwards for the cell subnet pair are
installed once by `cell net-install`. `.network` files are explicitly **not**
used on the **host** (host-side systemd-networkd may not run — Arch often
runs no network manager, Ubuntu desktop runs NetworkManager; both silently
ignore dropped files).
**Guest end:** the template's enabled `systemd-networkd` (§2) applies a
generated `.network` (Match on `Name=host0` — `OriginalName` is a `.link`
directive, invalid under `[Match]` in `.network`; with a wildcard fallback
pinned by reading the real interface name off a probe boot — do not guess)
assigning static `10.91.<n>.2` + default route via `10.91.<n>.1`). Delivery
of the per-cell `.network` file: `cell create` writes it into the cell's
**upper layer** (`upper/etc/systemd/network/10-lararium.network`) pre-boot —
the one drop-in that legitimately lands in `upper/`, since the read-only
template cannot carry per-cell state. The guest `.network`/`.netdev` pair is
part of C7's evidence chain: no IP in-guest fails C7 with the guest's
`ip addr` dump in the report, not a mystery.

**In-cell default route via the gateway is mandatory.** A route-less guest
fails at socket creation (`ENETUNREACH`) before any host rule sees a packet —
the cage is the host's answer to every packet, not the guest's routing table.

**Host nftables** (root, `cell net-install`; atomic full replace of both
tables → idempotent across cell churn; **two tables** because dnat is illegal
in filter hooks):

1. `table ip lararium_nat` — **regenerated whole-table on every start/stop**
   (rules exist for *booted* cells; atomic replace keeps churn race-free):
   one rule per booted cell,
   `iifname "<that cell's resolved host veth>" tcp dport { 80, 443 } dnat to
   <that cell's gateway addr>:<proxy_port>`. DNAT target is the cell's own
   veth gateway address (a local host address → traffic lands in `input`,
   exactly where the filter accepts it). No `route_localnet` hack, no loopback
   DNAT, no map indirection.
2. `table inet lararium_filter`:
   - `input` chain, in order:
     `iifname "v-lar-*" ct status dnat accept` (DNAT'd flows are locally
     destined — after prerouting they traverse **input**, not forward);
     `iifname "v-lar-*" tcp dport <proxy_port> fib daddr . iif type local
     accept` — **the cooperative
     path:** env-obeying clients aim directly at the proxy address and are
     never DNAT'd, so without this explicit accept the intended door itself
     would be dropped; the `fib daddr . iif type local` scope binds the
     accept to the destination being **this interface's own** gateway
     address — without it, Linux's weak host model lets one cell dial a
     neighboring cell's gateway and borrow its proxy (erratum 2026-10-01,
     live-proven cross-cell breach);
     `iifname "v-lar-*" ct state established,related accept`;
     `iifname "v-lar-*" meta l4proto tcp reject with tcp reset`; remaining
     `iifname "v-lar-*" drop`. The gateway address answers on the proxy
     door and nothing else (C6). Rest of input policy untouched. (`v-lar-*` =
     the host-veth naming namespace for cells: `cell start` renames the
     resolved host end to exactly `v-lar-<hash8>` — first 8 hex of
     sha256(cell id) — before addressing; cell.json records the name.
     Erratum 2026-09-30, PO-approved: literal `v-lar-<id>` breaks on ids
     over 9 chars (IFNAMSIZ=15); the glob is unchanged. TCP from cells gets
     `meta l4proto tcp reject with tcp reset`, not silent `drop`, so C6
     observes a refusal (ECONNREFUSED) instead of a timeout (erratum
     2026-10-01: bare `tcp reject` is an nft syntax error — `tcp` needs a
     field; live-verified `meta l4proto tcp reject` loads); non-TCP stays
     drop. `forward`
     chain stays a pure drop — a forward-path reset would leak host
     topology.)
   - **Firewall interop (erratum E3, 2026-10-01, bench #2):** nft and
     iptables register as **separate hook chains on the same hook** and
     the strictest verdict wins — an nft `accept` cannot bypass an
     iptables `INPUT` policy `DROP` (ufw/firewalld/legacy stacks drop
     cooperative-path SYNs even after our filter table counts them).
     `cell net-install` therefore ALSO maintains a tagged iptables user
     chain (`LARARIUM-INPUT`, comment `lararium-cage-interop`,
     Docker's shape): accept `ctstate DNAT` and `iif v-lar-+ daddr
     10.91.0.0/16 tcp dport <proxy_port>`; hooked into `INPUT` at
     position 1, idempotently (`-C` before `-I`), skipped when the
     `iptables` binary is absent (nft-only hosts). Scope is exactly the
     two cooperative flows — every other cell verdict still belongs to
     the nft filter table. `net-remove` unwires it.
   - **Boot-window + ingress hardening (erratum E4, 2026-10-01, review
     round 2):** (a) nspawn names the host end of its `--network-veth`
     `ve-<machine>` and brings it UP *before* our rename to `v-lar-*`
     lands — during that window the interface matches none of the
     `v-lar-*` rules, so the filter table additionally rejects
     everything from `{ "ve-*", "veth-*", "vb-*" }` except ICMP/ICMPv6
     (neighbor discovery stays alive; no ports exposed; forward path
     fully closed for those names). (b) The `forward` chain drops BOTH
     directions (`iifname` AND `oifname "v-lar-*"`): with
     `ip_forward=1`, an unsolicited external packet routed toward
     `10.91.<n>.2` would otherwise satisfy policy-accept — cells are
     egress-only. (c) The proxy destination floor covers the host's
     OWN non-loopback interface addresses (`net.InterfaceAddrs()`,
     enumerated per request — interfaces churn): dialing the host's
     LAN/bridge/tunnel IP from a cell is the same escape loopback was
     blocked for. Rebinding a public name to a local address is caught
     by the any-answer check.
   - `forward` chain: `iifname "v-lar-*" drop` — unconditional catch-all for
     everything the nat table didn't divert (ICMP, DNS, odd ports); since no
     non-DNAT path is ever forwarded, nothing needs masquerading.
- **Fail-closed:** no proxy configured → filter table installed, **nat table
  omitted** → total isolation. Drops are unconditional; DNAT is the only
  hole.
- Broadcast/multicast/LLMNR die in the same chains. Loopback in-cell is free
  (loopback is not the boundary).

**Proxy contract (Phase 3, stated honestly):** the dumb proxy is a
**forward proxy** — absolute-form `GET http://…` + `CONNECT` for TLS, with an
append-only access log (C7 evidence). Cells get
`HTTP_PROXY/HTTPS_PROXY=http://<gateway_addr>:<proxy_port>` (+ lowercase
twins, `NO_PROXY=localhost,127.0.0.1`) in the environment. DNAT is a
**no-bypass guarantee, not a usability guarantee**: a client that ignores the
env and aims 80/443 out is DNAT'd to the proxy and *refused* (a
relative-form request has no destination at a forward proxy) — a connection
failure, never an exit (check C7b). True transparent handling
(SO_ORIGINAL_DST/TPROXY) is explicitly out of scope (§9); custos (Phase 4)
replaces this contract wholesale. In-cell DNS is dropped: resolution for
proxied fetches happens at proxy level (absolute URL / CONNECT hostname).

Phase 3 proves the **cage**, not the policy: L7 rules, DNS re-resolution,
SSRF pinning, CONNECT filtering are custos.

## 5. Resource limits

Properties on the transient unit that *boots* the cell (§6): `MemoryMax`
default 8G, `CPUQuota` default 200%, `TasksMax` default 512 (`[cell]` in
lararium.yaml, per-cell override in cell.json). systemd enforces; our code
only asks. `IOWeight` default.

## 6. Lifecycle — boot vs exec (two operations, two commands)

- **`cell start <id>` boots the container.** `systemd-nspawn --machine=<id>
  --keep-unit --boot` (template `/sbin/init`) inside a transient systemd unit
  (`systemd-run --unit=lararium-cell-<id>`, root-mode or delegated
  user-scope) carrying §5 limits, §1 flags, §3 binds. **`--boot` is
  mandatory** (erratum 2026-09-30, live-probed: without it nspawn's default
  "init" is an interactive shell — no in-guest systemd, no machine bus,
  `cell run` dead). **`--keep-unit` is
  mandatory:** without it machined extracts the payload (in-cell PID 1) into
  a separate `machine-*.scope`, so §5 limits would cage only the supervisor
  and a crash-kill of the unit would orphan the still-running payload — the
  exact opposite of §5/§6's enforcement and containment claims. With
  `--keep-unit`, limits and lifecycle apply to the whole tree; `cell run`
  targets the registered machine name. Host veth setup (§4) runs **after
  spawn** (nspawn creates the pair; resolve → rename → address, bounded
  retry). Props: `Restart=no`, `KillMode=mixed`.
  Healthy = unit active **and** machine registered with machined.
- **`cell run <id> [--cwd P] [--timeout S] -- <cmd>` execs inside a booted
  cell** via machined. `--machine` **never boots**: unregistered → error
  `cell not running`, no implicit start. stdin/stdout/stderr piped, exit
  code propagated, default cwd `/workspace`; host-enforced timeout kills the
  exec scope only, not the cell.
- Other CLI: `build-template` (root), `create`, `stop` (unit stop + unmount
  merged), `status [id]`, `snapshot`, `destroy` (stop + unmount + `rm -rf`),
  `doctor`. create/start/stop idempotent.

**Crash doctrine:** `kill -9` the boot unit's main process → kernel tears
down netns + cgroup; `workspace/` and `<hearth>` survive on the host;
`upper/` disposable (possibly dirty — plain destroy/create). `start` again
yields a working cell. Nothing auto-restarts.

## 7. Tool bridge (v1)

hearthd's tool dispatcher calls §6 `run` semantics: exec in a booted cell,
pipes + exit code, cwd `/workspace`, timeout enforced host-side. No network
in the bridge. Full daemon wiring is Phase 3 tail work after M2 — the cage
suite drives `cell run` directly.

## 8. Cage-proof suite

`tests/cage/` — scripted; one report per test host (kernel, systemd, nspawn, nft
versions in the header); **exit 0 only if every check passes.** Fresh cell
per check unless stated.

| # | Check | Pass condition |
|---|---|---|
| C1 | direct HTTP | in-cell `curl -m 8 http://93.184.216.34` (proxy env unset, literal IP) **fails** |
| C2 | direct HTTPS | `curl -m 8 https://93.184.216.34` fails |
| C3 | ICMP | `ping -c 2 -W 3 1.1.1.1` fails |
| C4 | raw TCP odd port | `timeout 6 bash -c '</dev/tcp/93.184.216.34/81'` fails |
| C5 | external DNS | direct A query to 8.8.8.8 fails |
| C6 | host escape | gateway `10.91.<n>.1` refused on every port **except the proxy port**, which must connect (it is the door) — at minimum :22 and :8080 refused. For a **no-proxy** cell (C8) the door is closed too: **every** port including the proxy port must be refused |
| C7 | proxy path works | default cell env: HTTP fetch of example.com returns 200 (allowing a bounded post-start settle for veth addressing + guest networkd, e.g. retry ≤ 15 s) **and** the dumb proxy's access log gains a matching line (proves the packet took the door, not a hole) |
| C7b | bypass refused | in-cell client ignoring proxy env, relative-form request to 80 → connection refused/error by the proxy (NOT a 200); C1–C5 still fail in the same cell |
| C8 | no-proxy fail-closed | cell created with no proxy → C7-style fetch fails **and** C1–C6 still fail; no path bypasses |
| C9 | crash containment | SIGKILL boot unit → host unaffected, `start` works again, workspace intact |
| C10 | uid map + binds | (a) in-cell `id -u keeper` == 1000; (b) `cell run` a `sleep 300`, locate its host pid (unit cgroup walk), **host-visible uid == cell.json's recorded `subuid_base` + 1000** — not 0, not the operator uid. (`cell start` records the picked block in cell.json from nspawn's allocation — the test asserts against that record, no re-guessing; `--keep-unit` keeps the payload in the unit's cgroup so the walk is deterministic. The nspawn supervisor is legitimately host root and is *not* the subject); (c) keeper writes a file in `/workspace` in-cell → readable via the operator path, on-disk owner is the mapped uid |
| C11 | cgroup cap enforced behaviorally | in-cell python allocates MemoryMax + 256 MB → killed (exit 137) or boot unit's `memory.events` `oom_kill` counter increments. Reading `memory.max` in-cell is **not** accepted as evidence (cgroup namespace root reads `max` even when enforced) |
| C12 | destroy/rebuild | destroy + create + start + one `cell run` echo < 60 s |

## 9. Non-goals (Phase 3)

No MITM/TLS interception, no credential surrogation, no approval policy
(Phase 4 custos). No container image registry distribution. No transparent
proxy interception (SO_ORIGINAL_DST/TPROXY) — see §4's honest contract. No
hostile multi-tenancy beyond userns mapping (gVisor/firecracker remain the
hostile-hosting axis).

## 10. Verification bar

`go vet ./... && go test ./... -count=1` green, and the cage suite (§8)
green on every supported platform — exit 0 across C1–C12, with the report
header recording the kernel/systemd/nspawn/nft versions it ran against, so
any user can reproduce a result on their own machine. `cell doctor` passes
cleanly before any cell operation.
