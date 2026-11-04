package custos_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// Regression (dogfood 2026-11-04): hearthd persists the device registry at
// <hearth>/custos/devices.json per DEVICE-RAILS-SPEC D3, a later amendment to
// the §C2 inventory. Before the allowlist carried it, any home with paired
// devices failed custosd startup with ErrUnexpectedFile after a restart.
func TestCheckFileLawAllowsDevicesJSON(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	devices := filepath.Join(stateDir, "devices.json")
	if err := os.WriteFile(devices, []byte(`{"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.CheckFileLaw(); err != nil {
		t.Fatalf("CheckFileLaw rejected devices.json (DEVICE-RAILS D3 file): %v", err)
	}
}

func TestCheckFileLawStillRejectsUnknownFiles(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rogue := filepath.Join(stateDir, "not-a-registered-file")
	if err := os.WriteFile(rogue, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.CheckFileLaw(); err == nil {
		t.Fatal("CheckFileLaw accepted an unknown file in the state root")
	}
}
