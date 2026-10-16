package custos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

type testAuditSink struct {
	mu      sync.Mutex
	records []custos.AuditRecord
}

func (s *testAuditSink) Append(rec custos.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return nil
}

func (s *testAuditSink) Records() []custos.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]custos.AuditRecord(nil), s.records...)
}

// TestG2d_HTTPConnectAndList verifies:
// httptest SDK server, bearer from ${env:} -> ready, tools listed,
// Authorization header observed exactly once per request with correct value;
// needle set includes bearer (assert via scrubbed result echo).
func TestG2d_HTTPConnectAndList(t *testing.T) {
	bearerToken := "super-secret-bearer-test-token-12345"
	t.Setenv("TEST_G2D_BEARER", bearerToken)

	var authMu sync.Mutex
	var authHeaders [][]string

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-http-server",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_secret",
		Description: "echoes bearer token for testing scrubbing",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "The secret is " + bearerToken + " inside content."},
			},
		}, nil, nil
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		authHeaders = append(authHeaders, r.Header.Values("Authorization"))
		authMu.Unlock()
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	cfg := custos.MCPServerConfig{ //nolint:gosec // test server configuration
		Name:      "alpha-http",
		Transport: "http",
		URL:       ts.URL,
		Bearer:    "${env:TEST_G2D_BEARER}",
	}

	conn := custos.NewMCPConnector(cfg, t.TempDir())
	if err := conn.OnFirstUnlock(context.Background()); err != nil {
		t.Fatalf("OnFirstUnlock failed: %v", err)
	}

	st := conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("expected state ready, got %q (reason=%s)", st.State, st.Reason)
	}
	if st.Reason != custos.MCPReasonOK {
		t.Fatalf("expected reason ok, got %q", st.Reason)
	}
	if st.Tools != 1 {
		t.Fatalf("expected 1 tool, got %d", st.Tools)
	}
	if !conn.HasTool("get_secret") {
		t.Fatalf("expected get_secret tool listed")
	}

	// Verify Authorization header observed exactly once per request with correct value
	authMu.Lock()
	if len(authHeaders) == 0 {
		t.Fatalf("expected at least one HTTP request recorded")
	}
	for i, vals := range authHeaders {
		if len(vals) != 1 {
			t.Errorf("request %d: expected exactly 1 Authorization header, got %d: %v", i, len(vals), vals)
		} else if vals[0] != "Bearer "+bearerToken {
			t.Errorf("request %d: expected Authorization: Bearer %s, got %s", i, bearerToken, vals[0])
		}
	}
	authMu.Unlock()

	// Assert needle set includes bearer via scrubbed result echo
	res, err := conn.Execute(context.Background(), "get_secret", json.RawMessage("{}"), custos.Credential{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	resStr := string(res)
	if strings.Contains(resStr, bearerToken) {
		t.Fatalf("result contains unscrubbed bearer token: %s", resStr)
	}
	if !strings.Contains(resStr, "[redacted]") {
		t.Fatalf("expected [redacted] in result: %s", resStr)
	}
}

// TestG2d_AuthFailedTerminal verifies:
// server returns 401 -> disabled/auth_failed immediately (no backoff timer scheduled);
// an automatic refresh (list-changed path / retry timer) cannot revive it; ManualRefresh CAN retry it.
func TestG2d_AuthFailedTerminal(t *testing.T) {
	var allowAuth atomic.Bool
	allowAuth.Store(false)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-auth-server",
		Version: "1.0.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "auth_tool",
		Description: "auth tool",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
		}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowAuth.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Unauthorized"))
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	cfg := custos.MCPServerConfig{
		Name:      "auth-mcp",
		Transport: "http",
		URL:       ts.URL,
		Bearer:    "some-bearer-token",
	}

	conn := custos.NewMCPConnector(cfg, t.TempDir())
	_ = conn.OnFirstUnlock(context.Background())

	// Assert: disabled/auth_failed immediately, no backoff timer scheduled
	st := conn.Status()
	if st.State != custos.MCPStateDisabled {
		t.Fatalf("expected state disabled, got %q", st.State)
	}
	if st.Reason != custos.MCPReasonAuthFailed {
		t.Fatalf("expected reason auth_failed, got %q", st.Reason)
	}
	if conn.HasRetryTimerForTest() {
		t.Fatalf("expected no retry timer scheduled on auth_failed")
	}

	// Automatic refresh cannot revive it
	if err := conn.Refresh(context.Background()); err != nil {
		t.Errorf("Refresh returned unexpected error: %v", err)
	}
	st = conn.Status()
	if st.State != custos.MCPStateDisabled || st.Reason != custos.MCPReasonAuthFailed {
		t.Fatalf("automatic refresh revived disabled/auth_failed: %+v", st)
	}

	// Schedule debounced refresh (list-changed path) cannot revive it
	conn.ScheduleDebouncedRefreshForTest()
	time.Sleep(50 * time.Millisecond)
	st = conn.Status()
	if st.State != custos.MCPStateDisabled || st.Reason != custos.MCPReasonAuthFailed {
		t.Fatalf("debounced refresh revived disabled/auth_failed: %+v", st)
	}

	// Now fix server credentials and assert ManualRefresh CAN retry it
	allowAuth.Store(true)
	if err := conn.ManualRefresh(context.Background()); err != nil {
		t.Fatalf("ManualRefresh failed: %v", err)
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("expected state ready after ManualRefresh, got %q (reason=%s)", st.State, st.Reason)
	}
	if st.Reason != custos.MCPReasonOK {
		t.Fatalf("expected reason ok after ManualRefresh, got %q", st.Reason)
	}
}

