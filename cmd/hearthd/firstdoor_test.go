package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/surface"
)

// mockLocalServer creates a test HTTP server with OpenAI-compatible /v1/models response.
func mockLocalServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "test-model-1"},
					{"id": "test-model-2"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 1. TestFirstDoor_CreatesToken: interactive path ends with store containing the token,
// label = device name, URL printed exactly once (count occurrences of the secret in ALL output buffers).
func TestFirstDoor_CreatesToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)
	srv := mockLocalServer(t)

	deviceName := "office-macbook"
	inputLines := strings.Join([]string{
		"1",        // Local models
		"1",        // Pick server 1
		"1",        // Pick model 1
		"y",        // Same model for compact
		deviceName, // Device name
		"N",        // Remote access: N
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(inputLines),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,
		DiscoveryRoots: []string{srv.URL + "/v1"},
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	// Verify tokens.json exists with mode 0600
	tokensPath := filepath.Join(dir, "tokens.json")
	info, err := os.Stat(tokensPath)
	if err != nil {
		t.Fatalf("tokens.json not found: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected tokens.json mode 0600, got %o", perm)
	}

	// Verify ready URL was printed
	outStr := outBuf.String()
	errStr := errBuf.String()
	allOutput := outStr + "\n" + errStr

	re := regexp.MustCompile(`Open: http://[^/]+/#(lar1_[0-9A-Za-z]+)`)
	matches := re.FindStringSubmatch(outStr)
	if len(matches) < 2 {
		t.Fatalf("expected ready URL with token in output, got:\n%s", outStr)
	}
	tokenSecret := matches[1]

	// Count occurrences of secret token across ALL output buffers (must be exactly 1, spec §1.c, §3)
	occurrences := strings.Count(allOutput, tokenSecret)
	if occurrences != 1 {
		t.Errorf("expected secret token to appear exactly 1 time in all output, appeared %d times", occurrences)
	}

	// Verify warning line is printed
	if !strings.Contains(outStr, "Write it down now — it is shown only once.") {
		t.Errorf("expected warning line in output, got:\n%s", outStr)
	}

	// Verify token secret never written to config
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgBytes), tokenSecret) {
		t.Errorf("secret token leaked into config file %s", cfgPath)
	}

	// Verify token is verifiable in store under the device name label
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	store, err := openTestTokenStore(tokensPath)
	if err != nil {
		t.Fatalf("openTokenStore: %v", err)
	}
	if !store.Verify(tokenSecret) {
		t.Fatalf("store.Verify failed for token %s", tokenSecret)
	}

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
	if entries[0].Label != deviceName {
		t.Errorf("expected token label %q, got %q", deviceName, entries[0].Label)
	}
	_ = cfg
}

