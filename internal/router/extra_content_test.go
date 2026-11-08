package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestExtraContentNonStreamRoundTrip(t *testing.T) {
	tests := []struct {
		name         string
		responseJSON string
		wantExtra    string
	}{
		{
			name: "assistant message extra_content",
			responseJSON: `{
				"model": "gemini-3-flash",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [{
							"id": "call_ns_1",
							"type": "function",
							"function": {"name": "fetch", "arguments": "{\"k\":\"v\"}"}
						}],
						"extra_content": {"google": {"thought_signature": "dGVzdF9zaWduYXR1cmVfMQ=="}}
					},
					"finish_reason": "tool_calls"
				}]
			}`,
			wantExtra: `{"google":{"thought_signature":"dGVzdF9zaWduYXR1cmVfMQ=="}}`,
		},
		{
			name: "tool_call chunk level extra_content reassembled at message level",
			responseJSON: `{
				"model": "gemini-3-pro",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [{
							"id": "call_ns_2",
							"type": "function",
							"function": {"name": "lookup", "arguments": "{}"},
							"extra_content": {"google": {"thought_signature": "dGVzdF9zaWduYXR1cmVfMg=="}}
						}],
						"extra_content": null
					},
					"finish_reason": "tool_calls"
				}]
			}`,
			wantExtra: `{"google":{"thought_signature":"dGVzdF9zaWduYXR1cmVfMg=="}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var recordedRequests [][]byte

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				mu.Lock()
				recordedRequests = append(recordedRequests, body)
				reqIdx := len(recordedRequests)
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				if reqIdx == 1 {
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, tc.responseJSON)
				} else {
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"finished"}}]}`)
				}
			}))
			defer srv.Close()

			prov := NewOpenAI(srv.URL+"/v1", "key", "gemini-test")
			comp, err := prov.Complete(context.Background(), []Message{{Role: RoleUser, Content: "start"}}, Options{})
			if err != nil {
				t.Fatalf("first request failed: %v", err)
			}

			// Assert completion carries extra_content byte-identical to expected compact JSON.
			var wantBuf bytes.Buffer
			if err := json.Compact(&wantBuf, []byte(tc.wantExtra)); err != nil {
				t.Fatalf("compact wantExtra: %v", err)
			}
			var gotCompBuf bytes.Buffer
			if err := json.Compact(&gotCompBuf, comp.ExtraContent); err != nil {
				t.Fatalf("compact comp.ExtraContent: %v", err)
			}
			if !bytes.Equal(gotCompBuf.Bytes(), wantBuf.Bytes()) {
				t.Fatalf("comp.ExtraContent = %s, want %s", gotCompBuf.Bytes(), wantBuf.Bytes())
			}

			// Second request re-sending assistant message with extra_content.
			followUp := []Message{
				{Role: RoleUser, Content: "start"},
				{Role: RoleAssistant, ToolCalls: comp.ToolCalls, ExtraContent: comp.ExtraContent},
				{Role: RoleTool, ToolCallID: comp.ToolCalls[0].ID, Content: `{"result":"ok"}`},
			}
			_, err = prov.Complete(context.Background(), followUp, Options{})
			if err != nil {
				t.Fatalf("second request failed: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(recordedRequests) < 2 {
				t.Fatalf("expected at least 2 requests, got %d", len(recordedRequests))
			}

			var reqObj struct {
				Messages []struct {
					Role         string          `json:"role"`
					ExtraContent json.RawMessage `json:"extra_content"`
					ToolCalls    []struct {
						ID           string          `json:"id"`
						ExtraContent json.RawMessage `json:"extra_content"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(recordedRequests[1], &reqObj); err != nil {
				t.Fatalf("unmarshal second request: %v", err)
			}

			found := false
			for _, m := range reqObj.Messages {
				if m.Role == "assistant" {
					found = true
					var gotReqBuf bytes.Buffer
					if err := json.Compact(&gotReqBuf, m.ExtraContent); err != nil {
						t.Fatalf("compact second request extra_content: %v", err)
					}
					if !bytes.Equal(gotReqBuf.Bytes(), wantBuf.Bytes()) {
						t.Fatalf("second request assistant extra_content = %s, want %s", gotReqBuf.Bytes(), wantBuf.Bytes())
					}
					// The live Gemini compat endpoint only honors the
					// signature inside tool_calls[] entries (message-level
					// alone returns 400 "missing thought_signature" —
					// verified against the real API 2026-11-07). Pin it.
					if len(m.ToolCalls) == 0 {
						t.Fatal("assistant message carries no tool_calls in second request")
					}
					for _, tc := range m.ToolCalls {
						var tcBuf bytes.Buffer
						if err := json.Compact(&tcBuf, tc.ExtraContent); err != nil {
							t.Fatalf("compact tool_call extra_content: %v", err)
						}
						if !bytes.Equal(tcBuf.Bytes(), wantBuf.Bytes()) {
							t.Fatalf("second request tool_calls[%s].extra_content = %s, want %s", tc.ID, tcBuf.Bytes(), wantBuf.Bytes())
						}
					}
				}
			}
			if !found {
				t.Fatal("assistant message not found in second request")
			}
		})
	}
}

func TestExtraContentStreamRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		chunks    []string
		wantExtra string
	}{
		{
			name: "extra_content arrives in later chunk than tool_call",
			chunks: []string{
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s_1","type":"function","function":{"name":"fetch","arguments":"{}"}}]}}]}`,
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"extra_content":{"google":{"thought_signature":"c3RyZWFtX3NpZ19sYXRlcg=="}}},"finish_reason":"tool_calls"}]}`,
			},
			wantExtra: `{"google":{"thought_signature":"c3RyZWFtX3NpZ19sYXRlcg=="}}`,
		},
		{
			name: "extra_content in same chunk as tool_call",
			chunks: []string{
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s_2","type":"function","function":{"name":"search","arguments":"{}"}}],"extra_content":{"google":{"thought_signature":"c3RyZWFtX3NpZ19zYW1l"}}},"finish_reason":"tool_calls"}]}`,
			},
			wantExtra: `{"google":{"thought_signature":"c3RyZWFtX3NpZ19zYW1l"}}`,
		},
		{
			name: "extra_content inside tool_call delta chunk",
			chunks: []string{
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s_3","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"c3RyZWFtX3NpZ19pbnNpZGU="}}}]},"finish_reason":"tool_calls"}]}`,
			},
			wantExtra: `{"google":{"thought_signature":"c3RyZWFtX3NpZ19pbnNpZGU="}}`,
		},
		{
			name: "split deltas with multiple extra_content chunks (last non-empty wins)",
			chunks: []string{
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s_4","type":"function","function":{"name":"scan","arguments":""}}]}}]}`,
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"extra_content":{"google":{"thought_signature":"ZWFybHlfc2ln"}}}},{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
				`{"model":"gemini-3-flash","choices":[{"index":0,"delta":{"extra_content":{"google":{"thought_signature":"ZmluYWxfc2ln"}}},"finish_reason":"tool_calls"}]}`,
			},
			wantExtra: `{"google":{"thought_signature":"ZmluYWxfc2ln"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var recordedRequests [][]byte

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				mu.Lock()
				recordedRequests = append(recordedRequests, body)
				reqIdx := len(recordedRequests)
				mu.Unlock()

				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				if reqIdx == 1 {
					for _, c := range tc.chunks {
						fmt.Fprintf(w, "data: %s\n\n", c)
						if flusher != nil {
							flusher.Flush()
						}
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
				} else {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer srv.Close()

			r := NewRouter(Profile{Chain: []Target{{Provider: NewOpenAI(srv.URL+"/v1", "key", "gemini-test")}}})
			comp, err := r.CompleteStream(context.Background(), "chat", []Message{{Role: RoleUser, Content: "start"}}, Options{}, nil)
			if err != nil {
				t.Fatalf("first stream request failed: %v", err)
			}

			var wantBuf bytes.Buffer
			if err := json.Compact(&wantBuf, []byte(tc.wantExtra)); err != nil {
				t.Fatalf("compact wantExtra: %v", err)
			}
			var gotCompBuf bytes.Buffer
			if err := json.Compact(&gotCompBuf, comp.ExtraContent); err != nil {
				t.Fatalf("compact comp.ExtraContent: %v", err)
			}
			if !bytes.Equal(gotCompBuf.Bytes(), wantBuf.Bytes()) {
				t.Fatalf("comp.ExtraContent = %s, want %s", gotCompBuf.Bytes(), wantBuf.Bytes())
			}

			// Second request re-sending assistant message.
			followUp := []Message{
				{Role: RoleUser, Content: "start"},
				{Role: RoleAssistant, ToolCalls: comp.ToolCalls, ExtraContent: comp.ExtraContent},
				{Role: RoleTool, ToolCallID: comp.ToolCalls[0].ID, Content: `{"result":"ok"}`},
			}
			_, err = r.CompleteStream(context.Background(), "chat", followUp, Options{}, nil)
			if err != nil {
				t.Fatalf("second stream request failed: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(recordedRequests) < 2 {
				t.Fatalf("expected at least 2 requests, got %d", len(recordedRequests))
			}

			var reqObj struct {
				Messages []struct {
					Role         string          `json:"role"`
					ExtraContent json.RawMessage `json:"extra_content"`
					ToolCalls    []struct {
						ID           string          `json:"id"`
						ExtraContent json.RawMessage `json:"extra_content"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(recordedRequests[1], &reqObj); err != nil {
				t.Fatalf("unmarshal second request: %v", err)
			}

			found := false
			for _, m := range reqObj.Messages {
				if m.Role == "assistant" {
					found = true
					var gotReqBuf bytes.Buffer
					if err := json.Compact(&gotReqBuf, m.ExtraContent); err != nil {
						t.Fatalf("compact second request extra_content: %v", err)
					}
					if !bytes.Equal(gotReqBuf.Bytes(), wantBuf.Bytes()) {
						t.Fatalf("second request assistant extra_content = %s, want %s", gotReqBuf.Bytes(), wantBuf.Bytes())
					}
					// The live Gemini compat endpoint only honors the
					// signature inside tool_calls[] entries (message-level
					// alone returns 400 "missing thought_signature" —
					// verified against the real API 2026-11-07). Pin it.
					if len(m.ToolCalls) == 0 {
						t.Fatal("assistant message carries no tool_calls in second request")
					}
					for _, tc := range m.ToolCalls {
						var tcBuf bytes.Buffer
						if err := json.Compact(&tcBuf, tc.ExtraContent); err != nil {
							t.Fatalf("compact tool_call extra_content: %v", err)
						}
						if !bytes.Equal(tcBuf.Bytes(), wantBuf.Bytes()) {
							t.Fatalf("second request tool_calls[%s].extra_content = %s, want %s", tc.ID, tcBuf.Bytes(), wantBuf.Bytes())
						}
					}
				}
			}
			if !found {
				t.Fatal("assistant message not found in second request")
			}
		})
	}
}

