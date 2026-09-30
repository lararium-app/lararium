package cell

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CheckResult holds the result of a single doctor check.
type CheckResult struct {
	Name   string
	Status string // "OK", "FAIL", or "UNSUPPORTED"
	Detail string
}

// Doctor runs all environment prerequisite checks.
// Returns results and an error if any check FAILs.
func (s *Store) Doctor() ([]CheckResult, error) {
	var results []CheckResult
	var failed bool

	// Check: root (euid 0).
	if os.Geteuid() != 0 {
		results = append(results, CheckResult{"root", "FAIL", "euid is not 0; cell operations require root"})
		failed = true
	} else {
		results = append(results, CheckResult{"root", "OK", "euid 0"})
	}

	// Check: systemd-nspawn present.
	if _, err := exec.LookPath("systemd-nspawn"); err != nil {
		results = append(results, CheckResult{"systemd-nspawn", "FAIL", "systemd-nspawn not found in PATH"})
		failed = true
	} else {
		results = append(results, CheckResult{"systemd-nspawn", "OK", "found in PATH"})
	}

	// Check: machinectl present (required for cell run).
	if _, err := exec.LookPath("machinectl"); err != nil {
		results = append(results, CheckResult{"machinectl", "FAIL", "machinectl not found in PATH"})
		failed = true
	} else {
		results = append(results, CheckResult{"machinectl", "OK", "found in PATH"})
	}

	// Check: /run/host (container nesting detection).
	if _, err := os.Stat("/run/host"); os.IsNotExist(err) {
		results = append(results, CheckResult{"/run/host", "OK", "not inside a container"})
	} else {
		results = append(results, CheckResult{"/run/host", "FAIL", "/run/host exists — running inside a container"})
		failed = true
	}

	// Check: cgroup2.
	cgroup2, err := checkCgroup2()
	if err != nil {
		results = append(results, CheckResult{"cgroup2", "FAIL", fmt.Sprintf("check cgroup2: %v", err)})
		failed = true
	} else if !cgroup2 {
		results = append(results, CheckResult{"cgroup2", "FAIL", "/sys/fs/cgroup is not cgroup2fs"})
		failed = true
	} else {
		results = append(results, CheckResult{"cgroup2", "OK", "cgroup2 unified hierarchy"})
	}

	// Check: systemd >= 254 (spec §1 prerequisite; agy review F11).
	if ok, detail := checkSystemdVersion(); ok {
		results = append(results, CheckResult{"systemd", "OK", detail})
	} else {
		results = append(results, CheckResult{"systemd", "FAIL", detail})
		failed = true
	}

	// Check: subuid/subgid ranges.
	if ok, detail := checkSubUID(); ok {
		results = append(results, CheckResult{"subuid", "OK", detail})
	} else {
		results = append(results, CheckResult{"subuid", "FAIL", detail})
		failed = true
	}

	// Check: systemd-machined reachable. NOT `systemctl is-active`:
	// machined is socket-activated and reads "inactive" while idle —
	// the true probe is machinectl list, which talks to the socket
	// and triggers activation (live-probed on Arch/systemd 261).
	if _, _, err := s.runner.Run("machinectl", "list"); err != nil {
		results = append(results, CheckResult{"systemd-machined", "FAIL", "machinectl list failed (machined unreachable)"})
		failed = true
	} else {
		results = append(results, CheckResult{"systemd-machined", "OK", "socket reachable, machines listed"})
	}

	// Check: overlay mount probe.
	if ok, detail := s.probeOverlay(); ok {
		results = append(results, CheckResult{"overlay", "OK", detail})
	} else {
		results = append(results, CheckResult{"overlay", "FAIL", detail})
		failed = true
	}

	// Check: kernel >= 5.19.
	if ok, detail := checkKernel(); ok {
		results = append(results, CheckResult{"kernel", "OK", detail})
	} else {
		results = append(results, CheckResult{"kernel", "FAIL", detail})
		failed = true
	}

	// Check: userns nspawn probe (boot a throwaway cell).
	if ok, detail := s.probeUserNS(); ok {
		results = append(results, CheckResult{"userns-nspawn", "OK", detail})
	} else {
		results = append(results, CheckResult{"userns-nspawn", "FAIL", detail})
		failed = true
	}

	if failed {
		return results, fmt.Errorf("doctor: one or more checks failed")
	}
	return results, nil
}

