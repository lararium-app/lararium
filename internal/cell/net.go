package cell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Network doctrine per CELL-SPEC §4 (FROZEN, incl. errata E1/E2 approved
// 2026-09-30): each cell gets an nspawn veth; the host end is renamed to
// v-lar-<hash8> (IFNAMSIZ-safe), addressed 10.91.<n>.1/28, and the host
// nftables cage is the ONLY egress path (DNAT 80/443 to the per-cell
// dumb proxy; everything else reject/drop). Fail closed.

const (
	// SubnetBase is the third octet base: cell n uses 10.91.<n>.0/28.
	SubnetBase = 10
	// MaxSubnetIndex is inclusive: n in [0, 254] (255 rows are
	// reserved for headroom against tooling that dislikes .255).
	MaxSubnetIndex = 254
	// DefaultProxyPort — C6 refuses :22/:8080 specifically, so the
	// proxy door must be neither.
	DefaultProxyPort = 3128

	nftRunDir = "/run/lararium"

	// vethResolveTimeout bounds the wait for nspawn's veth peer to
	// appear after spawn.
	vethResolveTimeout = 30 * time.Second
)

// vethHostName returns the host-end interface name for a cell id:
// "v-lar-" + first 8 hex of sha256(id) = 14 chars <= IFNAMSIZ-1
// (erratum E1: literal v-lar-<id> breaks on ids over 9 chars).
func vethHostName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "v-lar-" + hex.EncodeToString(sum[:])[:8]
}

// GatewayIP returns the host-end address for subnet index n.
func gatewayIP(n int) string { return fmt.Sprintf("10.91.%d.1", n) }

// cellIP returns the guest-end address for subnet index n.
func cellIP(n int) string { return fmt.Sprintf("10.91.%d.2", n) }

// --- nft renderers (pure functions: table-driven unit tests, no root) ---

// renderFilterNft renders the static filter table for the proxy port.
// Rule order IS the contract (spec §4):
//  1. DNAT'd flows are locally destined and traverse input: accept.
//  2. Cooperative path (env-obeying client dialing its own gateway's
//     proxy port): accept, scoped by `fib daddr . iif type local` —
//     without the fib scope, the weak host model lets one cell dial a
//     neighboring cell's gateway and borrow its proxy (live-proven
//     breach 2026-10-01).
//  3. Established/related: accept.
//  4. All other TCP from cells: reject with tcp reset (erratum E2 —
//     bare `tcp reject` is an nft syntax error; `meta l4proto tcp`
//     loads, live-verified).
//  5. Terminal catch-all: drop (UDP/ICMP never fall through to the
//     host's own input policy).
//
// forward: unconditional drop for cell interfaces (a forward-path reset
// would leak host topology, so plain drop).
func renderFilterNft(proxyPort int) string {
	return fmt.Sprintf(`destroy table inet lararium_filter
table inet lararium_filter {
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "v-lar-*" ct status dnat accept
    iifname "v-lar-*" tcp dport %d fib daddr . iif type local accept
    iifname "v-lar-*" ct state established,related accept
    iifname "v-lar-*" meta l4proto tcp reject with tcp reset
    iifname "v-lar-*" drop
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "v-lar-*" drop
  }
}
`, proxyPort)
}

// natRule is one booted proxy-enabled cell's DNAT rule input.
type natRule struct {
	Veth      string // resolved host-end interface name
	GatewayIP string // the cell's own gateway address (10.91.<n>.1)
	ProxyPort int
}

