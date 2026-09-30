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

	// Check: subuid/subgid ranges.
	if ok, detail := checkSubUID(); ok {
		results = append(results, CheckResult{"subuid", "OK", detail})
	} else {
		results = append(results, CheckResult{"subuid", "FAIL", detail})
		failed = true
	}

	// Check: systemd-machined reachable.
	if _, _, err := s.runner.Run("systemctl", "is-active", "systemd-machined"); err != nil {
		results = append(results, CheckResult{"systemd-machined", "FAIL", "systemd-machined is not active"})
		failed = true
	} else {
		results = append(results, CheckResult{"systemd-machined", "OK", "active and reachable"})
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
	tmpdir, err := os.MkdirTemp("", "lararium-overlay-probe-*")
	if err != nil {
		return false, fmt.Sprintf("cannot create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	lower := filepath.Join(tmpdir, "lower")
	upper := filepath.Join(tmpdir, "upper")
	work := filepath.Join(tmpdir, "work")
	merged := filepath.Join(tmpdir, "merged")

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
	_, _, err = s.runner.Run("/bin/mount", "-t", "overlay", "overlay", merged, "-o", opts)
	if err != nil {
		return false, fmt.Sprintf("mount overlay failed: %v", err)
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
	// Use template if available, otherwise create a minimal rootfs.
	dir := s.TemplateDir()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Create a minimal dir for the probe.
		var err error
		dir, err = os.MkdirTemp("", "lararium-userns-probe-*")
		if err != nil {
			return false, fmt.Sprintf("cannot create temp dir: %v", err)
		}
		defer os.RemoveAll(dir)

		// Create minimal structure (usr/bin needed before id copy).
		for _, d := range []string{"bin", "sbin", "usr/bin", "usr/lib", "etc", "proc", "sys", "dev"} {
			os.MkdirAll(filepath.Join(dir, d), 0755)
		}
		// Copy /bin/sh into the temp rootfs.
		s.runner.Run("cp", "-a", "/bin/sh", filepath.Join(dir, "bin/sh"))
		s.runner.Run("cp", "-a", "/bin/ls", filepath.Join(dir, "bin/ls"))
		s.runner.Run("cp", "-a", "/usr/bin/id", filepath.Join(dir, "usr/bin/id"))
		// Copy required libs.
		s.runner.Run("cp", "-a", "/lib", filepath.Join(dir, "lib"))
		s.runner.Run("cp", "-a", "/lib64", filepath.Join(dir, "lib64"))
		s.runner.Run("cp", "-a", "/usr/lib", filepath.Join(dir, "usr/lib"))
	}

	// Run throwaway nspawn with --private-users=pick.
	args := []string{
		"--private-users=pick",
		"--private-users-ownership=auto",
		"--ephemeral",
		"--register=no",
		"-D", dir,
		"/bin/sh", "-c", "id -u",
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
	if output != "" {
		return true, fmt.Sprintf("userns nspawn probe succeeded (root maps to uid %s)", output)
	}

	return false, "nspawn probe produced no output"
}

// checkKernel verifies kernel version >= 5.19.
func checkKernel() (bool, string) {
	// Read kernel version from /proc/version or uname.
	re := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

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
	_, _ = strconv.Atoi(patch)
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
