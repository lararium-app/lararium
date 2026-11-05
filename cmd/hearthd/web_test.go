package main

import (
	"os"
	"path/filepath"
	"testing"

	internalweb "github.com/lararium-app/lararium/internal/web"
)

func TestWebToolRegistration(t *testing.T) {
	home := t.TempDir()

	t.Run("default enables web_fetch and web_browse", func(t *testing.T) {
		tools := buildTools(home)
		var hasFetch, hasBrowse bool
		for _, tool := range tools {
			if tool.Spec.Name == "web_fetch" {
				hasFetch = true
				if !tool.Trusted {
					t.Errorf("web_fetch should be trusted")
				}
			}
			if tool.Spec.Name == "web_browse" {
				hasBrowse = true
				if !tool.Trusted {
					t.Errorf("web_browse should be trusted")
				}
			}
		}
		if !hasFetch {
			t.Errorf("expected web_fetch to be registered by default")
		}
		if !hasBrowse {
			t.Errorf("expected web_browse to be registered by default")
		}
	})

	t.Run("explicitly disabled omits both tools", func(t *testing.T) {
		disabledCfg := internalweb.Config{
			Enabled:        false,
			BrowseTimeoutS: 30,
			MaxTextBytes:   200000,
		}
		tools := buildTools(home, disabledCfg)
		for _, tool := range tools {
			if tool.Spec.Name == "web_fetch" || tool.Spec.Name == "web_browse" {
				t.Fatalf("expected tool %q to be omitted when web.enabled=false", tool.Spec.Name)
			}
		}
	})

	t.Run("explicitly enabled includes both tools", func(t *testing.T) {
		enabledCfg := internalweb.Config{
			Enabled:        true,
			BrowseTimeoutS: 30,
			MaxTextBytes:   200000,
		}
		tools := buildTools(home, enabledCfg)
		var hasFetch, hasBrowse bool
		for _, tool := range tools {
			if tool.Spec.Name == "web_fetch" {
				hasFetch = true
			}
			if tool.Spec.Name == "web_browse" {
				hasBrowse = true
			}
		}
		if !hasFetch || !hasBrowse {
			t.Fatalf("expected both web tools to be registered, got fetch=%v browse=%v", hasFetch, hasBrowse)
		}
	})
}

func TestLoadConfig_WebBlock(t *testing.T) {
	t.Run("default config has web enabled", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")
		yamlContent := `
hearth:
  home: ` + dir + `
models:
  default:
    - local/model
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
`
		if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}
		if !cfg.Web.Enabled {
			t.Errorf("expected web.enabled=true by default")
		}
		if cfg.Web.BrowseTimeoutS != 30 {
			t.Errorf("expected browse_timeout_s=30, got %d", cfg.Web.BrowseTimeoutS)
		}
		if cfg.Web.MaxTextBytes != 200000 {
			t.Errorf("expected max_text_bytes=200000, got %d", cfg.Web.MaxTextBytes)
		}
	})

	t.Run("custom web block", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")
		yamlContent := `
hearth:
  home: ` + dir + `
models:
  default:
    - local/model
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
web:
  enabled: false
  browse_timeout_s: 45
  max_text_bytes: 100000
  chrome_path: /usr/bin/custom-chrome
`
		if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}
		if cfg.Web.Enabled {
			t.Errorf("expected web.enabled=false")
		}
		if cfg.Web.BrowseTimeoutS != 45 {
			t.Errorf("expected browse_timeout_s=45, got %d", cfg.Web.BrowseTimeoutS)
		}
		if cfg.Web.MaxTextBytes != 100000 {
			t.Errorf("expected max_text_bytes=100000, got %d", cfg.Web.MaxTextBytes)
		}
		if cfg.Web.ChromePath != "/usr/bin/custom-chrome" {
			t.Errorf("expected chrome_path=/usr/bin/custom-chrome, got %q", cfg.Web.ChromePath)
		}
	})

	t.Run("invalid web block fails startup", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")
		yamlContent := `
hearth:
  home: ` + dir + `
models:
  default:
    - local/model
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
web:
  browse_timeout_s: 0
`
		if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := LoadConfig(cfgPath)
		if err == nil {
			t.Fatal("expected LoadConfig to fail on browse_timeout_s=0, got nil")
		}
	})
}
