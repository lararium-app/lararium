// Package cell implements the Lararium cell lifecycle: templates,
// overlay-backed cells, start/stop/run, snapshots and host doctor.
package cell

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Start boots the cell container via systemd-run wrapping systemd-nspawn.
//
// systemd-run --unit=lararium-cell-<id> --keep-unit --property=MemoryMax=...
//
//	--property=CPUQuota=... --property=TasksMax=...
//	--property=Restart=no --property=KillMode=mixed
//	systemd-nspawn --machine=<id> --register=yes --keep-unit
//	--private-users=pick --private-users-ownership=auto
//	--network-veth -D <root>/cells/<id>/merged
//	--bind=<workspace>:/workspace --bind=<hearth>:/hearth
//	--bind=<bin>:/opt/lararium (via --bind-ro)
//
// Network order (spec §4, brief rev2): legacy-record subnet fix-up
// BEFORE mount; proxy-unit hygiene BEFORE spawn; veth resolve/rename/
// address + nat regen + proxy unit AFTER healthy; any post-spawn failure
// unwinds the boot (a cell never stays active with an unenforced
// network).
func (s *Store) Start(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	// Check template exists.
	if _, err := os.Stat(s.TemplateDir()); os.IsNotExist(err) {
		return fmt.Errorf("template not found: run 'cell build-template' first")
	}

	// Ensure bind sources exist — nspawn aborts on a missing bind source.
	if s.Hearth == "" {
		return fmt.Errorf("hearth path not configured (hearth: in lararium.yaml)")
	}
	if err := os.MkdirAll(s.Hearth, 0o755); err != nil {
		return fmt.Errorf("mkdir hearth: %w", err)
	}
	if err := os.MkdirAll(s.BinDir(id), 0o755); err != nil {
		return fmt.Errorf("mkdir bin: %w", err)
	}
	if err := os.MkdirAll(s.WorkspaceDir(id), 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	rec0, _ := s.Load(id)
	basePinned := rec0 != nil && rec0.SubUIDBase > 0

	// Refuse to double-start: an active unit would create a second
	// supervisor competing for the machine name. Spec §6: start is
	// idempotent — already-running is a no-op, not an error.
	// Exception (agy round-3 F6): a first boot that recorded its uid
	// base but crashed before the guest-root fix + restart leaves a
	// live container with a broken sudo — treat that as incomplete,
	// not idempotent: stop it and boot properly below.
	if s.isActive(id) {
		if !basePinned || rec0.GuestRootFixed {
			return nil
		}
		// base but crashed before the guest-root fix + restart: stop it
		// and boot properly below.
		if err := s.Stop(id); err != nil {
			return fmt.Errorf("stop incomplete first boot: %w", err)
		}
	}

	// Clear stale machined state from a previously killed/failed boot.
	// nspawn mounts a private tmpfs at /run/systemd/nspawn/unix-export/<id>
	// in its OWN mount namespace — invisible in the host /proc/mounts but
	// held as long as the supervisor process lives. A supervisor left
	// alive by an abnormal exit (machined shows the machine "cleaning")
	// therefore wedges every later boot with "Mount point … exists
	// already, refusing." Terminate + kill the stale machine by NAME
	// (killing only the unit leaves the out-of-unit supervisor alive —
	// live-probed), then give machined a beat to release the mount.
	if err := s.pruneStaleMachined(id); err != nil {
		return err
	}

	// T5 proxy-unit hygiene BEFORE spawn (C9): after a kill -9 the
	// proxy unit may still be loaded and systemd-run refuses a loaded
	// unit name. Errors are expected (unit usually absent) and ignored
	// inside stopProxyUnit.
	s.stopProxyUnit(id)

	// Guest-root fixes (sudo setuid copy-up) MUST happen while the
	// overlay is NOT live-mounted: mutating upper under a live mount
	// leaves the guest with stale lower dentries for the fixed paths
	// (live-probed: guest exec'd the lower sudo — "effective uid is
	// not 0" — while the host upper held the correct 4555 copy).
	// With a pinned uid base we can fix before boot; on the very
	// first boot the base is unknown until nspawn picks one, so we
	// fix after recording it and restart once.
	//
	// Snapshot/Restore may leave the overlay mounted while stopped
	// (agy round-3 F3). Any mount here means no live guest uses it —
	// drop it so the pre-boot fix never mutates a live upper, then
	// remount fresh below.
	if s.isMounted(s.MergedDir(id)) {
		if err := s.UnmountOverlay(id); err != nil {
			return fmt.Errorf("unmount stale overlay before start: %w", err)
		}
	}

	// T5 migration (spec §4): records written before T5 carry no
	// network state (VethHost empty). Allocate a subnet and write the
	// guest's static network config + proxy env HERE — the overlay is
	// guaranteed unmounted at this point (upper-under-live-mount is a
	// stale-dentry no-go, T4 invariant).
	if rec0 != nil && rec0.Gateway == "" {
		if err := s.migrateLegacyNetwork(id, rec0); err != nil {
			return err
		}
	}

	if basePinned {
		if err := s.fixGuestRootFiles(id, rec0.SubUIDBase); err != nil {
			return fmt.Errorf("copy up root-owned setuid/config files: %w", err)
		}
		// The pre-boot fix only runs when the fix was pending
		// (round-4 F3): the recovery path that stopped an
		// incomplete first boot lands here, and without saving
		// GuestRootFixed every later Start would stop-and-reboot
		// the healthy container again.
		if !rec0.GuestRootFixed {
			rec0.GuestRootFixed = true
			if err := s.Save(rec0); err != nil {
				return fmt.Errorf("record guest-root fix: %w", err)
			}
		}
	}

	// Ensure the overlay is mounted — Stop unmounts it; Start must
	// remount or nspawn sees an empty merged dir ("no OS tree").
	if !s.isMounted(s.MergedDir(id)) {
		if err := s.MountOverlay(id); err != nil {
			return fmt.Errorf("mount overlay: %w", err)
		}
	}

	// Build systemd-run command.
	unit := UnitName(id)
	// Limits: globals from lararium.yaml with per-cell overrides from
	// cell.json (spec §5; agy round-2 F14).
	rec, _ := s.Load(id)
	lim := s.effectiveLimits(rec)
	// NOTE: --keep-unit belongs to systemd-nspawn (it keeps the
	// systemd-run unit as its cgroup instead of creating a machine
	// scope). systemd-run itself does not know this flag — live-probed
	// on systemd 257: "unrecognized option".
	runnerArgs := []string{
		"--unit=" + unit,
		fmt.Sprintf("--property=MemoryMax=%dM", lim.MemoryMB),
		// E6: cgroup v2 defaults memory.swap.max=max — without
		// this the cap is soft (cells grow into host swap).
		"--property=MemorySwapMax=0",
		// E6b: systemd's default OOMPolicy=stop tears down the
		// WHOLE unit when the kernel OOM kills any task in the
		// cgroup — one runaway task would destroy the cell.
		// continue = kill the offender (payload sees 137), cell
		// survives (live-probed 2026-10-01: stop-sigterm on the
		// first 768M alloc attempt without this).
		"--property=OOMPolicy=continue",
		fmt.Sprintf("--property=CPUQuota=%s%%", lim.CPUQuota),
		fmt.Sprintf("--property=TasksMax=%d", lim.TasksMax),
		"--property=Restart=no",
		"--property=KillMode=mixed",
		// First boot of a freshly-reset upper (post-snapshot) does a
		// full ownership pass; observed to exceed the 90s default
		// start timeout on slower benches (live-probed). The cell is
		// long-running by nature — startup budget must not cap it.
		"--property=TimeoutStartSec=300",
	}

	// Build systemd-nspawn command.
	nspawnArgs := s.nspawnArgs(id)

	cmdArgs := append(append(runnerArgs, "systemd-nspawn"), nspawnArgs...)

	// Start detached via systemd-run. Best-effort reset of any stale
	// failed state from a previous crashed run (a failed unit with the
	// same name would block reuse until reset-failed).
	//nolint:errcheck // stale-failed reset is best-effort
	s.runner.Run("systemctl", "reset-failed", unit)
	if _, err := s.runner.RunCombined("/usr/bin/systemd-run", cmdArgs...); err != nil {
		return fmt.Errorf("systemd-run: %w", err)
	}

	// Wait for cell to become healthy (bounded 30s).
	// Cold first boots on slow storage take minutes to a live bus
	// (TimeoutStartSec=300 on the unit; 120s here is the observed
	// 46s worst case with margin).
	if err := s.waitForHealthy(id, 120*time.Second); err != nil {
		// Cell failed to start — stop the unit and drop the overlay
		// so no stale mount survives a failed boot (agy round-3 F7).
		//nolint:errcheck // cleanup after failed boot; primary error wins
		s.runner.Run("systemctl", "stop", unit)
		//nolint:errcheck // cleanup after failed boot; primary error wins
		s.UnmountOverlay(id)
		return fmt.Errorf("cell failed to become healthy: %w", err)
	}

	// Record uid_map: find the in-guest init process and read /proc/<pid>/uid_map.
	if err := s.recordUIDMap(id); err != nil {
		//nolint:errcheck // cleanup after failed boot; primary error wins
		s.runner.Run("systemctl", "stop", unit)
		//nolint:errcheck // cleanup after failed boot; primary error wins
		s.UnmountOverlay(id)
		return fmt.Errorf("record uid map: %w", err)
	}

	// Give the in-cell keeper (uid 1000) ownership of the workspace
	// bind source: binds are noidmap by default (man systemd-nspawn),
	// so host uid base+1000 appears as keeper inside the cell. Host
	// root:root shows up as nobody and is write-denied (live-probed).
	// Hearth/bin stay host-owned: keeper must not write them.
	// Idempotent: same base on every start.
	c, err := s.Load(id)
	if err == nil && c.SubUIDBase > 0 {
		keeper := c.SubUIDBase + 1000

		// First boot: the uid base was just picked, so the guest-root
		// copy-up could not run pre-mount. Apply it now with the
		// overlay NOT live (stop unmounts it) and boot once more so
		// the guest resolves the fixed inodes instead of stale lower
		// dentries (live-probed: mutating upper under a live mount
		// left the guest exec'ing the lower sudo).
		if !basePinned {
			if err := s.Stop(id); err != nil {
				return fmt.Errorf("restart for guest-root fix: %w", err)
			}
			if err := s.fixGuestRootFiles(id, c.SubUIDBase); err != nil {
				return fmt.Errorf("copy up root-owned setuid/config files: %w", err)
			}
			// Record the fix BEFORE restarting: if this process
			// dies right here, the next Start sees the incomplete
			// first boot and repeats it instead of short-circuiting
			// idempotently on the active broken-sudo container
			// (agy round-3 F6).
			c.GuestRootFixed = true
			if err := s.Save(c); err != nil {
				return fmt.Errorf("record guest-root fix: %w", err)
			}
			return s.Start(id)
		}

		if err := os.Chown(s.WorkspaceDir(id), keeper, keeper); err != nil {
			return fmt.Errorf("chown workspace to in-cell keeper: %w", err)
		}
		// <hearth> is SHARED across cells: chowning it to one cell's
		// keeper revokes the previous cell's access (agy round-2 F1).
		// Grant per-keeper read-write via ACLs instead — host keeps
		// ownership, every started cell's keeper is a grantee, and new
		// files inherit the grant via the default ACL. Binds are
		// noidmap, so the raw keeper uid in the ACL matches the
		// keeper's cred in-cell.
		if err := s.grantHearthAccess(keeper); err != nil {
			return fmt.Errorf("grant hearth access to in-cell keeper: %w", err)
		}
	}

	// --- T5 network bring-up (spec §4): AFTER healthy, and ANY
	// failure unwinds the boot — a cell never stays active with an
	// unenforced network (brief §1 unwind rule).
	if err := s.bringUpNetwork(id); err != nil {
		//nolint:errcheck // unwind after failed boot; primary error wins
		s.runner.Run("systemctl", "stop", unit)
		// F8 (agy): partial bring-up must not leak the renamed
		// veth, a half-lived proxy unit, or this cell's DNAT rule.
		// netDown is best-effort and idempotent by design.
		s.netDown(id)
		//nolint:errcheck // unwind after failed boot; primary error wins
		s.UnmountOverlay(id)
		//nolint:errcheck // best-effort: release a stale registration
		s.pruneStaleMachined(id)
		return fmt.Errorf("cell network bring-up failed (boot unwound): %w", err)
	}

	return nil
}

// fixGuestRootFiles copy-ups the sudo setuid binary and the sudoers
// files from the sealed template into the cell's upper layer, owned
// by the cell's root.
//
// Why: the shared template lives on the host owned by host root, and
// the guest maps host-root files to nobody (guest uid 0 only owns
// files at host uid = subuid base). sudo refuses to run when its
// binary or sudoers config is not uid-0-owned in-guest (live-probed:
// "sudo must be owned by uid 0 and have the setuid bit set"), which
// silently invalidates the template's NOPASSWD keeper promise.
// Copying up and chowning to base+0 gives the guest a genuinely
// root-owned setuid copy in its own upper layer (per-cell, so cells
// never share mutable setuid state). The seal stores sudo 0555, so
// the setuid bit is re-asserted here at copy time; the mapped uid-0
// setuid exec is how rootless podman runs sudo in containers.
//
// Guest sovereignty (agy round-3 F8): once a target exists in upper,
// the guest owns it — we do NOT clobber guest apt upgrades or custom
// sudoers on every start. Exception: a sudo binary that lost its
// required state (owner, setuid, non-empty) is repaired, since a
// broken sudo breaks the entire keep promise.
//
// Hardening (agy round-3 F1/F5, round-4 F1/F2): the guest can write
// upper freely (in-cell root), so ancestors are sanitized FIRST —
// each path component under upper is checked with Lstat and replaced
// by a real directory if the guest planted a symlink or file there.
// Only then is the target inspected (Lstat/RemoveAll never follow a
// FINAL symlink, but DO resolve ancestors — inspecting dst before
// sanitizing let `upper/usr -> /usr` escape to the host root fs and
// RemoveAll delete the host's /usr/bin/sudo, live-found by review).
// Copies go through a temp file + fchown/fchmod + atomic rename with
// full error propagation; a swallowed copy failure once recorded
// GuestRootFixed=true over a broken sudo. Safe against guest
// interference because this runs only while the cell is stopped
// (overlay unmounted) and upper sits under cells/<id> at 0700.
func (s *Store) fixGuestRootFiles(id string, base int) error {
	for _, rel := range []string{"usr/bin/sudo", "etc/sudoers", "etc/sudoers.d/keeper"} {
		src := filepath.Join(s.TemplateDir(), rel)
		fi, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue // older template without the drop-in
			}
			return err
		}
		if !fi.Mode().IsRegular() {
			continue
		}

		// 1) Sanitize ancestors BEFORE touching the target. Each
		// component is Lstat'ed (no following); a symlink or file
		// planted where a dir belongs is removed — Lstat/RemoveAll
		// do not follow a final symlink, so only the link dies,
		// never the escape target. Newly created dirs are chowned
		// to base: host root 0:0 maps to nobody in-guest and would
		// lock the cell's root out of creating files there (agy
		// round-3 F2).
		upper := s.UpperDir(id)
		walk := upper
		for _, part := range strings.Split(filepath.Dir(rel), "/") {
			walk = filepath.Join(walk, part)
			st, lerr := os.Lstat(walk)
			if lerr == nil && !st.IsDir() {
				if err := os.RemoveAll(walk); err != nil {
					return err
				}
				lerr = os.ErrNotExist
			}
			if os.IsNotExist(lerr) {
				if err := os.Mkdir(walk, 0o755); err != nil {
					if !os.IsExist(err) {
						return err
					}
				} else if err := os.Chown(walk, base, base); err != nil {
					return err
				}
			} else if lerr != nil {
				return lerr
			}
		}

		// 2) Inspect the target (ancestors are verified real dirs).
		// Guest-ownership policy (agy round-3 F8): an existing
		// healthy target stays untouched. A sudo that is EMPTY, not
		// owned by the cell's root, or missing its setuid bit
		// cannot work — repair it. A guest apt upgrade changes size
		// but keeps owner+setuid: that survives.
		dst := filepath.Join(upper, rel)
		if dfi, err := os.Lstat(dst); err == nil {
			needsRepair := false
			if dfi.Mode().IsRegular() {
				if rel == "usr/bin/sudo" {
					okOwner := false
					if st, ok := dfi.Sys().(*syscall.Stat_t); ok {
						okOwner = int(st.Uid) == base
					}
					okSetuid := dfi.Mode()&os.ModeSetuid != 0
					needsRepair = !okOwner || !okSetuid || dfi.Size() == 0
				}
				if !needsRepair {
					continue
				}
			}
			// Symlink or dir planted by the guest AT the target
			// (safe to remove now: ancestors are real dirs, and
			// RemoveAll does not follow a final symlink).
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}

		// 3) Copy via temp + atomic rename with real error
		// propagation (agy round-4 F2: a swallowed failure here
		// recorded the cell fixed while sudo stayed broken).
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".fixroot-")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		err = writeFixupCopy(tmp, src, fi, base, rel == "usr/bin/sudo")
		tmp.Close()
		if err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("copy up %s: %w", rel, err)
		}
		if err := os.Rename(tmpName, dst); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("install %s: %w", rel, err)
		}
	}
	return nil
}

