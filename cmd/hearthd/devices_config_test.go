package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevicesConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lararium.yaml")
	// Config with NO devices block at all
	err := os.WriteFile(p, []byte(`hearth:
  home: `+dir+`
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://x
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.Devices.IsAutoApprove() {
		t.Fatalf("expected devices.auto_approve to default to true, got false")
	}
	if cfg.Devices.AutoApprove == nil || !*cfg.Devices.AutoApprove {
		t.Fatalf("expected cfg.Devices.AutoApprove to be non-nil & true")
	}
}

func TestDevicesConfigExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lararium.yaml")
	err := os.WriteFile(p, []byte(`hearth:
  home: `+dir+`
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://x
devices:
  auto_approve: false
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Devices.IsAutoApprove() {
		t.Fatalf("expected devices.auto_approve to be false, got true")
	}
	if cfg.Devices.AutoApprove == nil || *cfg.Devices.AutoApprove {
		t.Fatalf("expected cfg.Devices.AutoApprove to be non-nil & false")
	}
}

func TestDevicesConfigExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lararium.yaml")
	err := os.WriteFile(p, []byte(`hearth:
  home: `+dir+`
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://x
devices:
  auto_approve: true
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.Devices.IsAutoApprove() {
		t.Fatalf("expected devices.auto_approve to be true")
	}
	if cfg.Devices.AutoApprove == nil || !*cfg.Devices.AutoApprove {
		t.Fatalf("expected cfg.Devices.AutoApprove to be non-nil & true")
	}
}

func TestDevicesConfigUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lararium.yaml")
	err := os.WriteFile(p, []byte(`hearth:
  home: `+dir+`
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://x
devices:
  unknown_option: true
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadConfig(p)
	if err == nil {
		t.Fatalf("expected LoadConfig to reject unknown key under devices, got nil")
	}
}
