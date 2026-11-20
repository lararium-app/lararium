package surface

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
)

type recordedPinCall struct {
	id  string
	pin *string
}

type fakePinSessionSource struct {
	fakeSessionSource

	setPinErr error
	calls     []recordedPinCall
}

func (f *fakePinSessionSource) SetModelPin(id string, pin *string) error {
	var cp *string
	if pin != nil {
		v := *pin
		cp = &v
	}
	f.calls = append(f.calls, recordedPinCall{id: id, pin: cp})
	return f.setPinErr
}

func TestPatchSessionRoute(t *testing.T) {
	const validID = "s_AAAAAAAAAAAAAAAAAAAAAAAAAA"
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	cat := sampleCatalog()

	tests := []struct {
		name         string
		path         string
		body         string
		catalog      ModelCatalog
		inFlight     bool
		nonSetter    bool
		setPinErr    error
		wantStatus   int
		wantErr      string
		wantCalls    int
		wantPinSet   bool
		wantPinValue string
	}{
		{
			name:         "valid online pin 200",
			path:         "/v1/sessions/" + validID,
			body:         `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:      cat,
			wantStatus:   http.StatusOK,
			wantCalls:    1,
			wantPinSet:   true,
			wantPinValue: "openrouter/anthropic/claude-3.5-sonnet",
		},
		{
			name:       "null clears pin 200",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":null}`,
			catalog:    cat,
			wantStatus: http.StatusOK,
			wantCalls:  1,
			wantPinSet: false,
		},
		{
			name:       "absent key clears pin 200",
			path:       "/v1/sessions/" + validID,
			body:       `{}`,
			catalog:    cat,
			wantStatus: http.StatusOK,
			wantCalls:  1,
			wantPinSet: false,
		},
		{
			name:       "turn in flight 409",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    cat,
			inFlight:   true,
			wantStatus: http.StatusConflict,
			wantErr:    "turn_in_flight",
			wantCalls:  0,
		},
		{
			name:       "ref absent from catalog 400",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"unknown/model"}`,
			catalog:    cat,
			wantStatus: http.StatusBadRequest,
			wantErr:    "invalid_model_pin",
			wantCalls:  0,
		},
		{
			name:       "ref with status no_key 400",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openai/gpt-4o"}`,
			catalog:    cat,
			wantStatus: http.StatusBadRequest,
			wantErr:    "invalid_model_pin",
			wantCalls:  0,
		},
		{
			name:         "ref with status offline accepted 200",
			path:         "/v1/sessions/" + validID,
			body:         `{"model_pin":"local/qwen2.5-coder-7b"}`,
			catalog:      cat,
			wantStatus:   http.StatusOK,
			wantCalls:    1,
			wantPinSet:   true,
			wantPinValue: "local/qwen2.5-coder-7b",
		},
		{
			name:       "nil catalog with pin 503",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    nil,
			wantStatus: http.StatusServiceUnavailable,
			wantErr:    "model_catalog_unavailable",
			wantCalls:  0,
		},
		{
			name:       "malformed session id 404",
			path:       "/v1/sessions/not-an-id",
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    cat,
			wantStatus: http.StatusNotFound,
			wantErr:    "not found",
			wantCalls:  0,
		},
		{
			name:       "source without ModelPinSetter 501",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    cat,
			nonSetter:  true,
			wantStatus: http.StatusNotImplemented,
			wantErr:    "not implemented",
			wantCalls:  0,
		},
		{
			name:       "source ErrNoSession 404",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    cat,
			setPinErr:  ErrNoSession,
			wantStatus: http.StatusNotFound,
			wantErr:    "not found",
			wantCalls:  1,
		},
		{
			name:       "source internal error 500",
			path:       "/v1/sessions/" + validID,
			body:       `{"model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			catalog:    cat,
			setPinErr:  errors.New("disk full"),
			wantStatus: http.StatusInternalServerError,
			wantErr:    "internal error",
			wantCalls:  1,
		},
		{
			name:       "invalid request body 400",
			path:       "/v1/sessions/" + validID,
			body:       `{not-json`,
			catalog:    cat,
			wantStatus: http.StatusBadRequest,
			wantErr:    "invalid request body",
			wantCalls:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pinSrc *fakePinSessionSource
			var sessions SessionSource
			if tc.nonSetter {
				sessions = &fakeSessionSource{}
			} else {
				pinSrc = &fakePinSessionSource{setPinErr: tc.setPinErr}
				sessions = pinSrc
			}

			hub := &fakeHub{inFlight: map[string]bool{}}
			if tc.inFlight {
				hub.inFlight[validID] = true
			}

			srv := &Server{
				Store:    store,
				Sessions: sessions,
				Hub:      hub,
				Models:   tc.catalog,
			}

			req := httptest.NewRequest(http.MethodPatch, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			srv.dispatch(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			if pinSrc != nil && len(pinSrc.calls) != tc.wantCalls {
				t.Fatalf("SetModelPin calls = %d, want %d", len(pinSrc.calls), tc.wantCalls)
			}

			if tc.wantErr != "" {
				var errBody map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
					t.Fatalf("unmarshal error body: %v", err)
				}
				if errBody["error"] != tc.wantErr {
					t.Errorf("error = %q, want %q", errBody["error"], tc.wantErr)
				}
				return
			}

			var resp struct {
				ID       string  `json:"id"`
				ModelPin *string `json:"model_pin"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if resp.ID != validID {
				t.Errorf("resp.id = %q, want %q", resp.ID, validID)
			}
			if tc.wantPinSet {
				if resp.ModelPin == nil || *resp.ModelPin != tc.wantPinValue {
					t.Errorf("resp.model_pin = %v, want %q", resp.ModelPin, tc.wantPinValue)
				}
				if pinSrc.calls[0].id != validID || pinSrc.calls[0].pin == nil || *pinSrc.calls[0].pin != tc.wantPinValue {
					t.Errorf("recorded call = %+v, want id=%q pin=%q", pinSrc.calls[0], validID, tc.wantPinValue)
				}
			} else {
				if resp.ModelPin != nil {
					t.Errorf("resp.model_pin = %q, want nil", *resp.ModelPin)
				}
				if pinSrc.calls[0].id != validID || pinSrc.calls[0].pin != nil {
					t.Errorf("recorded call = %+v, want id=%q pin=nil", pinSrc.calls[0], validID)
				}
			}
		})
	}
}

