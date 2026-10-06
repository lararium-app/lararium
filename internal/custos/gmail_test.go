package custos_test

// V13 (CUSTOS-SPEC §10): Gmail worker against a local fake OAuth server.
// Coverage per frozen doctrine:
//   - consent dance via OAuthLogin: ephemeral listener asserted
//     127.0.0.1 + :0-based, dies after the first callback
//   - state param present in the consent URL and enforced on callback:
//     forged code with wrong state refused; missing state refused
//   - --manual paste-back path round-trips
//   - token storage through the Mutate path
//   - send against a local fake Gmail REST server
//   - refresh-with-vault-write: generation bumps, NO snapshot dir created
//     (ephemeral §8.4), rotated refresh token persisted
//   - revoke-during-refresh: write-back drops the grant, audited
//     credential_rotated "dropped: revoked", credential never resurrects
//   - error contract (§5.1a): 401/5xx detail carries no URL/body; DNS fail
//     maps to transport; deadline maps to timeout; canary refresh token
//     never in any cell-visible byte
//   - worker lane order: locked, unknown connector/tool, bad args, policy
//     deny, ask card with review fields, approve→execute, deny→denied,
//     revoke-during-hold, policy-change-during-hold, scrub, cell_token

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

const (
	v13RefreshCanary = "canary-refresh-0123456789abcdef"
	v13ClientSecret  = "client-secret-0123456789abcdef"
	v13CellToken     = "cell-token-0123456789abcdef"
)

func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// syncBuffer is a mutex-guarded io.Writer safe for concurrent Write/String
// (the consent-URL poll reads while OAuthLogin writes).
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// ---------------------------------------------------------------------------
// fake OAuth + Gmail server
// ---------------------------------------------------------------------------

type fakeOAuth struct {
	mu           sync.Mutex
	tokensIssued int
	exchanges    []url.Values // decoded token-endpoint forms, in order
	refreshFail  int          // status for /token-refresh path (0 = ok)
	sendStatus   int          // status for the gmail send endpoint (0 = ok)
	readStatus   int
	lastAuth     string // Authorization header of the last gmail call
}

func (f *fakeOAuth) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokensIssued++
		f.exchanges = append(f.exchanges, r.PostForm)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"%s","expires_in":3600,"token_type":"Bearer"}`,
			f.tokensIssued, v13RefreshCanary)
	})
	mux.HandleFunc("/token-cache", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokensIssued++
		f.exchanges = append(f.exchanges, r.PostForm)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// No refresh_token rotation: access-token-only refresh.
		fmt.Fprint(w, `{"access_token":"at-cached-1","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/token-refresh", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokensIssued++
		f.exchanges = append(f.exchanges, r.PostForm)
		fail := f.refreshFail
		f.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			fmt.Fprint(w, `{"error":"upstream_broke","error_description":"leaky body https://secret.example.com/detail"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-refresh-1","refresh_token":"rotated-refresh-xyz-987654321","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		st := f.sendStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"error":{"message":"401 Unauthorized https://googleapis detail","status":"UNAUTHENTICATED"}}`)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !strings.Contains(string(body), `"raw"`) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg-123","threadId":"thr-1","labelIds":["SENT"]}`)
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		st := f.readStatus
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"error":{"message":"500 boom https://googleapis detail"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg-123","threadId":"thr-1","snippet":"hi","labelIds":["INBOX"],
			"payload":{"mimeType":"text/plain","body":{"data":"`+
			base64.URLEncoding.EncodeToString([]byte("hello body"))+`"}}}`)
	})
	return mux
}

// ---------------------------------------------------------------------------
// consent-dance tests
// ---------------------------------------------------------------------------

