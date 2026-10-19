package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/surface"
)

// TestV08ConfigCompatibility verifies that the strict config loader (KnownFields(true))
// accepts the canonical v0.8.4 config without schema errors.
func TestV08ConfigCompatibility(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")

	v08ConfigYAML := `hearth:
  home: /hearth
  compaction_trigger_pct: 80
  max_tokens: 512

serve:
  listen: "0.0.0.0:7717"
  allowed_hosts: ["localhost", "127.0.0.1", "[::1]"]

models:
  default:
    - local/your-model-id
  compact:
    - local/your-model-id

providers:
  - name: local
    base_url: http://host.docker.internal:8080/v1
`
	if err := os.WriteFile(cfgPath, []byte(v08ConfigYAML), 0o600); err != nil {
		t.Fatalf("writing v0.8 config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig rejected v0.8 config: %v", err)
	}

	if cfg.Hearth.Home != "/hearth" {
		t.Errorf("expected hearth.home /hearth, got %s", cfg.Hearth.Home)
	}
	if cfg.Serve.Listen != "0.0.0.0:7717" {
		t.Errorf("expected serve.listen 0.0.0.0:7717, got %s", cfg.Serve.Listen)
	}
	if len(cfg.Models.Default) != 1 || cfg.Models.Default[0] != "local/your-model-id" {
		t.Errorf("unexpected models.default: %+v", cfg.Models.Default)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "local" {
		t.Errorf("unexpected providers: %+v", cfg.Providers)
	}
}

// TestV08TokenAndAuditUpgrade verifies that door tokens and session event logs
// written by v0.8 persist cleanly and validate under current main.
func TestV08TokenAndAuditUpgrade(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "lararium.yaml")

	// 1. Write tokens.json with a v0.8 door token
	const rawToken = "lar1_TestUpgradeToken0123456789ABC"
	hasher := sha256.Sum256([]byte(rawToken))
	hashHex := hex.EncodeToString(hasher[:])

	tokens := []map[string]string{
		{
			"hash":    hashHex,
			"label":   "v08-operator",
			"created": "2026-10-16T00:00:00Z",
		},
	}
	tokensBytes, err := json.Marshal(tokens)
	if err != nil {
		t.Fatalf("marshal tokens: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "tokens.json"), tokensBytes, 0o600); err != nil {
		t.Fatalf("write tokens.json: %v", err)
	}

	// Verify token store loads and validates the v0.8 token
	store, err := surface.OpenTokenStore(filepath.Join(home, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	if !store.Verify(rawToken) {
		t.Fatalf("v0.8 door token failed verification in upgraded store")
	}
	if store.Verify("lar1_WrongToken00000000000000000000") {
		t.Fatalf("invalid token incorrectly validated")
	}

	// 2. Write sessions/main/events.jsonl with a v0.8 chat turn
	sessionDir := filepath.Join(home, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir sessionDir: %v", err)
	}
	v08Event := `{"role":"user","seq":1,"src":{"channel":"api","device":""},"t":"msg","text":"hello v0.8","ts":"2026-10-16T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(v08Event), 0o600); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}

	// Verify penatus event log continuity
	log, err := penatus.OpenLog(sessionDir)
	if err != nil {
		t.Fatalf("penatus.OpenLog: %v", err)
	}
	if len(log.Live()) != 1 {
		t.Fatalf("expected 1 event, got %d", len(log.Live()))
	}
	if log.Live()[0].Seq != 1 {
		t.Fatalf("expected event seq 1, got %d", log.Live()[0].Seq)
	}

	// 3. Write minimal config and persona files to verify backup verification
	if err := os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("v0.8 soul"), 0o600); err != nil {
		t.Fatalf("write SOUL.md: %v", err)
	}
	cfgContent := `hearth:
  home: ` + home + `
serve:
  listen: "127.0.0.1:7717"
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	bundlePath := filepath.Join(t.TempDir(), "test-bundle.zip")
	if err := backup.CreateOffline(home, cfgPath, bundlePath, false, nil); err != nil {
		t.Fatalf("CreateOffline failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(bundlePath, &stdout, &stderr)
	if err != nil {
		t.Fatalf("backup.Verify failed: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("backup.Verify reported findings on upgraded v0.8 state: %+v", findings)
	}
}
