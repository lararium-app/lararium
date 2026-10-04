package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

type fakeProvider struct {
	name    string
	streams []string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "fake"}, nil
}

func (f *fakeProvider) Complete(_ context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	full := strings.Join(f.streams, "")
	return &router.Completion{Text: full, Model: "fake"}, nil
}

func (f *fakeProvider) StreamComplete(_ context.Context, _ []router.Message, _ router.Options, _ string, onDelta router.StreamHandler) (*router.Completion, error) {
	for _, chunk := range f.streams {
		if err := onDelta(chunk); err != nil {
			return nil, err
		}
	}
	full := strings.Join(f.streams, "")
	return &router.Completion{Text: full, Model: "fake"}, nil
}

func setupTestHub(t *testing.T, sp router.Provider) (*surface.Hub, *surface.ApprovalHub, string) {
	t.Helper()
	dir := t.TempDir()

	if _, err := penatus.CreateSession(dir, "main", "main"); err != nil {
		t.Fatal(err)
	}

	mainDir := filepath.Join(dir, "sessions", "main")
	log, err := penatus.OpenLog(mainDir)
	if err != nil {
		t.Fatal(err)
	}

	_ = log.Append(penatus.Event{
		T:  "msg",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"role":  json.RawMessage(`"assistant"`),
			"text":  json.RawMessage(`"system prompt"`),
			"model": json.RawMessage(`"test"`),
			"usage": json.RawMessage(`{"in":10,"out":5}`),
		},
	})

	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})

	cfg := surface.ServeConfig{
		ApprovalTimeout: 5 * time.Second,
		TurnTimeout:     10 * time.Second,
	}

	ap := surface.NewApprovalHub(cfg.ApprovalTimeout)
	hub := surface.NewHub(cfg, dir, 1000, 80, nil, rt, ap)
	return hub, ap, dir
}