// renderNatNft renders the whole nat table from the live set of booted
// proxy-enabled cells. Whole-table atomic replace (destroy+create in ONE
// nft file) — rules exist for booted cells only, so crashed cells' rules
// vanish by construction; no per-rule diff/flush (handle-based deletes
// are fragile under churn).
func renderNatNft(rules []natRule) string {
	var b strings.Builder
	b.WriteString("destroy table ip lararium_nat\n")
	if len(rules) == 0 {
		// Fail-closed: zero proxy-enabled booted cells -> table
		// absent (the destroy above removed it; nothing follows).
		return b.String()
	}
	b.WriteString(`table ip lararium_nat {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
`)
	for _, r := range rules {
		fmt.Fprintf(&b,
			"    iifname %q tcp dport { 80, 443 } dnat to %s:%d\n",
			r.Veth, r.GatewayIP, r.ProxyPort)
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// applyNftFile runs one nft -f batch atomically. `nft destroy table` of
// a nonexistent table exits 0 (live-verified nft 1.1.3), so the replace
// idiom is idempotent from empty.
func (s *Store) applyNftFile(path string) error {
	out, err := s.runner.RunCombined("nft", "-f", path)
	if err != nil {
		return fmt.Errorf("nft -f %s: %s", filepath.Base(path), strings.TrimSpace(string(out)))
	}
	return nil
}

// writeApplyNft renders to a private file under /run/lararium and applies
// it (0600: the nat rules list host topology; never world-readable).
func (s *Store) writeApplyNft(name, content string) error {
	if err := os.MkdirAll(nftRunDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(nftRunDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return s.applyNftFile(path)
}

// --- flock: one lock for subnet allocation AND nat regeneration ---

// lockFile returns the preferred flock path: /run/lock for the real
// (root) fleet. Unprivileged callers (unit tests, non-root doctor) fall
// back to Root — openCellLock handles the fallback when /run/lock
// exists but is not writable. Production is always root; the shared
// lock only serializes correctly when all writers agree on the path.
func (s *Store) lockFile() string {
	if err := os.MkdirAll("/run/lock", 0o755); err == nil {
		if f, ferr := os.OpenFile("/run/lock/lararium-cell.lock", os.O_CREATE|os.O_RDWR, 0o644); ferr == nil {
			f.Close()
			return "/run/lock/lararium-cell.lock"
		}
	}
	return filepath.Join(s.Root, "cell.lock")
}

// withCellLock runs fn under an exclusive flock covering the whole
// read-modify-write window (scan-allocate-write; concurrent creates
// cannot collide, concurrent starts/stop churn is last-writer-correct).
func (s *Store) withCellLock(fn func() error) error {
	path := s.lockFile()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open cell lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock cell lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // unlock is best-effort at scope end
	return fn()
}

// --- subnet allocation ---

// hostRoute is one IPv4 route relevant to collision checks.
type hostRoute struct {
	Network string // CIDR
	Dev     string
}

// hostIPv4Routes parses `ip -o -4 route show`.
func (s *Store) hostIPv4Routes() ([]hostRoute, error) {
	stdout, _, err := s.runner.Run("ip", "-o", "-4", "route", "show")
	if err != nil {
		return nil, fmt.Errorf("ip route show: %w", err)
	}
	var out []hostRoute
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		dst := fields[0]
		if !strings.Contains(dst, "/") {
			dst += "/32"
		}
		dev := ""
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				dev = fields[i+1]
				break
			}
		}
		out = append(out, hostRoute{Network: dst, Dev: dev})
	}
	return out, nil
}

// overlaps28 reports whether route r overlaps the cell subnet
// 10.91.<n>.0/28. Overlap iff one prefix contains the other, which for
// networks reduces to: one network address lies inside the other
// prefix. The default route (any prefix length 0) is NOT a conflict —
// every working host has one; only concrete subnet collisions block.
func (r hostRoute) overlaps28(n int) bool {
	_, theirNet, err := net.ParseCIDR(r.Network)
	if err != nil {
		return false // "default", "unreachable", non-IPv4 shapes
	}
	their := theirNet.IP.To4()
	if their == nil {
		return false // IPv6 never overlaps
	}
	ones, _ := theirNet.Mask.Size()
	if ones == 0 {
		return false
	}
	if n < 0 || n > MaxSubnetIndex {
		return false
	}
	ourNet := &net.IPNet{
		IP:   net.IPv4(byte(SubnetBase), 91, byte(n), 0),
		Mask: net.CIDRMask(28, 32),
	}
	return ourNet.Contains(their) || theirNet.Contains(ourNet.IP)
}

// allocateSubnetIndex finds the first free subnet index for a new cell.
// Caller must hold withCellLock. A candidate blocked by a foreign host
// route is SKIPPED (not fatal): "no free subnet" only when all 255
// candidates are claimed or conflicted, with the last conflict named.
func (s *Store) allocateSubnetIndex() (int, error) {
	used := map[int]bool{}
	ids, err := s.List()
	if err != nil {
		return -1, err
	}
	for _, id := range ids {
		c, err := s.Load(id)
		if err != nil {
			continue
		}
		// Only post-T5 records claim a subnet (legacy records have
		// SubnetIndex 0 but no network state; the first-start
		// migration rewrites the record on next boot).
		if c.Gateway != "" && c.SubnetIndex >= 0 && c.SubnetIndex <= MaxSubnetIndex {
			used[c.SubnetIndex] = true
		}
	}
	routes, err := s.hostIPv4Routes()
	if err != nil {
		return -1, err
	}
	lastConflict := ""
	for n := 0; n <= MaxSubnetIndex; n++ {
		if used[n] {
			continue
		}
		conflict := ""
		for _, r := range routes {
			// Our own booted cells' links never block the next
			// create (their route is ON v-lar-*).
			if strings.HasPrefix(r.Dev, "v-lar-") {
				continue
			}
			if r.overlaps28(n) {
				conflict = fmt.Sprintf("%s dev %s", r.Network, r.Dev)
				break
			}
		}
		if conflict != "" {
			lastConflict = conflict
			continue
		}
		return n, nil
	}
	if lastConflict != "" {
		return -1, fmt.Errorf(
			"no free cell subnet: all candidates claimed or blocked by host routes (last conflict: %s)",
			lastConflict)
	}
	return -1, fmt.Errorf("no free cell subnet (0-%d all in use)", MaxSubnetIndex)
}

// releaseSubnet drops the cell's subnet claim: destroy deletes cell.json
// wholesale, so the claim is released with it — nothing to do under the
// lock beyond regenerating nat (done by the caller). Kept as a named
// seam so destroy/stop call sites state intent.
func releaseSubnet() {}

// --- guest-side config writers (run on upper/ BEFORE overlay mount) ---

// guestNetworkFile renders the in-cell systemd-networkd config (spec §4):
// Match Name=host0 (OriginalName is a .link directive — invalid under
// [Match] in .network, live-verified), static address + default route via
// the gateway, no DNS (resolution happens at proxy level), IPv6/RA/LLMNR
// off — the guest must not improvise paths around the cage.
func guestNetworkFile(n int) string {
	return fmt.Sprintf(`[Match]
Name=host0

[Network]
Address=%s/28
Gateway=%s
IPv6AcceptRA=no
LLMNR=no
MulticastRouting=no
`, cellIP(n), gatewayIP(n))
}

// proxyEnvFile renders /etc/environment content for a proxy-enabled cell
// (both cases; NO_PROXY for loopback). Empty string = no proxy.
func proxyEnvFile(n int, proxyPort int) string {
	url := "http://" + net.JoinHostPort(gatewayIP(n), strconv.Itoa(proxyPort))
	return fmt.Sprintf(`HTTP_PROXY=%s
HTTPS_PROXY=%s
http_proxy=%s
https_proxy=%s
NO_PROXY=localhost,127.0.0.1
no_proxy=localhost,127.0.0.1
`, url, url, url, url)
}

// proxyEnvPairs returns the same vars as KEY=VALUE slices for
// `cell run` (--setenv on nspawn reaches PID 1 ONLY; exec units need
// them passed explicitly or C7 fails).
func proxyEnvPairs(n int, proxyPort int) []string {
	url := "http://" + net.JoinHostPort(gatewayIP(n), strconv.Itoa(proxyPort))
	return []string{
		"HTTP_PROXY=" + url,
		"HTTPS_PROXY=" + url,
		"http_proxy=" + url,
		"https_proxy=" + url,
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=localhost,127.0.0.1",
	}
}

// writeGuestNetwork drops 10-lararium.network into the cell's upper layer.
// Must run while the overlay is NOT mounted (upper mutation under a live
// mount yields stale dentries — T4 invariant).
func (s *Store) writeGuestNetwork(id string, n int) error {
	dir := filepath.Join(s.UpperDir(id), "etc/systemd/network")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "10-lararium.network"),
		[]byte(guestNetworkFile(n)), 0o644)
}

