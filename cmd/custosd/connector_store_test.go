package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A corrupt <hearth>/custos/connectors.json carries user intent (tombstones),
// so custosd refuses to start with a clear error and leaves the file alone
// (CONNECTOR-LIFE-SPEC D2).
func TestCustosdRefusesCorruptConnectorStore(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	stateDir := filepath.Join(home, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "lararium.yaml")
	if err := os.WriteFile(cfgPath, []byte("hearth:\n  home: "+home+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"connectors":[`)
	storePath := filepath.Join(stateDir, "connectors.json")
	if err := os.WriteFile(storePath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	var stderr safeBuffer
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("custosd started over a corrupt connector store")
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("custosd did not refuse a corrupt connector store")
	}
	if !strings.Contains(stderr.String(), "connector store") {
		t.Fatalf("stderr = %q, want a clear connector store error", stderr.String())
	}
	if got, _ := os.ReadFile(storePath); string(got) != string(corrupt) {
		t.Fatal("corrupt store was modified")
	}
}