// TestG2d_ThreeStrikes verifies:
// server that drops connections -> ready survives 1 and 2 consecutive failures,
// enters backoff on the 3rd with reason transport; after one successful request
// the counter resets (2 more failures keep ready).
func TestG2d_ThreeStrikes(t *testing.T) {
	var dropConns atomic.Bool
	dropConns.Store(false)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-strike-server",
		Version: "1.0.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ping",
		Description: "ping tool",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong"}},
		}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropConns.Load() {
			hj, ok := w.(http.Hijacker)
			if ok {
				c, _, _ := hj.Hijack()
				_ = c.Close()
				return
			}
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	cfg := custos.MCPServerConfig{
		Name:      "three-strikes-conn",
		Transport: "http",
		URL:       ts.URL,
	}

	conn := custos.NewMCPConnector(cfg, t.TempDir())
	if err := conn.OnFirstUnlock(context.Background()); err != nil {
		t.Fatalf("OnFirstUnlock failed: %v", err)
	}

	st := conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("expected state ready, got %q", st.State)
	}

	// Turn on dropping connections
	dropConns.Store(true)

	// Strike 1: failure 1 while ready -> survives in ready
	_, err := conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err == nil {
		t.Fatalf("expected error on dropped connection")
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("strike 1: expected state ready, got %q", st.State)
	}
	if conn.ConsecutiveFailuresForTest() != 1 {
		t.Fatalf("strike 1: expected consecutive failures 1, got %d", conn.ConsecutiveFailuresForTest())
	}

	// Strike 2: failure 2 while ready -> survives in ready
	_, err = conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err == nil {
		t.Fatalf("expected error on dropped connection")
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("strike 2: expected state ready, got %q", st.State)
	}
	if conn.ConsecutiveFailuresForTest() != 2 {
		t.Fatalf("strike 2: expected consecutive failures 2, got %d", conn.ConsecutiveFailuresForTest())
	}

	// Recover: successful request resets counter
	dropConns.Store(false)
	res, err := conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err != nil {
		t.Fatalf("expected success after recovery, got %v", err)
	}
	if !strings.Contains(string(res), "pong") {
		t.Fatalf("unexpected result: %s", string(res))
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("expected state ready after success, got %q", st.State)
	}
	if conn.ConsecutiveFailuresForTest() != 0 {
		t.Fatalf("expected consecutive failures reset to 0, got %d", conn.ConsecutiveFailuresForTest())
	}

	// Now drop connections again: 2 more failures keep ready
	dropConns.Store(true)

	// Failure 1 after reset
	_, err = conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err == nil {
		t.Fatalf("expected error on dropped connection")
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("post-reset failure 1: expected ready, got %q", st.State)
	}
	if conn.ConsecutiveFailuresForTest() != 1 {
		t.Fatalf("expected consecutive failures 1, got %d", conn.ConsecutiveFailuresForTest())
	}

	// Failure 2 after reset
	_, err = conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err == nil {
		t.Fatalf("expected error on dropped connection")
	}
	st = conn.Status()
	if st.State != custos.MCPStateReady {
		t.Fatalf("post-reset failure 2: expected ready, got %q", st.State)
	}
	if conn.ConsecutiveFailuresForTest() != 2 {
		t.Fatalf("expected consecutive failures 2, got %d", conn.ConsecutiveFailuresForTest())
	}

	// Strike 3 (failure 3): enters backoff with reason transport
	_, err = conn.Execute(context.Background(), "ping", json.RawMessage("{}"), custos.Credential{})
	if err == nil {
		t.Fatalf("expected error on dropped connection")
	}
	st = conn.Status()
	if st.State != custos.MCPStateBackoff {
		t.Fatalf("strike 3: expected state backoff, got %q", st.State)
	}
	if st.Reason != custos.MCPReasonTransport {
		t.Fatalf("strike 3: expected reason transport, got %q", st.Reason)
	}
	if !conn.HasRetryTimerForTest() {
		t.Fatalf("strike 3: expected retry timer scheduled")
	}
}