// writeFixupCopy streams src into the open temp file out, then sets
// ownership and mode via the SAME fd (path-based chmod could race a
// rename of dst). Chown first: the kernel strips setuid on ownership
// change. The seal stores sudo 0555, so the setuid bit is re-asserted
// here; os.Chmod takes os.FileMode — the flag is os.ModeSetuid
// (1<<22), NOT the raw 04000 bit (live-probed: silent no-op).
func writeFixupCopy(out *os.File, src string, fi os.FileInfo, base int, setuid bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chown(base, base); err != nil {
		return err
	}
	mode := fi.Mode().Perm()
	if setuid {
		mode |= os.ModeSetuid
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	return out.Sync()
}

// grantHearthAccess gives host uid `keeper` rwX on <hearth> (existing
// files + inherited defaults). Idempotent; repeated starts of the same
// cell re-apply the same entry.
func (s *Store) grantHearthAccess(keeper int) error {
	spec := fmt.Sprintf("u:%d:rwX", keeper)
	if _, _, err := s.runner.Run("setfacl", "-R", "-m", spec, s.Hearth); err != nil {
		return err
	}
	_, _, err := s.runner.Run("setfacl", "-R", "-d", "-m", spec, s.Hearth)
	return err
}

// revokeHearthAccess drops a destroyed cell's keeper grant. Best
// effort: a missing entry is fine.
func (s *Store) revokeHearthAccess(keeper int) {
	spec := fmt.Sprintf("u:%d", keeper)
	//nolint:errcheck // revoke is idempotent cleanup; entry may not exist
	s.runner.Run("setfacl", "-R", "-x", spec, s.Hearth)
	//nolint:errcheck // revoke is idempotent cleanup; entry may not exist
	s.runner.Run("setfacl", "-R", "-d", "-x", spec, s.Hearth)
}

// nspawnArgs returns the systemd-nspawn arguments for a cell.
// T5 will drop --private-network and add --network-veth wiring.
//
// uid base: --private-users=pick allocates from the host subuid pool
// and may pick a DIFFERENT base on every boot. Once a cell has run,
// its recorded base MUST be reused (explicit --private-users=<base>),
// or the overlay upper/work + bind ownerships no longer line up
// (live-probed: second start -> in-cell root-owned trees, keeper writes
// denied).
func (s *Store) nspawnArgs(id string) []string {
	ownership := s.Limits.Ownership
	if ownership == "" {
		ownership = "auto"
	}

	// Reuse the recorded uid base if the cell has booted before.
	privateUsers := "--private-users=pick"
	if c, err := s.Load(id); err == nil && c.SubUIDBase > 0 {
		privateUsers = fmt.Sprintf("--private-users=%d", c.SubUIDBase)
	}

	args := []string{
		"--machine=" + id,
		"--register=yes",
		"--keep-unit",
		// --boot makes nspawn exec the guest init (/sbin/init) as
		// PID 1. Without it nspawn drops an interactive shell — the
		// cell "boots" but has no systemd, no machine bus, and
		// `cell run` cannot reach it (live-probed).
		"--boot",
		privateUsers,
		"--private-users-ownership=" + ownership,
		// T5 (spec §4): the shipped network path. nspawn creates the
		// veth pair with the guest end named host0; the host end is
		// resolved BY MECHANISM after spawn (leader PID -> iflink),
		// renamed, addressed. Egress is governed by the host nftables
		// cage — --private-network was the T4 interim.
		"--network-veth",
		"-D", s.MergedDir(id),
	}

	// Bind mounts per spec §3. Read-only mounts use --bind-ro (live-
	// probed on systemd 257: the --bind= option grammar accepts only
	// rbind/norbind/noidmap/idmap/rootidmap/owneridmap — ":ro" and
	// ":nodev" are rejected with "Invalid bind mount option").
	args = append(args,
		"--bind="+s.WorkspaceDir(id)+":/workspace",
		"--bind="+s.Hearth+":/hearth",
		"--bind-ro="+s.BinDir(id)+":/opt/lararium",
	)

	return args
}

// pruneStaleMachined clears a zombie machined registration left by an
// abnormally-exited boot and waits until the kernel has actually
// released nspawn's private unix-export tmpfs.
//
// Two clocks matter: machined's registration table clears asynchronously
// after `terminate`, and the tmpfs (mounted by the supervisor in its own
// mount namespace) disappears only when that namespace dies — later than
// the registration. Returning between the two lets the next spawn see
// "Mount point '/run/systemd/nspawn/unix-export/<id>' exists already,
// refusing." (all live-probed). Must be called only when the unit is
// known inactive (the caller's double-start guard ensures that).
func (s *Store) pruneStaleMachined(id string) error {
	// systemd moved the private export tmpfs: <=257 mounted it at
	// unix-export/<id>, 262 at <id>/unix-export (live-probed on both
	// benches — missing the new path shipped C9 restarts broken:
	// nspawn refuses "Mount point ... exists already").
	exportDirs := []string{
		"/run/systemd/nspawn/unix-export/" + id,
		"/run/systemd/nspawn/" + id + "/unix-export",
	}

	_, _, showErr := s.runner.Run("machinectl", "show", id)
	if showErr == nil {
		// A stale registration means a supervisor still exists; a
		// terminate failure means we cannot guarantee a clean spawn.
		if _, stderr, err := s.runner.Run("machinectl", "terminate", id); err != nil {
			return fmt.Errorf("terminate stale machine %s: %s", id, strings.TrimSpace(string(stderr)))
		}
	}

	// Wait for machined to drop the registration.
	deadline := time.Now().Add(30 * time.Second)
	for showErr == nil {
		time.Sleep(250 * time.Millisecond)
		_, _, showErr = s.runner.Run("machinectl", "show", id)
		if time.Now().After(deadline) {
			return fmt.Errorf("stale machine %s will not terminate", id)
		}
	}

	// A supervisor that outlived machined's table (impossible for a
	// healthy nspawn, which unregisters at exit) holds the export dir
	// invisibly — kill it.
	if pid, err := s.findNspawnPID(id); err == nil && pid > 0 {
		//nolint:errcheck // kill targets a PID we just observed; racing exit is fine
		s.runner.Run("kill", "-9", strconv.Itoa(pid))
	}

	// Wait for the kernel to release the export mount. A clean exit
	// unwinds it in milliseconds; after SIGKILL the tmpfs can stay
	// propagated into the host table with no namespace left to
	// reclaim it (live-probed C9) — after a short grace, lazily
	// unmount the orphan ourselves. Safe here: the unit is inactive,
	// the registration is gone, and any live supervisor holding the
	// dir was just killed; the mount is an empty private tmpfs.
	for _, exportDir := range exportDirs {
		deadline = time.Now().Add(5 * time.Second)
		for isMountpoint(exportDir) {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if isMountpoint(exportDir) {
			//nolint:errcheck // verified by re-check below
			s.runner.Run("umount", "-l", exportDir)
			deadline = time.Now().Add(10 * time.Second)
			for isMountpoint(exportDir) {
				if time.Now().After(deadline) {
					return fmt.Errorf("stale unix-export mount at %s will not go away; unmount it manually", exportDir)
				}
				time.Sleep(250 * time.Millisecond)
			}
		}
	}
	return nil
}

// isMountpoint reports whether path appears as a mount point in the
// host mount table.
func isMountpoint(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == path {
			return true
		}
	}
	return false
}

// waitForHealthy waits until the cell is EXEC-CAPABLE: unit active,
// registered with machined, AND the machine bus answers a trivial
// systemd-run. Gating on active-only shipped a start that returned
// before `cell run` could work (live: "Failed to connect to system
// scope bus via machine transport" for up to ~60s after a healthy-
// looking start on eMMC storage, 2026-09-30). machined registration
// is required but not sufficient; the bus probe is the real contract.
func (s *Store) waitForHealthy(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	unit := UnitName(id)

	for time.Now().Before(deadline) {
		if _, _, err := s.runner.Run("systemctl", "is-active", "--quiet", unit); err == nil {
			// Spec §6: Healthy = unit active AND machine registered
			// with machined. Without the second half, `cell run`
			// racing Start hits "cell not running" (agy review F3).
			if _, _, err := s.runner.Run("machinectl", "show", id); err == nil {
				// Third gate: the machine bus must answer. This is
				// the exact call `cell run` makes; if it works here,
				// Start's contract ("ready to exec") holds.
				if _, _, err := s.runner.Run("/usr/bin/systemd-run",
					"--machine="+id, "--wait", "--pipe", "--quiet", "--collect",
					"--", "/bin/true"); err == nil {
					return nil
				}
			}
		} else if state := s.unitState(unit); state == "failed" {
			// Boot crashed (e.g. supervisor killed): fail now, don't
			// burn the whole window (prior live defect).
			return fmt.Errorf("cell %s boot failed (unit failed)", id)
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("cell %s did not become healthy within %v", id, timeout)
}

// unitState returns ActiveState for a unit ("" if unreadable).
func (s *Store) unitState(unit string) string {
	stdout, _, err := s.runner.Run("systemctl", "show", "-p", "ActiveState", unit)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// recordUIDMap reads /proc/<pid>/uid_map for the in-guest init process
// and records SubUIDBase in cell.json.
func (s *Store) recordUIDMap(id string) error {
	unit := UnitName(id)

	// Find the main PID of the systemd-run unit. (No -v: not a valid
	// systemctl option; the MainPID= line is parsed below.)
	stdout, _, err := s.runner.Run("systemctl", "show", "-p", "MainPID", unit)
	if err != nil {
		return fmt.Errorf("get MainPID: %w", err)
	}

	// Parse MainPID from systemctl output.
	var mainPID int
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.HasPrefix(line, "MainPID=") {
			pidStr := strings.TrimPrefix(line, "MainPID=")
			pid, err := strconv.Atoi(pidStr)
			if err != nil || pid == 0 {
				continue
			}
			mainPID = pid
			break
		}
	}

	if mainPID == 0 {
		// Fallback: find nspawn process for this machine.
		mainPID, err = s.findNspawnPID(id)
		if err != nil || mainPID == 0 {
			return fmt.Errorf("could not find nspawn process for cell %s", id)
		}
	}

	// Walk the cgroup process tree to find the mapped descendant (not the host-root supervisor).
	pid, err := s.findMappedPID(mainPID)
	if err != nil {
		return fmt.Errorf("find mapped pid: %w", err)
	}

	// Read /proc/<pid>/uid_map.
	uidMapPath := fmt.Sprintf("/proc/%d/uid_map", pid)
	data, err := os.ReadFile(uidMapPath)
	if err != nil {
		return fmt.Errorf("read uid_map: %w", err)
	}

	lines := strings.TrimSpace(string(data))
	if lines == "" {
		return fmt.Errorf("empty uid_map for cell %s", id)
	}

	// Parse first line: "container-uid host-uid count"
	fields := strings.Fields(lines)
	if len(fields) < 3 {
		return fmt.Errorf("invalid uid_map format: %s", lines)
	}

	// Check for unmapped (identity) mapping: 0 0 4294967295
	if fields[0] == "0" && fields[1] == "0" && fields[2] == "4294967295" {
		return fmt.Errorf("no user namespace mapping for cell %s (identity map)", id)
	}

	hostUID, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("parse host uid: %w", err)
	}

	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return fmt.Errorf("parse uid count: %w", err)
	}

	// Load or create cell record and update SubUIDBase.
	c, err := s.Load(id)
	if err != nil {
		c = &Cell{ID: id, Created: time.Now()}
	}
	c.SubUIDBase = hostUID
	c.SubUIDCount = count

	if err := s.Save(c); err != nil {
		return fmt.Errorf("save cell.json: %w", err)
	}

	return nil
}

// findNspawnPID finds the PID of the systemd-nspawn process for a given machine.
func (s *Store) findNspawnPID(id string) (int, error) {
	stdout, _, err := s.runner.Run("ps", "-eo", "pid,args", "--no-headers")
	if err != nil {
		return 0, err
	}

	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cmd := strings.Join(fields[1:], " ")
		if !strings.Contains(cmd, "systemd-nspawn") {
			continue
		}
		// Match --machine=<id> as a WHOLE token: substring match
		// let id "c1" match "--machine=c10", so pruneStaleMachined
		// SIGKILLed the wrong container (agy round-2 F2).
		for _, f := range fields[1:] {
			if f == "--machine="+id {
				return pid, nil
			}
		}
	}

	return 0, fmt.Errorf("nspawn process not found for %s", id)
}