// 2. TestFirstDoor_DeclineDeviceName: empty/whitespace-only name re-prompts;
// cancel leaves no token created.
func TestFirstDoor_DeclineDeviceName(t *testing.T) {
	t.Run("whitespace-only re-prompts and cancel leaves no token", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := createSetupTestConfig(t, dir)
		srv := mockLocalServer(t)

		inputLines := strings.Join([]string{
			"1",      // Local models
			"1",      // Server 1
			"1",      // Model 1
			"y",      // Same model for compact
			"   ",    // Whitespace-only name -> must re-prompt
			"cancel", // Cancel setup
		}, "\n") + "\n"

		var outBuf, errBuf bytes.Buffer
		noDefault := ""
		opts := SetupOptions{
			ConfigPath:        cfgPath,
			In:                strings.NewReader(inputLines),
			Out:               &outBuf,
			Err:               &errBuf,
			IsTTY:             true,
			DiscoveryRoots:    []string{srv.URL + "/v1"},
			DefaultDeviceName: &noDefault,
		}

		err := RunSetup(opts)
		if err == nil {
			t.Fatal("expected RunSetup to return error on cancel, got nil")
		}

		errStr := errBuf.String()
		if !strings.Contains(errStr, "device name cannot be empty") {
			t.Errorf("expected empty/whitespace error in stderr: %s", errStr)
		}

		// Assert no token created
		tokensPath := filepath.Join(dir, "tokens.json")
		if data, readErr := os.ReadFile(tokensPath); readErr == nil {
			var entries []map[string]any
			if jsonErr := json.Unmarshal(data, &entries); jsonErr == nil && len(entries) > 0 {
				t.Fatalf("expected 0 tokens created on cancel, got %d", len(entries))
			}
		}
	})

	t.Run("empty name with no default re-prompts and EOF leaves no token", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := createSetupTestConfig(t, dir)
		srv := mockLocalServer(t)

		inputLines := strings.Join([]string{
			"1", // Local models
			"1", // Server 1
			"1", // Model 1
			"y", // Same model for compact
			"",  // Empty input with no default -> re-prompt
			// EOF follows
		}, "\n") + "\n"

		emptyDefault := ""
		var outBuf, errBuf bytes.Buffer
		opts := SetupOptions{
			ConfigPath:        cfgPath,
			In:                strings.NewReader(inputLines),
			Out:               &outBuf,
			Err:               &errBuf,
			IsTTY:             true,
			DiscoveryRoots:    []string{srv.URL + "/v1"},
			DefaultDeviceName: &emptyDefault,
		}

		err := RunSetup(opts)
		if err == nil {
			t.Fatal("expected RunSetup to return error on EOF, got nil")
		}

		errStr := errBuf.String()
		if !strings.Contains(errStr, "device name cannot be empty") {
			t.Errorf("expected empty error in stderr: %s", errStr)
		}

		// Assert no token created
		tokensPath := filepath.Join(dir, "tokens.json")
		if data, readErr := os.ReadFile(tokensPath); readErr == nil {
			var entries []map[string]any
			if jsonErr := json.Unmarshal(data, &entries); jsonErr == nil && len(entries) > 0 {
				t.Fatalf("expected 0 tokens created on cancel, got %d", len(entries))
			}
		}
	})
}

// 3. TestFirstDoor_AllowedHostsWritten: y-path appends validated hosts to allowed_hosts,
// dedupes, preserves unrelated config keys; malformed host (scheme/path/space) rejected with re-prompt.
func TestFirstDoor_AllowedHostsWritten(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)
	srv := mockLocalServer(t)

	inputLines := strings.Join([]string{
		"1",                            // Local models
		"1",                            // Server 1
		"1",                            // Model 1
		"y",                            // Compact same
		"my-laptop",                    // Device name
		"y",                            // Allow access outside: y
		"http://100.121.44.7",          // Malformed: scheme -> re-prompt
		"100.121.44.7/api",             // Malformed: path -> re-prompt
		"100.121.44.7 with space:7717", // Malformed: whitespace -> re-prompt
		"100.121.44.7",                 // Valid host 1
		"100.121.44.7",                 // Duplicate host -> deduped
		"hearth.my-tailnet.ts.net",     // Valid host 2
		"127.0.0.1",                    // Loopback -> skipped from appending
		"",                             // Empty line -> finish hosts
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(inputLines),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,
		DiscoveryRoots: []string{srv.URL + "/v1"},
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	errStr := errBuf.String()
	if !strings.Contains(errStr, "path or slash") {
		t.Errorf("expected path/slash error in stderr: %s", errStr)
	}
	if !strings.Contains(errStr, "whitespace") {
		t.Errorf("expected whitespace error in stderr: %s", errStr)
	}

	// Verify allowed_hosts in rewritten config
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	allowed := cfg.Serve.AllowedHosts
	has100 := false
	hasTailnet := false
	count100 := 0
	for _, h := range allowed {
		if h == "100.121.44.7" {
			has100 = true
			count100++
		}
		if h == "hearth.my-tailnet.ts.net" {
			hasTailnet = true
		}
	}

	if !has100 {
		t.Errorf("expected 100.121.44.7 in allowed_hosts: %+v", allowed)
	}
	if count100 != 1 {
		t.Errorf("expected 100.121.44.7 to appear exactly once (deduped), appeared %d times: %+v", count100, allowed)
	}
	if !hasTailnet {
		t.Errorf("expected hearth.my-tailnet.ts.net in allowed_hosts: %+v", allowed)
	}

	// Verify unrelated config keys and comments survived
	rawBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawBytes)
	if !strings.Contains(raw, "# Reference config header comment") {
		t.Errorf("header comment lost in round-trip")
	}
	if !strings.Contains(raw, "# Serve config comment") {
		t.Errorf("serve comment lost in round-trip")
	}
	if !strings.Contains(raw, "# Trailing comments must survive round-trip") {
		t.Errorf("trailing comment lost in round-trip")
	}
	if cfg.Hearth.CompactionTriggerPct != 80 || cfg.Hearth.MaxTokens != 512 {
		t.Errorf("unrelated hearth keys corrupted: %+v", cfg.Hearth)
	}
}