// TestG2d_BearerNeverRenders verifies:
// force an SDK error with the auth server; assert the bearer value appears nowhere in Status(),
// returned error strings, or audit records (mirror the I2 probe shape).
func TestG2d_BearerNeverRenders(t *testing.T) {
	bearerToken := "super-confidential-bearer-probe-token-998877"
	t.Setenv("PROBE_BEARER", bearerToken)

	// Auth server returns 401 Unauthorized echoing the URL and path
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("401 Unauthorized: token rejected"))
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	auditSink := &testAuditSink{}
	cfg := custos.MCPServerConfig{ //nolint:gosec // test server configuration
		Name:      "bearer-probe",
		Transport: "http",
		URL:       ts.URL + "/mcp?query=param",
		Bearer:    "${env:PROBE_BEARER}",
	}

	conn := custos.NewMCPConnector(cfg, t.TempDir(), custos.WithAuditSink(auditSink))

	// 1. Force error on initial unlock/connect
	unlockErr := conn.OnFirstUnlock(context.Background())
	if unlockErr != nil && strings.Contains(unlockErr.Error(), bearerToken) {
		t.Fatalf("bearer token found in unlock error: %v", unlockErr)
	}

	// 2. Force error on Execute
	_, execErr := conn.Execute(context.Background(), "any_tool", json.RawMessage("{}"), custos.Credential{})
	if execErr == nil {
		t.Fatalf("expected error from Execute on auth failure")
	}
	if strings.Contains(execErr.Error(), bearerToken) {
		t.Fatalf("bearer token found in Execute error: %v", execErr)
	}

	// 3. Force error on Refresh
	refreshErr := conn.Refresh(context.Background())
	if refreshErr != nil && strings.Contains(refreshErr.Error(), bearerToken) {
		t.Fatalf("bearer token found in Refresh error: %v", refreshErr)
	}

	// 4. Force error on ManualRefresh
	manualErr := conn.ManualRefresh(context.Background())
	if manualErr != nil && strings.Contains(manualErr.Error(), bearerToken) {
		t.Fatalf("bearer token found in ManualRefresh error: %v", manualErr)
	}

	// 5. Assert bearer never appears in Status()
	st := conn.Status()
	stBytes, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("json.Marshal(Status) error: %v", err)
	}
	if strings.Contains(string(stBytes), bearerToken) {
		t.Fatalf("bearer token found in Status(): %s", string(stBytes))
	}
	if strings.Contains(st.Name, bearerToken) || strings.Contains(st.State, bearerToken) || strings.Contains(st.Reason, bearerToken) {
		t.Fatalf("bearer token found in Status() fields: %+v", st)
	}

	// 6. Assert bearer never appears in audit records
	for _, rec := range auditSink.Records() {
		if strings.Contains(rec.Cred, bearerToken) {
			t.Fatalf("bearer token found in audit record Cred: %+v", rec)
		}
		if strings.Contains(rec.Host, bearerToken) {
			t.Fatalf("bearer token found in audit record Host: %+v", rec)
		}
		if strings.Contains(rec.Tool, bearerToken) {
			t.Fatalf("bearer token found in audit record Tool: %+v", rec)
		}
		for _, name := range rec.Names {
			if strings.Contains(name, bearerToken) {
				t.Fatalf("bearer token found in audit record Names: %+v", rec)
			}
		}
	}
}

// TestG2d_ListTimeout verifies:
// server that accepts but never answers tools/list -> backoff/list_timeout within list_timeout.
func TestG2d_ListTimeout(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-timeout-server",
		Version: "1.0.0",
	}, nil)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	// Server accepts connect/discover/initialize, but hangs when tools/list is called
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil && (bytes.Contains(bodyBytes, []byte(`"method":"tools/list"`)) || bytes.Contains(bodyBytes, []byte(`"method": "tools/list"`))) {
			// Accept request but never answer; wait for client to timeout
			<-r.Context().Done()
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	listTimeout := 150 * time.Millisecond
	cfg := custos.MCPServerConfig{
		Name:        "timeout-conn",
		Transport:   "http",
		URL:         ts.URL,
		ListTimeout: listTimeout,
	}

	conn := custos.NewMCPConnector(cfg, t.TempDir())

	start := time.Now()
	err := conn.OnFirstUnlock(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("OnFirstUnlock returned unexpected error: %v", err)
	}

	// Must settle within reasonable margin of list_timeout
	if elapsed > 3*time.Second {
		t.Fatalf("took too long to timeout: %v (expected ~%v)", elapsed, listTimeout)
	}

	st := conn.Status()
	if st.State != custos.MCPStateBackoff {
		t.Fatalf("expected state backoff, got %q", st.State)
	}
	if st.Reason != custos.MCPReasonListTimeout {
		t.Fatalf("expected reason list_timeout, got %q", st.Reason)
	}
	if !conn.HasRetryTimerForTest() {
		t.Fatalf("expected retry timer scheduled on list_timeout")
	}
}
