package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1. Full interactive wizard on an EMPTY config dir, local-provider path.
func TestSetup_BootstrapMissingConfig_Local(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	srv := mockLocalServer(t)

	// Inputs: choice 1 -> base URL -> model -> compact same -> device name "" -> remote "N"
	inputs := strings.Join([]string{
		"1",             // Choice 1: Local models
		srv.URL + "/v1", // Base URL
		"1",             // Model choice 1 (test-local-model)
		"y",             // Same for compact
		"",              // Device name (default)
		"N",             // Remote access (N)
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	defaultDev := "test-device"
	opts := SetupOptions{
		ConfigPath:        cfgPath,
		In:                strings.NewReader(inputs),
		Out:               &outBuf,
		Err:               &errBuf,
		IsTTY:             true,
		DiscoveryRoots:    []string{}, // empty roots triggers base URL prompt
		DefaultDeviceName: &defaultDev,
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	// Verify config was written
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}

	// Verify LoadConfig passes (home + model chain satisfied)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig on result failed: %v", err)
	}
	if cfg.Hearth.Home != "hearth" {
		t.Errorf("expected hearth.home = \"hearth\", got %q", cfg.Hearth.Home)
	}
	if len(cfg.Models.Default) == 0 || cfg.Models.Default[0] != "local/test-model-1" {
		t.Errorf("unexpected models.default chain: %v", cfg.Models.Default)
	}
	if len(cfg.Models.Compact) == 0 || cfg.Models.Compact[0] != "local/test-model-1" {
		t.Errorf("unexpected models.compact chain: %v", cfg.Models.Compact)
	}

	// Remote step declined = no allowed_hosts write
	expectedAllowedHosts := []string{"localhost", "127.0.0.1", "[::1]"}
	if len(cfg.Serve.AllowedHosts) != len(expectedAllowedHosts) {
		t.Errorf("expected %v allowed_hosts, got %v", expectedAllowedHosts, cfg.Serve.AllowedHosts)
	}

	// Verify token created in token store under seeded hearth.home
	tokensPath := filepath.Join(dir, cfg.Hearth.Home, "tokens.json")
	data, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatalf("reading tokens.json: %v", err)
	}
	var entries []struct {
		Hash    string `json:"hash"`
		Label   string `json:"label"`
		Created string `json:"created"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("unmarshaling tokens.json: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 token entry, got %d", len(entries))
	}
	if entries[0].Label != defaultDev {
		t.Errorf("expected token label %q, got %q", defaultDev, entries[0].Label)
	}
}

// 2. Same wizard, custom provider path (http loopback base URL, empty key), ends with working config.
func TestSetup_BootstrapMissingConfig_Custom(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	srv := mockLocalServer(t)

	// Inputs: choice 36 (Custom) -> name -> base URL -> empty key -> model -> compact same -> device name "" -> remote "N"
	inputs := strings.Join([]string{
		"36",             // Choice 36: Custom endpoint
		"my-custom-prov", // Provider name
		srv.URL + "/v1",  // Base URL (loopback)
		"",               // API key (empty allowed on loopback)
		"1",              // Model choice 1
		"y",              // Same for compact
		"",               // Device name (default)
		"N",              // Remote access (N)
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	defaultDev := "custom-device"
	opts := SetupOptions{
		ConfigPath:        cfgPath,
		In:                strings.NewReader(inputs),
		Out:               &outBuf,
		Err:               &errBuf,
		IsTTY:             true,
		DiscoveryRoots:    []string{},
		DefaultDeviceName: &defaultDev,
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig on result failed: %v", err)
	}
	if cfg.Hearth.Home != "hearth" {
		t.Errorf("expected hearth.home = \"hearth\", got %q", cfg.Hearth.Home)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].Name != "my-custom-prov" {
		t.Errorf("expected provider name \"my-custom-prov\", got %q", cfg.Providers[0].Name)
	}
	if cfg.Providers[0].BaseURL != srv.URL+"/v1" {
		t.Errorf("expected base_url %q, got %q", srv.URL+"/v1", cfg.Providers[0].BaseURL)
	}
	if cfg.Providers[0].APIKeyEnv != "" {
		t.Errorf("expected empty APIKeyEnv on loopback empty key, got %q", cfg.Providers[0].APIKeyEnv)
	}
	if len(cfg.Models.Default) == 0 || cfg.Models.Default[0] != "my-custom-prov/test-model-1" {
		t.Errorf("unexpected models.default chain: %v", cfg.Models.Default)
	}

	// Verify token created
	tokensPath := filepath.Join(dir, cfg.Hearth.Home, "tokens.json")
	data, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatalf("reading tokens.json: %v", err)
	}
	var entries []struct {
		Hash    string `json:"hash"`
		Label   string `json:"label"`
		Created string `json:"created"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("unmarshaling tokens.json: %v", err)
	}
	if len(entries) != 1 || entries[0].Label != defaultDev {
		t.Errorf("unexpected token entries: %v", entries)
	}
}

