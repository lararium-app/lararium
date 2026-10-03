package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSessionSource struct {
	sessions  []SessionInfo
	events    []json.RawMessage
	inFlight  bool
	listErr   error
	createErr error
	eventsErr error
}

func (f *fakeSessionSource) List() ([]SessionInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.sessions, nil
}

func (f *fakeSessionSource) Create(title, modelPin string) (SessionInfo, error) {
	if f.createErr != nil {
		return SessionInfo{}, f.createErr
	}
	return SessionInfo{ID: "s_NEWSESSIONID1234567890", Title: &title, ModelPin: &modelPin}, nil
}

func (f *fakeSessionSource) Events(id string, afterSeq int64, limit int) ([]json.RawMessage, bool, error) {
	if f.eventsErr != nil {
		return nil, false, f.eventsErr
	}

	var result []json.RawMessage
	for _, evt := range f.events {
		var e map[string]any
		if err := json.Unmarshal(evt, &e); err != nil {
			continue
		}
		if seq, ok := e["seq"].(float64); ok {
			if int64(seq) <= afterSeq {
				continue
			}
		}
		if len(result) >= limit {
			break
		}
		result = append(result, evt)
	}

	return result, f.inFlight, nil
}

type fakeHub struct {
	inFlight map[string]bool
	cancel   map[string]bool
}

func (f *fakeHub) InFlight(id string) bool {
	return f.inFlight[id]
}

func (f *fakeHub) Cancel(id string) bool {
	if f.cancel[id] {
		delete(f.cancel, id)
		return true
	}
	return false
}

func TestHealthHandler(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp["ok"].(bool) {
		t.Errorf("ok = false, want true")
	}
	if resp["version"] == nil {
		t.Errorf("version missing")
	}
}

func TestListSessionsHandler(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	sessions := &fakeSessionSource{
		sessions: []SessionInfo{
			{ID: "s_AAAAAAAAAAAAAAAAAAAAAAAAAA", Created: "2024-01-01T00:00:00Z", Kind: "side"},
			{ID: "main", Created: "2024-01-01T00:00:00Z", Kind: "main"},
		},
	}
	srv := &Server{Store: store, Sessions: sessions, Hub: &fakeHub{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.listSessionsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []SessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("len = %d, want 2", len(resp))
	}
}

func TestCreateSessionHandler(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	sessions := &fakeSessionSource{}
	srv := &Server{Store: store, Sessions: sessions, Hub: &fakeHub{}}

	body := `{"title":"Test","model_pin":"model-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.createSessionHandler(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp SessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ID == "" {
		t.Errorf("empty ID")
	}
}

func TestEventsHandler(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	events := []json.RawMessage{
		json.RawMessage(`{"seq":1,"t":"user"}`),
		json.RawMessage(`{"seq":2,"t":"assistant"}`),
	}
	sessions := &fakeSessionSource{events: events, inFlight: true}
	srv := &Server{Store: store, Sessions: sessions, Hub: &fakeHub{}}

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantEvents int
		wantFlight bool
	}{
		{"default", "", http.StatusOK, 2, true},
		{"after_seq", "?after_seq=1", http.StatusOK, 1, true},
		{"limit clamp", "?limit=5000", http.StatusOK, 2, true}, // capped at 1000, we only have 2
		{"limit zero", "?limit=0", http.StatusBadRequest, 0, false},
		{"limit negative", "?limit=-1", http.StatusBadRequest, 0, false},
		{"limit garbage", "?limit=abc", http.StatusBadRequest, 0, false},
		{"after_seq negative", "?after_seq=-1", http.StatusBadRequest, 0, false},
		{"after_seq garbage", "?after_seq=abc", http.StatusBadRequest, 0, false},
		{"invalid id", "/v1/sessions/invalid/events", http.StatusNotFound, 0, false},
	}

	validSessionID := "s_ABCDEFGHIJKLMNOPQRSTUVWXYZ" // 26 uppercase chars

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/sessions/" + validSessionID + "/events" + tc.query
			if tc.name == "invalid id" {
				path = tc.query
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.eventsHandler(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				var resp map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				evts := resp["events"].([]any)
				if len(evts) != tc.wantEvents {
					t.Errorf("events len = %d, want %d", len(evts), tc.wantEvents)
				}
				if resp["in_flight"] != tc.wantFlight {
					t.Errorf("in_flight = %v, want %v", resp["in_flight"], tc.wantFlight)
				}
			}
		})
	}
}

func TestCancelHandler(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	validSessionID := "s_ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	tests := []struct {
		name       string
		hub        *fakeHub
		wantStatus int
	}{
		{"in flight true", &fakeHub{inFlight: map[string]bool{validSessionID: true}, cancel: map[string]bool{validSessionID: true}}, http.StatusOK},
		{"in flight false", &fakeHub{inFlight: map[string]bool{validSessionID: false}, cancel: map[string]bool{}}, http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: tc.hub}
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+validSessionID+"/cancel", nil)
			w := httptest.NewRecorder()
			srv.cancelHandler(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				var resp map[string]bool
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if !resp["cancelled"] {
					t.Errorf("cancelled = false, want true")
				}
			}
		})
	}
}

func TestBodyLimitMiddleware(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}

	mux := http.NewServeMux()
	// Use createSessionHandler which reads the request body
	mux.HandleFunc("/v1/sessions", srv.createSessionHandler)

	// Apply middleware to all routes like the server does
	wrappedMux := bodyLimitMiddleware(mux)

	body := strings.Repeat("x", 1<<20+1) // 1 MiB + 1
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer lar1_valid")
	w := httptest.NewRecorder()
	wrappedMux.ServeHTTP(w, req)

	t.Logf("Response code: %d, body: %s", w.Code, w.Body.String())

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (413)", w.Code, http.StatusRequestEntityTooLarge)
	}
}
