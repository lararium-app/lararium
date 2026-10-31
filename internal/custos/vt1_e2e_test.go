package custos_test

// V-T1 suite (CREDHANDLE-SPEC "Exit criterion"): end-to-end proof that a
// credential handle lets an agent USE a secret without SEEING it. Everything
// runs in-process: the real vault, worker server (cell socket), surface
// /v1/credentials handlers, and a real loop.Session whose "model" is a stub
// router provider that CAPTURES every messages() payload it is sent.
//
// Run all six proofs together with:
//
//	go test ./internal/custos/ -run VT1 -v -count=1

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// suiteSecret is the distinctive fixture secret for every V-T1 proof.
const suiteSecret = "sk-liv-VT1SUITE-00000000000000000000-9876"

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// safeBuf is a goroutine-safe buffer used to capture daemon log output.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// vt1Connector is the connector under test. No web-login connector type
// exists yet, so this is the closest real path: a worker-lane Connector whose
// Execute authenticates to an HTTPS endpoint with the credential the worker
// resolved from the handle (api_key as bearer, the shape gmail.go uses).
type vt1Connector struct {
	client *http.Client
	url    string
}

func (*vt1Connector) Name() string             { return "vt1" }
func (*vt1Connector) Credential() string       { return "vt1" }
func (*vt1Connector) Host() string             { return "vt1.test" }
func (*vt1Connector) HasTool(tool string) bool { return tool == "a" || tool == "b" }
func (*vt1Connector) ValidateArgs(string, json.RawMessage) error {
	return nil
}
func (*vt1Connector) ReviewFields(string, json.RawMessage) string { return "vt1" }

func (c *vt1Connector) Execute(ctx context.Context, tool string, _ json.RawMessage, cred custos.Credential) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/login/"+tool, nil)
	if err != nil {
		return nil, err
	}
	if cred.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+cred.Secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return json.Marshal(map[string]any{"status": resp.StatusCode, "authenticated": resp.StatusCode == http.StatusOK})
}

// vt1Seen is one request observed by the fixture HTTPS server.
type vt1Seen struct {
	path string
	auth string
}

// vt1World is the whole in-process fixture.
type vt1World struct {
	h      *workerHarness
	logbuf *safeBuf
	api    http.Handler
	token  string

	mu   sync.Mutex
	seen []vt1Seen // guarded by mu

	modelMu  sync.Mutex
	payloads []string // every messages() payload sent to the model
}

func (w *vt1World) requests() []vt1Seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]vt1Seen(nil), w.seen...)
}

func (w *vt1World) modelPayloads() []string {
	w.modelMu.Lock()
	defer w.modelMu.Unlock()
	return append([]string(nil), w.payloads...)
}

// vt1CredAdapter adapts *custos.Vault to surface.CredentialSource for the
// test (the production adapter lives in package main of cmd/hearthd).
type vt1CredAdapter struct{ v *custos.Vault }

func (a vt1CredAdapter) List() ([]surface.CredentialInfo, error) {
	views, err := a.v.ListHandleViews()
	if err != nil {
		return nil, err
	}
	out := make([]surface.CredentialInfo, len(views))
	for i, v := range views {
		out[i] = vt1Info(v)
	}
	return out, nil
}

func vt1Info(v custos.HandleView) surface.CredentialInfo {
	info := surface.CredentialInfo{Handle: v.Handle, Name: v.Name, Kind: v.Kind, Last4: v.Last4, Created: v.Created}
	if v.Expires != "" {
		e := v.Expires
		info.Expires = &e
	}
	if v.Rate != nil {
		info.Rate = &surface.CredentialRate{N: v.Rate.N, Window: v.Rate.Window}
	}
	return info
}