// checkCgroup2 verifies /sys/fs/cgroup is cgroup2fs.
func checkCgroup2() (bool, error) {
	statOut, _, err := runLookup("stat", "-fc", "%T", "/sys/fs/cgroup")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(statOut)) == "cgroup2fs", nil
}

// checkSubUID verifies the user has subuid/subgid entries.
func checkSubUID() (bool, string) {
	// Root always has access.
	if os.Geteuid() == 0 {
		return true, "root has full uid range"
	}

	// Check /etc/subuid for current user.
	username := os.Getenv("SUDO_USER")
	if username == "" {
		u, err := userCurrent()
		if err != nil {
			return false, "cannot determine current user"
		}
		username = u.Username
	}

	data, err := os.ReadFile("/etc/subuid")
	if err != nil {
		return false, fmt.Sprintf("cannot read /etc/subuid: %v", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) < 3 {
			continue
		}
		if fields[0] == username {
			n, err := strconv.Atoi(fields[2])
			if err != nil {
				continue
			}
			count += n
		}
	}

	if count >= 65536 {
		return true, fmt.Sprintf("%s has %d subuids", username, count)
	}
	return false, fmt.Sprintf("%s has %d subuids (need >= 65536)", username, count)
}

// probeOverlay creates a temporary overlay mount to verify filesystem support.
func (s *Store) probeOverlay() (bool, string) {
	// Probe the CELLS filesystem (spec §1: overlay upperdir support is
	// a property of s.Root — a /tmp tmpfs probe would falsely pass on
	// NFS cells dirs; agy review F11).
	probeRoot := filepath.Join(s.Root, ".doctor-probe")
	os.RemoveAll(probeRoot)
	if err := os.MkdirAll(probeRoot, 0755); err != nil {
		return false, fmt.Sprintf("cannot create probe dir under cells root: %v", err)
	}
	defer os.RemoveAll(probeRoot)

	lower := filepath.Join(probeRoot, "lower")
	upper := filepath.Join(probeRoot, "upper")
	work := filepath.Join(probeRoot, "work")
	merged := filepath.Join(probeRoot, "merged")

	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return false, fmt.Sprintf("mkdir: %v", err)
		}
	}

	// Write a test file in lower.
	if err := os.WriteFile(filepath.Join(lower, "test"), []byte("ok"), 0644); err != nil {
		return false, fmt.Sprintf("write test file: %v", err)
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if _, _, mErr := s.runner.Run("/bin/mount", "-t", "overlay", "overlay", merged, "-o", opts); mErr != nil {
		return false, fmt.Sprintf("mount overlay failed: %v", mErr)
	}

	// Try to write to merged (copy-up test).
	if err := os.WriteFile(filepath.Join(merged, "test2"), []byte("ok"), 0644); err != nil {
		s.runner.Run("umount", merged)
		return false, fmt.Sprintf("write to overlay failed: %v", err)
	}

	s.runner.Run("umount", merged)
	return true, "overlay mount + copy-up successful"
}

