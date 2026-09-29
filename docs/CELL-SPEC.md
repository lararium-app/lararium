# CELL-SPEC — sandbox contract (`cell/1`)

Status: DRAFT v2.3 — three automated review rounds applied (see changelogs);
supersedes v2.2. **v1/v2 approvals void — fresh sign-off required (§10).**
Scope: Phase 3 of BUILD-PROCESS.md. Defines the **cell**: the per-user sandbox
container that executes tools. The daemon (hearthd) never executes tools in its
own process space; it dispatches them into a running cell.

Terminology: *cell* = one nspawn container + its overlay root + its network +
its cgroup. *Template* = the pristine cached rootfs that forms every cell's
lower layer. *Egress proxy* = the dumb pass-through forwarder (Phase 3) that
later becomes custos (Phase 4).

Changelog v2.2→v2.3 (third agy round — 4 findings, all accepted):
1. §4 — **coop-path hole (the serious one):** env-cooperating clients aim
   directly at the proxy port and are never DNAT'd, so a `ct status dnat`-only
   accept left the intended door itself dropped. Input chain now accepts: dnat
   traffic, explicit `proxyport` tcp, established/related — then drops.
2. §4/§6 — veth timing corrected: `--network-veth` creates the pair *at
   spawn*; host-end addressing runs post-spawn against the interface nspawn
   actually created (resolved via peer-symlink readlink, never a guessed
   name), with bounded retry; C7 allows that settle window.
3. §2/§4 — nat-table regeneration pinned to **start/stop** (not
   create/destroy) since rules cover *booted* cells.
4. §2/§4 — guest `.network` written into `upper/` **before** the overlay is
   mounted (create order pinned: mkdir → seed upper → mount); mutating a
   mounted upperdir from the host is undefined-behavior territory.

Changelog v2.1→v2.2 (agy confirmation pass: all 7 prior fixes confirmed, then
6 new findings, all real, all fixed):
1. §6 — `--keep-unit` mandated: machined otherwise migrates the payload into
   a separate `machine-*.scope`, so cgroup limits would cage only the
   supervisor and C9's kill would orphan the still-running container.
2. §4 — nat rules are per-cell (`iifname "v-cell-<id>"` → that cell's own
   gateway address), whole-table atomic replace on churn; the single global
   rule as written could not address per-`<n>` gateways.
3. §4 — per-cell guest `.network` delivery pinned: written into `upper/`
   pre-boot by `cell create` (template is RO; nothing else wrote it before).
4. C10b — subuid base comes from cell.json (recorded at start), not guessed;
   `pick` allocates dynamically.
5. §6 — `StopKillMode=mixed` is not a systemd property → `KillMode=mixed`.
6. §1 — kernel floor raised to 5.19: idmapped mounts over overlayfs landed
   later than plain idmapped mounts (5.15).

Changelog v2→v2.1 (from the automated review gate; all seven verified real):
1. §4 — DNAT was in a `forward` hook, where nftables rejects it: DNAT lives
   only in `nat`-type prerouting/output. Rewritten: `ip` table, nat-prerouting
   for the DNAT; consequence traced — DNAT'd flows are locally destined and
   traverse **input**, so the input policy accepts exactly `ct status dnat`
   and forward becomes a pure drop-all.
2. §4 — DNAT-to-dumb-proxy honesty: transparently intercepted clients send
   relative-form requests a forward proxy cannot answer. The contract is now
   explicitly *no-bypass guarantee, not usability*: env-ignoring traffic gets
   refused (fail-closed), never served; real transparent handling is scoped
   out (§9) with a new check C7b pinning the refusal.
3. §1/§3 — `--private-users=pick` and manual `--uid-map` are mutually
   exclusive; manual maps dropped. Spec now pins `pick` +
   `--private-users-ownership=auto` (idmapped mounts), because chown-mode
   would recursively copy-up the entire read-only template into `upper/` at
   every boot.
4. §2/§4 — `.network` files depend on systemd-networkd running (not the case
   on Ubuntu-desktop/typical-Arch hosts): host-end addressing moves to
   idempotent `ip` commands run by `cell start`; the guest half is fixed by
   installing + enabling `systemd-networkd` **in the template**.