// findMappedPID walks the process tree under parentPID to find a descendant
// that has a non-identity uid_map (i.e., is inside the user namespace).
func (s *Store) findMappedPID(parentPID int) (int, error) {
	// Walk the UNIT cgroup subtree. After start, nspawn moves the
	// supervisor into <unit>/supervisor and the container payload lives
	// in the SIBLING <unit>/payload — walking from MainPID's own cgroup
	// would never see payload. Derive the unit base by trimming the
	// supervisor/payload suffix. (/proc/<pid>/children is absent on this
	// kernel; cgroup2 member lists are always there.)
	cgRel, err := procCgroupPath(parentPID)
	cgRel = strings.TrimSuffix(cgRel, "/supervisor")
	cgRel = strings.TrimSuffix(cgRel, "/payload")

	// Bounded retry: payload appears only once the guest init forks
	// (machined registration can precede it). Generous window: a first
	// boot after a snapshot-reset upper does a full ownership pass and
	// can be slow on weaker hardware (live-probed >80s to login prompt).
	deadline := time.Now().Add(120 * time.Second)
	for {
		// No point waiting for a supervisor that never existed or
		// already died (unit tests, or a spawn that failed instantly).
		if _, statErr := os.Stat(fmt.Sprintf("/proc/%d", parentPID)); statErr != nil {
			break
		}
		if err == nil && cgRel != "" && cgRel != "/" {
			base := filepath.Join("/sys/fs/cgroup", filepath.Clean(cgRel))
			var best int
			//nolint:nilerr // unreadable cgroup subdirs are skipped, not fatal
			_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.IsDir() || best != 0 {
					return nil
				}
				procs, readErr := os.ReadFile(filepath.Join(p, "cgroup.procs"))
				if readErr != nil {
					return nil
				}
				for _, ps := range strings.Fields(string(procs)) {
					pid, aErr := strconv.Atoi(ps)
					if aErr != nil {
						continue
					}
					if m, mErr := s.checkPIDMapped(pid); mErr == nil && m > 0 {
						best = m
						return filepath.SkipAll
					}
				}
				return nil
			})
			if best != 0 {
				return best, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
		// Re-read in case MainPID's cgroup path moved (supervisor).
		cgRel, err = procCgroupPath(parentPID)
		cgRel = strings.TrimSuffix(cgRel, "/supervisor")
		cgRel = strings.TrimSuffix(cgRel, "/payload")
	}

	// Fallback: check the given PID itself — but only if it is a
	// real mapped process; the supervisor is host-root (identity
	// map -> 0,nil) and /proc/0 does not exist (agy review F9).
	pid, err := s.checkPIDMapped(parentPID)
	if err != nil {
		return 0, err
	}
	if pid == 0 {
		return 0, fmt.Errorf("no uid-mapped process found under the cell within window")
	}
	return pid, nil
}