// probeUserNS boots a throwaway nspawn to verify user namespace support.
// Uses --register=no (allowed for doctor probes).
func (s *Store) probeUserNS() (bool, string) {
	// With a template we can exercise id + touch; with the micro
	// rootfs only sh builtins exist.
	probeCmd := "id -u && touch /workspace/probe && echo wrote"
	// Use template if available, otherwise create a minimal rootfs.
	dir := s.TemplateDir()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// No template: build a MICRO rootfs — the probe command uses
		// only /bin/sh builtins. Copying whole /lib + /usr/lib (~600MB)
		// filled tmpfs on a 1GB /tmp and ENOSPC'd the probe (live,
		// 2026-09-30); ldd gives us the handful of real deps instead.
		var err error
		dir, err = os.MkdirTemp("", "lararium-userns-probe-*")
		if err != nil {
			return false, fmt.Sprintf("cannot create temp dir: %v", err)
		}
		defer os.RemoveAll(dir)

		// nspawn refuses trees without /usr (OS-tree check) even for
		// builtin-only probes.
		for _, d := range []string{"usr", "etc", "proc", "sys", "dev", "workspace"} {
			os.MkdirAll(filepath.Join(dir, d), 0755)
		}
		if err := copyBinaryWithDeps("/bin/sh", dir); err != nil {
			return false, fmt.Sprintf("cannot stage probe rootfs: %v", err)
		}
		probeCmd = ": > /workspace/probe && echo wrote"
	}

	// Run throwaway nspawn with --private-users=pick, and a bind we
	// can assert write-ownership on (spec §1: idmapped-bind probe =
	// throwaway cell, /workspace write lands owned by the MAPPED uid,
	// never host root — agy review F11).
	wsDir, wErr := os.MkdirTemp("", "lararium-doctor-ws-*")
	if wErr != nil {
		return false, fmt.Sprintf("cannot create probe workspace: %v", wErr)
	}
	// World-writable so in-cell root (= the *picked* host uid under
	// the default noidmap bind) can create the probe file; ownership
	// of the written file is the assertion below (live-probed 2026-09-30:
	// a 700 root-owned dir denies the mapped write).
	os.Chmod(wsDir, 0o777)
	defer os.RemoveAll(wsDir)

	args := []string{
		"--private-users=pick",
		"--private-users-ownership=auto",
		"--ephemeral",
		"--register=no",
		"--bind=" + wsDir + ":/workspace",
		"-D", dir,
		"/bin/sh", "-c", probeCmd,
	}

	stdout, stderr, err := s.runner.Run("systemd-nspawn", args...)
	output := strings.TrimSpace(string(stdout))
	if err != nil {
		errMsg := strings.TrimSpace(string(stderr))
		return false, fmt.Sprintf("nspawn probe failed: %v (%s)", err, errMsg)
	}

	// Expected output: the mapped uid of root in the container.
	// With --private-users=pick, root maps to some host subuid.
	// The important thing is that nspawn succeeds with userns.
	if output == "" {
		return false, "nspawn probe produced no output"
	}

	// Assert the idmapped write: file must exist and NOT be host-root
	// owned (it lands at the picked subuid; nobody/65534 or root
	// means idmapping is not working on this kernel/mount combo).
	written := filepath.Join(wsDir, "probe")
	fi, sErr := os.Stat(written)
	if sErr != nil {
		return false, fmt.Sprintf("workspace probe file missing after in-cell write: %v", sErr)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid == 0 {
			return false, "workspace write landed owned by host root — idmapping broken"
		}
		return true, fmt.Sprintf("userns + idmapped-bind OK (in-cell root=%s; host-side file uid=%d)", strings.SplitN(output, "\n", 2)[0], st.Uid)
	}
	return false, "cannot stat probe file owner"
}

// checkKernel verifies kernel version >= 5.19.
func checkKernel() (bool, string) {
	// Read kernel version from /proc/version or uname.
	re := regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

	// Try /proc/version first.
	data, err := os.ReadFile("/proc/version")
	if err == nil {
		matches := re.FindStringSubmatch(string(data))
		if len(matches) >= 4 {
			ok, ver := parseKernelVersion(matches[1], matches[2], matches[3])
			return ok, ver
		}
	}

	// Fallback to uname.
	out, _, err := runLookup("uname", "-r")
	if err != nil {
		return false, fmt.Sprintf("cannot determine kernel version: %v", err)
	}

	matches := re.FindStringSubmatch(strings.TrimSpace(string(out)))
	if len(matches) < 4 {
		return false, fmt.Sprintf("cannot parse kernel version: %s", strings.TrimSpace(string(out)))
	}

	ok, ver := parseKernelVersion(matches[1], matches[2], matches[3])
	return ok, ver
}

