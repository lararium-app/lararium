package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

func TestCapsCatalogOnlineOfflineNoKeyAndPinProfiles(t *testing.T) {
	onlineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-4o","context_length":128000,"projector":"mmproj.gguf"},
				{"id":"llama-local"}
			]}`))
		case "/props":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"n_ctx":32768}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer onlineSrv.Close()

	offlineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer offlineSrv.Close()

	noKeySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer noKeySrv.Close()

	thinkFalse := false
	home := t.TempDir()
	cfg := &Config{
		Hearth: HearthConfig{Home: home},
		Providers: []ProviderConf{
			{Name: "online", BaseURL: onlineSrv.URL + "/v1", APIKey: "k-online", Think: &thinkFalse},
			{Name: "offline", BaseURL: offlineSrv.URL + "/v1", APIKey: "k-offline"},
			{Name: "nokey", BaseURL: noKeySrv.URL + "/v1"},
		},
		Models: ModelsConfig{
			Default: []string{"online/gpt-4o", "offline/backup-model"},
			Compact: []string{"nokey/small-model"},
		},
	}

	kp, err := newKeyPlumbing(cfg)
	if err != nil {
		t.Fatalf("newKeyPlumbing: %v", err)
	}
	byName := kp.BuildProviders(cfg)
	rt := router.NewRouter(router.Profile{
		Chain: []router.Target{{Provider: byName["online"], Model: "gpt-4o"}},
	})

	cat := newCapsCatalog(home, cfg, kp, rt, byName)
	cat.Refresh(context.Background())

	resp := cat.Catalog()
	byRef := make(map[string]surface.ModelEntry, len(resp.Models))
	for _, m := range resp.Models {
		byRef[m.Ref] = m
	}

	// Online model with explicit context_length, vision projector, tools inferred, think=false.
	gpt, ok := byRef["online/gpt-4o"]
	if !ok {
		t.Fatalf("missing online/gpt-4o in catalog: %+v", resp.Models)
	}
	if gpt.Status != "online" {
		t.Errorf("online/gpt-4o status = %q, want online", gpt.Status)
	}
	if !gpt.IsDefault || gpt.IsCompact {
		t.Errorf("online/gpt-4o flags default=%v compact=%v, want true/false", gpt.IsDefault, gpt.IsCompact)
	}
	if gpt.Capabilities.ContextWindow != 128000 || !gpt.Capabilities.Vision || !gpt.Capabilities.Tools || gpt.Capabilities.Thinking || !gpt.Capabilities.Streaming {
		t.Errorf("online/gpt-4o caps = %+v", gpt.Capabilities)
	}
	if gpt.ProbedAt == nil || *gpt.ProbedAt == "" {
		t.Errorf("online/gpt-4o probed_at should be set")
	}

	// Online model falling back to /props n_ctx=32768.
	llama, ok := byRef["online/llama-local"]
	if !ok {
		t.Fatalf("missing online/llama-local in catalog")
	}
	if llama.Capabilities.ContextWindow != 32768 {
		t.Errorf("online/llama-local context_window = %d, want 32768", llama.Capabilities.ContextWindow)
	}

	// Offline chain ref still gets a row with status="offline" and zero caps.
	off, ok := byRef["offline/backup-model"]
	if !ok {
		t.Fatalf("missing offline/backup-model chain ref in catalog")
	}
	if off.Status != "offline" {
		t.Errorf("offline/backup-model status = %q, want offline", off.Status)
	}
	if off.Capabilities != (surface.ModelCapabilities{}) {
		t.Errorf("offline/backup-model caps = %+v, want zero", off.Capabilities)
	}
	if !off.IsDefault {
		t.Errorf("offline/backup-model is_default = false, want true")
	}

	// No-key chain ref gets status="no_key" and is_compact=true.
	nk, ok := byRef["nokey/small-model"]
	if !ok {
		t.Fatalf("missing nokey/small-model chain ref in catalog")
	}
	if nk.Status != "no_key" {
		t.Errorf("nokey/small-model status = %q, want no_key", nk.Status)
	}
	if !nk.IsCompact {
		t.Errorf("nokey/small-model is_compact = false, want true")
	}

	// Pin profiles must be registered on the router.
	for _, ref := range []string{"online/gpt-4o", "online/llama-local", "offline/backup-model", "nokey/small-model"} {
		if !rt.HasProfile("pin:" + ref) {
			t.Errorf("router missing profile pin:%s", ref)
		}
	}

	// caps.json round-trip: a fresh catalog loaded from disk preserves probed rows without probing.
	cat2 := newCapsCatalog(home, cfg, kp, rt, byName)
	resp2 := cat2.Catalog()
	if len(resp2.Models) != len(resp.Models) {
		t.Fatalf("reloaded catalog len = %d, want %d", len(resp2.Models), len(resp.Models))
	}
	var disk capsCache
	raw, err := os.ReadFile(filepath.Join(home, "caps.json"))
	if err != nil {
		t.Fatalf("read caps.json: %v", err)
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("unmarshal caps.json: %v", err)
	}
	if disk.Version != 1 || !disk.Providers["online"].OK {
		t.Errorf("unexpected caps.json contents: %+v", disk)
	}
}

func TestCapsCatalogColdStartAndCorruptCache(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		Hearth:    HearthConfig{Home: home},
		Providers: []ProviderConf{{Name: "p", BaseURL: "http://example.com/v1", APIKey: "k"}},
		Models:    ModelsConfig{Default: []string{"p/m1"}},
	}

	// Cold start: missing caps.json -> no warning, chain ref has nil ProbedAt.
	cat := &capsCatalog{
		cachePath:    filepath.Join(home, "caps.json"),
		providers:    cfg.Providers,
		defaultChain: cfg.Models.Default,
		resolveKey:   func(p ProviderConf) string { return p.APIKey },
		stderr:       &bytes.Buffer{},
		cache:        capsCache{Version: 1, Providers: map[string]providerCapsRow{}},
	}
	cat.loadCache()
	if !cat.isCold() {
		t.Fatalf("expected cold cache on missing caps.json")
	}
	resp := cat.Catalog()
	if len(resp.Models) != 1 || resp.Models[0].ProbedAt != nil {
		t.Fatalf("cold catalog = %+v, want 1 row with nil ProbedAt", resp.Models)
	}

	// Corrupt caps.json -> warns once on stderr and starts empty without failing.
	if err := os.WriteFile(filepath.Join(home, "caps.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	catCorrupt := &capsCatalog{
		cachePath:    filepath.Join(home, "caps.json"),
		providers:    cfg.Providers,
		defaultChain: cfg.Models.Default,
		resolveKey:   func(p ProviderConf) string { return p.APIKey },
		stderr:       &errBuf,
		cache:        capsCache{Version: 1, Providers: map[string]providerCapsRow{}},
	}
	catCorrupt.loadCache()
	if !catCorrupt.isCold() {
		t.Fatalf("expected empty cache on corrupt caps.json")
	}
	if !strings.Contains(errBuf.String(), "warning:") {
		t.Fatalf("expected stderr warning on corrupt caps.json, got %q", errBuf.String())
	}
}

func TestCapsCatalogRefresherStopsOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	cat := &capsCatalog{
		cachePath:    filepath.Join(home, "caps.json"),
		providers:    []ProviderConf{{Name: "p", BaseURL: srv.URL + "/v1"}},
		defaultChain: []string{"p/m1"},
		resolveKey:   func(ProviderConf) string { return "" },
		interval:     20 * time.Millisecond,
		probeTimeout: 2 * time.Second,
		stderr:       &bytes.Buffer{},
		cache:        capsCache{Version: 1, Providers: map[string]providerCapsRow{}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := cat.Start(ctx)

	// Wait briefly for initial cold-start refresh.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !cat.isCold() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cat.isCold() {
		cancel()
		t.Fatal("refresher did not run initial cold-start cycle")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher goroutine did not stop after ctx cancel")
	}
}
