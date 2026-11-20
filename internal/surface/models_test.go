package surface

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type fakeCatalog struct {
	resp ModelsResponse
}

func (f *fakeCatalog) Catalog() ModelsResponse {
	return f.resp
}

func sampleCatalog() *fakeCatalog {
	return &fakeCatalog{
		resp: ModelsResponse{
			Models: []ModelEntry{
				{
					Ref:         "openrouter/anthropic/claude-3.5-sonnet",
					Provider:    "openrouter",
					ModelID:     "anthropic/claude-3.5-sonnet",
					DisplayName: "Claude 3.5 Sonnet",
					Capabilities: ModelCapabilities{
						Thinking:      false,
						Vision:        true,
						Tools:         true,
						Streaming:     true,
						ContextWindow: 200000,
					},
					Status:    "online",
					IsDefault: true,
					IsCompact: false,
				},
				{
					Ref:         "openai/gpt-4o",
					Provider:    "openai",
					ModelID:     "gpt-4o",
					DisplayName: "GPT-4o",
					Capabilities: ModelCapabilities{
						Thinking:      false,
						Vision:        true,
						Tools:         true,
						Streaming:     true,
						ContextWindow: 128000,
					},
					Status:    "no_key",
					IsDefault: false,
					IsCompact: false,
				},
				{
					Ref:         "local/qwen2.5-coder-7b",
					Provider:    "local",
					ModelID:     "qwen2.5-coder-7b",
					DisplayName: "Qwen 2.5 Coder 7B (Local)",
					Capabilities: ModelCapabilities{
						Thinking:      true,
						Vision:        false,
						Tools:         true,
						Streaming:     true,
						ContextWindow: 32768,
					},
					Status:    "offline",
					IsDefault: false,
					IsCompact: true,
				},
			},
			Defaults: ModelDefaults{
				ChatChain:    []string{"openrouter/anthropic/claude-3.5-sonnet", "local/qwen2.5-coder-7b"},
				CompactChain: []string{"local/qwen2.5-coder-7b"},
			},
		},
	}
}

func TestModelsRoute(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	populated := sampleCatalog()

	tests := []struct {
		name       string
		method     string
		catalog    ModelCatalog
		wantStatus int
		wantErr    string
		checkBody  func(t *testing.T, raw []byte)
	}{
		{
			name:       "populated catalog 200",
			method:     http.MethodGet,
			catalog:    populated,
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, raw []byte) {
				t.Helper()
				var got ModelsResponse
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal ModelsResponse: %v", err)
				}
				if !reflect.DeepEqual(got, populated.resp) {
					t.Errorf("decoded body = %+v, want %+v", got, populated.resp)
				}

				// Verify every frozen wire JSON key name is present.
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					t.Fatalf("unmarshal map: %v", err)
				}
				for _, k := range []string{"models", "defaults"} {
					if _, ok := obj[k]; !ok {
						t.Errorf("missing top-level key %q in %s", k, string(raw))
					}
				}
				defaults, ok := obj["defaults"].(map[string]any)
				if !ok {
					t.Fatalf("defaults is not an object: %T", obj["defaults"])
				}
				for _, k := range []string{"chat_chain", "compact_chain"} {
					if _, ok := defaults[k]; !ok {
						t.Errorf("missing defaults key %q", k)
					}
				}
				models, ok := obj["models"].([]any)
				if !ok || len(models) != len(populated.resp.Models) {
					t.Fatalf("models = %v, want len %d", obj["models"], len(populated.resp.Models))
				}
				first, ok := models[0].(map[string]any)
				if !ok {
					t.Fatalf("models[0] is not an object: %T", models[0])
				}
				for _, k := range []string{
					"ref", "provider", "model_id", "display_name",
					"capabilities", "status", "is_default", "is_compact",
				} {
					if _, ok := first[k]; !ok {
						t.Errorf("missing model key %q", k)
					}
				}
				caps, ok := first["capabilities"].(map[string]any)
				if !ok {
					t.Fatalf("capabilities is not an object: %T", first["capabilities"])
				}
				for _, k := range []string{
					"thinking", "vision", "tools", "streaming", "context_window",
				} {
					if _, ok := caps[k]; !ok {
						t.Errorf("missing capabilities key %q", k)
					}
				}
			},
		},
		{
			name:       "nil catalog 503",
			method:     http.MethodGet,
			catalog:    nil,
			wantStatus: http.StatusServiceUnavailable,
			wantErr:    "model_catalog_unavailable",
		},
		{
			name:   "nil models slice marshals as empty array",
			method: http.MethodGet,
			catalog: &fakeCatalog{
				resp: ModelsResponse{
					Models: nil,
					Defaults: ModelDefaults{
						ChatChain:    []string{},
						CompactChain: []string{},
					},
				},
			},
			wantStatus: http.StatusOK,
			checkBody: func(t *testing.T, raw []byte) {
				t.Helper()
				if !bytes.Contains(raw, []byte(`"models":[]`)) {
					t.Errorf("raw JSON %s does not contain %q", string(raw), `"models":[]`)
				}
				var got ModelsResponse
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(got.Models) != 0 {
					t.Errorf("models len = %d, want 0", len(got.Models))
				}
			},
		},
		{
			name:       "POST /v1/models 405",
			method:     http.MethodPost,
			catalog:    populated,
			wantStatus: http.StatusMethodNotAllowed,
			wantErr:    "method not allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{
				Store:    store,
				Sessions: &fakeSessionSource{},
				Hub:      &fakeHub{},
				Models:   tc.catalog,
			}
			req := httptest.NewRequest(tc.method, "/v1/models", nil)
			w := httptest.NewRecorder()
			srv.dispatch(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantErr != "" {
				var errBody map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
					t.Fatalf("unmarshal error body: %v", err)
				}
				if errBody["error"] != tc.wantErr {
					t.Errorf("error = %q, want %q", errBody["error"], tc.wantErr)
				}
			}
			if tc.checkBody != nil {
				tc.checkBody(t, w.Body.Bytes())
			}
		})
	}
}

