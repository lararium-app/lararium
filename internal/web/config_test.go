package web

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfig_Defaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled {
		t.Errorf("expected Enabled=true by default, got %v", cfg.Enabled)
	}
	if cfg.BrowseTimeoutS != 30 {
		t.Errorf("expected BrowseTimeoutS=30 by default, got %d", cfg.BrowseTimeoutS)
	}
	if cfg.MaxTextBytes != 200000 {
		t.Errorf("expected MaxTextBytes=200000 by default, got %d", cfg.MaxTextBytes)
	}
}

func TestConfig_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name       string
		yamlData   string
		wantCfg    Config
		wantErrStr string
	}{
		{
			name:     "empty block gets defaults",
			yamlData: `web: {}`,
			wantCfg: Config{
				Enabled:        true,
				BrowseTimeoutS: 30,
				MaxTextBytes:   200000,
			},
		},
		{
			name: "custom valid values",
			yamlData: `
web:
  enabled: false
  browse_timeout_s: 45
  max_text_bytes: 12345
  chrome_path: /opt/bin/chromium
`,
			wantCfg: Config{
				Enabled:        false,
				BrowseTimeoutS: 45,
				MaxTextBytes:   12345,
				ChromePath:     "/opt/bin/chromium",
			},
		},
		{
			name: "partial values retain defaults",
			yamlData: `
web:
  browse_timeout_s: 60
`,
			wantCfg: Config{
				Enabled:        true,
				BrowseTimeoutS: 60,
				MaxTextBytes:   200000,
			},
		},
		{
			name: "zero browse_timeout_s is rejected",
			yamlData: `
web:
  browse_timeout_s: 0
`,
			wantErrStr: "web.browse_timeout_s must be positive",
		},
		{
			name: "negative browse_timeout_s is rejected",
			yamlData: `
web:
  browse_timeout_s: -10
`,
			wantErrStr: "web.browse_timeout_s must be positive",
		},
		{
			name: "zero max_text_bytes is rejected",
			yamlData: `
web:
  max_text_bytes: 0
`,
			wantErrStr: "web.max_text_bytes must be positive",
		},
		{
			name: "negative max_text_bytes is rejected",
			yamlData: `
web:
  max_text_bytes: -50
`,
			wantErrStr: "web.max_text_bytes must be positive",
		},
		{
			name: "unknown fields are rejected",
			yamlData: `
web:
  unknown_setting: true
`,
			wantErrStr: "not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			type wrapped struct {
				Web Config `yaml:"web"`
			}
			var w wrapped
			dec := yaml.NewDecoder(strings.NewReader(tc.yamlData))
			dec.KnownFields(true)
			err := dec.Decode(&w)

			if tc.wantErrStr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrStr)
				}
				if !strings.Contains(err.Error(), tc.wantErrStr) {
					t.Fatalf("expected error containing %q, got: %v", tc.wantErrStr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.Web.Enabled != tc.wantCfg.Enabled {
				t.Errorf("Enabled = %v, want %v", w.Web.Enabled, tc.wantCfg.Enabled)
			}
			if w.Web.BrowseTimeoutS != tc.wantCfg.BrowseTimeoutS {
				t.Errorf("BrowseTimeoutS = %d, want %d", w.Web.BrowseTimeoutS, tc.wantCfg.BrowseTimeoutS)
			}
			if w.Web.MaxTextBytes != tc.wantCfg.MaxTextBytes {
				t.Errorf("MaxTextBytes = %d, want %d", w.Web.MaxTextBytes, tc.wantCfg.MaxTextBytes)
			}
			if tc.wantCfg.ChromePath != "" && w.Web.ChromePath != tc.wantCfg.ChromePath {
				t.Errorf("ChromePath = %q, want %q", w.Web.ChromePath, tc.wantCfg.ChromePath)
			}
		})
	}
}

func TestConfig_DetectChromeEnv(t *testing.T) {
	t.Setenv("LARARIUM_CHROME", "/custom/test/chrome")
	if p := DetectChrome(); p != "/custom/test/chrome" {
		t.Errorf("DetectChrome() = %q, want /custom/test/chrome", p)
	}
}
