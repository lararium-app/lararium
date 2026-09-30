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
//	--private-network -D <root>/cells/<id>/merged
//	--bind=<workspace>:/workspace --bind=<hearth>:/hearth
//	--bind=<bin>:/opt/lararium (via --bind-ro)
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
	if err := os.MkdirAll(s.Hearth, 0755); err != nil {
		return fmt.Errorf("mkdir hearth: %w", err)
	}
	if err := os.MkdirAll(s.BinDir(id), 0755); err != nil {
		return fmt.Errorf("mkdir bin: %w", err)
	}
	if err := os.MkdirAll(s.WorkspaceDir(id), 0755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	// Refuse to double-start: an active unit would create a second
	// supervisor competing for the machine name. Spec §6: start is
	// idempotent — already-running is a no-op, not an error.
	if s.isActive(id) {
		return nil
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
	s.runner.Run("systemctl", "reset-failed", unit)
	if _, err := s.runner.RunCombined("/usr/bin/systemd-run", cmdArgs...); err != nil {
		return fmt.Errorf("systemd-run: %w", err)
	}

	// Wait for cell to become healthy (bounded 30s).
	// Cold first boots on slow storage take minutes to a live bus
	// (TimeoutStartSec=300 on the unit; 120s here is the observed
	// 46s worst case with margin).
	if err := s.waitForHealthy(id, 120*time.Second); err != nil {
		// Cell failed to start — stop the unit.
		s.runner.Run("systemctl", "stop", unit)
		return fmt.Errorf("cell failed to become healthy: %w", err)
	}

	// Record uid_map: find the in-guest init process and read /proc/<pid>/uid_map.
	if err := s.recordUIDMap(id); err != nil {
		s.runner.Run("systemctl", "stop", unit)
		return fmt.Errorf("record uid map: %w", err)
	}

	// Give the in-cell keeper (uid 1000) ownership of the workspace
	// bind source: binds are noidmap by default (man systemd-nspawn),
	// so host uid base+1000 appears as keeper inside the cell. Host
	// root:root shows up as nobody and is write-denied (live-probed).
	// Hearth/bin stay host-owned: keeper must not write them.
	// Idempotent: same base on every start.
	if c, err := s.Load(id); err == nil && c.SubUIDBase > 0 {
		keeper := c.SubUIDBase + 1000
		if err := s.fixGuestRootFiles(id, c.SubUIDBase); err != nil {
			return fmt.Errorf("copy up root-owned setuid/config files: %w", err)
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
		dst := filepath.Join(s.UpperDir(id), rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return err
		}
		in.Close()
		if err := out.Close(); err != nil {
			return err
		}
		// Host uid `base` is the guest's root. Chown FIRST: the
		// kernel strips setuid on ownership change, so chmod with
		// the setuid bit must come after.
		if err := os.Chown(dst, base, base); err != nil {
			return err
		}
		// The sealed template stores sudo as 0555 (the seal drops
		// setuid), so the setuid bit is added back here for the
		// binary only, per-cell, inside the mapped uid range.
		// NOTE: os.Chmod takes an os.FileMode — the setuid flag is
		// os.ModeSetuid (1<<22); the raw 04000 bit is NOT it and
		// chmods silently without it (live-probed).
		mode := fi.Mode().Perm()
		if rel == "usr/bin/sudo" {
			mode |= os.ModeSetuid
		}
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
	}
	return nil
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
	s.runner.Run("setfacl", "-R", "-x", spec, s.Hearth)
	s.runner.Run("setfacl", "-R", "-d", "-x", spec, s.Hearth)
}

// nspawnArgs returns the systemd-nspawn arguments for a cell.
// T5 will drop --private-network and add --network-veth wiring.
//
// uid base: --private-users=pick allocates from the host subuid pool
// and may pick a DIFFERENT base on every boot. Once a cell has run,
// its recorded base MUST be reused (explicit --private-users=<base>),
// or the overlay upper/work + bind ownerships no longer line up
// (live-probed: second start → in-cell root-owned trees, keeper writes
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
		"--private-network",
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
	exportDir := "/run/systemd/nspawn/unix-export/" + id

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
		s.runner.Run("kill", "-9", strconv.Itoa(pid))
	}

	// Wait for the kernel to release the export mount.
	deadline = time.Now().Add(120 * time.Second)
	for isMountpoint(exportDir) {
		if time.Now().After(deadline) {
			return fmt.Errorf("stale unix-export mount at %s will not go away; unmount it manually", exportDir)
		}
		time.Sleep(250 * time.Millisecond)
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
	// map → 0,nil) and /proc/0 does not exist (agy review F9).
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

	return nil
}

// lookPath is a local wrapper for exec.LookPath.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Create initializes a new cell: creates directories, seeds upper, mounts overlay,
// and writes cell.json.
func (s *Store) Create(id string) error {
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
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Seed upper/ BEFORE mount.
	if err := s.seedUpper(id); err != nil {
		return fmt.Errorf("seed upper: %w", err)
	}

	// Mount overlay.
	if err := s.MountOverlay(id); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	// Write cell.json.
	c := &Cell{
		ID:      id,
		Created: time.Now(),
	}
	if err := s.Save(c); err != nil {
		return fmt.Errorf("save cell.json: %w", err)
	}

	return nil
}

// CellStatus holds the runtime status of a cell.
type CellStatus struct {
	ID      string
	Running bool
	Mounted bool
}

// Status returns the status of a single cell.
func (s *Store) Status(id string) (*CellStatus, error) {
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

	return &CellStatus{
		ID:      id,
		Running: running,
		Mounted: mounted,
	}, nil
}

// ListStatus returns the status of all cells.
func (s *Store) ListStatus() ([]CellStatus, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}

	var statuses []CellStatus
	for _, id := range ids {
		st, err := s.Status(id)
		if err != nil {
			continue
		}
		statuses = append(statuses, *st)
	}
	return statuses, nil
}