func TestNuntiusWireMaps(t *testing.T) {
	fp := &fakeProvider{
		name:    "fake",
		streams: []string{"hello", " ", "world", "!"},
	}
	hub, ap, home := setupTestHub(t, fp)
	source := surface.NewPenatusSource(home, hub)

	// Test sessionsAdapter
	sa := newSessionsAdapter(source)
	createdRef, err := sa.Create("Test Title", "pin-model")
	if err != nil {
		t.Fatalf("sessionsAdapter.Create: %v", err)
	}
	if createdRef.ID == "" || createdRef.Title != "Test Title" {
		t.Fatalf("unexpected createdRef: %+v", createdRef)
	}
	list, err := sa.List()
	if err != nil {
		t.Fatalf("sessionsAdapter.List: %v", err)
	}
	if len(list) < 2 { // main + newly created side session
		t.Fatalf("sessionsAdapter.List got %d, want >= 2", len(list))
	}

	// Test nuntiusWire
	wire := nuntiusWire(hub, ap, "prov/default-model")

	// 1. ResolveApproval routes ResolveFrom with source "telegram"
	id, ch := ap.Register("main", "test_tool", "summary", func(string) {})
	status := wire.ResolveApproval("main", id, true)
	if status != 200 {
		t.Fatalf("ResolveApproval status = %d, want 200", status)
	}
	select {
	case decision := <-ch:
		if !decision {
			t.Fatal("decision should be true")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decision")
	}
	if src := ap.Source("main", id); src != "telegram" {
		t.Fatalf("ap.Source = %q, want telegram", src)
	}
	if reason := ap.Reason("main", id); reason != "ok" {
		t.Fatalf("ap.Reason = %q, want ok", reason)
	}

	// 2. SessionOf round-trips
	sess, ok := wire.SessionOf(id)
	if !ok || sess != "main" {
		t.Fatalf("wire.SessionOf(%q) = (%q, %v), want (main, true)", id, sess, ok)
	}
	_, ok = wire.SessionOf("a_unknown_id")
	if ok {
		t.Fatal("wire.SessionOf unknown id should be false")
	}

	// 3. SetFanout/timeout setters don't panic on the real hub
	var pendingFired, terminalFired bool
	wire.SetFanout(
		func(id, sessionID, name, argsSummary string) { pendingFired = true },
		func(id, sessionID, state, reason, source string) { terminalFired = true },
	)
	id2, _ := ap.Register("main", "test2", "summary2", func(string) {})
	if !pendingFired {
		t.Fatal("SetFanout pending callback should have fired")
	}
	wire.ResolveApproval("main", id2, false)
	if !terminalFired {
		t.Fatal("SetFanout terminal callback should have fired")
	}

	wire.SetBridgeTimeout(10 * time.Second)
	wire.SetBridgeLive(func() bool { return true })
	wire.MarkUndeliverable("nonexistent")
	_ = wire.PendingApprovals("main")
	_ = wire.InFlight("main")
	_ = wire.CancelTurn("main")

	if mref := wire.ModelRef(); mref != "prov/default-model" {
		t.Fatalf("ModelRef = %q, want prov/default-model", mref)
	}

	// 4. engineAdapter's delta accumulation yields full-text-so-far
	ea := newEngineAdapter(hub)
	var deltas []string
	req := nuntius.TurnReq{
		SessionID: "main",
		Text:      "user query",
		OnDelta: func(textSoFar string) {
			deltas = append(deltas, textSoFar)
		},
	}
	res := ea.RunTurn(context.Background(), req)
	if res.Err != nil {
		t.Fatalf("engineAdapter.RunTurn failed: %v", res.Err)
	}
	if res.Text != "hello world!" {
		t.Fatalf("res.Text = %q, want %q", res.Text, "hello world!")
	}
	expectedDeltas := []string{
		"hello",
		"hello ",
		"hello world",
		"hello world!",
	}
	if !reflect.DeepEqual(deltas, expectedDeltas) {
		t.Fatalf("deltas = %v, want %v", deltas, expectedDeltas)
	}
}

func TestNuntiusConfig(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. nuntius: {enabled: true, bot_token_env: X} parses through strict loader
	validYAML := fmt.Sprintf(`
hearth:
  home: %s
models:
  default: [p/m]
providers:
  - name: p
    base_url: http://127.0.0.1:9/v1
nuntius:
  enabled: true
  bot_token_env: TEST_ENV_VAR
`, home)
	validPath := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(validPath, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(validPath)
	if err != nil {
		t.Fatalf("LoadConfig valid failed: %v", err)
	}
	if !cfg.Nuntius.Enabled {
		t.Fatal("cfg.Nuntius.Enabled should be true")
	}
	if cfg.Nuntius.BotTokenEnv != "TEST_ENV_VAR" {
		t.Fatalf("cfg.Nuntius.BotTokenEnv = %q, want TEST_ENV_VAR", cfg.Nuntius.BotTokenEnv)
	}

	// 2. Unknown nuntius key -> error
	unknownYAML := fmt.Sprintf(`
hearth:
  home: %s
models:
  default: [p/m]
providers:
  - name: p
    base_url: http://127.0.0.1:9/v1
nuntius:
  enabled: true
  unknown_key: true
`, home)
	unknownPath := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(unknownPath, []byte(unknownYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(unknownPath); err == nil {
		t.Fatal("LoadConfig with unknown nuntius key must fail")
	}

	// 3. Enabled with missing env -> serve continues, no bridge
	cfg.Nuntius.BotTokenEnv = "NONEXISTENT_TEST_ENV_VAR_HEARTHD_99"
	_ = os.Unsetenv("NONEXISTENT_TEST_ENV_VAR_HEARTHD_99")

	hub, ap, _ := setupTestHub(t, &fakeProvider{name: "fake"})
	source := surface.NewPenatusSource(home, hub)

	bridge, stop, err := startNuntius(context.Background(), cfg, home, hub, source, ap)
	if err != nil {
		t.Fatalf("startNuntius with missing env returned error: %v", err)
	}
	if bridge != nil || stop != nil {
		t.Fatal("bridge and stop must be nil when token is missing")
	}
}

func TestScrubWriter(t *testing.T) {
	var buf strings.Builder
	secret := "123456789:ABCDEF_secret_token"
	w := newScrubWriter(&buf, secret)

	_, err := fmt.Fprintf(w, "https://api.telegram.org/bot%s/getUpdates", secret)
	if err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("token was not scrubbed: %q", out)
	}
	if !strings.Contains(out, "«token»") {
		t.Fatalf("expected «token» replacement: %q", out)
	}
}

func TestStartNuntiusPostures(t *testing.T) {
	fp := &fakeProvider{name: "fake", streams: []string{"x"}}
	hub, ap, home := setupTestHub(t, fp)
	source := surface.NewPenatusSource(home, hub)
	ctx := context.Background()

	// (a) disabled -> dormant, no error, bridge nil.
	cfg := &Config{}
	b, stop, err := startNuntius(ctx, cfg, home, hub, source, ap)
	if err != nil || b != nil || stop != nil {
		t.Fatalf("disabled: b=%v stopSet=%v err=%v; want nil,nil,nil", b, stop != nil, err)
	}

	// (b) enabled without a token -> dormant (LogDormant), no error.
	t.Setenv("LARARIUM_TEST_NO_TOKEN", "")
	cfg = &Config{}
	cfg.Nuntius.Enabled = true
	cfg.Nuntius.BotTokenEnv = "LARARIUM_TEST_NO_TOKEN"
	b, stop, err = startNuntius(ctx, cfg, home, hub, source, ap)
	if err != nil || b != nil || stop != nil {
		t.Fatalf("no-token: b=%v stopSet=%v err=%v; want nil,nil,nil", b, stop != nil, err)
	}

	// (c) V15/N8: enabled with token but corrupt bridge state ->
	// dormant with remedy logged, web surface unaffected.
	t.Setenv("LARARIUM_TEST_FAKE_TOKEN", "123456:TEST-token")
	ndir := filepath.Join(home, "nuntius")
	if err := os.MkdirAll(ndir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ndir, "state.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = &Config{}
	cfg.Nuntius.Enabled = true
	cfg.Nuntius.BotTokenEnv = "LARARIUM_TEST_FAKE_TOKEN"
	b, stop, err = startNuntius(ctx, cfg, home, hub, source, ap)
	if err != nil {
		t.Fatalf("corrupt state must stay dormant, got error: %v", err)
	}
	if b != nil || stop != nil {
		t.Fatalf("corrupt state must not start a bridge: b=%v", b)
	}
}