// 4. TestFirstDoor_NeverWidensBind: compose-file editing is impossible from the wizard
// code path (assert no compose file written next to config) and the guidance line is printed even on N.
func TestFirstDoor_NeverWidensBind(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)
	srv := mockLocalServer(t)

	inputLines := strings.Join([]string{
		"1",            // Local models
		"1",            // Server 1
		"1",            // Model 1
		"y",            // Compact same
		"desktop-door", // Device name
		"N",            // Allow access from outside: N
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(inputLines),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,
		DiscoveryRoots: []string{srv.URL + "/v1"},
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	// Assert NO compose file written anywhere in or next to config directory (spec §2.c)
	for _, composeName := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		p := filepath.Join(dir, composeName)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("compose file %s was created by wizard; wizard must NEVER edit/create compose files", p)
		}
	}

	// Assert guidance line printed even on N
	outStr := outBuf.String()
	expectedSentence := "The wizard never widens the network bind; edit your compose file as shown."
	if !strings.Contains(outStr, expectedSentence) {
		t.Errorf("expected guidance sentence %q in output:\n%s", expectedSentence, outStr)
	}

	expectedPorts := `ports:
  - "<host-ip>:7717:7717"`
	if !strings.Contains(outStr, expectedPorts) {
		t.Errorf("expected port mapping line %q in output:\n%s", expectedPorts, outStr)
	}

	// Assert config listen address was not widened
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Serve.Listen != "127.0.0.1:7717" {
		t.Errorf("serve.listen was modified to %q; wizard must NEVER touch listen bind", cfg.Serve.Listen)
	}
}

// 5. TestFirstDoor_NonTTYUnchanged: existing printNonTTYState assertions still pass.
func TestFirstDoor_NonTTYUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(""),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          false,      // Non-TTY
		DiscoveryRoots: []string{}, // No discovered servers
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed on non-TTY: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "Current configuration state:") {
		t.Errorf("expected config state in output: %s", outStr)
	}
	if !strings.Contains(outStr, "OPENROUTER_API_KEY") {
		t.Errorf("expected env-var instructions in output: %s", outStr)
	}
	if !strings.Contains(outStr, "first-door token creation") {
		t.Errorf("expected mention of first-door token creation step in non-TTY output: %s", outStr)
	}
	if !strings.Contains(outStr, "remote-access guidance") {
		t.Errorf("expected mention of remote-access guidance step in non-TTY output: %s", outStr)
	}

	// Assert no files modified
	tokensPath := filepath.Join(dir, "tokens.json")
	if _, err := os.Stat(tokensPath); !os.IsNotExist(err) {
		t.Errorf("tokens.json should not exist on non-TTY run")
	}
	bakPath := cfgPath + ".bak"
	if _, err := os.Stat(bakPath); !os.IsNotExist(err) {
		t.Errorf(".bak should not exist on non-TTY run")
	}
}

// openTestTokenStore opens a TokenStore for test verification.
func openTestTokenStore(path string) (*surface.TokenStore, error) {
	return surface.OpenTokenStore(path)
}
