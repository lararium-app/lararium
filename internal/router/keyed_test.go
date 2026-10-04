package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/keystore"
)

// echoServer answers /chat/completions with a minimal completion and
// records the Authorization header it saw.
func echoServer(got *string, count *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get("Authorization")
		*count++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "ok"}},
			},
		})
	}))
}

func TestKeyedProviderUsesCtxSnapshot(t *testing.T) {
	var got string
	count := 0
	srv := echoServer(&got, &count)
	defer srv.Close()

	keyFn := func(ctx context.Context) string {
		if snap, ok := keystore.SnapshotFrom(ctx); ok {
			return snap["p"]
		}
		return "fallback"
	}
	p := NewOpenAIKeyed("openai", srv.URL, keyFn, "m", false)

	ctxA := keystore.WithSnapshot(context.Background(), map[string]string{"p": "key-A"})
	if _, err := p.Complete(ctxA, []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("call A: %v", err)
	}
	if got != "Bearer key-A" {
		t.Fatalf("call A saw %q, want %q", got, "Bearer key-A")
	}

	ctxB := keystore.WithSnapshot(context.Background(), map[string]string{"p": "key-B"})
	if _, err := p.Complete(ctxB, []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("call B: %v", err)
	}
	if got != "Bearer key-B" {
		t.Fatalf("call B saw %q, want %q", got, "Bearer key-B")
	}

	// No snapshot => keyFn fallback.
	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("call C: %v", err)
	}
	if got != "Bearer fallback" {
		t.Fatalf("no-snapshot call saw %q, want %q", got, "Bearer fallback")
	}
}

func TestKeyedProviderLocalFailRule(t *testing.T) {
	var got string
	count := 0
	srv := echoServer(&got, &count)
	defer srv.Close()

	// Empty key + loopback URL (httptest is 127.0.0.1): request goes
	// through with NO Authorization header — local llama.cpp behavior.
	p := NewOpenAIKeyed("openai", srv.URL, func(context.Context) string { return "" }, "m", false)
	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("loopback empty key: %v", err)
	}
	if got != "" {
		t.Fatalf("loopback: saw Authorization %q, want none", got)
	}

	// Empty key + non-loopback host: fail BEFORE any request.
	// A hostname that cannot resolve stands in for a remote provider;
	// the call must fail with ErrNoKey, not a transport error.
	remote := NewOpenAIKeyed("openai", "https://api.invalid.example/v1", func(context.Context) string { return "" }, "m", false)
	_, err := remote.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{})
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("remote empty key: got %v, want ErrNoKey", err)
	}
	if !strings.Contains(err.Error(), "api.invalid.example") {
		t.Fatalf("error should name the host, got %v", err)
	}

	// Streaming path enforces the same rule.
	s, ok := remote.(Streamer)
	if !ok {
		t.Fatal("keyed provider must implement Streamer")
	}
	_, err = s.StreamComplete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}, "m", func(string) error { return nil })
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("stream remote empty key: got %v, want ErrNoKey", err)
	}
}

func TestStaticProviderBehaviorUnchanged(t *testing.T) {
	var got string
	count := 0
	srv := echoServer(&got, &count)
	defer srv.Close()

	// Static empty key: legacy behavior — no header, no K6 error.
	p := NewOpenAI(srv.URL, "", "m")
	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("static empty key: %v", err)
	}
	if got != "" {
		t.Fatalf("static empty key saw %q, want no header", got)
	}

	// Static key: header present.
	p2 := NewOpenAIWith("openai", srv.URL, "sk-static", "m", true)
	if _, err := p2.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("static key: %v", err)
	}
	if got != "Bearer sk-static" {
		t.Fatalf("static key saw %q, want %q", got, "Bearer sk-static")
	}
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8080/v1", true},
		{"http://127.5.5.5:8080", true}, // whole 127/8
		{"http://localhost:8080", true},
		{"http://[::1]:8080/v1", true},
		{"http:///v1", true}, // empty host (unix-style)
		{"https://api.openrouter.example/v1", false},
		{"http://10.0.0.5:8080", false},
		{"http://198.51.100.2:13308/v1", false},
	}
	for _, c := range cases {
		if got := isLoopbackURL(c.url); got != c.want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestProviderNameIsConfigID(t *testing.T) {
	// Audit 5.7: Name() reports the config-declared provider id, and
	// legacy constructors keep the "openai" fallback.
	named := NewOpenAIKeyed("flashy", "http://127.0.0.1:1/v1",
		func(context.Context) string { return "k" }, "m", false)
	if got := named.Name(); got != "flashy" {
		t.Fatalf("named provider Name() = %q, want %q", got, "flashy")
	}
	legacy := NewOpenAI("http://127.0.0.1:1/v1", "", "m")
	if got := legacy.Name(); got != "openai" {
		t.Fatalf("legacy provider Name() = %q, want %q", got, "openai")
	}
}
