package main

import (
	"path/filepath"
	"testing"
)

func TestCustosLoadConfig_CwdFallback(t *testing.T) {
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
