package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/keystore"
)

// helper to create a valid minimal lararium.yaml fixture in dir.
func createSetupTestConfig(t *testing.T, dir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "lararium.yaml")
	content := `# Reference config header comment
hearth:
  home: ` + dir + `
  compaction_trigger_pct: 80
  max_tokens: 512

# Serve config comment
serve:
  listen: "127.0.0.1:7717"
  allowed_hosts: ["localhost", "127.0.0.1"]

# Models section comment
models:
  default:
    - local/initial-model
  compact:
    - local/initial-model

# Providers section comment
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1 # local model comment

# Trailing comments must survive round-trip
# - name: openrouter
#   base_url: https://openrouter.ai/api/v1
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// 1. httptest servers for discovery (one good /models, one malformed, one slow >1.5s).
func TestDiscovery_HTTPTest(t *testing.T) {
	// Good server
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "good-model-1"},
					{"id": "good-model-2"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer goodSrv.Close()

	// Malformed server
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{invalid-json"))
	}))
	defer badSrv.Close()

	// Slow server (>1.5s timeout)
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1800 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "slow-model"}},
		})
	}))
	defer slowSrv.Close()

	roots := []string{
		goodSrv.URL + "/v1",
		badSrv.URL + "/v1",
		slowSrv.URL + "/v1",
	}

	discovered := DiscoverLocalServers(roots, 1500*time.Millisecond)
	if len(discovered) != 1 {
		t.Fatalf("expected 1 discovered server, got %d: %+v", len(discovered), discovered)
	}
	if discovered[0].BaseURL != goodSrv.URL+"/v1" {
		t.Errorf("expected %s, got %s", goodSrv.URL+"/v1", discovered[0].BaseURL)
	}
	if len(discovered[0].Models) != 2 || discovered[0].Models[0] != "good-model-1" {
		t.Errorf("unexpected models: %+v", discovered[0].Models)
	}
}

// 2. happy path hosted preset (key stored via Store — assert file contents mode 0600 + name, and yaml has NO key literal).
func TestHappyPathHostedPreset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	mockHosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer sk-or-secret-key-12345" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "anthropic/claude-3.5-sonnet"},
					{"id": "openai/gpt-4o"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockHosted.Close()

	// OpenRouter is row 4 in CanonicalMenuItems.
	inputLines := strings.Join([]string{
		"4",                      // Choice: OpenRouter
		"sk-or-secret-key-12345", // API Key
		"1",                      // Model: first (anthropic/claude-3.5-sonnet)
		"y",                      // Same model for compact
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(inputLines),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,       // interactive
		DiscoveryRoots: []string{}, // zero discovered
		BaseOverrides: map[string]string{
			"openrouter": mockHosted.URL + "/v1",
		},
		Getenv: func(k string) string {
			return "" // no env shadow
		},
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), outBuf.String())
	}

	// Assert key stored via Store: keys.json exists, mode 0600
	keysPath := filepath.Join(dir, "keys.json")
	info, err := os.Stat(keysPath)
	if err != nil {
		t.Fatalf("keys.json not found: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected keys.json mode 0600, got %o", info.Mode().Perm())
	}

	store := keystore.New(dir)
	m, err := store.Read()
	if err != nil {
		t.Fatalf("reading keystore: %v", err)
	}
	if m["openrouter"] != "sk-or-secret-key-12345" {
		t.Errorf("expected key stored in keystore, got %q", m["openrouter"])
	}

	// Assert lararium.yaml has NO key literal
	rawYAML, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rawStr := string(rawYAML)
	if strings.Contains(rawStr, "sk-or-secret-key-12345") {
		t.Errorf("lararium.yaml leaked key literal:\n%s", rawStr)
	}
	if strings.Contains(rawStr, "api_key:") {
		t.Errorf("lararium.yaml contains api_key literal field:\n%s", rawStr)
	}
	if !strings.Contains(rawStr, "api_key_env: OPENROUTER_API_KEY") {
		t.Errorf("lararium.yaml missing api_key_env:\n%s", rawStr)
	}

	// Assert LoadConfig passes
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed after rewrite: %v", err)
	}
	if len(cfg.Models.Default) != 1 || cfg.Models.Default[0] != "openrouter/anthropic/claude-3.5-sonnet" {
		t.Errorf("unexpected default model chain: %v", cfg.Models.Default)
	}

	// Assert stdout mentions done line
	outStr := outBuf.String()
	if !strings.Contains(outStr, "stored in keys.json (0600) under name openrouter") {
		t.Errorf("output missing done line: %s", outStr)
	}
	if !strings.Contains(outStr, "daemon not restarted — restart hearthd to apply") {
		t.Errorf("output missing daemon reminder: %s", outStr)
	}
}

// 3. local auto-pick.
func TestLocalAutoPick(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "llama-3-8b"},
				{"id": "mistral-7b"},
			},
		})
	}))
	defer localSrv.Close()

	// Choice 1: Local models
	// Choice 1: first discovered server
	// Model choice 2: mistral-7b
	// Same for compact: y
	input := strings.Join([]string{
		"1", // Local models
		"1", // local server choice
		"2", // model: mistral-7b
		"y", // same model for compact
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(input),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,
		DiscoveryRoots: []string{localSrv.URL + "/v1"},
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v", err)
	}

	// Assert config updated with local provider and chosen model
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "local" {
		t.Fatalf("unexpected providers: %+v", cfg.Providers)
	}
	if cfg.Providers[0].BaseURL != localSrv.URL+"/v1" {
		t.Errorf("unexpected base_url: %s", cfg.Providers[0].BaseURL)
	}
	if len(cfg.Models.Default) != 1 || cfg.Models.Default[0] != "local/mistral-7b" {
		t.Errorf("unexpected models.default: %v", cfg.Models.Default)
	}

	// Assert keys.json was NOT created
	if _, err := os.Stat(filepath.Join(dir, "keys.json")); !os.IsNotExist(err) {
		t.Errorf("keys.json should not exist for keyless local provider")
	}

	if !strings.Contains(outBuf.String(), "no key required for local provider") {
		t.Errorf("expected 'no key required for local provider' in output: %s", outBuf.String())
	}
}

// 4. custom provider validation rejects (schemeless URL, bad name).
func TestCustomProviderValidationRejects(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	// Test inputs:
	// 1st choice: Custom endpoint (36)
	// 1st attempt: bad name "Invalid Name!" -> rejected
	// 2nd attempt: bad name "__proto__" -> rejected
	// 3rd attempt: valid name "good-provider"
	// 1st attempt: schemeless URL "localhost:8080/v1" -> rejected
	// 2nd attempt: valid URL "http://127.0.0.1:8080/v1"
	// Free-text model ID: "my-model"
	// Compact same: y
	input := strings.Join([]string{
		"36",                       // Custom endpoint
		"Invalid Name!",            // Bad name
		"__proto__",                // Bad name
		"good-provider",            // Good name
		"localhost:8080/v1",        // Schemeless URL
		"http://127.0.0.1:8080/v1", // Valid URL
		"",                         // API key (empty allowed for loopback)
		"my-model",                 // Model ID
		"y",                        // Compact model
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(input),
		Out:            &outBuf,
		Err:            &errBuf,
		IsTTY:          true,
		DiscoveryRoots: []string{}, // no discovered
	}

	if err := RunSetup(opts); err != nil {
		t.Fatalf("RunSetup failed: %v", err)
	}

	errStr := errBuf.String()
	if !strings.Contains(errStr, "invalid provider name") {
		t.Errorf("expected 'invalid provider name' in stderr: %s", errStr)
	}
	if !strings.Contains(errStr, "invalid base URL") {
		t.Errorf("expected 'invalid base URL' in stderr: %s", errStr)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Providers[0].Name != "good-provider" {
		t.Errorf("expected provider 'good-provider', got %s", cfg.Providers[0].Name)
	}
}

// 5. CANONICAL LIST test: menu rendering asserts exact rows/order
func TestCanonicalList(t *testing.T) {
	items := CanonicalMenuItems()
	if len(items) != 37 {
		t.Fatalf("expected 37 menu items, got %d", len(items))
	}
	expectedRows := []string{
		"Local models (run open models on this machine — no account or API key)",
		"Nous Portal (Everything your agent needs, 300+ models with bundled tool use)",
		"Fireworks AI (OpenAI-compatible direct model API)",
		"OpenRouter (Pay-per-use API aggregator)",
		"Mixture of Agents (named presets; aggregator acts after reference models)",
		"NovitaAI (Cloud: Model API, Agent Sandbox, GPU Cloud)",
		"LM Studio (Local desktop app with built-in model server)",
		"Anthropic (Claude models via API key)",
		"OpenAI (direct OpenAI API)",
		"Qwen (Qwen Cloud / DashScope API key)",
		"xAI Grok (direct API)",
		"Xiaomi MiMo (MiMo-V2.5 and V2 models: pro, omni, flash)",
		"Tencent Hunyuan (TokenHub)",
		"NVIDIA NIM (Nemotron models via build.nvidia.com or local NIM)",
		"GitHub Copilot",
		"Hugging Face Inference Providers",
		"Google AI Studio (Gemini API)",
		"Google Vertex AI (Gemini via GCP; OAuth2/ADC)",
		"DeepSeek (V3, R1, coder, direct API)",
		"Z.AI / GLM (Zhipu direct API)",
		"Kimi / Moonshot (Moonshot global endpoint)",
		"StepFun Step Plan (Agent / coding models via Step Plan API)",
		"MiniMax (Global endpoint)",
		"Ollama Cloud (Cloud-hosted open models, ollama.com)",
		"Arcee AI (Trinity models, direct API)",
		"GMI Cloud (Multi-model direct API)",
		"Kilo Code (Kilo Gateway API)",
		"OpenCode Zen (pay-as-you-go)",
		"AWS Bedrock (IAM/CLI auth)",
		"Azure Foundry (OpenAI-style endpoint, your Azure AI deployment)",
		"Vercel AI Gateway (Multi-model aggregator)",
		"Actual Computer (hosted inference via api.actual.inc)",
		"DeepInfra (100+ open models, pay-per-use)",
		"Upstage (Solar API)",
		"Nebius Token Factory (OpenAI-compatible inference)",
		"Custom endpoint (enter URL manually)",
		"Keep current config and exit",
	}
	for i, exp := range expectedRows {
		if items[i].Title != exp {
			t.Errorf("item %d title mismatch: expected %q, got %q", i+1, exp, items[i].Title)
		}
	}

	var buf bytes.Buffer
	RenderMenu(&buf, items)
	rendered := buf.String()
	for i, exp := range expectedRows {
		expectedLine := fmt.Sprintf("%d) %s", i+1, exp)
		if !strings.Contains(rendered, expectedLine) {
			t.Errorf("rendered menu missing expected line %q:\n%s", expectedLine, rendered)
		}
	}

	// Test placeholders re-show menu and touch no files
	placeholders := []string{"2", "5", "15", "18", "29"}
	for _, pChoice := range placeholders {
		dir := t.TempDir()
		cfgPath := createSetupTestConfig(t, dir)
		origBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}

		// User picks placeholder, gets rejection message, then picks 37 (Keep current config and exit)
		input := strings.Join([]string{
			pChoice, // placeholder
			"37",    // Keep current config and exit
		}, "\n") + "\n"

		var outBuf, errBuf bytes.Buffer
		opts := SetupOptions{
			ConfigPath: cfgPath,
			In:         strings.NewReader(input),
			Out:        &outBuf,
			Err:        &errBuf,
			IsTTY:      true,
		}

		if err := RunSetup(opts); err != nil {
			t.Fatalf("RunSetup failed for placeholder %s: %v", pChoice, err)
		}
		outStr := outBuf.String()
		if !strings.Contains(outStr, "not yet available — pick OpenRouter, Custom endpoint, or see docs") {
			t.Errorf("expected placeholder message for choice %s in output: %s", pChoice, outStr)
		}
		if !strings.Contains(outStr, "Keeping current config. Exiting.") {
			t.Errorf("expected exit message in output: %s", outStr)
		}

		// Assert files untouched
		newBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(origBytes, newBytes) {
			t.Errorf("config file was modified for placeholder %s", pChoice)
		}
		if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
			t.Errorf(".bak was created for placeholder %s", pChoice)
		}
	}
}

// 6. yaml round-trip preserves a hand-commented fixture file (comments + order + unrelated keys).
func TestYAMLRoundTrip_PreservesCommentsAndUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")
	fixture := `# Top-level file comment
