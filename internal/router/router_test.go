package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

func jsonHandler(t *testing.T, status int, body string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

const okBody = `{"model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`

type countingFail struct{ n int32 }

func (c *countingFail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&c.n, 1)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, `{"error":"boom"}`)
}

// ---- openai.go -----------------------------------------------------------

func TestCompleteHappyPathUsage(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, 200, okBody))
	defer srv.Close()
	p := NewOpenAI(srv.URL+"/v1", "key", "m1")
	c, err := p.Complete(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != "hello" || c.Model != "m1" || c.InTokens != 11 || c.OutTokens != 5 || !c.Probed {
		t.Fatalf("bad completion: %+v", c)
	}
}

func TestCompleteNoUsageEstimates(t *testing.T) {
	noUsage := `{"model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"abcdefgh"}}]}`
	srv := httptest.NewServer(jsonHandler(t, 200, noUsage))
	defer srv.Close()
	c, err := NewOpenAI(srv.URL+"/v1", "", "").Complete(context.Background(), nil, Options{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Probed {
		t.Fatal("Probed must be false without usage object")
	}
	if c.OutTokens != 2 { // 8 chars / 4
		t.Fatalf("estimate = %d, want 2", c.OutTokens)
	}
}

func TestCompleteToolCallRoundTrip(t *testing.T) {
	// Server echoes back the request so we can verify wire format.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		got := string(b[:n])
		// parameters must be a JSON object, tools nested under function
		for _, want := range []string{`"parameters":{"type":"object"}`, `"type":"function"`, `"name":"get_weather"`} {
			if !strings.Contains(got, want) {
				t.Errorf("request body missing %s: %s", want, got)
			}
		}
		fmt.Fprint(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}]}}]}`)
	}))
	defer srv.Close()
	c, err := NewOpenAI(srv.URL+"/v1", "", "").Complete(context.Background(),
		[]Message{{Role: RoleUser, Content: "weather?"}},
		Options{Tools: []ToolSpec{{Name: "get_weather", Description: "d", ParamsJSONSchema: `{"type":"object"}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].ID != "c1" || c.ToolCalls[0].Name != "get_weather" || c.ToolCalls[0].ArgsJSON != `{"city":"Rome"}` {
		t.Fatalf("bad tool calls: %+v", c.ToolCalls)
	}
}

func TestHTTPErrorsIncludeStatusAndCappedBody(t *testing.T) {
	long := `{"error":"` + strings.Repeat("x", 5000) + `"}`
	srv := httptest.NewServer(jsonHandler(t, 400, long))
	defer srv.Close()
	_, err := NewOpenAI(srv.URL+"/v1", "", "").Complete(context.Background(), nil, Options{})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err lacks status: %v", err)
	}
	if len(err.Error()) > 600 {
		t.Fatalf("body not capped at 400 bytes: len=%d", len(err.Error()))
	}
}

// ---- router.go: failover classification ----------------------------------

func TestFailoverRetryableThenSuccess(t *testing.T) {
	fail := &countingFail{}
	fsrv := httptest.NewServer(fail)
	defer fsrv.Close()
	osrv := httptest.NewServer(jsonHandler(t, 200, okBody))
	defer osrv.Close()

	r := NewRouter(Profile{Chain: []Target{
		{Provider: NewOpenAI(fsrv.URL+"/v1", "", "m"), Model: "m"},
		{Provider: NewOpenAI(osrv.URL+"/v1", "", "m"), Model: "m1"},
	}})
	c, err := r.Complete(context.Background(), "chat", nil, Options{MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != "hello" {
		t.Fatalf("wrong text %q", c.Text)
	}
	if n := atomic.LoadInt32(&fail.n); n != 3 {
		t.Fatalf("expected 3 attempts on dead target, got %d", n)
	}
}

func TestNonRetryableAdvancesWithZeroRetries(t *testing.T) {
	var n1 int32
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n1, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"error":"bad schema"}`)
	}))
	defer s1.Close()
	s2 := httptest.NewServer(jsonHandler(t, 200, okBody))
	defer s2.Close()

	r := NewRouter(Profile{Chain: []Target{
		{Provider: NewOpenAI(s1.URL+"/v1", "", "m")},
		{Provider: NewOpenAI(s2.URL+"/v1", "", "m1")},
	}})
	if _, err := r.Complete(context.Background(), "chat", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&n1); n != 1 {
		t.Fatalf("4xx must advance with exactly 1 request, got %d", n)
	}
}

func TestChainErrorListsAllLegs(t *testing.T) {
	s1 := httptest.NewServer(jsonHandler(t, 400, `{"e":"one"}`))
	defer s1.Close()
	s2 := httptest.NewServer(jsonHandler(t, 401, `{"e":"two"}`))
	defer s2.Close()
	r := NewRouter(Profile{Chain: []Target{
		{Provider: NewOpenAI(s1.URL+"/v1", "", "ma"), Model: "ma"},
		{Provider: NewOpenAI(s2.URL+"/v1", "", "mb"), Model: "mb"},
	}})
	_, err := r.Complete(context.Background(), "chat", nil, Options{})
	var ce *ChainError
	ok := errors.As(err, &ce)
	if !ok {
		t.Fatalf("want *ChainError, got %T", err)
	}
	if len(ce.Legs) != 2 {
		t.Fatalf("want 2 legs, got %d", len(ce.Legs))
	}
	msg := ce.Error()
	if !strings.Contains(msg, "HTTP 400") || !strings.Contains(msg, "HTTP 401") {
		t.Fatalf("chain error missing legs: %s", msg)
	}
}

func TestCallerCancelAbortsWithoutFailover(t *testing.T) {
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer block.Close()
	var hit2 int32
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit2, 1)
	}))
	defer s2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r := NewRouter(Profile{Chain: []Target{
		{Provider: NewOpenAI(block.URL+"/v1", "", "m")},
		{Provider: NewOpenAI(s2.URL+"/v1", "", "m")},
	}})
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := r.Complete(ctx, "chat", nil, Options{})
	if err == nil {
		t.Fatal("want error on cancel")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("failover happened despite caller cancel")
	}
	if atomic.LoadInt32(&hit2) != 0 {
		t.Fatal("second target must not be tried after caller cancel")
	}
}
