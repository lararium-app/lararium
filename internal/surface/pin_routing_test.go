package surface

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

type recordingProvider struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (p *recordingProvider) Name() string { return p.name }

func (p *recordingProvider) Complete(_ context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &router.Completion{Text: "reply from " + p.name, Model: p.name + "-model"}, nil
}

func (p *recordingProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{ContextLength: 8192, Source: "test"}, nil
}

func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestPinRoutingInTurns(t *testing.T) {
	home := t.TempDir()
	// Minimal valid persona files so loop.SystemPrompt succeeds.
	for _, f := range []struct{ name, body string }{
		{"SOUL.md", "---\npenatus: 1\n---\nBe helpful.\n"},
		{"IDENTITY.md", "---\npenatus: 1\n---\nHearth.\n"},
		{"USER.md", "---\npenatus: 1\n---\nOwner.\n"},
	} {
		if err := os.WriteFile(filepath.Join(home, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	defProv := &recordingProvider{name: "default"}
	pinProv := &recordingProvider{name: "pinned"}
	rt := router.NewRouter(router.Profile{
		Chain: []router.Target{{Provider: defProv, Model: "def-m"}},
	})
	rt.SetProfile("compact", router.Profile{
		Chain: []router.Target{{Provider: defProv, Model: "def-m"}},
	})
	rt.SetProfile("pin:pinned/fast", router.Profile{
		Chain: []router.Target{{Provider: pinProv, Model: "fast"}},
	})

	hub := NewHub(ServeConfig{}, home, 256, 80, nil, rt, NewApprovalHub(0))

	t.Run("pinned session with registered pin profile routes to pin provider", func(t *testing.T) {
		sid := "s_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		sessDir := hub.SessionDir(sid)
		if err := os.MkdirAll(sessDir, 0o700); err != nil {
			t.Fatal(err)
		}
		pin := "pinned/fast"
		hdr, _ := json.Marshal(penatus.SessionHeader{
			Penatus:  1,
			ID:       sid,
			Kind:     "side",
			ModelPin: &pin,
		})
		if err := os.WriteFile(filepath.Join(sessDir, "session.json"), hdr, 0o600); err != nil {
			t.Fatal(err)
		}

		if got := hub.resolveSessionChatProfile(sessDir, sid); got != "pin:pinned/fast" {
			t.Fatalf("resolveSessionChatProfile = %q, want pin:pinned/fast", got)
		}

		done := make(chan struct{})
		err := hub.StartRPCTurn(context.Background(), sid, "hello pinned", func(eventType, _ string) {
			if eventType == "turn_done" || eventType == "error" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		})
		if err != nil {
			t.Fatalf("StartRPCTurn: %v", err)
		}
		<-done

		if pinProv.count() != 1 {
			t.Errorf("pinned provider calls = %d, want 1", pinProv.count())
		}
		if defProv.count() != 0 {
			t.Errorf("default provider calls = %d, want 0", defProv.count())
		}
	})

	t.Run("pinned session with missing profile falls back to default without error", func(t *testing.T) {
		sid := "s_BBBBBBBBBBBBBBBBBBBBBBBBBB"
		sessDir := hub.SessionDir(sid)
		if err := os.MkdirAll(sessDir, 0o700); err != nil {
			t.Fatal(err)
		}
		pin := "drifted/missing"
		hdr, _ := json.Marshal(penatus.SessionHeader{
			Penatus:  1,
			ID:       sid,
			Kind:     "side",
			ModelPin: &pin,
		})
		if err := os.WriteFile(filepath.Join(sessDir, "session.json"), hdr, 0o600); err != nil {
			t.Fatal(err)
		}

		if got := hub.resolveSessionChatProfile(sessDir, sid); got != "" {
			t.Fatalf("resolveSessionChatProfile = %q, want empty fallback", got)
		}

		done := make(chan struct{})
		err := hub.StartRPCTurn(context.Background(), sid, "hello fallback", func(eventType, _ string) {
			if eventType == "turn_done" || eventType == "error" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		})
		if err != nil {
			t.Fatalf("StartRPCTurn: %v", err)
		}
		<-done

		if defProv.count() != 1 {
			t.Errorf("default provider calls = %d, want 1", defProv.count())
		}
	})
}
