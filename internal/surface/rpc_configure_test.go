package surface

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Configure satisfies SessionSource for fakeSessionSource without modifying
// existing *_test.go files.
func (f *fakeSessionSource) Configure(id string, modelPin *string, _ *int64) (SessionInfo, error) {
	if !ValidSessionID(id) {
		return SessionInfo{}, ErrNoSession
	}
	var pin *string
	if modelPin != nil && *modelPin != "" {
		v := *modelPin
		pin = &v
	}
	return SessionInfo{
		ID:       id,
		Kind:     "side",
		ModelPin: pin,
		Revision: 1,
	}, nil
}

type staticCatalog struct {
	resp ModelsResponse
}

func (c staticCatalog) Catalog() ModelsResponse { return c.resp }

func TestRPCModelsList(t *testing.T) {
	t.Run("nil catalog returns model_catalog_unavailable", func(t *testing.T) {
		_, store, ts := newTestRPCServer(t, 2*time.Second)
		tok, err := store.Create("models-nil")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)
		resp := client.callRPC(t, 1, "models.list", map[string]any{})
		assertRPCErrorReason(t, resp, "model_catalog_unavailable")
	})

	t.Run("returns models with probed_at and defaults", func(t *testing.T) {
		srv, store, ts := newTestRPCServer(t, 2*time.Second)
		probed := "2026-11-21T10:00:00Z"
		srv.Models = staticCatalog{
			resp: ModelsResponse{
				Models: []ModelEntry{
					{
						Ref:         "local/llama",
						Provider:    "local",
						ModelID:     "llama",
						DisplayName: "llama",
						Status:      "online",
						IsDefault:   true,
						ProbedAt:    &probed,
					},
					{
						Ref:         "cloud/gpt-4o",
						Provider:    "cloud",
						ModelID:     "gpt-4o",
						DisplayName: "gpt-4o",
						Status:      "no_key",
						ProbedAt:    nil,
					},
				},
				Defaults: ModelDefaults{
					ChatChain:    []string{"local/llama"},
					CompactChain: []string{"local/llama"},
				},
			},
		}
		tok, err := store.Create("models-ok")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)
		resp := client.callRPC(t, 2, "models.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out ModelsResponse
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("unmarshal models.list: %v", err)
		}
		if len(out.Models) != 2 {
			t.Fatalf("models len = %d, want 2", len(out.Models))
		}
		if out.Models[0].ProbedAt == nil || *out.Models[0].ProbedAt != probed {
			t.Errorf("models[0].probed_at = %v, want %q", out.Models[0].ProbedAt, probed)
		}
		if out.Models[1].ProbedAt != nil {
			t.Errorf("models[1].probed_at = %v, want nil", out.Models[1].ProbedAt)
		}
		if len(out.Defaults.ChatChain) != 1 || out.Defaults.ChatChain[0] != "local/llama" {
			t.Errorf("defaults = %+v", out.Defaults)
		}
	})
}

