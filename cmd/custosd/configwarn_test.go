package main

import (
	"path/filepath"
	"testing"
)

func TestStrayConnectorsWarning(t *testing.T) {
	if w := strayConnectorsWarning([]byte("connectors:\n  - name: x\n")); w == "" {
		t.Fatal("stray top-level connectors must warn")
	}
	if w := strayConnectorsWarning([]byte("custos:\n  connectors:\n    - name: x\n")); w != "" {
		t.Fatalf("correct shape must stay silent, got %q", w)
	}
	if w := strayConnectorsWarning([]byte("not: [valid: yaml: {{{")); w != "" {
		t.Fatalf("unparseable must stay silent, got %q", w)
	}
}

func TestCustosdLoadConfig_CwdFallback(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("HEARTH_HOME", "")

	workDir := t.TempDir()
	t.Chdir(workDir)

	stateDir, _ := loadConfig("lararium.yaml", "")
	// When hearth.home config is empty, hearthOverride is empty, and HEARTH_HOME is unset,
	// home must resolve to "." (cwd) giving stateDir "custos", NOT $HOME/.lararium/custos.
	if stateDir != "custos" {
		t.Fatalf("loadConfig stateDir = %q, want %q (home resolved to %q instead of cwd \".\")",
			stateDir, "custos", filepath.Dir(stateDir))
	}
}
