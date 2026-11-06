package surface

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

func TestNewServer(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}, Cfg: ServeConfig{AllowedHosts: []string{"example.com"}}}

	mux := NewServer(srv)

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req = req.WithContext(WithLocalAddr(req.Context(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7717}))
	req.Host = "localhost:7717"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestServerTimeouts(t *testing.T) {
	srv := &Server{}
	httpSrv := srv.newHTTPServer(nil)

	if httpSrv.ReadTimeout != 60*time.Second {
		t.Errorf("ReadTimeout = %v, want 60s (M17)", httpSrv.ReadTimeout)
	}
	if httpSrv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s (M17)", httpSrv.ReadHeaderTimeout)
	}
	if httpSrv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 to avoid breaking SSE streams (M17)", httpSrv.WriteTimeout)
	}
	if httpSrv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", httpSrv.IdleTimeout)
	}
}

func TestServerListenAndServeNormalizeError(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{Listen: "0.0.0.0:7717"}, // non-loopback, no allowed_hosts
	}

	err := srv.ListenAndServe()
	if err == nil {
		t.Errorf("expected error for non-loopback without allowed_hosts, got nil")
	}
}

// Regression: an unmatched path must 404 through the full chain — the
// first wiring registered the mux inside itself and stack-overflowed.
func TestUnknownRouteJSON404(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}
	h := NewServer(srv)
	for _, path := range []string{"/nope", "/v2/things", "/app.txt"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(WithLocalAddr(req.Context(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7717}))
		req.Host = "127.0.0.1:7717"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s content-type = %q, want application/json", path, ct)
		}
	}
}

// Regression: unknown-but-well-formed session must 404, not 200-empty.
func TestEventsUnknownSession404(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenTokenStore(dir + "/tokens.json")
	src := NewPenatusSource(dir, &fakeHub{})
	srv := &Server{Store: store, Sessions: src, Hub: &fakeHub{}}
	h := NewServer(srv)

	// mint a token so auth passes
	tok, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/s_AAAAAAAAAAAAAAAAAAAAAAAAAA/events", nil)
	req = req.WithContext(WithLocalAddr(req.Context(), &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7717}))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown session events = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

func TestListenAndServe_LateRegisterAfterSignal(t *testing.T) {
	dir := t.TempDir()

	// 1. Setup session and penatus log
	_, err := penatus.CreateSession(dir, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	mainDir := dir + "/sessions/main"
	log, err := penatus.OpenLog(mainDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = log.Append(penatus.Event{
		T:  "msg",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"role":  raw(t, "assistant"),
			"text":  raw(t, "system prompt"),
			"model": raw(t, "test"),
			"usage": mustJSON(t, map[string]int{"in": 10, "out": 5}),
		},
	})

	// 2. Setup stub router gated with channels (never time.Sleep)
	routerInvoked := make(chan struct{})
	proceedRouter := make(chan struct{})

	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			if n == 1 {
				// Gate the stub router with channels
				routerInvoked <- struct{}{}
				<-proceedRouter
				return &router.Completion{
					ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"late"}`}},
				}, nil
			}
			return &router.Completion{
				Text: "Tool was refused.",
			}, nil
		},
	}

	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})

	// 3. Setup untrusted tool to trigger approval
	tools := []loop.Tool{
		{
			Spec: router.ToolSpec{
				Name:             "echo",
				Description:      "Echo tool",
				ParamsJSONSchema: `{"type":"object","properties":{"v":{"type":"string"}}}`,
			},
			Trusted: false,
			Run: func(ctx context.Context, args string) (string, error) {
				return "ECHO:" + args, nil
			},
		},
	}

	cfg := ServeConfig{
		Listen:          "127.0.0.1:0",
		ApprovalTimeout: 5 * time.Second,
		TurnTimeout:     10 * time.Second,
	}

	ap := NewApprovalHub(cfg.ApprovalTimeout)
	hub := NewHub(cfg, dir, 1000, 80, tools, rt, ap)
	store, err := OpenTokenStore(dir + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewPenatusSource(dir, hub)

	srv := &Server{
		Cfg:         cfg,
		Store:       store,
		Sessions:    sessions,
		Hub:         hub,
		ShutdownSig: make(chan os.Signal, 1),
		ListenReady: make(chan struct{}),
	}
	srv.SetApprovalHub(ap)

	// Hold Drain() open with an in-flight verdictWg waiter so the TCP listener stays
	// open after Hub.Shutdown() while Drain() is executing (reproducing the #259 window).
	releaseVerdict := make(chan struct{})
	srv.verdictWg.Add(1)
	go func() {
		<-releaseVerdict
		srv.verdictWg.Done()
	}()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.ListenAndServe()
	}()

	// Wait for server to bind and start listening
	select {
	case <-srv.ListenReady:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not bind within timeout")
	}

	listenAddr := srv.Cfg.Listen

	// Send SIGTERM-like shutdown via the server's shutdown entry
	srv.ShutdownSig <- syscall.SIGTERM

	// Wait until ApprovalHub is shut down and latched closed (channel-driven)
	select {
	case <-ap.ClosedNotify():
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not close within timeout")
	}

	// At this point, Hub.Shutdown() has run, the latch is set, and Server.Drain()
	// is blocked waiting on verdictWg. The TCP listener is still open.
	// Race a tool-approval POST through after hub close:
	postDone := make(chan struct{})
	var postStatusCode int
	var postBodyBytes []byte
	var postErr error
	go func() {
		defer close(postDone)
		req, reqErr := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
		if reqErr != nil {
			postErr = reqErr
			return
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			postErr = err
			return
		}
		defer resp.Body.Close()
		postStatusCode = resp.StatusCode
		postBodyBytes, postErr = io.ReadAll(resp.Body)
	}()

	// Synchronize with stub router: verify request is received and waiting for model
	select {
	case <-routerInvoked:
	case <-time.After(5 * time.Second):
		t.Fatal("router was not invoked")
	}

	// Allow stub router to return the tool call
	close(proceedRouter)

	// Wait for POST response
	select {
	case <-postDone:
	case <-time.After(5 * time.Second):
		t.Fatal("POST did not complete")
	}
	if postErr != nil {
		t.Fatalf("POST failed: %v", postErr)
	}

	if postStatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", postStatusCode)
	}

	frames := parseSSEFrames(string(postBodyBytes))

	// Assert tool settled as denied
	var sawToolDone bool
	for _, f := range frames {
		if f["event"] == "tool_done" {
			sawToolDone = true
			data, _ := f["data"].(map[string]any)
			if ok, _ := data["ok"].(bool); ok {
				t.Fatalf("tool_done ok = true, want false (denied)")
			}
			approvalStr, _ := data["approval"].(string)
			if !strings.HasPrefix(approvalStr, "denied:") {
				t.Fatalf("approval = %q, want denied prefix", approvalStr)
			}
		}
	}
	if !sawToolDone {
		t.Fatal("did not see tool_done event in SSE frames")
	}

	// Release verdictWg waiter so Drain() can complete
	close(releaseVerdict)

	// Assert ListenAndServe returns cleanly (Drain finished, srv.Shutdown finished)
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("ListenAndServe error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Drain unblocked")
	}

	// Assert no outstanding waiters or cards in hub
	if pending := ap.Pending(); len(pending) != 0 {
		t.Fatalf("hub pending cards = %d, want 0", len(pending))
	}
	if cards := ap.PendingTurnCards(); len(cards) != 0 {
		t.Fatalf("hub pending turn cards = %d, want 0", len(cards))
	}
}