// writeGuestProxyEnv drops /etc/environment into upper (proxy-enabled
// cells only). Same offline-only constraint as writeGuestNetwork.
func (s *Store) writeGuestProxyEnv(id string, n int, proxyPort int) error {
	dir := filepath.Join(s.UpperDir(id), "etc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "environment"),
		[]byte(proxyEnvFile(n, proxyPort)), 0o644)
}

// --- host veth resolution (spec §4: by mechanism, never name-guessing) ---

// resolveHostVeth finds the host end of the cell's nspawn veth pair:
//  1. leader PID from machined,
//  2. inside the guest netns, the peer index of host0 (`ip -o link`:
//     `host0@if<N>` / iflink — a cross-netns veth reports the PEER's
//     ifindex as seen in ITS namespace; for the guest-side end that is
//     the host namespace),
//  3. on the host, the interface with that ifindex.
//
// Bounded retry: the peer appears only after nspawn finishes netns
// setup.
func (s *Store) resolveHostVeth(id string) (string, error) {
	deadline := time.Now().Add(vethResolveTimeout)
	var lastErr string
	for {
		leader, err := s.leaderPID(id)
		if err != nil {
			lastErr = err.Error()
		} else if name, perr := s.resolveHostVethByLeader(leader); perr == nil {
			return name, nil
		} else {
			lastErr = perr.Error()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("could not resolve host veth for cell %s within %v: %s",
				id, vethResolveTimeout, lastErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// leaderPID asks machined for the container leader (PID 1 of the guest).
func (s *Store) leaderPID(id string) (int, error) {
	stdout, _, err := s.runner.Run("machinectl", "show", "-p", "Leader", id)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if v, ok := strings.CutPrefix(line, "Leader="); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("bad Leader value %q", v)
			}
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no Leader for %s", id)
}

// resolveHostVethByLeader maps leader PID -> host-end interface name.
func (s *Store) resolveHostVethByLeader(leader int) (string, error) {
	// Guest side: peer ifindex of host0. PROVEN LIVE: `cat
	// /sys/class/net/host0/iflink` via `nsenter --net` CANNOT work —
	// sysfs stays the host's mount; only the netns switches. Netlink
	// inside the guest netns does: `ip -o link show dev host0` prints
	// "N: host0@ifM: <flags>..." where M is the host-namespace
	// ifindex of our end (ifindices are globally unique).
	stdout, stderr, err := s.runner.Run("nsenter", "--net=/proc/"+
		strconv.Itoa(leader)+"/ns/net", "ip", "-o", "link", "show", "dev", "host0")
	if err != nil {
		return "", fmt.Errorf("read guest peer index: %s", strings.TrimSpace(
			string(append(stdout, stderr...))))
	}
	peerIdx, err := parsePeerIfindex(string(stdout))
	if err != nil {
		return "", err
	}
	// Host side: find the interface whose ifindex is peerIdx.
	linkOut, _, err := s.runner.Run("ip", "-o", "link", "show")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(linkOut), "\n") {
		// "12: veth123@if7: <FLAGS>..."
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if strings.TrimSpace(line[:colon]) == peerIdx {
			rest := line[colon+1:]
			name := strings.TrimSpace(rest)
			if at := strings.IndexByte(name, '@'); at >= 0 {
				name = name[:at]
			}
			if q := strings.IndexByte(name, ':'); q >= 0 {
				name = name[:q]
			}
			if name == "" {
				continue
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("no host interface with ifindex %s yet", peerIdx)
}

// parsePeerIfindex extracts M from `ip -o link` output for a veth
// end: "N: host0@ifM: <FLAGS>...". A bare name without "@if" means
// no peer index is visible yet (peer gone or not created) — error,
// the caller's bounded retry handles it.
func parsePeerIfindex(out string) (string, error) {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	at := strings.Index(line, "@if")
	if at < 0 {
		return "", fmt.Errorf("no @if peer marker in %q", line)
	}
	rest := line[at+3:]
	if end := strings.IndexByte(rest, ':'); end >= 0 {
		rest = rest[:end]
	}
	idx := strings.TrimSpace(rest)
	if _, err := strconv.Atoi(idx); err != nil {
		return "", fmt.Errorf("bad peer ifindex in %q: %w", line, err)
	}
	return idx, nil
}

// configureHostVeth brings the resolved host end up as the cell gateway:
// down FIRST (renaming an IFF_UP interface fails EBUSY — live review
// finding), rename to the deterministic name, address, up, per-veth
// sysctls. Idempotent (addr replace; rename skipped when already named).
func (s *Store) configureHostVeth(id string, subnet int) error {
	resolved, err := s.resolveHostVeth(id)
	if err != nil {
		return err
	}
	target := vethHostName(id)

	if resolved != target {
		if _, stderr, err := s.runner.Run("ip", "link", "set", resolved, "down"); err != nil {
			return fmt.Errorf("link down %s: %s", resolved, strings.TrimSpace(string(stderr)))
		}
		if _, stderr, err := s.runner.Run("ip", "link", "set", resolved, "name", target); err != nil {
			return fmt.Errorf("rename %s -> %s: %s", resolved, target, strings.TrimSpace(string(stderr)))
		}
	}
	if _, stderr, err := s.runner.Run("ip", "addr", "replace",
		fmt.Sprintf("%s/28", gatewayIP(subnet)), "dev", target); err != nil {
		return fmt.Errorf("addr on %s: %s", target, strings.TrimSpace(string(stderr)))
	}
	// Per-veth values cannot exist at net-install time (spec §4;
	// review finding) — set them at start, BEFORE link up: IPv6
	// disabled kills the fe80 surface (agy F10: a host process on
	// :::3128 could otherwise be reached through the coop accept's
	// fib-local match); rp_filter loose (2) makes the cage not care
	// what the host's global rp_filter says (bench #2: a host-wide
	// rp_filter=1 must not become a coop-path dependency);
	// accept_source_route off. Global default.disable_ipv6 is
	// DELIBERATELY untouched — it would silently break every other
	// veth on the host (docker6, etc.).
	type sysctlKV struct{ family, key, val string }
	for _, kv := range []sysctlKV{
		{"ipv4", "forwarding", "1"},
		{"ipv4", "proxy_arp", "0"},
		{"ipv4", "send_redirects", "0"},
		{"ipv4", "accept_source_route", "0"},
		{"ipv4", "rp_filter", "2"},
		{"ipv6", "accept_source_route", "0"},
		{"ipv6", "disable_ipv6", "1"},
		{"ipv6", "accept_ra", "0"},
		{"ipv6", "accept_ra_rt_info_max_plen", "0"},
		{"ipv6", "router_solicitations", "0"},
	} {
		p := "/proc/sys/net/" + kv.family + "/conf/" + target + "/" + kv.key
		if err := os.WriteFile(p, []byte(kv.val), 0o644); err != nil {
			return fmt.Errorf("sysctl %s.%s=%s: %w", kv.family, kv.key, kv.val, err)
		}
	}
	if _, stderr, err := s.runner.Run("ip", "link", "set", target, "up"); err != nil {
		return fmt.Errorf("link up %s: %s", target, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// teardownHostVeth removes the host end after stop/destroy. nspawn
// removes its own veth when the container exits (the interface is gone
// already in the common case); this is idempotent cleanup for the crash
// leftovers.
func (s *Store) teardownHostVeth(id string) {
	//nolint:errcheck // the link usually disappears with the container
	s.runner.Run("ip", "link", "del", vethHostName(id))
}

// --- nat regeneration (whole-table, under flock) ---

// ReconcileNat rebuilds lararium_nat from the live set of booted,
// proxy-enabled cells (called from start AND stop/destroy so crashed
// cells' rules vanish on the next churn). Filter table is NEVER touched
// here — full-flush churn would disturb live conntrack for running cells.
//
// The ENTIRE read-build-apply runs under the flock (agy F4: an
// enumerate-then-locked-apply window lets a concurrent starter's newer
// view be overwritten by a stale one — lost DNAT rules).
func (s *Store) ReconcileNat() error {
	return s.withCellLock(func() error {
		var rules []natRule
		ids, err := s.List()
		if err != nil {
			return err
		}
		for _, id := range ids {
			c, err := s.Load(id)
			if err != nil {
				continue
			}
			if !s.proxyActive() || !c.ProxyOn() || c.VethHost == "" {
				continue
			}
			if !s.isActive(id) {
				continue
			}
			rules = append(rules, natRule{
				Veth:      c.VethHost,
				GatewayIP: c.Gateway,
				ProxyPort: s.proxyPort(),
			})
		}
		return s.writeApplyNft("nat.nft", renderNatNft(rules))
	})
}

// --- net-install (root, idempotent) — ORDER IS THE CONTRACT (spec §4) ---

// InstallNetwork installs the host-side cage. The ORDER is the contract:
//  1. filter table FIRST — forwarding must never be on without the cage;
//  2. THEN ip_forward=1 (a later-step failure rolls the sysctl back);
//  3. conservative defaults for interfaces created later.
func (s *Store) InstallNetwork() error {
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}
	port := s.proxyPort()

	if err := s.writeApplyNft("filter.nft", renderFilterNft(port)); err != nil {
		return fmt.Errorf("install filter table: %w", err)
	}

	old, hadOld := s.readSysctl("net.ipv4.ip_forward")
	if err := s.writeSysctl("net.ipv4.ip_forward", "1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	rollback := func(cause error) error {
		if hadOld && old != "1" {
			//nolint:errcheck // rollback is best-effort; primary error wins
			s.writeSysctl("net.ipv4.ip_forward", old)
		}
		return cause
	}

	// Future interfaces default closed (per-veth overrides happen at
	// start).
	if err := s.writeSysctl("net.ipv4.conf.default.forwarding", "0"); err != nil {
		return rollback(fmt.Errorf("set default.forwarding: %w", err))
	}
	if err := s.writeSysctl("net.ipv4.conf.default.proxy_arp", "0"); err != nil {
		return rollback(fmt.Errorf("set default.proxy_arp: %w", err))
	}

	// Firewall interop (erratum E3, bench #2 finding): iptables and
	// nft run as SEPARATE hook chains and the strictest verdict
	// wins — an nft accept cannot bypass an iptables INPUT policy
	// DROP (ufw/firewalld/legacy). Docker solves this with a
	// user-chain ACCEPT inserted at INPUT position 1; same shape
	// here, tagged for idempotent replacement. The rule ACCEPTs
	// only traffic already bound for the cell mesh gateway going
	// to the proxy door; everything else from v-lar-* stays at the
	// mercy of the nft filter table (which rejects/drops).
	if err := s.installIptablesInterop(); err != nil {
		return rollback(fmt.Errorf("iptables interop: %w", err))
	}
	return nil
}

// iptablesInteropChain is the tagged user chain Docker-style: rules in
// it survive ufw reload (ufw does not flush unknown user chains), and
// the tag makes our own management idempotent.
const (
	iptablesInteropChain = "LARARIUM-INPUT"
	iptablesInteropTag   = "lararium-cage-interop"
)

// installIptablesInterop creates/replaces the LARARIUM-INPUT ACCEPT
// chain and wires it into INPUT once, guarded by `command -v iptables`
// (some minimal hosts ship nft only — the nft cage alone suffices
// there).
func (s *Store) installIptablesInterop() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		//nolint:nilerr // absence is a supported state, not a failure
		return nil // nft-only host: nothing to interop with
	}
	ipt := func(args ...string) (string, error) {
		out, stderr, err := s.runner.Run("iptables", args...)
		return string(append(out, stderr...)), err
	}
	// Rebuild chain contents from scratch (idempotent).
	if _, err := ipt("-N", iptablesInteropChain); err != nil {
		// Exists already: flush it.
		if out, ferr := ipt("-F", iptablesInteropChain); ferr != nil {
			return fmt.Errorf("flush %s: %s", iptablesInteropChain, out)
		}
	}
	// The ONLY flows we open through a foreign INPUT policy: DNAT'd
	// survivors (established flows come back as ESTABLISHED) and
	// new proxied dials to our own gateways' door port.
	// 10.91.0.0/16 is the whole cell mesh; -i limit keeps foreign
	// noise out (no cell traffic arrives without v-lar-* iif, and
	// the mesh daddr is host-local by construction).
	type r struct{ args []string }
	for _, rule := range []r{
		{[]string{
			"-A", iptablesInteropChain, "-m", "conntrack",
			"--ctstate", "DNAT", "-m", "comment", "--comment", iptablesInteropTag, "-j", "ACCEPT",
		}},
		{[]string{
			"-A", iptablesInteropChain, "-i", "v-lar-+", "-d", "10.91.0.0/16",
			"-p", "tcp", "--dport", strconv.Itoa(s.proxyPort()),
			"-m", "comment", "--comment", iptablesInteropTag, "-j", "ACCEPT",
		}},
	} {
		if out, err := ipt(rule.args...); err != nil {
			return fmt.Errorf("populate %s: %s", iptablesInteropChain, out)
		}
	}
	// Hook into INPUT exactly once, at the top (above distro
	// firewalls' own jumps — ufw-before-input etc.). -C exits 0
	// when present; the runner surfaces non-zero as err.
	if _, err := ipt("-C", "INPUT", "-j", iptablesInteropChain); err == nil {
		return nil // already hooked
	}
	if out, err := ipt("-I", "INPUT", "1", "-j", iptablesInteropChain); err != nil {
		return fmt.Errorf("hook %s into INPUT: %s", iptablesInteropChain, out)
	}
	return nil
}

// RemoveNetwork tears the host-side cage down (spec §6): nft tables,
// the iptables interop hook, v-lar-* leftovers. ip_forward is NOT
// touched (other workloads may depend on it once we are gone;
// disabling silently would break them — the cage's own fail-closed
// state comes from the filter table, which is fully removed here).
func (s *Store) RemoveNetwork() error {
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}
	for _, tbl := range []string{"inet lararium_filter", "ip lararium_nat"} {
		if _, stderr, err := s.runner.Run("nft", "delete", "table", tbl); err != nil {
			msg := strings.ToLower(string(stderr))
			if !strings.Contains(msg, "no such file") && !strings.Contains(msg, "does not exist") {
				return fmt.Errorf("delete table %s: %s", tbl, strings.TrimSpace(string(stderr)))
			}
		}
	}
	s.removeIptablesInterop()
	// Leftover host ends of destroyed cells (guest ends die with
	// their netns). Deliberately best-effort — teardown must not
	// fail on an un-deletable link; the next call retries. NOTE:
	// `ip link show <name>` is exact-match, so list all and filter
	// the v-lar- prefix here.
	links, _, qErr := s.runner.Run("ip", "-o", "link", "show")
	if qErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(links)), "\n") {
			// format: "12: v-lar-ab12cd34: <...>"
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.HasPrefix(fields[1], "v-lar-") {
				continue
			}
			name := strings.TrimSuffix(fields[1], ":")
			s.runner.Run("ip", "link", "del", name) //nolint:errcheck // best-effort
		}
	}
	return nil
}

// removeIptablesInterop unwires the E3 hook (best-effort: hosts
// without iptables, or where a distro firewall already flushed it,
// are clean by definition).
func (s *Store) removeIptablesInterop() {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	//nolint:errcheck // best-effort teardown, order-insensitive
	s.runner.Run("iptables", "-D", "INPUT", "-j", iptablesInteropChain)
	//nolint:errcheck // best-effort teardown, order-insensitive
	s.runner.Run("iptables", "-F", iptablesInteropChain)
	//nolint:errcheck // best-effort teardown, order-insensitive
	s.runner.Run("iptables", "-X", iptablesInteropChain)
}

func (s *Store) readSysctl(key string) (string, bool) {
	data, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func (s *Store) writeSysctl(key, val string) error {
	return os.WriteFile("/proc/sys/"+strings.ReplaceAll(key, ".", "/"),
		[]byte(val), 0o644)
}

// --- lifecycle seams used by Start/Stop/Destroy ---

// bringUpNetwork wires the host end, regenerates nat (whole-table under
// flock), and starts the per-cell proxy listener for proxy-enabled
// cells. Call AFTER waitForHealthy (the veth peer exists once the guest
// netns is up) and before returning success from Start — any error must
// unwind the boot (spec §4 fail-closed).
func (s *Store) bringUpNetwork(id string) error {
	c, err := s.Load(id)
	if err != nil {
		return err
	}
	if !c.SubnetAllocated() {
		return fmt.Errorf("cell %s has no network state (create/migrate first)", id)
	}
	if err := s.configureHostVeth(id, c.SubnetIndex); err != nil {
		return err
	}
	if err := s.ReconcileNat(); err != nil {
		return err
	}
	if s.proxyActive() && c.ProxyOn() {
		if err := s.startProxyUnit(id, c); err != nil {
			return err
		}
		// Settle-wait (agy F9; brief §1 mandates it): a proxy that
		// fails to bind or crash-loops must unwind the boot — a
		// "running" cell whose door is dead hands every guest
		// client a refused connection and breaks C1's contract.
		if err := s.waitProxyListening(c); err != nil {
			return err
		}
	}
	return nil
}

// waitProxyListening polls until the cell's proxy accepts TCP on its
// gateway port or the bounded window expires.
func (s *Store) waitProxyListening(c *Cell) error {
	addr := net.JoinHostPort(c.Gateway, strconv.Itoa(s.proxyPort()))
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		d := net.Dialer{Timeout: 1500 * time.Millisecond}
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("proxy for cell %s not listening on %s within 15s: %w",
		c.ID, addr, lastErr)
}

// netDown drops the proxy listener, host veth, and the cell's nat rule.
// Best-effort by design: called from Stop/Destroy where the primary
// teardown already succeeded; leftovers are cleaned by the next
// start/reconcile (veth usually dies with the container).
func (s *Store) netDown(id string) {
	s.stopProxyUnit(id)
	s.teardownHostVeth(id)
	releaseSubnet()
	if err := s.ReconcileNat(); err != nil {
		// A failed nat regen leaves a stale DNAT rule pointing at a
		// dead gateway address — the filter table still rejects
		// everything non-proxy from cells, and the next churn fixes
		// it. Loud on stderr so a human notices (cage is defense in
		// depth, not this rule alone).
		fmt.Fprintf(os.Stderr, "warning: nat reconcile after %s down: %v\n", id, err)
	}
}

// proxyEnvForRun returns the proxy env pairs for `cell run` on this
// cell (nil when the fleet or the cell opts out). --setenv on nspawn
// reaches PID 1 only — exec units need the vars passed explicitly (C7).
func (s *Store) proxyEnvForRun(id string) []string {
	c, err := s.Load(id)
	if err != nil || !s.proxyActive() || !c.ProxyOn() || !c.SubnetAllocated() {
		return nil
	}
	return proxyEnvPairs(c.SubnetIndex, s.proxyPort())
}

// --- proxy unit lifecycle (one transient unit per cell; unit IS the
// lifecycle — no IPC, no shared listeners) ---

// proxyUnit is the transient unit name for a cell's dumb proxy.
func proxyUnit(id string) string { return "lararium-proxy@" + id + ".service" }

// stopProxyUnit stops + unloads a cell's proxy unit. Errors ignored:
// after kill -9 the unit may or may not still be loaded, and both are
// fine — reset-failed makes the NAME reusable for systemd-run.
func (s *Store) stopProxyUnit(id string) {
	//nolint:errcheck // a not-running proxy unit is not an error state
	s.runner.Run("systemctl", "stop", proxyUnit(id))
	//nolint:errcheck // reset-failed is best-effort hygiene
	s.runner.Run("systemctl", "reset-failed", proxyUnit(id))
}

// startProxyUnit spawns the per-cell dumb proxy bound to the cell's own
// gateway address, bound to the cell's lifecycle (BindsTo: the proxy
// dies with its cell under ANY teardown, including unit stop from a
// crash path). Call only AFTER configureHostVeth (binding a nonexistent
// local address = EADDRNOTAVAIL) and only for proxy-enabled cells.
func (s *Store) startProxyUnit(id string, c *Cell) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logDir := filepath.Join(s.Root, "proxy")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	args := []string{
		"--unit=" + proxyUnit(id),
		"--property=BindsTo=" + UnitName(id) + ".service",
		"--property=After=" + UnitName(id) + ".service",
		"--property=Restart=no",
		"--property=NoNewPrivileges=yes",
		// The proxy runs OUTSIDE the cell's cgroup: cap it so a
		// hostile guest cannot lean on host memory/tasks through
		// the door (agy F6).
		"--property=MemoryMax=128M",
		"--property=TasksMax=64",
		exe,
		"proxy",
		"--listen", fmt.Sprintf("%s:%d", c.Gateway, s.proxyPort()),
		"--peer", c.CellIPAddr,
		"--log", s.ProxyLogPath(),
	}
	if _, err := s.runner.RunCombined("/usr/bin/systemd-run", args...); err != nil {
		return fmt.Errorf("start proxy unit for %s: %w", id, err)
	}
	return nil
}

// ProxyLogPath is where the dumb proxy appends its access log (C7
// evidence). Also the CLI's default --log value — one source of truth.
func (s *Store) ProxyLogPath() string { return filepath.Join(s.Root, "proxy", "access.log") }

// --- helpers bridging Cell record <-> network state ---

// proxyPort returns the effective proxy port (default 3128; C6 pins
// :22/:8080 as refused, so the door is neither).
func (s *Store) proxyPort() int {
	if s.ProxyPort > 0 {
		return s.ProxyPort
	}
	return DefaultProxyPort
}

// proxyActive reports whether the fleet-wide proxy is on (lararium.yaml
// may disable it globally — spec §4 fail-closed: filter stays installed,
// nat stays empty).
func (s *Store) proxyActive() bool { return s.ProxyGlobal }

// migrateLegacyNetwork gives a pre-T5 cell record its subnet and
// writes the guest's static network config + proxy env offline. The
// caller guarantees the overlay is unmounted (upper-under-live-mount
// is a stale-dentry no-go, T4 invariant). Gateway=="" is the legacy
// sentinel: fillNetworkState stamps it on every post-T5 record (a zero
// SubnetIndex is a legitimate index, so it cannot serve).
func (s *Store) migrateLegacyNetwork(id string, rec *Cell) error {
	var n int
	if err := s.withCellLock(func() error {
		var aErr error
		n, aErr = s.allocateSubnetIndex()
		return aErr
	}); err != nil {
		return fmt.Errorf("allocate subnet for legacy cell record: %w", err)
	}
	if err := s.writeGuestNetwork(id, n); err != nil {
		return fmt.Errorf("write guest network config: %w", err)
	}
	if s.proxyActive() && rec.ProxyOn() {
		if err := s.writeGuestProxyEnv(id, n, s.proxyPort()); err != nil {
			return fmt.Errorf("write guest proxy env: %w", err)
		}
	}
	fillNetworkState(rec, n)
	if err := s.Save(rec); err != nil {
		return fmt.Errorf("record network state: %w", err)
	}
	return nil
}

// fillNetworkState stamps network fields after subnet allocation.
func fillNetworkState(c *Cell, n int) {
	c.SubnetIndex = n
	c.Gateway = gatewayIP(n)
	c.CellIPAddr = cellIP(n)
	c.VethHost = vethHostName(c.ID)
}
