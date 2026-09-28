package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseHandler(t *testing.T, chunks []string, trailingJunk bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if trailingJunk {
			fmt.Fprint(w, "this is not an sse line at all\n")
			fmt.Fprint(w, "data: {not json\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

func TestStreamReassemblesSplitToolCall(t *testing.T) {
	chunks := []string{
		`{"model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q"}}]}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\":\"ro"}}]}}]}`,
		`{"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"me\"}"}}]}}]}`,
	}
	srv := httptest.NewServer(sseHandler(t, chunks, true))
	defer srv.Close()

	r := NewRouter(Profile{Chain: []Target{{Provider: NewOpenAI(srv.URL+"/v1", "", "m")}}})
	var got strings.Builder
	c, err := r.CompleteStream(context.Background(), "chat", nil, Options{}, func(d string) error {
		got.WriteString(d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "Hello" || c.Text != "Hello" {
		t.Fatalf("stream text = %q / %q", got.String(), c.Text)
	}
	if len(c.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d: %+v", len(c.ToolCalls), c.ToolCalls)
	}
	tc := c.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "lookup" || tc.ArgsJSON != `{"q":"rome"}` {
		t.Fatalf("bad merged tool call: %+v", tc)
	}
}

func TestStreamTwoToolCallsInIndexOrder(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"second","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"first","arguments":"{}"}}]}}]}`,
	}
	srv := httptest.NewServer(sseHandler(t, chunks, false))
	defer srv.Close()
	r := NewRouter(Profile{Chain: []Target{{Provider: NewOpenAI(srv.URL+"/v1", "", "m")}}})
	c, err := r.CompleteStream(context.Background(), "chat", nil, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ToolCalls) != 2 || c.ToolCalls[0].Name != "first" || c.ToolCalls[1].Name != "second" {
		t.Fatalf("index order broken: %+v", c.ToolCalls)
	}
}