// procCgroupPath returns the cgroup2 path of a pid ("0::/path" line).
func procCgroupPath(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no cgroup2 line for pid %d", pid)
}

// checkPIDMapped returns the PID if it has a non-identity uid_map, 0 otherwise.
func (s *Store) checkPIDMapped(pid int) (int, error) {
	uidMapPath := fmt.Sprintf("/proc/%d/uid_map", pid)
	data, err := os.ReadFile(uidMapPath)
	if err != nil {
		return 0, err
	}

	lines := strings.TrimSpace(string(data))
	if lines == "" {
		return 0, nil
	}

	// Identity map means no userns. /proc uid_map columns are
	// space-PADDED ("         0          0 4294967295"), so compare
	// parsed fields, never the raw string.
	fields := strings.Fields(lines)
	if len(fields) == 3 && fields[0] == "0" && fields[1] == "0" && fields[2] == "4294967295" {
		return 0, nil
	}

	return pid, nil
}

// Stop stops the cell's systemd unit and unmounts the overlay.
func (s *Store) Stop(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	unit := UnitName(id)

	// Spec §6: stop is idempotent — already-stopped is a no-op.
	if !s.isActive(id) {
		// T5: still guarantee proxy/veth/nat are gone (a crash can
		// leave the listener or host end behind).
		s.netDown(id)
		// Still ensure the overlay is down (a crash between unit
		// stop and unmount can leave the mount behind).
		if s.isMounted(s.MergedDir(id)) {
			return s.UnmountOverlay(id)
		}
		return nil
	}

	// Stop the systemd unit.
	if _, stderr, err := s.runner.Run("systemctl", "stop", unit); err != nil {
		return fmt.Errorf("systemctl stop: %s", strings.TrimSpace(string(stderr)))
	}

	// T5: drop the proxy listener + host veth, regenerate nat (the
	// cell's DNAT rule vanishes with its active unit).
	s.netDown(id)

	// Unmount overlay.
	if err := s.UnmountOverlay(id); err != nil {
		return fmt.Errorf("unmount overlay: %w", err)
	}

	return nil
}