func (a vt1CredAdapter) Create(req surface.CredentialCreate) (surface.CredentialInfo, error) {
	var rate *custos.HandleRate
	if req.Rate != nil {
		rate = &custos.HandleRate{N: req.Rate.N, Window: req.Rate.Window}
	}
	rec, err := a.v.CreateCredentialHandle("", req.Name, req.Secret, req.Scope,
		time.Duration(req.TTLSeconds)*time.Second, rate, "web")
	if err != nil {
		return surface.CredentialInfo{}, fmt.Errorf("create failed")
	}
	return vt1Info(custos.ViewOf(rec, "api_key")), nil
}

func (a vt1CredAdapter) Revoke(handle string) error { return a.v.RevokeHandle("", handle, "web") }

type vt1Sessions struct{}

func (vt1Sessions) List() ([]surface.SessionInfo, error) { return nil, nil }
func (vt1Sessions) Create(string, string) (surface.SessionInfo, error) {
	return surface.SessionInfo{}, nil
}

func (vt1Sessions) Events(string, int64, int) ([]json.RawMessage, bool, error) {
	return nil, false, nil
}
func (vt1Sessions) Delete(string) error { return nil }

type vt1Hub struct{}

func (vt1Hub) InFlight(string) bool { return false }
func (vt1Hub) Cancel(string) bool   { return false }
func (vt1Hub) Shutdown()            {}

func newVT1World(t *testing.T) *vt1World {
	t.Helper()
	w := &vt1World{logbuf: &safeBuf{}}

	// Daemon log capture (standard logger, as the daemon pieces use it).
	prevW, prevF := log.Writer(), log.Flags()
	log.SetOutput(w.logbuf)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevF) })

	// Fixture HTTPS server: 200 only for the real secret.
	tsrv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.mu.Lock()
		w.seen = append(w.seen, vt1Seen{path: r.URL.Path, auth: auth})
		w.mu.Unlock()
		if auth != "Bearer "+suiteSecret {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = rw.Write([]byte("welcome"))
	}))
	tsrv.Config.ErrorLog = log.Default()
	tsrv.StartTLS()
	t.Cleanup(tsrv.Close)

	w.h = setupWorkerHarness(t, &fakeOAuth{})
	client := tsrv.Client()
	if tr, ok := client.Transport.(*http.Transport); ok && tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if err := w.h.ws.RegisterConnector(&vt1Connector{client: client, url: tsrv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.h.v.Policy().AddCredentialRule("vt1/*", "auto", false, "cli", "cli"); err != nil {
		t.Fatal(err)
	}

	// Surface admin handler over the real vault.
	store, err := surface.OpenTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	w.token, err = store.Create("vt1-suite")
	if err != nil {
		t.Fatal(err)
	}
	srv := &surface.Server{
		Store: store, Sessions: vt1Sessions{}, Hub: vt1Hub{},
		Cfg: surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetCredentialSource(vt1CredAdapter{v: w.h.v})
	w.api = surface.NewServer(srv)
	return w
}

// createViaAdmin performs the admin POST /v1/credentials (secret in body) and
// returns the raw response body and the minted handle.
func (w *vt1World) createViaAdmin(t *testing.T, extra string) (string, string) {
	t.Helper()
	body := `{"name":"vt1","secret":"` + suiteSecret + `"` + extra + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/credentials", strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+w.token)
	rec := httptest.NewRecorder()
	w.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin POST = %d %s", rec.Code, rec.Body.String())
	}
	var info surface.CredentialInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.Handle == "" {
		t.Fatalf("bad create response %q: %v", rec.Body.String(), err)
	}
	return rec.Body.String(), info.Handle
}

// workerCall sends one worker-lane call (non-fatal: safe off the test
// goroutine, as loop tools may run elsewhere).
func (w *vt1World) workerCall(tool, args string) (custos.WorkerResponse, error) {
	var resp custos.WorkerResponse
	conn, err := net.Dial("unix", w.h.sock)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	line, err := json.Marshal(custos.WorkerRequest{
		Op: "call", Connector: "vt1", Tool: tool, Args: json.RawMessage(args), CellToken: v13CellToken,
	})
	if err != nil {
		return resp, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return resp, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	respLine, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return resp, err
	}
	err = json.Unmarshal(respLine, &resp)
	return resp, err
}

// vt1Model is the stub "model": round 1 emits the scripted tool calls, later
// rounds answer. Every Complete() input is captured verbatim.
type vt1Model struct {
	w     *vt1World
	calls []router.ToolCall
	n     int32
}

func (*vt1Model) Name() string { return "vt1model" }
func (*vt1Model) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (m *vt1Model) Complete(_ context.Context, msgs []router.Message, opts router.Options) (*router.Completion, error) {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	m.w.modelMu.Lock()
	m.w.payloads = append(m.w.payloads, string(raw))
	m.w.modelMu.Unlock()
	if atomic.AddInt32(&m.n, 1) == 1 && len(opts.Tools) > 0 {
		return &router.Completion{ToolCalls: m.calls, Model: "vt1model"}, nil
	}
	return &router.Completion{Text: "done", Model: "vt1model"}, nil
}

// runSession drives one real loop turn; returns events.jsonl contents.
// Redaction is wired exactly as the daemon does it: Secrets = vault.Needles.
func (w *vt1World) runSession(t *testing.T, calls []router.ToolCall, tools []loop.Tool, ap loop.Approver) string {
	t.Helper()
	dir := t.TempDir()
	lg, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &vt1Model{w: w, calls: calls}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: m}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: m}}})
	s := &loop.Session{Log: lg, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80, Secrets: w.h.v.Needles}
	if _, err := s.RunTurnTools(context.Background(), "go", nil, tools, ap); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// connectorTool exposes one worker-lane tool to the loop (the cell side).