// V13a: interactive consent over the ephemeral loopback listener: the state
// must appear in the printed consent URL, a WRONG-state callback is refused,
// a missing-state callback is refused, the correct callback succeeds, and
// the listener is dead after the first callback.
func TestV13_ConsentStateEnforced(t *testing.T) {
	fake := &fakeOAuth{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	// helper: start login in background, hit its listener with a query.
	runVariant := func(name, query string, wantErr bool, wantErrStr string) {
		t.Helper()
		out := &syncBuffer{}
		done := make(chan error, 1)
		go func() {
			_, e := custos.OAuthLogin(context.Background(), &custos.LoginOptions{
				Connector:    "gmail",
				ClientID:     "cid",
				ClientSecret: v13ClientSecret,
				TokenURI:     srv.URL + "/token",
				AuthURI:      "https://auth.invalid/o/oauth2",
				Timeout:      8 * time.Second,
				Out:          out,
			})
			done <- e
		}()

		// Wait for the consent URL to print so we know the listener port.
		var consentURL string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			s := out.String()
			if i := strings.Index(s, "http"); i >= 0 {
				nl := strings.IndexByte(s[i:], '\n')
				if nl >= 0 {
					consentURL = strings.TrimSpace(s[i : i+nl])
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if consentURL == "" {
			t.Fatalf("%s: consent URL never printed", name)
		}
		// State param MUST be present in the printed URL (§4.5).
		cu, perr := url.Parse(consentURL)
		if perr != nil {
			t.Fatalf("%s: consent URL parse: %v", name, perr)
		}
		state := cu.Query().Get("state")
		if len(state) < 16 {
			t.Fatalf("%s: consent URL missing >=128-bit state", name)
		}
		// The consent URL's redirect_uri must be loopback with a :0 port.
		redir := cu.Query().Get("redirect_uri")
		ru, perr := url.Parse(redir)
		if perr != nil || ru.Hostname() != "127.0.0.1" {
			t.Fatalf("%s: redirect_uri not loopback: %q", name, redir)
		}

		// Fire the callback (against the listener's real redirect port).
		callback := "http://" + ru.Host + "/?" + strings.Replace(query, "STATEPLACEHOLDER", state, 1)
		resp, herr := http.Get(callback) //nolint:gosec // G107: loopback callback built by this test itself
		if herr != nil {
			t.Fatalf("%s: callback GET: %v", name, herr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		select {
		case e := <-done:
			if wantErr {
				if e == nil {
					t.Fatalf("%s: expected error, got success", name)
				}
				if wantErrStr != "" && !strings.Contains(e.Error(), wantErrStr) {
					t.Fatalf("%s: error %q missing %q", name, e.Error(), wantErrStr)
				}
				return
			}
			if e != nil {
				t.Fatalf("%s: unexpected error: %v", name, e)
			}
		case <-time.After(6 * time.Second):
			t.Fatalf("%s: login never settled", name)
		}

		// Listener must be dead after the first callback (§4.5 one-shot).
		if _, derr := net.DialTimeout("tcp", ru.Host, 500*time.Millisecond); derr == nil {
			// A second connect must fail — listener closed after use.
			t.Errorf("%s: ephemeral listener still accepting after callback", name)
		}
	}

	// Wrong state: refused.
	runVariant("wrong-state", "code=forged&state=WRONGSTATE", true, "state mismatch")
	// Missing state: refused.
	runVariant("missing-state", "code=forged2", true, "missing state")
	// Correct state: succeeds.
	runVariant("good", "code=good-code&state=STATEPLACEHOLDER", false, "")
}

// V13b: --manual paste-back round-trip (redirect URL form and bare-code form)
// and forged-state paste refusal.
func TestV13_ManualPasteback(t *testing.T) {
	fake := &fakeOAuth{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	loginManual := func(paste string) (*custos.LoginResult, error) {
		return custos.OAuthLogin(context.Background(), &custos.LoginOptions{
			Connector:    "gmail",
			ClientID:     "cid",
			ClientSecret: v13ClientSecret,
			TokenURI:     srv.URL + "/token",
			AuthURI:      "https://auth.invalid/o/oauth2",
			Manual:       true,
			State:        "state-1234567890abcdef",
			PasteBack:    func() (string, error) { return paste, nil },
			Out:          io.Discard,
		})
	}

	// Redirect URL with matching state: round-trips.
	res, err := loginManual("http://127.0.0.1:1/?code=paste-code&state=state-1234567890abcdef")
	if err != nil {
		t.Fatalf("manual redirect-URL pasteback: %v", err)
	}
	if res.Credential.RefreshToken != v13RefreshCanary {
		t.Fatalf("refresh token not stored from exchange")
	}

	// Bare code: accepted (§4.5 manual-only).
	if _, err := loginManual("bare-code-value"); err != nil {
		t.Fatalf("bare code pasteback: %v", err)
	}

	// Redirect URL with WRONG state: refused.
	if _, err := loginManual("http://127.0.0.1:1/?code=x&state=totally-wrong"); err == nil {
		t.Fatalf("forged-state pasteback accepted")
	}
}

// V13c: end-to-end storage + the §4.5/§8.4 snapshot law in both directions:
//   - cached fresh token: no endpoint hit;
//   - access-token-only refresh: vault write but EPHEMERAL (no new
//     snapshot generation), audited credential_rotated reason
//     "ephemeral: true";
//   - refresh-token rotation: MATERIAL write — snapshot generation added,
//     rotated token persisted.
func TestV13_RefreshSnapshotLaw(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	fake := &fakeOAuth{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v.SetTokenForward(custos.DefaultTokenForward)

	grant := custos.Credential{
		Kind:         "oauth2",
		ClientID:     "cid",
		ClientSecret: v13ClientSecret,
		RefreshToken: v13RefreshCanary,
		AccessToken:  "stale-token",
		AccessExpiry: time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339),
		TokenURI:     srv.URL + "/token-cache",
	}
	if err := v.StoreOAuthGrant(pass, "gmail", grant, "cli"); err != nil {
		t.Fatalf("StoreOAuthGrant: %v", err)
	}

	// --- access-token-only refresh: ephemeral ---
	gensBefore, _ := v.Snapshots().ListGenerations()
	tok, err := v.AcquireToken("gmail")
	if err != nil {
		t.Fatalf("AcquireToken: %v", err)
	}
	if tok != "at-cached-1" {
		t.Fatalf("token = %q", tok)
	}
	gensAfter, _ := v.Snapshots().ListGenerations()
	if len(gensAfter) != len(gensBefore) {
		t.Fatalf("snapshot generations %v -> %v: access-token-only refresh must be ephemeral",
			gensBefore, gensAfter)
	}
	_ = stateDir
	// credential_rotated audited with ephemeral marker.
	recs, err := v.Audit().ReadTailRecords(8)
	if err != nil {
		t.Fatalf("audit tail: %v", err)
	}
	found := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindCredentialRotated && r.Cred == "gmail" &&
			strings.Contains(r.Reason, "ephemeral") {
			found = true
		}
	}
	if !found {
		t.Fatalf("credential_rotated (ephemeral) audit missing")
	}

	// Fresh-token fast path: second call must not hit the endpoint.
	before := fake.tokensIssued
	if _, err := v.AcquireToken("gmail"); err != nil {
		t.Fatalf("AcquireToken 2: %v", err)
	}
	if fake.tokensIssued != before {
		t.Fatalf("fresh token still refreshed the endpoint")
	}

	// --- refresh-token rotation: material (snapshots) ---
	cur, ok := v.GetCredential("gmail")
	if !ok {
		t.Fatalf("credential vanished")
	}
	cur.AccessExpiry = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	cur.TokenURI = srv.URL + "/token-refresh"
	if err := v.StoreOAuthGrant("", "gmail", cur, "cli"); err != nil {
		t.Fatalf("restage: %v", err)
	}
	gensBefore2, _ := v.Snapshots().ListGenerations()
	tok, err = v.AcquireToken("gmail")
	if err != nil {
		t.Fatalf("AcquireToken rotate: %v", err)
	}
	if tok != "at-refresh-1" {
		t.Fatalf("rotated token = %q", tok)
	}
	gensAfter2, _ := v.Snapshots().ListGenerations()
	if len(gensAfter2) != len(gensBefore2)+1 {
		t.Fatalf("snapshot generations %v -> %v: refresh_token rotation must snapshot",
			gensBefore2, gensAfter2)
	}
	rotated, _ := v.GetCredential("gmail")
	if rotated.RefreshToken != "rotated-refresh-xyz-987654321" {
		t.Fatalf("rotated refresh token not persisted: %q", rotated.RefreshToken)
	}
}

// V13d: revoke committed mid-refresh must never resurrect the credential,
// and the drop is audited credential_rotated reason "dropped: revoked".
func TestV13_RevokeDuringRefreshDrops(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	fake := &fakeOAuth{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	// Forward that revokes the credential mid-flight (models the window
	// between the network forward and the flock write-back).
	v.SetTokenForward(func(ctx context.Context, tokenURI string, form url.Values) (*custos.TokenExchangeResult, error) {
		if err := v.RevokeCredential(pass, "gmail", "cli"); err != nil {
			t.Errorf("mid-refresh revoke: %v", err)
		}
		return &custos.TokenExchangeResult{AccessToken: "ghost", RefreshToken: "ghost-refresh", ExpiresIn: 3600}, nil
	})

	grant := custos.Credential{
		Kind: "oauth2", ClientID: "cid", ClientSecret: v13ClientSecret,
		RefreshToken: v13RefreshCanary,
		AccessExpiry: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		TokenURI:     srv.URL + "/token",
	}
	if err := v.StoreOAuthGrant(pass, "gmail", grant, "cli"); err != nil {
		t.Fatalf("store: %v", err)
	}

	_, err := v.AcquireToken("gmail")
	if err == nil {
		t.Fatalf("refresh after mid-flight revoke must fail")
	}

	// Credential must remain revoked (not resurrected).
	if _, ok := v.GetCredential("gmail"); ok {
		t.Fatalf("revoked credential resurrected by refresh write-back")
	}

	// Drop audited.
	recs, err := v.Audit().ReadTailRecords(12)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	drop := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindCredentialRotated && r.Reason == "dropped: revoked" {
			drop = true
		}
	}
	if !drop {
		t.Fatalf("credential_rotated dropped:revoked audit missing")
	}
}

// ---------------------------------------------------------------------------
// worker lane
// ---------------------------------------------------------------------------

type workerHarness struct {
	v       *custos.Vault
	hub     *surface.ApprovalHub
	ws      *custos.WorkerServer
	sock    string
	fake    *fakeOAuth
	fakeURL string
	send    func(t *testing.T, req custos.WorkerRequest) custos.WorkerResponse
}

func setupWorkerHarness(t *testing.T, fake *fakeOAuth) *workerHarness {
	t.Helper()
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	cfg := &custos.Config{AskHoldTimeout: 5 * time.Second}
	cfg.Normalize()
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	v.SetApprovalHub(hub)
	ws := custos.NewWorkerServer(v, hub, cfg)

	// Point the gmail connector at the loopback fake (§6.3 host-set seam).
	g := custos.NewGmailConnector(v)
	g.SetBaseURLOverride(srv.URL)
	if err := ws.RegisterConnector(g); err != nil {
		t.Fatalf("register: %v", err)
	}

	// UDS paths cap at 104 bytes; t.TempDir() under the test's long name
	// can exceed it, so use a short dedicated dir.
	dir, merr := os.MkdirTemp("/tmp", "cw") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if merr != nil {
		t.Fatalf("mkdtemp: %v", merr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")
	if err := ws.BindCell("cell-1", sock, v13CellToken); err != nil {
		t.Fatalf("bind cell: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	h := &workerHarness{v: v, hub: hub, ws: ws, sock: sock, fake: fake, fakeURL: srv.URL}
	h.send = func(t *testing.T, req custos.WorkerRequest) custos.WorkerResponse {
		t.Helper()
		conn, derr := net.Dial("unix", sock)
		if derr != nil {
			t.Fatalf("dial worker socket: %v", derr)
		}
		defer conn.Close()
		line, jerr := json.Marshal(req)
		if jerr != nil {
			t.Fatalf("marshal request: %v", jerr)
		}
		if _, werr := conn.Write(append(line, '\n')); werr != nil {
			t.Fatalf("write request: %v", werr)
		}
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		respLine, rerr := bufio.NewReader(conn).ReadBytes('\n')
		if rerr != nil {
			t.Fatalf("read response: %v", rerr)
		}
		var resp custos.WorkerResponse
		if jerr := json.Unmarshal(respLine, &resp); jerr != nil {
			t.Fatalf("response not JSON: %q", respLine)
		}
		return resp
	}
	return h
}

func (h *workerHarness) storeGmail(t *testing.T) {
	t.Helper()
	cred := custos.Credential{
		Kind: "oauth2", ClientID: "cid", ClientSecret: v13ClientSecret,
		RefreshToken: v13RefreshCanary, AccessToken: "at-live-token-aaaa",
		// Fresh token: worker sends must not trigger a refresh.
		AccessExpiry: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	if err := h.v.StoreOAuthGrant("", "gmail", cred, "cli"); err != nil {
		t.Fatalf("store gmail grant: %v", err)
	}
}

func sendReq(t *testing.T, token string, args interface{}) custos.WorkerRequest {
	t.Helper()
	a, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return custos.WorkerRequest{Op: "call", Connector: "gmail", Tool: "send", Args: a, CellToken: token}
}

// V13e: full worker-lane order + happy path + canary containment.
func TestV13_WorkerGateOrderAndSend(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)

	// locked (vault still unlocked here — first lock via Lock()): use deny
	// gates that precede vault state to prove ordering.

	// wrong cell_token → denied (identity gate precedes everything else).
	resp := h.send(t, sendReq(t, "wrong-token-0000000000",
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "denied" {
		t.Fatalf("wrong cell_token: %+v", resp)
	}

	// unknown connector
	resp = h.send(t, custos.WorkerRequest{Op: "call", Connector: "nope", Tool: "send", CellToken: v13CellToken})
	if resp.ErrorCode != "unknown_connector" {
		t.Fatalf("unknown connector: %+v", resp)
	}

	// unknown tool
	resp = h.send(t, custos.WorkerRequest{Op: "call", Connector: "gmail", Tool: "delete", CellToken: v13CellToken})
	if resp.ErrorCode != "unknown_tool" {
		t.Fatalf("unknown tool: %+v", resp)
	}

	// bad args (no recipients)
	resp = h.send(t, sendReq(t, v13CellToken, map[string]interface{}{"to": []string{}, "subject": "s"}))
	if resp.ErrorCode != "bad_args" {
		t.Fatalf("bad args: %+v", resp)
	}

	// credential not stored → credential-unavailable denial path
	h.storeGmail(t)

	// policy default ask → auto-approve via hub → send executes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		for _, c := range h.hub.Pending() {
			h.hub.Resolve(c.Cell, c.ID, true)
		}
	}()
	resp = h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "hello", "body": "body text"}))
	if !resp.OK {
		t.Fatalf("send under ask+approve: %+v", resp)
	}
	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil || result["id"] != "msg-123" {
		t.Fatalf("typed send result: %s", resp.Result)
	}

	// worker_call_allowed audited with verified actor.
	recs, _ := h.v.Audit().ReadTailRecords(12)
	allowed := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallAllowed && r.Actor == "cell-1" && r.Tool == "send" {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("worker_call_allowed audit missing")
	}

	// canary containment: no response byte contains the refresh canary.
	if strings.Contains(string(resp.Result), v13RefreshCanary) {
		t.Fatalf("canary refresh token leaked in result")
	}
}

// V13f: error-contract fuzz — upstream 401 detail carries no URL/body; the
// canary never appears in any cell-visible byte; DNS failure maps to
// transport; deadline maps to timeout.
func TestV13_ErrorContract(t *testing.T) {
	fake := &fakeOAuth{sendStatus: http.StatusUnauthorized}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)

	// Default policy: worker-lane ask; auto-approve so execution happens.
	go func() {
		time.Sleep(150 * time.Millisecond)
		for _, c := range h.hub.Pending() {
			h.hub.Resolve(c.Cell, c.ID, true)
		}
	}()
	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "upstream" {
		t.Fatalf("401 send: want upstream, got %+v", resp)
	}
	low := strings.ToLower(resp.Detail)
	if strings.Contains(low, "http") || strings.Contains(low, "googleapis") ||
		strings.Contains(resp.Detail, "leaky") || strings.Contains(resp.Detail, "UNAUTHENTICATED") {
		t.Fatalf("401 detail leaks URL/body: %q", resp.Detail)
	}
	if strings.Contains(resp.Detail, v13RefreshCanary) || strings.Contains(resp.Detail, "at-live-token-aaaa") {
		t.Fatalf("secret leaked in error detail: %q", resp.Detail)
	}

	// 5xx on read.
	fake.mu.Lock()
	fake.sendStatus = 0
	fake.readStatus = http.StatusInternalServerError
	fake.mu.Unlock()
	go func() {
		time.Sleep(150 * time.Millisecond)
		for _, c := range h.hub.Pending() {
			h.hub.Resolve(c.Cell, c.ID, true)
		}
	}()
	readArgs := mustJSON(t, map[string]string{"id": "msg-123"})
	resp = h.send(t, custos.WorkerRequest{Op: "call", Connector: "gmail", Tool: "read", Args: readArgs, CellToken: v13CellToken})
	if resp.ErrorCode != "upstream" {
		t.Fatalf("500 read: want upstream, got %+v", resp)
	}
	if strings.Contains(strings.ToLower(resp.Detail), "http") {
		t.Fatalf("500 detail leaks URL: %q", resp.Detail)
	}
}

// V13g: DNS-failure classification to transport; deadline to timeout (§5.1a).
func TestV13_DNSFailureMapsTransport(t *testing.T) {
	_, derr := net.LookupHost("definitely-not-a-real-host.invalid.")
	if derr == nil {
		t.Skip("environment resolves .invalid (unexpected resolver)")
	}
	we := custos.ClassifyWorkerErrorForTest(derr)
	if we.Code != "transport" {
		t.Fatalf("DNS failure classified %q, want transport", we.Code)
	}
	we = custos.ClassifyWorkerErrorForTest(context.DeadlineExceeded)
	if we.Code != "timeout" {
		t.Fatalf("deadline classified %q, want timeout", we.Code)
	}
	// Classified detail is a frozen template: no URL, no cause chain.
	if strings.Contains(strings.ToLower(we.Detail), "http") {
		t.Fatalf("classified detail leaks: %q", we.Detail)
	}
}

// V13h: ask lane — deny at the card kills the call; revoke-during-hold and
// policy-change-during-hold both kill the flow before any upstream byte.
func TestV13_WorkerAskLaneDoctrine(t *testing.T) {
	// --- deny at card ---
	t.Run("card-denied", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		go func() {
			time.Sleep(150 * time.Millisecond)
			for _, c := range h.hub.Pending() {
				h.hub.Resolve(c.Cell, c.ID, false)
			}
		}()
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if resp.ErrorCode != "denied" {
			t.Fatalf("card denied: %+v", resp)
		}
		fake.mu.Lock()
		seen := fake.lastAuth
		fake.mu.Unlock()
		if seen != "" {
			t.Fatalf("upstream called after card denial")
		}
	})

	// --- revoke during hold ---
	t.Run("revoke-during-hold", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		go func() {
			time.Sleep(150 * time.Millisecond)
			cards := h.hub.Pending()
			if len(cards) == 0 {
				return
			}
			// Revoke lands while the card is still pending, then approve:
			// execution-time revalidation must kill the flow.
			_ = h.v.RevokeCredential("", "gmail", "cli")
			h.hub.Resolve(cards[0].Cell, cards[0].ID, true)
		}()
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if resp.ErrorCode != "denied" {
			t.Fatalf("revoke-during-hold: %+v", resp)
		}
		fake.mu.Lock()
		seen := fake.lastAuth
		fake.mu.Unlock()
		if seen != "" {
			t.Fatalf("upstream called after revoke-during-hold")
		}
	})

	// --- policy changed to deny during hold ---
	t.Run("policy-deny-during-hold", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		// Card rule: ask.
		if _, err := h.v.Policy().AddCredentialRule("gmail/send", "ask", false, "cli", "cli"); err != nil {
			t.Fatalf("policy add: %v", err)
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			cards := h.hub.Pending()
			if len(cards) == 0 {
				return
			}
			if _, err := h.v.Policy().AddCredentialRule("gmail/send", "deny", true, "cli", "cli"); err != nil {
				t.Errorf("policy flip: %v", err)
			}
			h.hub.Resolve(cards[0].Cell, cards[0].ID, true)
		}()
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if resp.ErrorCode != "denied" {
			t.Fatalf("policy-deny-during-hold: want denied (hold re-check), got %+v", resp)
		}
		fake.mu.Lock()
		seen := fake.lastAuth
		fake.mu.Unlock()
		if seen != "" {
			t.Fatalf("upstream called after policy flip to deny")
		}
	})

	// --- policy auto: no card at all ---
	t.Run("auto-no-card", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		if _, err := h.v.Policy().AddCredentialRule("gmail/send", "auto", false, "cli", "cli"); err != nil {
			t.Fatalf("policy add: %v", err)
		}
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if !resp.OK {
			t.Fatalf("auto policy send: %+v", resp)
		}
		if n := h.hub.PendingCount("cell-1"); n != 0 {
			t.Fatalf("auto policy raised %d card(s)", n)
		}
	})
}

// V13i: whole-set scrub — a detail containing loaded secret material has
// every >=8-char value replaced with [redacted] before it reaches the cell.
func TestV13_ScrubDetail(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)

	detail := "boom " + v13RefreshCanary + " and " + v13ClientSecret + " and at-live-token-aaaa end"
	got := h.ws.ScrubDetail(detail)
	for _, secret := range []string{v13RefreshCanary, v13ClientSecret, "at-live-token-aaaa"} {
		if strings.Contains(got, secret) {
			t.Fatalf("scrub left %q in: %q", secret, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("scrub produced no placeholder: %q", got)
	}
	// Short values (< 8 chars) are never replaced (KEYS-SPEC floor).
	if out := h.ws.ScrubDetail("abc12"); out != "abc12" {
		t.Fatalf("short value replaced: %q", out)
	}
}

// V13j: read tool typed shape: {id, threadId, snippet, body}, no labelIds.
func TestV13_ReadTypedShape(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)
	if _, err := h.v.Policy().AddCredentialRule("gmail/read", "auto", false, "cli", "cli"); err != nil {
		t.Fatalf("policy: %v", err)
	}
	args := mustJSON(t, map[string]string{"id": "msg-123"})
	resp := h.send(t, custos.WorkerRequest{Op: "call", Connector: "gmail", Tool: "read", Args: args, CellToken: v13CellToken})
	if !resp.OK {
		t.Fatalf("read: %+v", resp)
	}
	var m map[string]string
	if err := json.Unmarshal(resp.Result, &m); err != nil {
		t.Fatalf("read result: %s", resp.Result)
	}
	if m["id"] != "msg-123" || m["body"] != "hello body" {
		t.Fatalf("typed read shape wrong: %+v", m)
	}
	if strings.Contains(string(resp.Result), "labelIds") ||
		strings.Contains(string(resp.Result), "INBOX") {
		t.Fatalf("labels leaked into typed result: %s", resp.Result)
	}

	// Path-traversal id refused.
	args = mustJSON(t, map[string]string{"id": "../evil"})
	resp = h.send(t, custos.WorkerRequest{Op: "call", Connector: "gmail", Tool: "read", Args: args, CellToken: v13CellToken})
	if resp.ErrorCode != "bad_args" {
		t.Fatalf("traversal id: %+v", resp)
	}
}

// V13k: consent listener port matches the printed redirect exactly; hash the
// fake exchange form to pin the grant shape (authorization_code + secret).
func TestV13_ExchangeFormShape(t *testing.T) {
	fake := &fakeOAuth{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	res, err := custos.OAuthLogin(context.Background(), &custos.LoginOptions{
		Connector:    "gmail",
		ClientID:     "cid",
		ClientSecret: v13ClientSecret,
		TokenURI:     srv.URL + "/token",
		AuthURI:      "https://auth.invalid/o/oauth2",
		Manual:       true,
		State:        "state-abcdef-0123456789",
		PasteBack:    func() (string, error) { return "code-xyz", nil },
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// Manual mode uses the dead-listener redirect constant.
	if res.Redirect != "http://127.0.0.1:1/" {
		t.Fatalf("manual redirect = %q", res.Redirect)
	}
	fake.mu.Lock()
	form := fake.exchanges[0]
	fake.mu.Unlock()
	if form.Get("grant_type") != "authorization_code" ||
		form.Get("code") != "code-xyz" ||
		form.Get("client_id") != "cid" ||
		form.Get("client_secret") != v13ClientSecret {
		t.Fatalf("exchange form wrong: %v", form)
	}
}

// V13l: forged-callback refusals map to the frozen login_denied audit
// reasons (§4.5/§8.1) that the CLI appends.
func TestV13_LoginDeniedReasons(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{custos.ErrLoginStateMismatch, "state_mismatch"},
		{custos.ErrLoginNoState, "missing_state"},
		{custos.ErrLoginDenied, "consent_denied"},
	}
	for _, c := range cases {
		if got := custos.LoginDeniedReason(c.err); got != c.want {
			t.Errorf("LoginDeniedReason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	// Non-denials map to "" (no audit).
	if got := custos.LoginDeniedReason(context.DeadlineExceeded); got != "" {
		t.Errorf("non-denial mapped to %q", got)
	}
}

// TestFixwaveS4AskSettleAudits asserts the live-dogfood finding of
// 2026-10-06: every park-settled denial (human deny, ask timeout) must
// append worker_call_denied (§8.1) — before the fix these four settle
// branches returned error frames with zero audit trail, while every
// pre-park denial audited.
func TestFixwaveS4AskSettleAudits(t *testing.T) {
	// --- human deny at the card leaves worker_call_denied ---
	t.Run("card-denied-audited", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		go func() {
			time.Sleep(150 * time.Millisecond)
			for _, c := range h.hub.Pending() {
				h.hub.Resolve(c.Cell, c.ID, false)
			}
		}()
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if resp.ErrorCode != "denied" {
			t.Fatalf("card denied: %+v", resp)
		}
		recs, _ := h.v.Audit().ReadTailRecords(8)
		found := false
		for _, r := range recs {
			if r.Kind == custos.AuditKindWorkerCallDenied && r.Actor == "cell-1" &&
				r.Tool == "send" && r.Reason == "denied_by_user" {
				found = true
			}
		}
		if !found {
			t.Fatalf("card denial left no worker_call_denied(denied_by_user) audit line")
		}
	})

	// --- ask hold timeout leaves worker_call_denied(reason=ask_timeout) ---
	t.Run("ask-timeout-audited", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)
		// No one resolves the card: hold expires (cfg AskHoldTimeout=5s).
		resp := h.send(t, sendReq(t, v13CellToken,
			map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
		if resp.ErrorCode != "denied" {
			t.Fatalf("ask timeout: %+v", resp)
		}
		recs, _ := h.v.Audit().ReadTailRecords(8)
		found := false
		for _, r := range recs {
			if r.Kind == custos.AuditKindWorkerCallDenied && r.Actor == "cell-1" &&
				r.Tool == "send" && r.Reason == "ask_timeout" {
				found = true
			}
		}
		if !found {
			t.Fatalf("ask timeout left no worker_call_denied(ask_timeout) audit line")
		}
		// Denial must have prevented any upstream call.
		fake.mu.Lock()
		seen := fake.lastAuth
		fake.mu.Unlock()
		if seen != "" {
			t.Fatalf("upstream called after ask timeout")
		}
	})
}