5. C10 — asserted the nspawn supervisor's uid; that process is legitimately
   host root. Rewritten to target payload processes (mapped uid of a known
   exec'd process as seen from the host).
6. C11 — in-cell `memory.max` reads `max` under cgroup namespace even when
   the cap is real. Replaced with a behavioral enforcement probe (allocation
   must OOM).
7. §8 — machine-id check kept; kernel floor for idmapped bind mounts pinned
   (5.15+) with a live `doctor` probe instead of a version-string trust.

Changelog v1→v2 (architecture review): userns mandated; OverlayFS replaces
reflink copies (ext4 has no reflink); in-cell default route (route-less
guests fail ENETUNREACH before any host rule runs); `cell start` (boot) vs
`cell run` (exec) split (`systemd-run --machine` execs, never boots).

---

## 1. Isolation primitive

**systemd-nspawn** with **user namespaces**, pinned to:
`--private-users=pick --private-users-ownership=auto`

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
  path) require 5.19, not the 5.15 floor plain idmapped mounts do; both
  benches (6.12, 7.2) clear it easily. Verified by live probe, not version
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
  systemd-networkd` → create uid 1000 user `keeper` (in the `sudo` group
  **inside the cell only**; harmless under §1, lets the agent install a
  package without host privileges) → **enable `systemd-networkd.service`
  inside the template** (minbase doesn't ship it enabled; the guest has no
  other way to configure its interface, §4) → machine-id zeroed → cached
  read-only at `/var/lib/lararium/template/<distro>-<ver>/` with manifest
  `template.json` (distro, version, built-at, sha256 of the tree).
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
    **destroy** = unmount + `rm -rf upper`;
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
mechanism, never by guessing:** read the guest ifindex via machined/`ip -o
link` peer-symlink (`.../lowerindex`) or the unit's netns, then `ip addr
replace 10.91.<n>.1/28 dev <resolved>` + `ip link set ... up`, bounded retry
until the peer appears. `net.sysctl` forwards for the cell subnet pair are
installed once by `cell net-install`. `.network` files are explicitly **not**
used on the **host** (host-side systemd-networkd may not run — Arch often
runs no network manager, Ubuntu desktop runs NetworkManager; both silently
ignore dropped files).
**Guest end:** the template's enabled `systemd-networkd` (§2) applies a
generated `.network` (Match on `OriginalName=host0`, with a wildcard fallback
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
     `iifname "v-lar-*" tcp dport <proxy_port> accept` — **the cooperative
     path:** env-obeying clients aim directly at the proxy address and are
     never DNAT'd, so without this explicit accept the intended door itself
     would be dropped;
     `iifname "v-lar-*" ct state established,related accept`;
     `iifname "v-lar-*" drop`. The gateway address answers on the proxy
     door and nothing else (C6). Rest of input policy untouched. (`v-lar-*` =
     the host-veth naming namespace for cells: `cell start` renames the
     resolved host end to exactly `v-lar-<id>` — the glob above matches it —
     before addressing; cell.json records the name.)
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
  --keep-unit` (template `/sbin/init`) inside a transient systemd unit
  (`systemd-run --unit=lararium-cell-<id>`, root-mode or delegated
  user-scope) carrying §5 limits, §1 flags, §3 binds. **`--keep-unit` is
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

## 8. Cage-proof suite (the G3b evidence)

`tests/cage/` — scripted; one report per bench (kernel, systemd, nspawn, nft
versions in the header); **exit 0 only if every check passes.** Fresh cell
per check unless stated.

| # | Check | Pass condition |
|---|---|---|
| C1 | direct HTTP | in-cell `curl -m 8 http://93.184.216.34` (proxy env unset, literal IP) **fails** |
| C2 | direct HTTPS | `curl -m 8 https://93.184.216.34` fails |
| C3 | ICMP | `ping -c 2 -W 3 1.1.1.1` fails |
| C4 | raw TCP odd port | `timeout 6 bash -c '</dev/tcp/93.184.216.34/81'` fails |
| C5 | external DNS | direct A query to 8.8.8.8 fails |
| C6 | host escape | gateway `10.91.<n>.1` refused on every port **except the proxy port**, which must connect (it is the door) — at minimum :22 and :8080 refused |
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

## 10. Verification bar & review gates

`go vet ./... && go test ./... -count=1` green; cage suite green on **two
bench systems**, reports attached to the PR; `cell doctor` output included
per bench. **Gate G3a = Product Owner approval of this text before code
lands** (v1/v2 approvals void). **Gate G3b = suite review + two-bench green.**

Every artifact reaching the Product Owner has passed the standard review
bundle (BUILD-PROCESS §Review gates): Hermes static sweep, fresh-context
Hermes reviewer, and an independent **Antigravity CLI headless pass**
(`agy --model gemini-3.1-pro-high -p=…`). Findings bundled, conflicts
flagged; the PO decides once on the bundle.