func (w *vt1World) connectorTool(name, workerTool string) loop.Tool {
	return loop.Tool{
		Spec:    router.ToolSpec{Name: name, ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run: func(_ context.Context, argsJSON string) (string, error) {
			resp, err := w.workerCall(workerTool, argsJSON)
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(resp)
			return string(b), err
		},
	}
}

// assertNoSecret greps one artifact for the secret and its base64 form.
func assertNoSecret(t *testing.T, label, data string) {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString([]byte(suiteSecret))
	if strings.Contains(data, suiteSecret) {
		t.Errorf("%s contains the secret", label)
	}
	if strings.Contains(data, b64) {
		t.Errorf("%s contains base64(secret)", label)
	}
}

func (w *vt1World) auditAll(t *testing.T) string {
	t.Helper()
	return strings.Join(readAuditLines(t, w.h.v.StateDir()), "\n")
}

// ---------------------------------------------------------------------------
// the six proofs
// ---------------------------------------------------------------------------

// V_T1a — roadmap exit criterion: a connector authenticates to an HTTPS
// fixture using a vault handle given as the tool `credential` arg; the secret
// appears in no event, daemon log line, or model payload (nor as base64).
func TestVT1a_HandleAuthenticatesNoLeak(t *testing.T) {
	w := newVT1World(t)
	_, handle := w.createViaAdmin(t, "")

	events := w.runSession(t,
		[]router.ToolCall{{ID: "c1", Name: "vt1_a", ArgsJSON: `{"credential":"` + handle + `"}`}},
		[]loop.Tool{w.connectorTool("vt1_a", "a")}, nil)

	// The fixture HTTPS server received the real secret.
	seen := w.requests()
	if len(seen) != 1 || seen[0].path != "/login/a" || seen[0].auth != "Bearer "+suiteSecret {
		t.Fatalf("fixture server did not receive the real secret: %+v", seen)
	}
	if !strings.Contains(events, `\"authenticated\":true`) {
		t.Fatalf("tool result did not show authentication:\n%s", events)
	}

	assertNoSecret(t, "events.jsonl", events)
	assertNoSecret(t, "daemon log", w.logbuf.String())
	payloads := w.modelPayloads()
	if len(payloads) < 2 {
		t.Fatalf("expected >=2 model payloads, got %d", len(payloads))
	}
	for i, p := range payloads {
		assertNoSecret(t, fmt.Sprintf("messages() payload #%d", i), p)
	}
	// The audit trail records the use, with no secret.
	assertNoSecret(t, "audit", w.auditAll(t))
	if used := auditByKind(t, w.h.v.StateDir(), custos.AuditKindHandleUsed); len(used) != 1 {
		t.Fatalf("handle_used audits = %d, want 1", len(used))
	}
}

// V_T1b — write-only POST: response, audit lines and event lines carry last4
// but never the full value.
func TestVT1b_WriteOnlyCreate(t *testing.T) {
	w := newVT1World(t)
	body, handle := w.createViaAdmin(t, "")

	last4 := suiteSecret[len(suiteSecret)-4:]
	if last4 != "9876" {
		t.Fatalf("fixture last4 = %q", last4)
	}
	var info surface.CredentialInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil || info.Last4 != last4 {
		t.Fatalf("create response lacks last4 %q: %s", last4, body)
	}
	assertNoSecret(t, "create response body", body)

	// GET list: also last4 only.
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+w.token)
	rec := httptest.NewRecorder()
	w.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), last4) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecret(t, "list response body", rec.Body.String())

	// Use the handle so events and audit contain real activity.
	events := w.runSession(t,
		[]router.ToolCall{{ID: "c1", Name: "vt1_a", ArgsJSON: `{"credential":"` + handle + `"}`}},
		[]loop.Tool{w.connectorTool("vt1_a", "a")}, nil)

	lines := readAuditLines(t, w.h.v.StateDir())
	if len(lines) == 0 {
		t.Fatal("no audit lines")
	}
	for i, l := range lines {
		assertNoSecret(t, fmt.Sprintf("audit line %d", i), l)
	}
	if created := auditByKind(t, w.h.v.StateDir(), custos.AuditKindHandleCreated); len(created) != 1 {
		t.Fatalf("handle_created audits = %d, want 1", len(created))
	}
	evLines := strings.Split(strings.TrimSpace(events), "\n")
	if len(evLines) == 0 {
		t.Fatal("no event lines")
	}
	for i, l := range evLines {
		assertNoSecret(t, fmt.Sprintf("event line %d", i), l)
	}
	assertNoSecret(t, "daemon log", w.logbuf.String())
}

