package custos_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

func TestResolveHearthHome_Precedence(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	fallbackExpected := filepath.Join(tempHome, ".lararium")

	overridePath := filepath.Join(t.TempDir(), "flag-hearth")
	envPath := filepath.Join(t.TempDir(), "env-hearth")
	cfgPath := filepath.Join(t.TempDir(), "cfg-hearth")

	cases := []struct {
		name       string
		override   string
		envVal     *string // nil means unset
		configHome string
		want       string
	}{
		{
			name:       "override takes absolute precedence",
			override:   overridePath,
			envVal:     &envPath,
			configHome: cfgPath,
			want:       overridePath,
		},
		{
			name:       "env HEARTH_HOME takes precedence over config",
			override:   "",
			envVal:     &envPath,
			configHome: cfgPath,
			want:       envPath,
		},
		{
			name:       "env HEARTH_HOME takes precedence over fallback when config empty",
			override:   "",
			envVal:     &envPath,
			configHome: "",
			want:       envPath,
		},
		{
			name:       "configHome takes precedence when HEARTH_HOME unset",
			override:   "",
			envVal:     nil,
			configHome: cfgPath,
			want:       cfgPath,
		},
		{
			name:       "configHome takes precedence when HEARTH_HOME set-but-empty",
			override:   "",
			envVal:     ptrString(""),
			configHome: cfgPath,
			want:       cfgPath,
		},
		{
			name:       "fallback to HOME/.lararium when all unset",
			override:   "",
			envVal:     nil,
			configHome: "",
			want:       fallbackExpected,
		},
		{
			name:       "fallback to HOME/.lararium when HEARTH_HOME set-but-empty and configHome empty",
			override:   "",
			envVal:     ptrString(""),
			configHome: "",
			want:       fallbackExpected,
		},
		{
			name:       "tilde expansion on configHome",
			override:   "",
			envVal:     nil,
			configHome: "~/.custom-hearth",
			want:       filepath.Join(tempHome, ".custom-hearth"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal == nil {
				os.Unsetenv("HEARTH_HOME")
			} else {
				t.Setenv("HEARTH_HOME", *tc.envVal)
			}

			got := custos.ResolveHearthHome(tc.override, tc.configHome)
			if got != tc.want {
				t.Errorf("ResolveHearthHome(%q, %q) = %q, want %q", tc.override, tc.configHome, got, tc.want)
			}
		})
	}
}

func TestResolveHearthHomeWithFallback_Precedence(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	overridePath := filepath.Join(t.TempDir(), "flag-hearth")
	envPath := filepath.Join(t.TempDir(), "env-hearth")
	cfgPath := filepath.Join(t.TempDir(), "cfg-hearth")

	cases := []struct {
		name       string
		override   string
		envVal     *string
		configHome string
		fallback   string
		want       string
	}{
		{
			name:       "custos and custosd cwd fallback: all unset resolves to .",
			override:   "",
			envVal:     nil,
			configHome: "",
			fallback:   ".",
			want:       ".",
		},
		{
			name:       "custos and custosd cwd fallback: HEARTH_HOME empty resolves to .",
			override:   "",
			envVal:     ptrString(""),
			configHome: "",
			fallback:   ".",
			want:       ".",
		},
		{
			name:       "override takes precedence over dot fallback",
			override:   overridePath,
			envVal:     &envPath,
			configHome: cfgPath,
			fallback:   ".",
			want:       overridePath,
		},
		{
			name:       "env takes precedence over dot fallback",
			override:   "",
			envVal:     &envPath,
			configHome: cfgPath,
			fallback:   ".",
			want:       envPath,
		},
		{
			name:       "configHome takes precedence over dot fallback",
			override:   "",
			envVal:     nil,
			configHome: cfgPath,
			fallback:   ".",
			want:       cfgPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal == nil {
				os.Unsetenv("HEARTH_HOME")
			} else {
				t.Setenv("HEARTH_HOME", *tc.envVal)
			}

			got := custos.ResolveHearthHomeWithFallback(tc.override, tc.configHome, tc.fallback)
			if got != tc.want {
				t.Errorf("ResolveHearthHomeWithFallback(%q, %q, %q) = %q, want %q",
					tc.override, tc.configHome, tc.fallback, got, tc.want)
			}
		})
	}
}

func ptrString(s string) *string {
	return &s
}