// Destroy stops the cell, unmounts the overlay, and removes upper/work dirs.
// Plain umount only — never umount -l.
func (s *Store) Destroy(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	// Stop if running.
	_, _, err := s.runner.Run("systemctl", "is-active", UnitName(id))
	if err == nil {
		if stopErr := s.Stop(id); stopErr != nil {
			return fmt.Errorf("stop during destroy: %w", stopErr)
		}
	} else {
		// Not running, just unmount if mounted.
		if s.isMounted(s.MergedDir(id)) {
			if umountErr := s.UnmountOverlay(id); umountErr != nil {
				return fmt.Errorf("unmount during destroy: %w", umountErr)
			}
		}
	}

	// Verify not mounted before delete.
	if s.isMounted(s.MergedDir(id)) {
		return fmt.Errorf("refusing to delete: overlay still mounted at %s", s.MergedDir(id))
	}

	// Remove upper and work dirs.
	for _, d := range []string{s.UpperDir(id), s.WorkDir(id)} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove %s: %w", d, err)
		}
	}

	// Drop this cell's keeper grant on <hearth> before its identity
	// (cell.json) disappears; the uid goes back to the pool and a
	// future cell must not inherit someone else's ACL (round-2 F1).
	if c, err := s.Load(id); err == nil && c.SubUIDBase > 0 {
		s.revokeHearthAccess(c.SubUIDBase + 1000)
	}

	// Remove the whole cell dir: merged, cell.json, snapshots/, bin/,
	// log/, workspace/. Leaving snapshots behind leaks disk and — worse
	// — a later `create <same-id>` would silently re-inherit stale
	// workspace/snapshots state (live-probed: 815M survived destroy).
	if err := os.RemoveAll(s.CellDir(id)); err != nil {
		return fmt.Errorf("remove cell dir: %w", err)
	}

	// T5: final network teardown (proxy unit usually already gone via
	// BindsTo; this covers destroy-while-stopped and crash leftovers)
	// and nat regeneration WITHOUT this cell (the record is deleted, so
	// the claim is released with it).
	s.netDown(id)

	return nil
}