hearth:
  # Hearth home comment
  home: ` + dir + `
  compaction_trigger_pct: 75
  max_tokens: 1024

# Unrelated top-level section: custos
custos:
  door_token: test-token
  doors_sock: /tmp/custos.sock

# Serve section
serve:
  listen: "0.0.0.0:7717"
  allowed_hosts: ["localhost"]

# Models section with comments
models:
  default:
    - oldprov/oldmodel
  compact:
    - oldprov/oldmodel

# Providers section with comments
providers:
  - name: oldprov
    base_url: http://127.0.0.1:9999/v1 # inline provider comment

# Footer comments at bottom of file
# - example footer line 1
# - example footer line 2
`
	if err := os.WriteFile(cfgPath, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := updateYAML(cfgPath, "openrouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", "anthropic/claude-3.5-sonnet", "anthropic/claude-3.5-sonnet"); err != nil {
		t.Fatalf("updateYAML failed: %v", err)
	}

	newBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	newStr := string(newBytes)

	// Check that comments survived
	requiredStrings := []string{
		"# Top-level file comment",
		"# Hearth home comment",
		"# Unrelated top-level section: custos",
		"door_token: test-token",
		"doors_sock: /tmp/custos.sock",
		"# Serve section",
		"# Models section with comments",
		"# Providers section with comments",
		"# Footer comments at bottom of file",
		"# - example footer line 1",
		"# - example footer line 2",
		"openrouter/anthropic/claude-3.5-sonnet",
		"name: openrouter",
		"base_url: https://openrouter.ai/api/v1",
		"api_key_env: OPENROUTER_API_KEY",
	}
	for _, req := range requiredStrings {
		if !strings.Contains(newStr, req) {
			t.Errorf("modified YAML missing required content %q:\n%s", req, newStr)
		}
	}

	// Validate config still loads
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("LoadConfig failed on updated YAML: %v", err)
	}
}

// 7. non-TTY no-op.
func TestNonTTYNoOp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	origBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

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

	// Assert output contains current config and env-var instructions
	outStr := outBuf.String()
	if !strings.Contains(outStr, "Current configuration state:") {
		t.Errorf("expected config state in output: %s", outStr)
	}
	if !strings.Contains(outStr, "OPENROUTER_API_KEY") {
		t.Errorf("expected env-var instructions in output: %s", outStr)
	}

	// Assert no files modified
	newBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, newBytes) {
		t.Errorf("config file was modified during non-TTY no-op")
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup file .bak should NOT be created during non-TTY no-op")
	}
}

// 8. re-run idempotence (bak kept from first run).
func TestReRunIdempotence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	originalBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m1"}, {"id": "m2"}}})
	}))
	defer localSrv.Close()

	// Run 1: pick local, server 1, model m1, compact y
	input1 := "1\n1\n1\ny\n"
	var out1, err1 bytes.Buffer
	opts1 := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(input1),
		Out:            &out1,
		Err:            &err1,
		IsTTY:          true,
		DiscoveryRoots: []string{localSrv.URL + "/v1"},
	}
	if err := RunSetup(opts1); err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}

	// Check .bak exists and matches original
	bakBytes, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("reading .bak: %v", err)
	}
	if !bytes.Equal(originalBytes, bakBytes) {
		t.Errorf(".bak does not match original bytes after run 1")
	}

	// Run 2: pick local, server 1, model m2, compact y
	input2 := "1\n1\n2\ny\n"
	var out2, err2 bytes.Buffer
	opts2 := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader(input2),
		Out:            &out2,
		Err:            &err2,
		IsTTY:          true,
		DiscoveryRoots: []string{localSrv.URL + "/v1"},
	}
	if err := RunSetup(opts2); err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}

	// Check config updated to m2
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Models.Default[0] != "local/m2" {
		t.Errorf("expected model m2 after run 2, got %s", cfg.Models.Default[0])
	}

	// Assert .bak STILL matches the original first-run bytes
	bakBytesAfterRun2, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("reading .bak after run 2: %v", err)
	}
	if !bytes.Equal(originalBytes, bakBytesAfterRun2) {
		t.Errorf(".bak was overwritten during run 2!")
	}
}

type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitForOutput(t *testing.T, sw *syncWriter, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(sw.String(), target) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output: %s", target, sw.String())
}

// 9. TestNoAutoPoll: pre-planted httptest server on discovery port;
// assert menu renders WITHOUT any HTTP hit to it until user enters "1", THEN the scan sees it.
func TestNoAutoPoll(t *testing.T) {
	dir := t.TempDir()
	cfgPath := createSetupTestConfig(t, dir)

	var hitCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "auto-model-1"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Case 1: Exiting without picking option 1 never polls
	var exitOut, exitErr bytes.Buffer
	exitOpts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             strings.NewReader("37\n"),
		Out:            &exitOut,
		Err:            &exitErr,
		IsTTY:          true,
		DiscoveryRoots: []string{srv.URL + "/v1"},
	}
	if err := RunSetup(exitOpts); err != nil {
		t.Fatalf("exit run failed: %v", err)
	}
	if hitCount.Load() != 0 {
		t.Fatalf("server was hit during exit run: %d hits", hitCount.Load())
	}

	// Case 2: Menu renders WITHOUT any HTTP hit until user enters 1, THEN scan sees it.
	pr, pw := io.Pipe()
	sw := &syncWriter{}
	doneCh := make(chan error, 1)
	opts := SetupOptions{
		ConfigPath:     cfgPath,
		In:             pr,
		Out:            sw,
		Err:            io.Discard,
		IsTTY:          true,
		DiscoveryRoots: []string{srv.URL + "/v1"},
	}
	go func() {
		doneCh <- RunSetup(opts)
	}()

	waitForOutput(t, sw, "Select a provider:", 3*time.Second)
	if hitCount.Load() != 0 {
		t.Fatalf("pre-menu discovery ran! hitCount = %d", hitCount.Load())
	}

	// Enter "1" (Local models)
	_, _ = pw.Write([]byte("1\n"))

	// Wait until scan runs and shows found server
	waitForOutput(t, sw, "Discovered local servers:", 3*time.Second)
	if hitCount.Load() == 0 {
		t.Fatalf("scan did not run after picking 1")
	}

	// Pick server 1, model 1, compact same (y)
	_, _ = pw.Write([]byte("1\n1\ny\n"))

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("RunSetup failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSetup timed out")
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(cfg.Models.Default) != 1 || cfg.Models.Default[0] != "local/auto-model-1" {
		t.Errorf("expected local/auto-model-1, got %v", cfg.Models.Default)
	}
}

// 10. docker shim test if cheap.
func TestDockerShim(t *testing.T) {
	tmpDir := t.TempDir()
	setupDoneFile := filepath.Join(tmpDir, ".setup-done")
	recordLog := filepath.Join(tmpDir, "calls.log")

	// Create fake hearthd in tmpDir
	fakeHearthd := filepath.Join(tmpDir, "hearthd")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
exit 0
`, recordLog)
	if err := os.WriteFile(fakeHearthd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Case 1: .setup-done does NOT exist, and [ -t 0 ] simulated
	// We run the shim script through sh -c
	shimCmd := fmt.Sprintf(`if [ ! -f %s ]; then %s setup || true; touch %s; fi; exec %s serve`,
		setupDoneFile, fakeHearthd, setupDoneFile, fakeHearthd)

	cmd := exec.Command("sh", "-c", shimCmd)
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim execution failed: %v, out: %s", err, string(out))
	}

	// Verify .setup-done was created
	if _, err := os.Stat(setupDoneFile); err != nil {
		t.Errorf(".setup-done was not created: %v", err)
	}

	logBytes, err := os.ReadFile(recordLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) != 2 || lines[0] != "setup" || lines[1] != "serve" {
		t.Errorf("unexpected call log for first run: %+v", lines)
	}

	// Case 2: .setup-done ALREADY exists -> only calls serve
	_ = os.Remove(recordLog)
	cmd2 := exec.Command("sh", "-c", shimCmd)
	cmd2.Dir = tmpDir
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second shim execution failed: %v, out: %s", err, string(out2))
	}

	logBytes2, err := os.ReadFile(recordLog)
	if err != nil {
		t.Fatal(err)
	}
	lines2 := strings.Split(strings.TrimSpace(string(logBytes2)), "\n")
	if len(lines2) != 1 || lines2[0] != "serve" {
		t.Errorf("unexpected call log for second run (setup-done exists): %+v", lines2)
	}
}
