package cellunit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/cell"
)

// fakeRunner records all invocations for assertion.
type fakeRunner struct {
	invocations []fakeCall
	errors      map[string]error // keyed by "cmd arg1 arg2 ..."
	// activeSkip: answer this many initial `systemctl is-active` checks
	// with "inactive" before switching to "active". Models a Start flow
	// where the double-start guard sees an idle cell but waitForHealthy
	// then sees the unit come up.
	activeSkip int
	// machineUnknown flips machinectl show to "not known" after a
	// terminate, modeling machined releasing a stale registration.
	machineUnknown bool
}

type fakeCall struct {
	Method string // "Run", "RunCombined", "StartDetached"
	Cmd    string
	Args   []string
}

func (f *fakeRunner) Run(cmd string, args ...string) ([]byte, []byte, error) {
	f.invocations = append(f.invocations, fakeCall{"Run", cmd, args})
	key := cmd + " " + strings.Join(args, " ")
	if err := f.errors[key]; err != nil {
		return nil, []byte(err.Error()), err
	}
	// For systemctl is-active, honor the activeSkip counter.
	if cmd == "systemctl" && len(args) > 0 && args[0] == "is-active" {
		if f.activeSkip > 0 {
			f.activeSkip--
			return nil, []byte("inactive\n"), fmt.Errorf("inactive")
		}
		return []byte("active\n"), nil, nil
	}
	// For systemctl show, return a fake MainPID.
	if cmd == "systemctl" && len(args) > 0 && args[0] == "show" {
		return []byte("MainPID=12345\n"), nil, nil
	}
	// For machinectl show: known by default (a running machine);
	// `machinectl terminate` flips it away, and tests can override
	// via f.errors for the not-running case.
	if cmd == "machinectl" && len(args) > 0 && args[0] == "show" {
		if f.machineUnknown {
			return nil, nil, fmt.Errorf("Machine %s not known.", args[1])
		}
		return nil, nil, nil
	}
	if cmd == "machinectl" && len(args) > 0 && args[0] == "terminate" {
		f.machineUnknown = true
	}
	// A successful systemd-run boots nspawn, which re-registers the
	// machine with machined (models the stale-prune -> spawn -> register
	// sequence the real system runs).
	if cmd == "systemd-run" || cmd == "/usr/bin/systemd-run" {
		f.machineUnknown = false
	}
	return nil, nil, nil
}

func (f *fakeRunner) RunCombined(cmd string, args ...string) ([]byte, error) {
	f.invocations = append(f.invocations, fakeCall{"RunCombined", cmd, args})
	key := cmd + " " + strings.Join(args, " ")
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	// A successful systemd-run boots nspawn, which registers the
	// machine with machined (stale-prune -> spawn -> register).
	if cmd == "/usr/bin/systemd-run" || cmd == "systemd-run" {
		f.machineUnknown = false
	}
	return nil, nil
}

func (f *fakeRunner) StartDetached(cmd string, args ...string) (int, error) {
	f.invocations = append(f.invocations, fakeCall{"StartDetached", cmd, args})
	key := cmd + " " + strings.Join(args, " ")
	if err := f.errors[key]; err != nil {
		return 0, err
	}
	return 42, nil
}

// isBusProbeCall identifies waitForHealthy's readiness probe
// (systemd-run --machine=... -- /bin/true), distinguishing it from
// the cell-spawn invocation in Start.
func isBusProbeCall(args []string) bool {
	for _, a := range args {
		if a == "/bin/true" {
			return true
		}
	}
	return false
}

func containsAll(haystack, needle []string) bool {
	for _, n := range needle {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestMain(m *testing.M) {
	// Skip root-gated tests in non-root CI.
	if os.Geteuid() == 0 {
		os.Exit(m.Run())
	}
	os.Exit(m.Run())
}

func TestValidateID(t *testing.T) {
	tests := []struct {
		id   string
		want error
	}{
		{"mycell", nil},
		{"test-123", nil},
		{"a", nil},
		{"..", cell.ErrInvalidID},
		{"a/b", cell.ErrInvalidID},
		{"a..b", cell.ErrInvalidID}, // ".." as substring is rejected
		{"", cell.ErrInvalidID},
		{"a/..", cell.ErrInvalidID},
		{"../a", cell.ErrInvalidID},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			tmp := t.TempDir()
			store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

			// Test via Create (which calls validateID).
			err := store.Create(tt.id)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Errorf("Create(%q) error = %v, want %v", tt.id, err, tt.want)
				}
				return
			}
			// For valid IDs, Create will fail because no template exists.
			// That's expected — we just want to verify validateID passed.
			if err != nil && errors.Is(err, cell.ErrInvalidID) {
				t.Errorf("Create(%q) got ErrInvalidID, want success (or template error)", tt.id)
			}
		})
	}
}