func TestPenatusSourceSetModelPin(t *testing.T) {
	dir := t.TempDir()
	src := NewPenatusSource(dir, &fakeHub{})

	sess, err := src.Create("Side Chat", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ModelPin != nil {
		t.Fatalf("initial ModelPin = %q, want nil", *sess.ModelPin)
	}

	readHeader := func(id string) penatus.SessionHeader {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "sessions", id, "session.json"))
		if err != nil {
			t.Fatalf("read session.json: %v", err)
		}
		var h penatus.SessionHeader
		if err := json.Unmarshal(data, &h); err != nil {
			t.Fatalf("unmarshal session.json: %v", err)
		}
		return h
	}

	// Set a pin and verify persistence + preservation of other header fields.
	pin := "openrouter/anthropic/claude-3.5-sonnet"
	if err := src.SetModelPin(sess.ID, &pin); err != nil {
		t.Fatalf("SetModelPin(set): %v", err)
	}
	h := readHeader(sess.ID)
	if h.ModelPin == nil || *h.ModelPin != pin {
		t.Errorf("header.ModelPin = %v, want %q", h.ModelPin, pin)
	}
	if h.Title == nil || *h.Title != "Side Chat" {
		t.Errorf("header.Title = %v, want %q", h.Title, "Side Chat")
	}
	if h.Parent == nil || *h.Parent != "main" {
		t.Errorf("header.Parent = %v, want %q", h.Parent, "main")
	}
	// created is a required, non-pointer field: a read-modify-write that
	// drops it silently writes an invalid header (PENATUS §2).
	if h.Created == "" {
		t.Error("header.Created was dropped by SetModelPin")
	}
	if h.Penatus == 0 {
		t.Error("header.Penatus was dropped by SetModelPin")
	}

	// Clear the pin with nil and verify removal.
	if err := src.SetModelPin(sess.ID, nil); err != nil {
		t.Fatalf("SetModelPin(clear): %v", err)
	}
	h = readHeader(sess.ID)
	if h.ModelPin != nil {
		t.Errorf("header.ModelPin after clear = %q, want nil", *h.ModelPin)
	}

	// Unknown well-formed session ID returns ErrNoSession.
	if err := src.SetModelPin("s_UNKNOWN123456789012345678", &pin); !errors.Is(err, ErrNoSession) {
		t.Errorf("SetModelPin(unknown) = %v, want ErrNoSession", err)
	}

	// Malformed session ID returns ErrNoSession.
	if err := src.SetModelPin("../escape", &pin); !errors.Is(err, ErrNoSession) {
		t.Errorf("SetModelPin(malformed) = %v, want ErrNoSession", err)
	}

	// Wire through HTTP PATCH against real PenatusSource.
	store, _ := OpenTokenStore(dir + "/tokens.json")
	srv := &Server{
		Store:    store,
		Sessions: src,
		Hub:      &fakeHub{},
		Models:   sampleCatalog(),
	}

	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+sess.ID, strings.NewReader(`{"model_pin":"local/qwen2.5-coder-7b"}`))
	w := httptest.NewRecorder()
	srv.dispatch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH existing = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	h = readHeader(sess.ID)
	if h.ModelPin == nil || *h.ModelPin != "local/qwen2.5-coder-7b" {
		t.Errorf("persisted ModelPin = %v, want %q", h.ModelPin, "local/qwen2.5-coder-7b")
	}

	req404 := httptest.NewRequest(http.MethodPatch, "/v1/sessions/s_UNKNOWN123456789012345678", strings.NewReader(`{"model_pin":"local/qwen2.5-coder-7b"}`))
	w404 := httptest.NewRecorder()
	srv.dispatch(w404, req404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing = %d, want 404 (body %s)", w404.Code, w404.Body.String())
	}
}