// lookPath is a local wrapper for exec.LookPath.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Create initializes a new cell: creates directories, allocates the
// cell subnet, seeds upper (with the guest network config), mounts the
// overlay, and writes cell.json.
//
// noProxy (optional, default false): C8's per-cell opt-out — no proxy
// env in the guest, no nat rule, no proxy unit at start.
func (s *Store) Create(id string, noProxy ...bool) error {
	optNoProxy := len(noProxy) > 0 && noProxy[0]
	if err := validateID(id); err != nil {
		return err
	}

	// Check template exists.
	if _, err := os.Stat(s.TemplateDir()); os.IsNotExist(err) {
		return fmt.Errorf("template not found: run 'cell build-template' first")
	}

	// Spec §6: create is idempotent — an existing cell is a no-op.
	// Must NOT fall through: re-running the create path would reset
	// cell.json and lose the pinned uid base. (agy review F6.)
	if _, err := s.Load(id); err == nil {
		return nil
	}

	// Create all required directories.
	for _, d := range []string{
		s.UpperDir(id), s.WorkDir(id), s.MergedDir(id),
		s.WorkspaceDir(id), s.BinDir(id), s.SnapshotsDir(id), s.LogDir(id),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	// Owner-only on the CELL DIRECTORY (agy round-3 F4): upper/ holds
	// guest-controlled setuid binaries; a world-traversable path lets
	// any unprivileged host user EXEC them (host kernel evaluates
	// host uids — execve is not blocked by the userns mapping).
	// Locking cells/<id> blocks the traversal for every layer below
	// (upper, snapshots) while daemon+nspawn (host root, the owner)
	// keep full access; the workspace bind target bypasses path
	// traversal (VFS resolves the mount directly).
	//
	// NOT upper/ or merged/ themselves: upper's top inode IS the
	// guest's / — a 0700 there is owned by host root (unmapped ->
	// nobody in-guest) and strips the guest's search permission on
	// its own root (live-probed: container halts at boot).
	if err := os.Chmod(s.CellDir(id), 0o700); err != nil {
		return fmt.Errorf("chmod cell dir: %w", err)
	}

	// T5 (spec §4): the flock covers the WHOLE scan-allocate-...
	//-commit cycle THROUGH the cell.json write (agy F3: releasing
	// after allocation let a concurrent create scan a cell.json-less
	// subnet and double-claim it). Everything between alloc and Save
	// is local fs work — bounded hold time, no external calls.
	if err := s.withCellLock(func() error {
		subnet, aErr := s.allocateSubnetIndex()
		if aErr != nil {
			return aErr
		}

		// Seed upper/ BEFORE mount.
		if sErr := s.seedUpper(id); sErr != nil {
			return fmt.Errorf("seed upper: %w", sErr)
		}

		// Guest network config + proxy env go into the SEALED
		// upper before the overlay is mounted (upper-under-live-
		// mount mutation yields stale dentries — T4 invariant).
		if wErr := s.writeGuestNetwork(id, subnet); wErr != nil {
			return fmt.Errorf("write guest network config: %w", wErr)
		}
		proxyOn := s.proxyActive() && !optNoProxy
		if proxyOn {
			if wErr := s.writeGuestProxyEnv(id, subnet, s.proxyPort()); wErr != nil {
				return fmt.Errorf("write guest proxy env: %w", wErr)
			}
		}

		// Mount overlay.
		if mErr := s.MountOverlay(id); mErr != nil {
			return fmt.Errorf("mount overlay: %w", mErr)
		}

		// cell.json write = the claim commit, inside the window.
		rec := &Cell{
			ID:      id,
			Created: time.Now(),
		}
		fillNetworkState(rec, subnet)
		if !proxyOn {
			off := false
			rec.ProxyEnabled = &off
		}
		if svErr := s.Save(rec); svErr != nil {
			return fmt.Errorf("save cell.json: %w", svErr)
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

// Status holds the runtime status of a cell.
type Status struct {
	ID      string
	Running bool
	Mounted bool
}

// Status returns the status of a single cell.
func (s *Store) Status(id string) (*Status, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}

	_, err := s.Load(id)
	if err != nil {
		return nil, err
	}

	unit := UnitName(id)
	_, _, activeErr := s.runner.Run("systemctl", "is-active", unit)
	running := activeErr == nil

	mounted := s.isMounted(s.MergedDir(id))

	return &Status{
		ID:      id,
		Running: running,
		Mounted: mounted,
	}, nil
}

// ListStatus returns the status of all cells.
func (s *Store) ListStatus() ([]Status, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}

	var statuses []Status
	for _, id := range ids {
		st, err := s.Status(id)
		if err != nil {
			continue
		}
		statuses = append(statuses, *st)
	}
	return statuses, nil
}