func TestNoExtraContentOmittedFromWire(t *testing.T) {
	tests := []struct {
		name     string
		isStream bool
	}{
		{name: "non-stream llama.cpp shape", isStream: false},
		{name: "stream llama.cpp shape", isStream: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recordedBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				recordedBody = b

				if tc.isStream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
				}
			}))
			defer srv.Close()

			prov := NewOpenAI(srv.URL+"/v1", "", "local-model")
			msgs := []Message{
				{Role: RoleSystem, Content: "system prompt"},
				{Role: RoleUser, Content: "user query"},
				{
					Role: RoleAssistant,
					ToolCalls: []ToolCall{{
						ID:       "c_1",
						Name:     "echo",
						ArgsJSON: `{"q":"test"}`,
					}},
				},
				{Role: RoleTool, ToolCallID: "c_1", Content: "tool response"},
			}

			if tc.isStream {
				r := NewRouter(Profile{Chain: []Target{{Provider: prov}}})
				_, err := r.CompleteStream(context.Background(), "chat", msgs, Options{}, nil)
				if err != nil {
					t.Fatalf("stream call: %v", err)
				}
			} else {
				_, err := prov.Complete(context.Background(), msgs, Options{})
				if err != nil {
					t.Fatalf("non-stream call: %v", err)
				}
			}

			if bytes.Contains(recordedBody, []byte("extra_content")) {
				t.Fatalf("expected serialized request to NOT contain 'extra_content', but got:\n%s", string(recordedBody))
			}
		})
	}
}
