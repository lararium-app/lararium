package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBwrapDir writes a `bwrap` stub that prints one argv element per line
// and exits, and returns the dir to prepend to PATH.
func fakeBwrapDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(dir, "bwrap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSandbox_WrappedArgv(t *testing.T) {
	t.Setenv("PATH", fakeBwrapDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := filepath.Join(t.TempDir(), "home")
	state := filepath.Join(t.TempDir(), "state")
	profile := filepath.Join(state, "profile")

	bwrap, err := bwrapAvailable()
	if err != nil {
		t.Fatalf("bwrapAvailable: %v", err)
	}
	shim, cleanup, err := writeBwrapShim(bwrap, "/opt/chrome/chrome", isolationParams{
		Home: home, StateRoot: state, UserDataDir: profile,
	})
	if err != nil {
		t.Fatalf("writeBwrapShim: %v", err)
	}
	defer cleanup()

	st, err := os.Stat(shim)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("shim mode = %v, want 0755", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Dir(shim))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm()&0o077 != 0 {
		t.Errorf("shim dir mode = %v, want private", dst.Mode().Perm())
	}
	body, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "#!/bin/sh\n") {
		t.Errorf("shim is not a POSIX sh script: %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(body)), `"$@"`) {
		t.Errorf("shim must forward \"$@\": %q", body)
	}

	// Run the shim against the fake bwrap with chromedp-style trailing flags.
	out, err := exec.Command(shim, "--remote-debugging-port=0", "--headless").Output()
	if err != nil {
		t.Fatalf("run shim: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

	has := func(seq ...string) bool {
		for i := 0; i+len(seq) <= len(argv); i++ {
			ok := true
			for j := range seq {
				if argv[i+j] != seq[j] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	for _, want := range [][]string{
		{"--die-with-parent"},
		{"--unshare-user"},
		{"--unshare-uts"},
		{"--unshare-ipc"},
		{"--unshare-pid"},
		{"--ro-bind", "/", "/"},
		{"--dev", "/dev"},
		{"--proc", "/proc"},
		{"--tmpfs", home},
		{"--tmpfs", state},
		{"--tmpfs", "/tmp"},
		{"--bind", profile, profile},
		{"--", "/opt/chrome/chrome", "--remote-debugging-port=0", "--headless"},
	} {
		if !has(want...) {
			t.Errorf("argv missing %v\nargv: %v", want, argv)
		}
	}
	for _, a := range argv {
		if a == "--unshare-net" || a == "--unshare-all" {
			t.Errorf("network must stay shared, found %s", a)
		}
	}
	binds := 0
	for _, a := range argv {
		if a == "--bind" || a == "--bind-try" || a == "--ro-bind-try" {
			binds++
		}
	}
	if binds != 1 {
		t.Errorf("want exactly one rw bind (userDataDir), got %d\nargv: %v", binds, argv)
	}

	// State root inside HOME is covered by the HOME tmpfs: no second tmpfs.
	inside := bwrapProfile(isolationParams{Home: home, StateRoot: filepath.Join(home, ".lararium"), UserDataDir: profile})
	n := 0
	for i, a := range inside {
		if a == "--tmpfs" && inside[i+1] != "/tmp" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("state root inside HOME should not get its own tmpfs; got %d tmpfs mounts: %v", n, inside)
	}
}

func TestSandbox_MissingBwrapRefuses(t *testing.T) {
	orig := execLookPath
	t.Cleanup(func() { execLookPath = orig })
	execLookPath = func(file string) (string, error) {
		if file == "bwrap" {
			return "", exec.ErrNotFound
		}
		return orig(file)
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	chrome := filepath.Join(dir, "fakechrome")
	if err := os.WriteFile(chrome, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Isolate = true
	cfg.ChromePath = chrome
	b := NewBrowser(t.TempDir(), cfg, WithAllowedTestHosts("127.0.0.1"))

	obs, err := b.Browse(t.Context(), "http://127.0.0.1:1/", "goal")
	if err == nil || err.Error() != ErrIsolationUnavailable {
		t.Fatalf("Browse = (%q, %v), want error %q", obs, err, ErrIsolationUnavailable)
	}
	want := "browser isolation unavailable: install bubblewrap or set web.isolate=false"
	if err.Error() != want {
		t.Errorf("error text = %q, want %q", err.Error(), want)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("browser was spawned despite missing bwrap")
	}
}

func TestSandbox_KnobOffRunsDirect(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	orig := isolationLogf
	t.Cleanup(func() { isolationLogf = orig })
	isolationLogf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, format)
	}
	// bwrap present or not must not matter when the knob is off.
	origLP := execLookPath
	t.Cleanup(func() { execLookPath = origLP })
	execLookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	cfg := DefaultConfig()
	cfg.Isolate = false
	b := NewBrowser(t.TempDir(), cfg)

	for range 3 {
		path, cleanup, err := b.launcherPath("/usr/bin/chromium", "/some/profile")
		if err != nil {
			t.Fatalf("launcherPath: %v", err)
		}
		if path != "/usr/bin/chromium" {
			t.Errorf("legacy launch must exec the browser directly, got %q", path)
		}
		cleanup()
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one warning line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "WITHOUT process isolation") {
		t.Errorf("warning does not state missing isolation: %q", lines[0])
	}

	// Legacy argv is untouched: the existing flags, including no-sandbox.
	opts := buildExecAllocatorOptions("/usr/bin/chromium", "/some/profile", "")
	if len(opts) == 0 {
		t.Fatal("no allocator options built")
	}
}

func TestSandbox_ConfigDefaultAndKnob(t *testing.T) {
	if !DefaultConfig().Isolate {
		t.Error("isolate must default to true")
	}
}

func TestSandbox_RealBwrap(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	if os.Getenv("LARARIUM_CHROME") == "" && DetectChrome() == "" {
		t.Skip("chrome not available")
	}
	probe := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "true")
	if err := probe.Run(); err != nil {
		t.Skipf("bwrap cannot create user namespaces here: %v", err)
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	profile := filepath.Join(state, "profile")
	for _, d := range []string{home, state, profile} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".lar-phone-test"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "keystore"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	bwrap, err := bwrapAvailable()
	if err != nil {
		t.Fatal(err)
	}
	shim, cleanup, err := writeBwrapShim(bwrap, "/bin/sh", isolationParams{Home: home, StateRoot: state, UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	script := `test -e ` + shQuote(filepath.Join(home, ".lar-phone-test")) + `; echo $? > ` + shQuote(filepath.Join(profile, "home-probe")) +
		`; test -e ` + shQuote(filepath.Join(state, "keystore")) + `; echo $? > ` + shQuote(filepath.Join(profile, "state-probe")) +
		`; echo survived > ` + shQuote(filepath.Join(profile, "written"))
	if out, err := exec.Command(shim, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("run isolated probe: %v\n%s", err, out)
	}

	for name, want := range map[string]string{"home-probe": "1", "state-probe": "1"} {
		got, err := os.ReadFile(filepath.Join(profile, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %q (path must be hidden inside sandbox)", name, got, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(profile, "written")); err != nil || strings.TrimSpace(string(got)) != "survived" {
		t.Errorf("file written into userDataDir did not survive: %q, %v", got, err)
	}
}
