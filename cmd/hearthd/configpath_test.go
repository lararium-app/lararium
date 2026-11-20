package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPath(t *testing.T) {
	tests := []struct {
		name         string
		env          *string
		createCwd    bool
		createSystem bool
		wantSystem   bool
		wantExact    string
	}{
		{
			name:         "env wins over existing cwd AND existing system file",
			env:          ptr("custom-env.yaml"),
			createCwd:    true,
			createSystem: true,
			wantExact:    "custom-env.yaml",
		},
		{
			name:         "cwd used when env unset",
			env:          nil,
			createCwd:    true,
			createSystem: true,
			wantExact:    "lararium.yaml",
		},
		{
			name:         "system used when neither env nor cwd",
			env:          nil,
			createCwd:    false,
			createSystem: true,
			wantSystem:   true,
		},
		{
			name:         "plain fallback when none exist",
			env:          nil,
			createCwd:    false,
			createSystem: false,
			wantExact:    "lararium.yaml",
		},
		{
			name:         "env set to empty string behaves as unset",
			env:          ptr(""),
			createCwd:    false,
			createSystem: true,
			wantSystem:   true,
		},
		{
			name:         "env set to empty string behaves as unset with cwd",
			env:          ptr(""),
			createCwd:    true,
			createSystem: true,
			wantExact:    "lararium.yaml",
		},
		{
			name:         "env set to empty string behaves as unset when none exist",
			env:          ptr(""),
			createCwd:    false,
			createSystem: false,
			wantExact:    "lararium.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwdDir := t.TempDir()
			t.Chdir(cwdDir)

			sysDir := t.TempDir()
			sysPath := filepath.Join(sysDir, "lararium.yaml")

			if tt.createCwd {
				if err := os.WriteFile(filepath.Join(cwdDir, "lararium.yaml"), []byte("dummy-cwd"), 0o600); err != nil {
					t.Fatalf("failed to create cwd config: %v", err)
				}
			}
			if tt.createSystem {
				if err := os.WriteFile(sysPath, []byte("dummy-system"), 0o600); err != nil {
					t.Fatalf("failed to create system config: %v", err)
				}
			}

			if tt.env != nil {
				t.Setenv("LARARIUM_CONFIG", *tt.env)
			} else {
				// "unset" is exercised as empty-string: the chain
				// treats empty exactly as unset (covered by its own
				// case), and t.Setenv restores the real value.
				t.Setenv("LARARIUM_CONFIG", "")
			}

			want := tt.wantExact
			if tt.wantSystem {
				want = sysPath
			}

			got := defaultConfigPath(sysPath)
			if got != want {
				t.Errorf("defaultConfigPath(%q) = %q, want %q", sysPath, got, want)
			}
		})
	}
}

func ptr[T any](v T) *T {
	return &v
}