type trackingSessionSource struct {
	fakeSessionSource

	createCalls int
	lastPin     string
}

func (f *trackingSessionSource) Create(title, modelPin string) (SessionInfo, error) {
	f.createCalls++
	f.lastPin = modelPin
	return f.fakeSessionSource.Create(title, modelPin)
}

func TestCreateSessionModelPinValidation(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	cat := sampleCatalog()

	tests := []struct {
		name        string
		catalog     ModelCatalog
		body        string
		wantStatus  int
		wantErr     string
		wantCreated int
		wantPin     string
	}{
		{
			name:        "unknown model_pin rejected 400",
			catalog:     cat,
			body:        `{"title":"Test","model_pin":"unknown/model"}`,
			wantStatus:  http.StatusBadRequest,
			wantErr:     "invalid_model_pin",
			wantCreated: 0,
		},
		{
			name:        "no_key model_pin rejected 400",
			catalog:     cat,
			body:        `{"title":"Test","model_pin":"openai/gpt-4o"}`,
			wantStatus:  http.StatusBadRequest,
			wantErr:     "invalid_model_pin",
			wantCreated: 0,
		},
		{
			name:        "nil catalog with model_pin rejected 503",
			catalog:     nil,
			body:        `{"title":"Test","model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantErr:     "model_catalog_unavailable",
			wantCreated: 0,
		},
		{
			name:        "valid online model_pin accepted 201",
			catalog:     cat,
			body:        `{"title":"Test","model_pin":"openrouter/anthropic/claude-3.5-sonnet"}`,
			wantStatus:  http.StatusCreated,
			wantCreated: 1,
			wantPin:     "openrouter/anthropic/claude-3.5-sonnet",
		},
		{
			name:        "valid offline model_pin accepted 201",
			catalog:     cat,
			body:        `{"title":"Test","model_pin":"local/qwen2.5-coder-7b"}`,
			wantStatus:  http.StatusCreated,
			wantCreated: 1,
			wantPin:     "local/qwen2.5-coder-7b",
		},
		{
			name:        "no model_pin with nil catalog unchanged 201",
			catalog:     nil,
			body:        `{"title":"No Pin"}`,
			wantStatus:  http.StatusCreated,
			wantCreated: 1,
		},
		{
			name:        "no model_pin with catalog unchanged 201",
			catalog:     cat,
			body:        `{"title":"No Pin"}`,
			wantStatus:  http.StatusCreated,
			wantCreated: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &trackingSessionSource{}
			srv := &Server{
				Store:    store,
				Sessions: sessions,
				Hub:      &fakeHub{},
				Models:   tc.catalog,
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			srv.dispatch(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if sessions.createCalls != tc.wantCreated {
				t.Errorf("createCalls = %d, want %d", sessions.createCalls, tc.wantCreated)
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
			var got SessionInfo
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal SessionInfo: %v", err)
			}
			if got.ID == "" {
				t.Errorf("empty session ID")
			}
			if sessions.lastPin != tc.wantPin {
				t.Errorf("created model_pin = %q, want %q", sessions.lastPin, tc.wantPin)
			}
		})
	}
}