// V_T1c — rate N=1: first call authenticated; second in-window call executes
// unauthenticated, audits exactly one handle_denied_rate, and leaks nothing.
func TestVT1c_RateLimit(t *testing.T) {
	w := newVT1World(t)
	_, handle := w.createViaAdmin(t, `,"rate":{"n":1,"window_seconds":3600}`)
	args := `{"credential":"` + handle + `"}`

	r1, err := w.workerCall("a", args)
	if err != nil || !r1.OK || !strings.Contains(string(r1.Result), `"authenticated":true`) {
		t.Fatalf("first call = %+v err=%v", r1, err)
	}
	r2, err := w.workerCall("a", args)
	if err != nil || !r2.OK || !strings.Contains(string(r2.Result), `"authenticated":false`) {
		t.Fatalf("second call must execute unauthenticated: %+v err=%v", r2, err)
	}

	seen := w.requests()
	if len(seen) != 2 || seen[0].auth != "Bearer "+suiteSecret || seen[1].auth != "" {
		t.Fatalf("fixture saw %+v", seen)
	}
	if got := auditByKind(t, w.h.v.StateDir(), custos.AuditKindHandleDeniedRate); len(got) != 1 {
		t.Fatalf("handle_denied_rate audits = %d, want 1", len(got))
	}
	raw, _ := json.Marshal(r2)
	assertNoSecret(t, "denied response", string(raw))
	if strings.Contains(string(raw), handle) {
		t.Errorf("denied response echoes the handle: %s", raw)
	}
	assertNoSecret(t, "audit", w.auditAll(t))
}