func TestCellLoadSave(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	c := &cell.Cell{
		ID:          "testcell",
		SubUIDBase:  100000,
		SubUIDCount: 65536,
	}

	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load("testcell")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != c.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, c.ID)
	}
	if loaded.SubUIDBase != c.SubUIDBase {
		t.Errorf("SubUIDBase = %d, want %d", loaded.SubUIDBase, c.SubUIDBase)
	}
	if loaded.SubUIDCount != c.SubUIDCount {
		t.Errorf("SubUIDCount = %d, want %d", loaded.SubUIDCount, c.SubUIDCount)
	}
}

func TestCellNotFound(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	_, err := store.Load("nonexistent")
	if !errors.Is(err, cell.ErrCellNotFound) {
		t.Errorf("Load(nonexistent) error = %v, want ErrCellNotFound", err)
	}
}

func TestCellList(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create two cells.
	for _, id := range []string{"alpha", "beta"} {
		c := &cell.Cell{ID: id}
		if err := store.Save(c); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	ids, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("List returned %d cells, want 2", len(ids))
	}
}

func TestStartArgv(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create template directory.
	if err := os.MkdirAll(store.TemplateDir(), 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}

	// Create cell.json.
	c := &cell.Cell{ID: "test123"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Create merged dir.
	if err := os.MkdirAll(store.MergedDir("test123"), 0o755); err != nil {
		t.Fatalf("mkdir merged: %v", err)
	}

	runner := &fakeRunner{activeSkip: 1}
	store.SetRunner(runner)

	// Start will try to run systemd-run, then wait for healthy, then record uid_map.
	// We expect the systemd-run call to succeed (fake returns nil).
	err := store.Start("test123")
	// Start may fail at uid_map recording (no /proc/12345), but we check the argv.
	_ = err

	// Verify systemd-run was called with correct arguments.
	found := false
	for _, call := range runner.invocations {
		if call.Cmd != "systemd-run" && call.Cmd != "/usr/bin/systemd-run" {
			continue
		}
		if isBusProbeCall(call.Args) {
			continue // waitForHealthy's readiness probe, not the spawn
		}
		found = true

		// Check systemd-run properties. (--keep-unit is NOT a systemd-run
		// option — live-probed on systemd 257. It belongs to nspawn and
		// must appear exactly once, after the nspawn argument.)
		requiredRunFlags := []string{
			"--unit=lararium-cell-test123",
			"--property=MemoryMax=8192M",
			"--property=MemorySwapMax=0",    // E6: hard cap, no swap lending
			"--property=OOMPolicy=continue", // E6b: kill hog, spare cell
			"--property=CPUQuota=200%",
			"--property=TasksMax=512",
			"--property=Restart=no",
			"--property=KillMode=mixed",
		}
		for _, flag := range requiredRunFlags {
			if !containsAll(call.Args, []string{flag}) {
				t.Errorf("systemd-run missing flag %q in args: %v", flag, call.Args)
			}
		}

		// Check nspawn is the executable.
		if !containsAll(call.Args, []string{"systemd-nspawn"}) {
			t.Errorf("systemd-run should exec systemd-nspawn, args: %v", call.Args)
		}

		// --keep-unit must appear exactly once and belong to nspawn
		// (everything after the "systemd-nspawn" arg belongs to nspawn).
		keepCount := 0
		nspawnIdx := -1
		for i, arg := range call.Args {
			if arg == "systemd-nspawn" && nspawnIdx < 0 {
				nspawnIdx = i
			}
			if arg == "--keep-unit" {
				keepCount++
				if nspawnIdx < 0 || i < nspawnIdx {
					t.Errorf("--keep-unit passed to systemd-run (invalid); args: %v", call.Args)
				}
			}
		}
		if keepCount != 1 {
			t.Errorf("--keep-unit count = %d, want exactly 1 (nspawn's); args: %v", keepCount, call.Args)
		}

		// Check nspawn flags.
		requiredNspawnFlags := []string{
			"--machine=test123",
			"--register=yes",
			"--boot",
			"--keep-unit",
			"--private-users=pick",
			"--private-users-ownership=auto",
			"--network-veth",
		}
		for _, flag := range requiredNspawnFlags {
			if !containsAll(call.Args, []string{flag}) {
				t.Errorf("nspawn missing flag %q in args: %v", flag, call.Args)
			}
		}
		// T5: --private-network must be GONE (the veth is the cage's
		// anchor; --private-network would leave it unattached).
		if containsAll(call.Args, []string{"--private-network"}) {
			t.Errorf("nspawn still passes --private-network (T4 leftover): %v", call.Args)
		}

		// Check -D points to merged dir.
		dFlag := false
		for i, arg := range call.Args {
			if arg == "-D" && i+1 < len(call.Args) {
				if strings.HasSuffix(call.Args[i+1], "cells/test123/merged") {
					dFlag = true
				}
			}
		}
		if !dFlag {
			t.Errorf("nspawn missing -D <merged> in args: %v", call.Args)
		}

		// Check bind mounts.
		binds := []string{
			"--bind=", // workspace
		}
		for _, prefix := range binds {
			foundBind := false
			for _, arg := range call.Args {
				if strings.HasPrefix(arg, prefix) && strings.HasSuffix(arg, ":/workspace") {
					foundBind = true
					break
				}
			}
			if !foundBind {
				t.Errorf("nspawn missing workspace bind in args: %v", call.Args)
			}
		}
	}

	if !found {
		t.Errorf("systemd-run was not called; invocations: %v", runner.invocations)
	}
}

func TestRunArgv(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	runner := &fakeRunner{}
	store.SetRunner(runner)

	// Run should check machinectl show first, then call systemd-run.
	// machinectl show succeeds by default in our fake.
	opts := cell.RunOpts{
		CWD:     "/workspace",
		Timeout: 30,
	}

	_, err := store.Run("mycell", "echo hello", opts)
	// May fail if runner returns error, but we check argv.
	_ = err

	// Verify systemd-run was called with --machine.
	found := false
	for _, call := range runner.invocations {
		if call.Cmd != "systemd-run" && call.Cmd != "/usr/bin/systemd-run" {
			continue
		}
		if isBusProbeCall(call.Args) {
			continue // waitForHealthy's readiness probe, not the spawn
		}
		found = true

		required := []string{
			"--machine=mycell",
			"--wait",
			"--pipe",
		}
		for _, flag := range required {
			if !containsAll(call.Args, []string{flag}) {
				t.Errorf("systemd-run --machine missing flag %q in args: %v", flag, call.Args)
			}
		}

		// Per spec §6 the payload is a plain /bin/sh -c under machined
		// (systemd-nspawn has NO "exec" verb and no -w flag — live-probed
		// on systemd 257; fabricating them breaks every Run at runtime).
		if containsAll(call.Args, []string{"systemd-nspawn"}) {
			t.Errorf("Run must not invoke systemd-nspawn (machined exec only), args: %v", call.Args)
		}
		if !containsAll(call.Args, []string{"--", "/bin/sh", "-c", "echo hello"}) {
			t.Errorf("Run payload missing '-- /bin/sh -c <cmd>', args: %v", call.Args)
		}
		if !containsAll(call.Args, []string{"--property=WorkingDirectory=/workspace"}) {
			t.Errorf("Run missing WorkingDirectory property, args: %v", call.Args)
		}
	}

	if !found {
		t.Errorf("systemd-run was not called; invocations: %v", runner.invocations)
	}
}

func TestRunNotRunning(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	runner := &fakeRunner{}
	// Make machinectl show fail.
	runner.errors = map[string]error{
		"machinectl show mycell": fmt.Errorf("no such machine"),
	}
	store.SetRunner(runner)

	_, err := store.Run("mycell", "echo hello", cell.RunOpts{})
	if !errors.Is(err, cell.ErrNotRunning) {
		t.Errorf("Run(not registered) error = %v, want ErrNotRunning", err)
	}

	// Verify runner was only called for machinectl show, not systemd-run.
	for _, call := range runner.invocations {
		if (call.Cmd == "systemd-run" || call.Cmd == "/usr/bin/systemd-run") && !isBusProbeCall(call.Args) {
			t.Errorf("Run should not call systemd-run when cell not running; got call: %v", call)
		}
	}
}

func TestMountOverlayArgv(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create dirs.
	for _, d := range []string{store.UpperDir("mtest"), store.WorkDir("mtest"), store.MergedDir("mtest")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	runner := &fakeRunner{}
	store.SetRunner(runner)

	err := store.MountOverlay("mtest")
	if err != nil {
		t.Fatalf("MountOverlay: %v", err)
	}

	// Verify mount was called with overlay type.
	if len(runner.invocations) == 0 {
		t.Fatal("no runner invocations")
	}

	call := runner.invocations[0]
	if call.Cmd != "/bin/mount" {
		t.Errorf("expected /bin/mount, got %q", call.Cmd)
	}

	// Check args contain -t overlay.
	if !containsAll(call.Args, []string{"-t", "overlay"}) {
		t.Errorf("mount missing -t overlay in args: %v", call.Args)
	}

	// Check -o contains lowerdir, upperdir, workdir.
	var optsArg string
	for i, arg := range call.Args {
		if arg == "-o" && i+1 < len(call.Args) {
			optsArg = call.Args[i+1]
			break
		}
	}
	if optsArg == "" {
		t.Errorf("mount missing -o in args: %v", call.Args)
	}
	if !strings.Contains(optsArg, "lowerdir=") {
		t.Errorf("mount -o missing lowerdir: %q", optsArg)
	}
	if !strings.Contains(optsArg, "upperdir=") {
		t.Errorf("mount -o missing upperdir: %q", optsArg)
	}
	if !strings.Contains(optsArg, "workdir=") {
		t.Errorf("mount -o missing workdir: %q", optsArg)
	}
}

func TestDestroyUnmountThenDelete(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create cell.json and dirs.
	c := &cell.Cell{ID: "dtest"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, d := range []string{store.UpperDir("dtest"), store.WorkDir("dtest"), store.MergedDir("dtest")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	runner := &fakeRunner{}
	// Make systemctl is-active fail (cell not running).
	runner.errors = map[string]error{
		"systemctl is-active lararium-cell-dtest": fmt.Errorf("inactive"),
	}
	store.SetRunner(runner)

	err := store.Destroy("dtest")
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Verify cell.json was removed.
	if _, err := os.Stat(store.CellJSONPath("dtest")); !os.IsNotExist(err) {
		t.Error("cell.json should be removed after destroy")
	}

	// Verify upper was removed.
	if _, err := os.Stat(store.UpperDir("dtest")); !os.IsNotExist(err) {
		t.Error("upper/ should be removed after destroy")
	}

	// Verify work was removed.
	if _, err := os.Stat(store.WorkDir("dtest")); !os.IsNotExist(err) {
		t.Error("work/ should be removed after destroy")
	}
}

func TestStartWithoutTemplate(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// No template directory exists.
	err := store.Start("notest")
	if err == nil {
		t.Fatal("Start without template should fail")
	}
	if !strings.Contains(err.Error(), "build-template") {
		t.Errorf("Start error should mention build-template: %v", err)
	}
}

func TestCreateWithoutTemplate(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	err := store.Create("notest")
	if err == nil {
		t.Fatal("Create without template should fail")
	}
	if !strings.Contains(err.Error(), "build-template") {
		t.Errorf("Create error should mention build-template: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create template dir.
	if err := os.MkdirAll(store.TemplateDir(), 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}

	// Create cell.json.
	c := &cell.Cell{ID: "dup"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Spec §6: create is idempotent — existing cell is a no-op,
	// cell.json must survive untouched (uid base preserved).
	err := store.Create("dup")
	if err != nil {
		t.Errorf("Create(dup) error = %v, want nil (idempotent)", err)
	}
	if _, err := store.Load("dup"); err != nil {
		t.Errorf("cell.json lost after idempotent re-create: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "lararium.yaml")

	// Write minimal config.
	configData := []byte(`schema_version: 1
cell:
  memory_mb: 4096
`)
	if err := os.WriteFile(cfgPath, configData, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := cell.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Cell.MemoryMB != 4096 {
		t.Errorf("MemoryMB = %d, want 4096", cfg.Cell.MemoryMB)
	}
	if cfg.Cell.CPUQuota != "200" {
		t.Errorf("CPUQuota = %q, want %q", cfg.Cell.CPUQuota, "200")
	}
	if cfg.Cell.TasksMax != 512 {
		t.Errorf("TasksMax = %d, want 512", cfg.Cell.TasksMax)
	}
	if cfg.Cell.Ownership != "auto" {
		t.Errorf("Ownership = %q, want %q", cfg.Cell.Ownership, "auto")
	}
}

func TestConfigEmpty(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "lararium.yaml")

	// Write empty config.
	if err := os.WriteFile(cfgPath, []byte{}, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := cell.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	def := cell.DefaultLimits()
	if cfg.Cell.MemoryMB != def.MemoryMB {
		t.Errorf("MemoryMB = %d, want %d", cfg.Cell.MemoryMB, def.MemoryMB)
	}
	if cfg.Cell.CPUQuota != def.CPUQuota {
		t.Errorf("CPUQuota = %q, want %q", cfg.Cell.CPUQuota, def.CPUQuota)
	}
}

func TestCellJSONSchema(t *testing.T) {
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	c := &cell.Cell{
		ID:          "schema",
		SubUIDBase:  100000,
		SubUIDCount: 65536,
	}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Read raw JSON and verify field names.
	data, err := os.ReadFile(store.CellJSONPath("schema"))
	if err != nil {
		t.Fatalf("read cell.json: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse cell.json: %v", err)
	}

	expectedFields := []string{"id", "subuid_base", "created", "subuid_count"}
	for _, field := range expectedFields {
		if _, ok := raw[field]; !ok {
			t.Errorf("cell.json missing field %q", field)
		}
	}
}

func TestUnitName(t *testing.T) {
	name := cell.UnitName("mycell")
	if name != "lararium-cell-mycell" {
		t.Errorf("UnitName(mycell) = %q, want %q", name, "lararium-cell-mycell")
	}
}

func TestNspawnArgs(t *testing.T) {
	// nspawnArgs is unexported; argv assertions are covered by TestStartArgv.
	// This test verifies the flags are present in the Start invocation.
	tmp := t.TempDir()
	store := cell.NewStore(tmp, tmp+"/hearth", cell.DefaultLimits(), &fakeRunner{})

	// Create template directory.
	if err := os.MkdirAll(store.TemplateDir(), 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}

	// Create cell.json.
	c := &cell.Cell{ID: "nspawn-test"}
	if err := store.Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Create merged dir.
	if err := os.MkdirAll(store.MergedDir("nspawn-test"), 0o755); err != nil {
		t.Fatalf("mkdir merged: %v", err)
	}

	runner := &fakeRunner{activeSkip: 1}
	store.SetRunner(runner)

	// Start will call systemd-run with nspawn args.
	_ = store.Start("nspawn-test")

	// Verify nspawn flags in the systemd-run invocation.
	found := false
	for _, call := range runner.invocations {
		if call.Cmd != "systemd-run" && call.Cmd != "/usr/bin/systemd-run" {
			continue
		}
		if isBusProbeCall(call.Args) {
			continue // waitForHealthy's readiness probe, not the spawn
		}
		found = true

		required := []string{
			"--machine=nspawn-test",
			"--register=yes",
			"--boot",
			"--keep-unit",
			"--private-users=pick",
			"--private-users-ownership=auto",
			"--network-veth",
		}
		for _, flag := range required {
			if !containsAll(call.Args, []string{flag}) {
				t.Errorf("nspawn missing flag %q in args: %v", flag, call.Args)
			}
		}
		if containsAll(call.Args, []string{"--private-network"}) {
			t.Errorf("nspawn still passes --private-network (T4 leftover): %v", call.Args)
		}

		// Check bind mounts (bin is via --bind-ro; option tokens like
		// :ro/:nodev are not valid nspawn bind options — live-probed).
		binds := []string{":/workspace", ":/hearth", ":/opt/lararium"}
		for _, bind := range binds {
			foundBind := false
			for _, arg := range call.Args {
				if strings.HasSuffix(arg, bind) {
					foundBind = true
					break
				}
			}
			if !foundBind {
				t.Errorf("nspawn missing bind %q in args: %v", bind, call.Args)
			}
		}
	}

	if !found {
		t.Errorf("systemd-run was not called; invocations: %v", runner.invocations)
	}
}