func TestRPCSessionsConfigure(t *testing.T) {
	home := t.TempDir()
	// Create sessions/main directory WITHOUT session.json to test main synthesis.
	if err := os.MkdirAll(filepath.Join(home, "sessions", "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	src := NewPenatusSource(home, nil)
	side, err := src.Create("Side", "")
	if err != nil {
		t.Fatalf("Create side session: %v", err)
	}

	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	srv.Sessions = src
	srv.Models = staticCatalog{
		resp: ModelsResponse{
			Models: []ModelEntry{
				{Ref: "p/good", Provider: "p", ModelID: "good", Status: "online"},
				{Ref: "p/nokey", Provider: "p", ModelID: "nokey", Status: "no_key"},
			},
		},
	}

	tok, err := store.Create("cfg-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("CAS happy path sets pin and bumps revision", func(t *testing.T) {
		resp := client.callRPC(t, 10, "sessions.configure", map[string]any{
			"id":                side.ID,
			"model_pin":         "p/good",
			"expected_revision": 0,
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK      bool              `json:"ok"`
			Applied map[string]string `json:"applied"`
			Session SessionInfo       `json:"session"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !out.OK || out.Applied["model_pin"] != "ok" {
			t.Fatalf("unexpected ok/applied: %+v", out)
		}
		if out.Session.ModelPin == nil || *out.Session.ModelPin != "p/good" {
			t.Fatalf("session.model_pin = %v, want p/good", out.Session.ModelPin)
		}
		if out.Session.Revision != 1 {
			t.Fatalf("session.revision = %d, want 1", out.Session.Revision)
		}
	})

	t.Run("CAS conflict returns revision_conflict without mutating", func(t *testing.T) {
		resp := client.callRPC(t, 11, "sessions.configure", map[string]any{
			"id":                side.ID,
			"model_pin":         nil,
			"expected_revision": 0,
		})
		assertRPCErrorReason(t, resp, "revision_conflict")
	})

	t.Run("invalid and uncataloged pins return invalid_model_pin", func(t *testing.T) {
		respNoKey := client.callRPC(t, 12, "sessions.configure", map[string]any{
			"id":        side.ID,
			"model_pin": "p/nokey",
		})
		assertRPCErrorReason(t, respNoKey, "invalid_model_pin")

		respUnknown := client.callRPC(t, 13, "sessions.configure", map[string]any{
			"id":        side.ID,
			"model_pin": "other/missing",
		})
		assertRPCErrorReason(t, respUnknown, "invalid_model_pin")
	})

	t.Run("absent model_pin leaves pin and revision unchanged", func(t *testing.T) {
		resp := client.callRPC(t, 14, "sessions.configure", map[string]any{
			"id":                side.ID,
			"expected_revision": 1,
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			Applied map[string]string `json:"applied"`
			Session SessionInfo       `json:"session"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Applied) != 0 {
			t.Errorf("applied = %+v, want empty", out.Applied)
		}
		if out.Session.Revision != 1 || out.Session.ModelPin == nil || *out.Session.ModelPin != "p/good" {
			t.Errorf("session mutated unexpectedly: %+v", out.Session)
		}
	})

	t.Run("SetModelPin and Configure share monotonic revision counter", func(t *testing.T) {
		if err := src.SetModelPin(side.ID, nil); err != nil {
			t.Fatalf("SetModelPin: %v", err)
		}
		// Revision is now 2, so expected_revision=1 must conflict.
		conflict := client.callRPC(t, 15, "sessions.configure", map[string]any{
			"id":                side.ID,
			"model_pin":         "p/good",
			"expected_revision": 1,
		})
		assertRPCErrorReason(t, conflict, "revision_conflict")

		// expected_revision=2 succeeds and bumps to 3.
		okResp := client.callRPC(t, 16, "sessions.configure", map[string]any{
			"id":                side.ID,
			"model_pin":         "p/good",
			"expected_revision": 2,
		})
		if okResp.Error != nil {
			t.Fatalf("unexpected error: %+v", okResp.Error)
		}
		var out struct {
			Session SessionInfo `json:"session"`
		}
		if err := json.Unmarshal(okResp.Result, &out); err != nil {
			t.Fatal(err)
		}
		if out.Session.Revision != 3 {
			t.Errorf("revision = %d, want 3", out.Session.Revision)
		}
	})

	t.Run("main session synthesizes missing session.json", func(t *testing.T) {
		resp := client.callRPC(t, 17, "sessions.configure", map[string]any{
			"id":                "main",
			"model_pin":         "p/good",
			"expected_revision": 0,
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error on main synthesis: %+v", resp.Error)
		}
		var out struct {
			Session SessionInfo `json:"session"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatal(err)
		}
		if out.Session.ID != "main" || out.Session.Kind != "main" || out.Session.Revision != 1 {
			t.Errorf("unexpected synthesized main session: %+v", out.Session)
		}
		if out.Session.ModelPin == nil || *out.Session.ModelPin != "p/good" {
			t.Errorf("main model_pin = %v, want p/good", out.Session.ModelPin)
		}
		if _, err := os.Stat(filepath.Join(home, "sessions", "main", "session.json")); err != nil {
			t.Errorf("main session.json not written: %v", err)
		}
	})

	t.Run("unknown session id returns no_such_session", func(t *testing.T) {
		resp := client.callRPC(t, 18, "sessions.configure", map[string]any{
			"id":        "s_ZZZZZZZZZZZZZZZZZZZZZZZZZZ",
			"model_pin": "p/good",
		})
		assertRPCErrorReason(t, resp, "no_such_session")
	})
}