// V_T1d — a handle scoped to tool A is refused for tool B: unauthenticated
// execution and exactly one handle_denied_scope audit.
func TestVT1d_ScopeBinding(t *testing.T) {
	w := newVT1World(t)
	_, handle := w.createViaAdmin(t, `,"scope":"vt1/a"`)
	args := `{"credential":"` + handle + `"}`

	rb, err := w.workerCall("b", args)
	if err != nil || !rb.OK || !strings.Contains(string(rb.Result), `"authenticated":false`) {
		t.Fatalf("tool B must run unauthenticated: %+v err=%v", rb, err)
	}
	if seen := w.requests(); len(seen) != 1 || seen[0].path != "/login/b" || seen[0].auth != "" {
		t.Fatalf("fixture saw %+v", seen)
	}
	if got := auditByKind(t, w.h.v.StateDir(), custos.AuditKindHandleDeniedScope); len(got) != 1 {
		t.Fatalf("handle_denied_scope audits = %d, want 1", len(got))
	}
	raw, _ := json.Marshal(rb)
	assertNoSecret(t, "denied response", string(raw))

	// Control: tool A with the same handle still authenticates.
	ra, err := w.workerCall("a", args)
	if err != nil || !strings.Contains(string(ra.Result), `"authenticated":true`) {
		t.Fatalf("tool A control = %+v err=%v", ra, err)
	}
}

// V_T1e — the existing V10 blind-CONNECT test (proxy_test.go) is untouched by
// this slice and still passes. It is re-run here as a subtest so one
// `-run VT1` invocation covers it.
func TestVT1e_V10ConnectUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command("git", "status", "--porcelain", "--", "proxy_test.go").CombinedOutput()
		if err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				t.Fatalf("proxy_test.go (V10) has local changes: %s", s)
			}
			diff, derr := exec.Command("git", "diff", "HEAD", "--quiet", "--", "proxy_test.go").CombinedOutput()
			if derr != nil {
				t.Fatalf("proxy_test.go (V10) differs from HEAD: %s", diff)
			}
		} else {
			t.Logf("git unavailable here, skipping diff check: %v", err)
		}
	}
	t.Run("V10_ConnectPassthrough", TestV10_ConnectPassthrough)
}

// V_T1f — a tool result carrying the secret from a NON-custos rail (a plain
// loop.Tool, untrusted so it goes through the approval path) appears in
// events.jsonl only as [redacted], and the messages() replay shows [redacted].
func TestVT1f_NonCustosRailResultRedacted(t *testing.T) {
	w := newVT1World(t)
	// The vault must know the secret (needle source); it does via the admin POST.
	w.createViaAdmin(t, "")

	fixture := loop.Tool{
		Spec:    router.ToolSpec{Name: "fixture_leak", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: false,
		Run: func(context.Context, string) (string, error) {
			return "leaked: " + suiteSecret, nil
		},
	}
	approve := func(string, string) (bool, string) { return true, "approved:vt1" }
	events := w.runSession(t,
		[]router.ToolCall{{ID: "c1", Name: "fixture_leak", ArgsJSON: `{}`}},
		[]loop.Tool{fixture}, approve)

	assertNoSecret(t, "events.jsonl", events)
	if !strings.Contains(events, "leaked: [redacted]") {
		t.Fatalf("events.jsonl lacks redacted result:\n%s", events)
	}
	payloads := w.modelPayloads()
	if len(payloads) < 2 {
		t.Fatalf("expected >=2 model payloads, got %d", len(payloads))
	}
	for i, p := range payloads {
		assertNoSecret(t, fmt.Sprintf("messages() payload #%d", i), p)
	}
	// The replay (second round) carries the tool result as [redacted].
	if !strings.Contains(payloads[len(payloads)-1], "leaked: [redacted]") {
		t.Fatalf("messages() replay lacks [redacted] result:\n%s", payloads[len(payloads)-1])
	}
	assertNoSecret(t, "daemon log", w.logbuf.String())
}