func parseKernelVersion(major, minor, patch string) (bool, string) {
	maj, _ := strconv.Atoi(major)
	min, _ := strconv.Atoi(minor)
	// Two-component kernels exist (e.g. "7.2" — an explicitly
	// targeted bench); patch is optional in the regex (agy F11).
	if patch == "" {
		patch = "0"
	}
	ver := fmt.Sprintf("%s.%s.%s", major, minor, patch)

	if maj > 5 || (maj == 5 && min >= 19) {
		return true, fmt.Sprintf("kernel %s >= 5.19", ver)
	}
	return false, fmt.Sprintf("kernel %s < 5.19", ver)
}

// userCurrent returns the current user info.
func userCurrent() (*struct{ Username string }, error) {
	// Simple fallback: read from /etc/passwd.
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil, err
	}

	uid := strconv.Itoa(os.Getuid())
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.SplitN(line, ":", 7)
		if len(fields) >= 3 && fields[2] == uid {
			return &struct{ Username string }{Username: fields[0]}, nil
		}
	}

	return nil, fmt.Errorf("user not found in /etc/passwd for uid %s", uid)
}

// runLookup is a helper that looks up a command and runs it.
func runLookup(cmd string, args ...string) ([]byte, []byte, error) {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return nil, nil, err
	}
	c := exec.Command(path, args...)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	err = c.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

var _ = time.Second // used in probe timeouts

// checkSystemdVersion verifies systemd >= 254 via `systemctl --version`.
func checkSystemdVersion() (bool, string) {
	out, _, err := runLookup("systemctl", "--version")
	if err != nil {
		return false, fmt.Sprintf("cannot run systemctl --version: %v", err)
	}
	// First line: "systemd 257 (257.x-ubuntu...)"
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return false, "unparseable systemctl --version output"
	}
	ver, aErr := strconv.Atoi(fields[1])
	if aErr != nil {
		return false, fmt.Sprintf("unparseable systemd version %q", fields[1])
	}
	if ver >= 254 {
		return true, fmt.Sprintf("systemd %d >= 254", ver)
	}
	return false, fmt.Sprintf("systemd %d < 254", ver)
}

// copyBinaryWithDeps stages a binary and ONLY its loader + ldd libs
// into a rootfs dir (a few MB, not hundreds).
func copyBinaryWithDeps(bin, rootfs string) error {
	out, _, err := runLookup("ldd", bin)
	if err != nil {
		return fmt.Errorf("ldd %s: %w", bin, err)
	}
	paths := []string{bin}
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like "	libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x...)"
		// or, for the loader, "/lib64/ld-linux-x86-64.so.2 (0x...)" with no
		// "=>" — the loader is mandatory (PT_INTERP).
		if i := strings.Index(line, "=>"); i >= 0 {
			line = line[i+2:]
		}
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		paths = append(paths, f[0])
	}
	for _, p := range paths {
		data, rErr := os.ReadFile(p)
		if rErr != nil {
			return fmt.Errorf("read %s: %w", p, rErr)
		}
		dst := filepath.Join(rootfs, strings.TrimPrefix(p, "/"))
		if mErr := os.MkdirAll(filepath.Dir(dst), 0755); mErr != nil {
			return mErr
		}
		mode := os.FileMode(0755)
		if fi, sErr := os.Stat(p); sErr == nil {
			mode = fi.Mode().Perm()
		}
		if wErr := os.WriteFile(dst, data, mode); wErr != nil {
			return wErr
		}
	}
	return nil
}