// 3. Missing config + non-TTY: prints guidance, exit 0, writes NO files.
func TestSetup_BootstrapMissingConfig_NonTTY(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath: cfgPath,
		In:         strings.NewReader(""),
		Out:        &outBuf,
		Err:        &errBuf,
		IsTTY:      false,
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed on missing config + non-TTY: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, cfgPath) {
		t.Errorf("expected config path %q in output:\n%s", cfgPath, outStr)
	}
	guidance := "no config yet — run interactively once: hearthd setup"
	if !strings.Contains(outStr, guidance) {
		t.Errorf("expected guidance %q in output:\n%s", guidance, outStr)
	}

	// Assert NO files written
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 files written in %s, found %d", dir, len(entries))
	}
}

// 4. Corrupt existing config: still hard error (both TTY and non-TTY).
func TestSetup_CorruptConfig_HardError(t *testing.T) {
	corruptYAML := []byte("invalid: yaml: [\nunclosed bracket\n")

	t.Run("TTY returns hard error", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")
		if err := os.WriteFile(cfgPath, corruptYAML, 0o644); err != nil {
			t.Fatal(err)
		}

		var outBuf, errBuf bytes.Buffer
		opts := SetupOptions{
			ConfigPath: cfgPath,
			In:         strings.NewReader(""),
			Out:        &outBuf,
			Err:        &errBuf,
			IsTTY:      true,
		}

		err := RunSetup(opts)
		if err == nil {
			t.Fatal("expected error on corrupt config with TTY, got nil")
		}
		if !strings.Contains(err.Error(), "cannot load existing config") {
			t.Errorf("expected 'cannot load existing config' in error: %v", err)
		}
	})

	t.Run("non-TTY returns hard error", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")
		if err := os.WriteFile(cfgPath, corruptYAML, 0o644); err != nil {
			t.Fatal(err)
		}

		var outBuf, errBuf bytes.Buffer
		opts := SetupOptions{
			ConfigPath: cfgPath,
			In:         strings.NewReader(""),
			Out:        &outBuf,
			Err:        &errBuf,
			IsTTY:      false,
		}

		err := RunSetup(opts)
		if err == nil {
			t.Fatal("expected error on corrupt config with non-TTY, got nil")
		}
		if !strings.Contains(err.Error(), "cannot load existing config") {
			t.Errorf("expected 'cannot load existing config' in error: %v", err)
		}
	})
}

// 5. updateYAML on missing file creates valid YAML; on existing file leaves backup identical to old bytes.
func TestUpdateYAML_MissingAndExisting(t *testing.T) {
	t.Run("missing file creates valid YAML and no backup", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "lararium.yaml")

		err := updateYAML(cfgPath, "testprov", "http://127.0.0.1:8080/v1", "", "model-def", "model-comp")
		if err != nil {
			t.Fatalf("updateYAML failed on missing file: %v", err)
		}

		// Verify backup file was NOT created
		if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
			t.Errorf("backup file should not exist for newly created config")
		}

		// Verify config passes LoadConfig
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig failed on new config: %v", err)
		}
		if cfg.Hearth.Home != "hearth" {
			t.Errorf("expected hearth.home = \"hearth\", got %q", cfg.Hearth.Home)
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "testprov" {
			t.Errorf("unexpected providers: %+v", cfg.Providers)
		}
		if len(cfg.Models.Default) != 1 || cfg.Models.Default[0] != "testprov/model-def" {
			t.Errorf("unexpected models.default: %+v", cfg.Models.Default)
		}
		if len(cfg.Models.Compact) != 1 || cfg.Models.Compact[0] != "testprov/model-comp" {
			t.Errorf("unexpected models.compact: %+v", cfg.Models.Compact)
		}

		// Verify comments from template survive
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		for _, needle := range []string{
			"# Reference config for the Docker form",
			"# Web chat + HTTP API",
			"# Bring your own model",
			"# - name: openrouter",
			"# - name: openai",
			"# - name: anthropic",
		} {
			if !strings.Contains(content, needle) {
				t.Errorf("expected comment needle %q in created config:\n%s", needle, content)
			}
		}
	})

	t.Run("existing file leaves backup identical to old bytes", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := createSetupTestConfig(t, dir)
		oldBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}

		err = updateYAML(cfgPath, "newprov", "http://127.0.0.1:9090/v1", "NEW_KEY", "m1", "m2")
		if err != nil {
			t.Fatalf("updateYAML failed: %v", err)
		}

		bakBytes, err := os.ReadFile(cfgPath + ".bak")
		if err != nil {
			t.Fatalf("reading backup file: %v", err)
		}
		if !bytes.Equal(oldBytes, bakBytes) {
			t.Errorf("backup bytes do not match original config bytes")
		}
	})
}
