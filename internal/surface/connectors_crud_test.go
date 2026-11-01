package surface

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Distinctive fixture secrets: none of these may appear in ANY response
// body, card text, SSE payload or log line.
const (
	connSecretEnv    = "ENVSECRET-9f2c7a41d0b3"
	connSecretBearer = "BEARERSECRET-5e8a1c77aa90"
	connSecretURL    = "URLPASS-3d6b0e92f1c4"
	connSecretArg    = "ARGSECRET-71aa3c5be208"
)

type fakeConnDoor struct {
	mu       sync.Mutex
	statuses []ConnectorDoorStatus
	tools    map[string][]ConnectorDoorTool
	pokes    map[int]func()
	seq      int

	calls       []string
	probeRes    ConnectorProbe
	probeErr    error
	addErr      error
	removeErr   error
	stateErr    error
	refreshErr  error
	lastAdd     []byte
	lastProbe   []byte
	addDone     chan struct{}
	onSetState  func(name string, enabled bool)
	onRefreshOK func(name string)
}

func newFakeConnDoor() *fakeConnDoor {
	return &fakeConnDoor{
		tools:   map[string][]ConnectorDoorTool{},
		pokes:   map[int]func(){},
		addDone: make(chan struct{}, 8),
		probeRes: ConnectorProbe{
			Fingerprint: "abc123def456",
			Tools: []ConnectorDoorTool{
				{Name: "read_file", Flags: "read_only"},
				{Name: "write_file", Flags: "destructive,open_world"},
			},
		},
	}
}

func (f *fakeConnDoor) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeConnDoor) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeConnDoor) countPrefix(p string) int {
	n := 0
	for _, c := range f.callList() {
		if strings.HasPrefix(c, p) {
			n++
		}
	}
	return n
}

// set replaces the mirrored rail state and fires the pushed-broadcast pokes.
func (f *fakeConnDoor) set(sts []ConnectorDoorStatus, tools map[string][]ConnectorDoorTool) {
	f.mu.Lock()
	f.statuses = append([]ConnectorDoorStatus(nil), sts...)
	f.tools = map[string][]ConnectorDoorTool{}
	for k, v := range tools {
		f.tools[k] = v
	}
	pk := make([]func(), 0, len(f.pokes))
	for _, p := range f.pokes {
		pk = append(pk, p)
	}
	f.mu.Unlock()
	for _, p := range pk {
		p()
	}
}

func (f *fakeConnDoor) ConnectorStatuses() []ConnectorDoorStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ConnectorDoorStatus(nil), f.statuses...)
}

func (f *fakeConnDoor) ConnectorTools(name string) []ConnectorDoorTool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ConnectorDoorTool(nil), f.tools[name]...)
}

func (f *fakeConnDoor) SubscribeConnectors(poke func()) func() {
	f.mu.Lock()
	f.seq++
	id := f.seq
	f.pokes[id] = poke
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.pokes, id)
		f.mu.Unlock()
	}
}

func (f *fakeConnDoor) Probe(_ context.Context, cfg []byte) (ConnectorProbe, error) {
	f.record("probe")
	f.mu.Lock()
	f.lastProbe = append([]byte(nil), cfg...)
	f.mu.Unlock()
	if f.probeErr != nil {
		return ConnectorProbe{}, f.probeErr
	}
	return f.probeRes, nil
}

func (f *fakeConnDoor) Add(_ context.Context, name string, cfg []byte) error {
	f.record("add:" + name)
	f.mu.Lock()
	f.lastAdd = append([]byte(nil), cfg...)
	f.mu.Unlock()
	if f.addErr == nil {
		f.addDone <- struct{}{}
	}
	return f.addErr
}

func (f *fakeConnDoor) Remove(_ context.Context, name string) error {
	f.record("remove:" + name)
	return f.removeErr
}

func (f *fakeConnDoor) SetState(_ context.Context, name string, enabled bool) error {
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	f.record("state:" + name + ":" + state)
	if f.stateErr != nil {
		return f.stateErr
	}
	if f.onSetState != nil {
		f.onSetState(name, enabled)
	}
	return nil
}

func (f *fakeConnDoor) Refresh(_ context.Context, name string) error {
	f.record("refresh:" + name)
	if f.refreshErr != nil {
		return f.refreshErr
	}
	if f.onRefreshOK != nil {
		f.onRefreshOK(name)
	}
	return nil
}

type connEnv struct {
	srv  *Server
	h    http.Handler
	tok  string
	door *fakeConnDoor
	hub  *ApprovalHub
}

func setupConnEnv(t *testing.T, withDoor bool) *connEnv {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("conn-unit-test")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	hub := NewApprovalHub(time.Minute)
	srv.SetApprovalHub(hub)
	e := &connEnv{srv: srv, tok: tok, hub: hub, door: newFakeConnDoor()}
	if withDoor {
		srv.SetConnectorDoor(e.door)
	}
	e.h = NewServer(srv)
	return e
}

func (e *connEnv) do(method, path, body string) *httptest.ResponseRecorder {
	return credDo(e.h, method, path, e.tok, body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var mu sync.Mutex
	buf := &lockedBuf{mu: &mu}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf.Buffer
}

type lockedBuf struct {
	bytes.Buffer
	mu *sync.Mutex
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// secretCfg is a valid stdio config laden with fixture secrets in every
// literal-bearing field.
func secretCfg(name string) string {
	return `{"name":"` + name + `","transport":"stdio","command":"/usr/bin/tool",` +
		`"args":["--api-key","` + connSecretArg + `","--mode=fast"],` +
		`"env":{"API_TOKEN":"` + connSecretEnv + `","REGION":"eu"},` +
		`"bearer":"` + connSecretBearer + `"}`
}

const httpCfgTmpl = `{"name":"NAME","transport":"http","url":"https://user:` + connSecretURL + `@api.example.com/mcp/v1?key=` + connSecretURL + `"}`

func cardsOf(t *testing.T, e *connEnv) []CustosCard {
	t.Helper()
	w := e.do(http.MethodGet, "/v1/custos/cards", "")
	var got struct {
		Cards []CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("cards body: %v %q", err, w.Body.String())
	}
	return got.Cards
}

func assertNoSecrets(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range []string{connSecretEnv, connSecretBearer, connSecretURL, connSecretArg} {
		if strings.Contains(text, s) {
			t.Fatalf("%s leaks secret %q: %s", where, s, text)
		}
	}
}

func TestConnectors_Auth_BearerRequired(t *testing.T) {
	e := setupConnEnv(t, true)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/mcp/connectors", ""},
		{http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha")},
		{http.MethodPatch, "/v1/mcp/connectors/alpha", secretCfg("alpha")},
		{http.MethodDelete, "/v1/mcp/connectors/alpha", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/enable", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/disable", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/reauth", ""},
		{http.MethodGet, "/v1/mcp/connectors/events", ""},
		{http.MethodGet, "/v1/mcp/connector-form", ""},
	}
	for _, rt := range routes {
		for _, tok := range []string{"", "not-a-real-token"} {
			w := credDo(e.h, rt.method, rt.path, tok, rt.body)
			if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != `{"error":"unauthorized"}` {
				t.Fatalf("%s %s tok=%q: %d %q", rt.method, rt.path, tok, w.Code, w.Body.String())
			}
		}
	}
	if n := len(e.door.callList()); n != 0 {
		t.Fatalf("door verbs sent before auth: %v", e.door.callList())
	}
}

func TestConnectors_Post_HappyPathRaisesCardNoSecrets(t *testing.T) {
	logBuf := captureLog(t)
	e := setupConnEnv(t, true)

	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["name"] != "alpha" || !ValidApprovalID(resp["card_id"]) || len(resp) != 2 {
		t.Fatalf("202 body = %v", resp)
	}
	assertNoSecrets(t, "202 body", w.Body.String())

	// Probe ran exactly once, with the config; nothing was added yet.
	if e.door.countPrefix("probe") != 1 || e.door.countPrefix("add:") != 0 {
		t.Fatalf("calls = %v", e.door.callList())
	}

	cards := cardsOf(t, e)
	if len(cards) != 1 || cards[0].ID != resp["card_id"] {
		t.Fatalf("cards = %+v", cards)
	}
	review := cards[0].Review
	for _, want := range []string{
		`ADD connector "alpha"`, "transport: stdio", "/usr/bin/tool",
		"fingerprint: abc123def456",
		"read_file [read_only]", "write_file [destructive,open_world]",
		"API_TOKEN", "REGION", "names only", "tools (2)",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("review missing %q:\n%s", want, review)
		}
	}
	assertNoSecrets(t, "card review", review)
	raw, _ := json.Marshal(cards)
	assertNoSecrets(t, "cards JSON", string(raw))
	if !strings.Contains(review, "--mode=fast") || !strings.Contains(review, "[redacted]") {
		t.Fatalf("args not shown/masked as designed:\n%s", review)
	}

	// The pending add is visible on the inventory as not-connected.
	lw := e.do(http.MethodGet, "/v1/mcp/connectors", "")
	assertNoSecrets(t, "inventory", lw.Body.String())
	var inv struct {
		Connectors []ConnectorCard `json:"connectors"`
	}
	_ = json.Unmarshal(lw.Body.Bytes(), &inv)
	if len(inv.Connectors) != 1 || inv.Connectors[0].AppState != AppStateNotConnected ||
		inv.Connectors[0].State != connStatePending {
		t.Fatalf("pending row = %+v", inv.Connectors)
	}
	assertNoSecrets(t, "log", logBuf.String())
}

func TestConnectors_Post_HTTPCardStripsUserinfoAndQuery(t *testing.T) {
	e := setupConnEnv(t, true)
	w := e.do(http.MethodPost, "/v1/mcp/connectors", strings.Replace(httpCfgTmpl, "NAME", "web", 1))
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	review := cardsOf(t, e)[0].Review
	if !strings.Contains(review, "url: https://api.example.com/mcp/v1?…") {
		t.Fatalf("url line:\n%s", review)
	}
	assertNoSecrets(t, "http card", review)
}

func TestConnectors_Post_ValidationTable(t *testing.T) {
	e := setupConnEnv(t, true)
	cases := []struct{ name, body, reason string }{
		{"not json", `nope`, "bad_request"},
		{"array", `[]`, "bad_request"},
		{"unknown key", `{"name":"a","transport":"stdio","command":"/bin/x","zzz":1}`, "unknown_key"},
		{"wrong type", `{"name":"a","transport":"stdio","command":"/bin/x","args":"x"}`, "unknown_key"},
		{"bad list_timeout", `{"name":"a","transport":"stdio","command":"/bin/x","list_timeout":"soon"}`, "unknown_key"},
		{"bad name", `{"name":"A!","transport":"stdio","command":"/bin/x"}`, "bad_name"},
		{"digit first", `{"name":"1a","transport":"stdio","command":"/bin/x"}`, "bad_name"},
		{"reserved", `{"name":"gmail","transport":"stdio","command":"/bin/x"}`, "reserved_name"},
		{"reserved events", `{"name":"events","transport":"stdio","command":"/bin/x"}`, "reserved_name"},
		{"bad transport", `{"name":"a","transport":"ws","command":"/bin/x"}`, "bad_transport"},
		{"missing command", `{"name":"a","transport":"stdio"}`, "missing_key"},
		{"relative command", `{"name":"a","transport":"stdio","command":"tool"}`, "bad_command"},
		{"missing url", `{"name":"a","transport":"http"}`, "missing_key"},
		{"http public host", `{"name":"a","transport":"http","url":"http://example.com/x"}`, "bad_url"},
		{"ftp", `{"name":"a","transport":"http","url":"ftp://10.0.0.1/x"}`, "bad_url"},
		{"max conc low", `{"name":"a","transport":"stdio","command":"/bin/x","max_concurrent_calls":0}`, "bad_max_concurrent"},
		{"max conc high", `{"name":"a","transport":"stdio","command":"/bin/x","max_concurrent_calls":17}`, "bad_max_concurrent"},
	}
	for _, c := range cases {
		w := e.do(http.MethodPost, "/v1/mcp/connectors", c.body)
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusBadRequest || got["reason"] != c.reason || got["error"] == "" {
			t.Fatalf("%s: %d %s (want reason %s)", c.name, w.Code, w.Body.String(), c.reason)
		}
	}
	if len(e.door.callList()) != 0 || len(cardsOf(t, e)) != 0 {
		t.Fatalf("invalid configs must not probe or raise cards: %v", e.door.callList())
	}
	// Accepted shapes: loopback / RFC1918 http.
	for _, u := range []string{"http://127.0.0.1:8080/x", "http://localhost/x", "http://10.1.2.3/x", "http://192.168.0.5/x", "http://172.20.0.1/x"} {
		if _, reason := validateConnectorConfig([]byte(`{"name":"a","transport":"http","url":"` + u + `"}`)); reason != "" {
			t.Fatalf("%s rejected: %s", u, reason)
		}
	}
}

func TestConnectors_Post_ProbeFailureIs400NoCard(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.probeErr = &ConnectorReasonError{Reason: "spawn_failed"}
	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadRequest || got["reason"] != "spawn_failed" {
		t.Fatalf("probe failure = %d %s", w.Code, w.Body.String())
	}
	assertNoSecrets(t, "probe failure body", w.Body.String())
	if len(cardsOf(t, e)) != 0 {
		t.Fatal("card raised on probe failure")
	}
	// The pending gate was released: a retry is not pending_add.
	e.door.probeErr = nil
	if w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha")); w.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
}

func TestConnectors_Post_Conflicts(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "taken", State: "ready"}}, nil)

	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("taken"))
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusConflict || got["reason"] != "duplicate_name" {
		t.Fatalf("duplicate = %d %s", w.Code, w.Body.String())
	}
	if e.door.countPrefix("probe") != 0 {
		t.Fatal("duplicate must not probe")
	}

	if w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("fresh")); w.Code != http.StatusAccepted {
		t.Fatalf("first = %d %s", w.Code, w.Body.String())
	}
	w = e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("fresh"))
	got = nil
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusConflict || got["reason"] != "pending_add" {
		t.Fatalf("pending = %d %s", w.Code, w.Body.String())
	}
	if e.door.countPrefix("probe") != 1 {
		t.Fatalf("pending dup probed again: %v", e.door.callList())
	}
}

func TestConnectors_Allow_TriggersAddAndRecordsMeta(t *testing.T) {
	e := setupConnEnv(t, true)
	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	rw := e.do(http.MethodPost, "/v1/custos/cards/"+resp["card_id"]+"/resolve", `{"verdict":"once"}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", rw.Code, rw.Body.String())
	}
	select {
	case <-e.door.addDone:
	case <-time.After(3 * time.Second):
		t.Fatal("MCP-ADD never sent after allow")
	}
	e.door.mu.Lock()
	sent := string(e.door.lastAdd)
	e.door.mu.Unlock()
	if !strings.Contains(sent, `"name":"alpha"`) || !strings.Contains(sent, connSecretEnv) {
		t.Fatalf("daemon did not get the full config: %s", sent)
	}

	// Daemon now reports the connector; the inventory carries meta, never values.
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 2}},
		map[string][]ConnectorDoorTool{"alpha": e.door.probeRes.Tools})
	waitFor(t, "pending cleared", func() bool {
		e.srv.connLife.mu.Lock()
		defer e.srv.connLife.mu.Unlock()
		return len(e.srv.connLife.pending) == 0
	})
	lw := e.do(http.MethodGet, "/v1/mcp/connectors", "")
	assertNoSecrets(t, "inventory", lw.Body.String())
	var inv struct {
		Connectors []ConnectorCard `json:"connectors"`
	}
	_ = json.Unmarshal(lw.Body.Bytes(), &inv)
	if len(inv.Connectors) != 1 {
		t.Fatalf("inventory = %s", lw.Body.String())
	}
	row := inv.Connectors[0]
	if row.Fingerprint != "abc123def456" || row.AppState != AppStateReadAct || !row.HasWriteTools ||
		len(row.Env) != 2 || row.Env[0] != "API_TOKEN" || !strings.HasPrefix(row.URLOrCommand, "/usr/bin/tool") {
		t.Fatalf("row = %+v", row)
	}
}

func TestConnectors_DenyAndTimeout_LeaveNoState(t *testing.T) {
	for _, mode := range []string{"deny", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			logBuf := captureLog(t)
			e := setupConnEnv(t, true)
			if mode == "timeout" {
				e.srv.SetConnectorCardHold(40 * time.Millisecond)
			}
			w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if mode == "deny" {
				rw := e.do(http.MethodPost, "/v1/custos/cards/"+resp["card_id"]+"/resolve", `{"verdict":"deny"}`)
				if rw.Code != http.StatusOK {
					t.Fatalf("deny resolve = %d %s", rw.Code, rw.Body.String())
				}
			}
			waitFor(t, "denied path", func() bool {
				e.srv.connLife.mu.Lock()
				defer e.srv.connLife.mu.Unlock()
				return len(e.srv.connLife.pending) == 0
			})
			if e.door.countPrefix("add:") != 0 {
				t.Fatalf("MCP-ADD sent on %s: %v", mode, e.door.callList())
			}
			e.srv.connLife.mu.Lock()
			nMeta := len(e.srv.connLife.meta)
			e.srv.connLife.mu.Unlock()
			if nMeta != 0 {
				t.Fatal("meta recorded on denied add")
			}
			waitFor(t, "denied log", func() bool { return strings.Contains(logBuf.String(), "not approved") })
			assertNoSecrets(t, "log", logBuf.String())
			inv := e.do(http.MethodGet, "/v1/mcp/connectors", "")
			if !strings.Contains(inv.Body.String(), `"connectors":[]`) {
				t.Fatalf("inventory after %s = %s", mode, inv.Body.String())
			}
			// Name is free again.
			if w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha")); w.Code != http.StatusAccepted {
				t.Fatalf("re-POST = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestConnectors_Patch_DiffCard(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 2}},
		map[string][]ConnectorDoorTool{"alpha": {{Name: "alpha_a", Flags: "read_only"}, {Name: "shared", Flags: "none"}}})
	e.srv.connLife.mu.Lock()
	e.srv.connLife.meta = map[string]connectorMeta{"alpha": {Fingerprint: "cafe0001", Env: []string{"OLD_VAR", "KEEP"}}}
	e.srv.connLife.mu.Unlock()
	e.door.probeRes = ConnectorProbe{Fingerprint: "cafe0002", Tools: []ConnectorDoorTool{
		{Name: "shared", Flags: "none"}, {Name: "alpha_c", Flags: "read_only"}}}

	body := `{"transport":"stdio","command":"/usr/bin/tool","env":{"KEEP":"` + connSecretEnv + `","NEW_VAR":"` + connSecretBearer + `"}}`
	w := e.do(http.MethodPatch, "/v1/mcp/connectors/alpha", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("PATCH = %d %s", w.Code, w.Body.String())
	}
	assertNoSecrets(t, "202", w.Body.String())
	// The probe saw the injected name.
	e.door.mu.Lock()
	probed := string(e.door.lastProbe)
	e.door.mu.Unlock()
	if !strings.Contains(probed, `"name":"alpha"`) {
		t.Fatalf("probe body lacks injected name: %s", probed)
	}

	review := cardsOf(t, e)[0].Review
	for _, want := range []string{
		`EDIT connector "alpha"`,
		"tools added: alpha_c",
		"tools removed: alpha_a",
		"fingerprint: cafe0001 -> cafe0002",
		"env added: NEW_VAR; removed: OLD_VAR",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("diff review missing %q:\n%s", want, review)
		}
	}
	assertNoSecrets(t, "edit card", review)

	// A second edit while pending → 409 pending_add.
	w = e.do(http.MethodPatch, "/v1/mcp/connectors/alpha", body)
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusConflict || got["reason"] != "pending_add" {
		t.Fatalf("second PATCH = %d %s", w.Code, w.Body.String())
	}

	// Allow → MCP-ADD (replace).
	id := cardsOf(t, e)[0].ID
	if rw := e.do(http.MethodPost, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"once"}`); rw.Code != http.StatusOK {
		t.Fatalf("resolve = %d", rw.Code)
	}
	select {
	case <-e.door.addDone:
	case <-time.After(3 * time.Second):
		t.Fatal("edit allow never sent MCP-ADD")
	}
}

func TestConnectors_Patch_Errors(t *testing.T) {
	e := setupConnEnv(t, true)
	body := `{"transport":"stdio","command":"/usr/bin/tool"}`
	if w := e.do(http.MethodPatch, "/v1/mcp/connectors/ghost", body); w.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown = %d %s", w.Code, w.Body.String())
	}
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready"}}, nil)
	w := e.do(http.MethodPatch, "/v1/mcp/connectors/alpha", `{"name":"beta","transport":"stdio","command":"/usr/bin/tool"}`)
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadRequest || got["reason"] != "bad_name" {
		t.Fatalf("name mismatch = %d %s", w.Code, w.Body.String())
	}
	if e.door.countPrefix("probe") != 0 {
		t.Fatal("errors must not probe")
	}
}

func TestConnectors_Delete_NoCardMapsToRemove(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready"}}, nil)
	w := e.do(http.MethodDelete, "/v1/mcp/connectors/alpha", "")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("DELETE = %d %q", w.Code, w.Body.String())
	}
	if c := e.door.callList(); len(c) != 1 || c[0] != "remove:alpha" {
		t.Fatalf("calls = %v", c)
	}
	if len(cardsOf(t, e)) != 0 {
		t.Fatal("DELETE raised a card")
	}

	// Idempotent: a not_found refusal is still 204.
	e.door.removeErr = &ConnectorReasonError{Reason: "not_found"}
	if w := e.do(http.MethodDelete, "/v1/mcp/connectors/ghost", ""); w.Code != http.StatusNoContent {
		t.Fatalf("idempotent DELETE = %d %s", w.Code, w.Body.String())
	}
	// Other failures are 502.
	e.door.removeErr = &ConnectorReasonError{Reason: "store_failed"}
	w = e.do(http.MethodDelete, "/v1/mcp/connectors/alpha", "")
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadGateway || got["reason"] != "store_failed" {
		t.Fatalf("failed DELETE = %d %s", w.Code, w.Body.String())
	}
	e.door.removeErr = errors.New("boom")
	if w := e.do(http.MethodDelete, "/v1/mcp/connectors/alpha", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("transport DELETE = %d", w.Code)
	}
}

func TestConnectors_Delete_CancelsPendingEdit(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready"}}, nil)
	if w := e.do(http.MethodPatch, "/v1/mcp/connectors/alpha", `{"transport":"stdio","command":"/usr/bin/tool"}`); w.Code != http.StatusAccepted {
		t.Fatalf("PATCH = %d", w.Code)
	}
	if w := e.do(http.MethodDelete, "/v1/mcp/connectors/alpha", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	waitFor(t, "pending cancelled", func() bool {
		e.srv.connLife.mu.Lock()
		defer e.srv.connLife.mu.Unlock()
		return len(e.srv.connLife.pending) == 0
	})
	if e.door.countPrefix("add:") != 0 {
		t.Fatal("cancelled edit still sent MCP-ADD")
	}
}

func TestConnectors_EnableDisable_NoCard(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 1}},
		map[string][]ConnectorDoorTool{"alpha": {{Name: "t", Flags: "read_only"}}})
	e.door.onSetState = func(name string, enabled bool) {
		st := ConnectorDoorStatus{Name: name, State: "ready", Tools: 1}
		if !enabled {
			st = ConnectorDoorStatus{Name: name, State: "disabled", Reason: "user_disabled"}
		}
		e.door.set([]ConnectorDoorStatus{st}, map[string][]ConnectorDoorTool{"alpha": {{Name: "t", Flags: "read_only"}}})
	}

	w := e.do(http.MethodPost, "/v1/mcp/connectors/alpha/disable", "")
	var row ConnectorCard
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil || w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body.String())
	}
	if row.Name != "alpha" || row.Enabled || row.AppState != AppStateDisabled {
		t.Fatalf("disable row = %+v", row)
	}
	w = e.do(http.MethodPost, "/v1/mcp/connectors/alpha/enable", "")
	_ = json.Unmarshal(w.Body.Bytes(), &row)
	if w.Code != http.StatusOK || !row.Enabled || row.AppState != AppStateReadOnly {
		t.Fatalf("enable = %d %+v", w.Code, row)
	}
	if c := e.door.callList(); len(c) != 2 || c[0] != "state:alpha:disabled" || c[1] != "state:alpha:enabled" {
		t.Fatalf("calls = %v", c)
	}
	if len(cardsOf(t, e)) != 0 {
		t.Fatal("toggle raised a card")
	}

	e.door.stateErr = &ConnectorReasonError{Reason: "not_found"}
	if w := e.do(http.MethodPost, "/v1/mcp/connectors/ghost/enable", ""); w.Code != http.StatusNotFound {
		t.Fatalf("enable unknown = %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/v1/mcp/connectors/alpha/enable", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET enable = %d", w.Code)
	}
}

func TestConnectors_Reauth_RidesRefresh(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "disabled", Reason: "credential_unavailable"}}, nil)

	// Failure: frozen reason surfaced, state stays needs-re-auth.
	e.door.refreshErr = &ConnectorReasonError{Reason: "credential_unavailable"}
	w := e.do(http.MethodPost, "/v1/mcp/connectors/alpha/reauth", "")
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusConflict || got["reason"] != "credential_unavailable" {
		t.Fatalf("failed reauth = %d %s", w.Code, w.Body.String())
	}
	row, _ := e.srv.connectorRow("alpha")
	if row.AppState != AppStateNeedsReauth {
		t.Fatalf("state after failed reauth = %s", row.AppState)
	}

	// Success: refresh re-ensures, rail returns to ready, row returned.
	e.door.refreshErr = nil
	e.door.onRefreshOK = func(name string) {
		e.door.set([]ConnectorDoorStatus{{Name: name, State: "ready", Tools: 1}},
			map[string][]ConnectorDoorTool{name: {{Name: "t", Flags: "read_only"}}})
	}
	w = e.do(http.MethodPost, "/v1/mcp/connectors/alpha/reauth", "")
	var okRow ConnectorCard
	_ = json.Unmarshal(w.Body.Bytes(), &okRow)
	if w.Code != http.StatusOK || okRow.AppState != AppStateReadOnly {
		t.Fatalf("reauth = %d %s", w.Code, w.Body.String())
	}
	if e.door.countPrefix("refresh:alpha") != 2 || len(cardsOf(t, e)) != 0 {
		t.Fatalf("calls = %v", e.door.callList())
	}
}

func TestConnectors_DoorErrorMapping(t *testing.T) {
	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready"}}, nil)

	e.door.probeErr = ErrConnectorDaemonTooOld
	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("fresh"))
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadGateway || got["reason"] != "daemon_too_old" || !strings.Contains(got["error"], "daemon too old") {
		t.Fatalf("too old = %d %s", w.Code, w.Body.String())
	}
	e.door.probeErr = ErrConnectorDoorDown
	if w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("fresh")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("door down = %d", w.Code)
	}
	e.door.probeErr = ErrLocked
	if w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("fresh")); w.Code != http.StatusLocked {
		t.Fatalf("locked = %d", w.Code)
	}
	e.door.removeErr = ErrConnectorDaemonTooOld
	if w := e.do(http.MethodDelete, "/v1/mcp/connectors/alpha", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("delete too old = %d", w.Code)
	}
	e.door.stateErr = ErrConnectorDaemonTooOld
	if w := e.do(http.MethodPost, "/v1/mcp/connectors/alpha/disable", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("state too old = %d", w.Code)
	}
}

func TestConnectors_DoorAbsent_503(t *testing.T) {
	e := setupConnEnv(t, false)
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha")},
		{http.MethodPatch, "/v1/mcp/connectors/alpha", secretCfg("alpha")},
		{http.MethodDelete, "/v1/mcp/connectors/alpha", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/enable", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/disable", ""},
		{http.MethodPost, "/v1/mcp/connectors/alpha/reauth", ""},
		{http.MethodGet, "/v1/mcp/connectors/events", ""},
	} {
		w := e.do(rt.method, rt.path, rt.body)
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusServiceUnavailable || got["reason"] != "door_not_connected" || got["error"] == "" {
			t.Fatalf("%s %s = %d %s", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
	// Legacy inventory + static form still answer.
	if w := e.do(http.MethodGet, "/v1/mcp/connectors", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"connectors":[]`) {
		t.Fatalf("GET inventory = %d %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodGet, "/v1/mcp/connector-form", ""); w.Code != http.StatusOK {
		t.Fatalf("form = %d", w.Code)
	}
}

func TestConnectors_ApprovalsAbsent_503(t *testing.T) {
	e := setupConnEnv(t, true)
	e.srv.SetApprovalHub(nil)
	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
	if w.Code != http.StatusServiceUnavailable || e.door.countPrefix("probe") != 0 {
		t.Fatalf("no hub = %d %s calls=%v", w.Code, w.Body.String(), e.door.callList())
	}
}

func TestConnectors_Form_ShapeAndNoHostData(t *testing.T) {
	e := setupConnEnv(t, false)
	w := e.do(http.MethodGet, "/v1/mcp/connector-form", "")
	if w.Code != http.StatusOK {
		t.Fatalf("form = %d", w.Code)
	}
	var form struct {
		SchemaVersion int `json:"schema_version"`
		Fields        []struct {
			Key        string         `json:"key"`
			Type       string         `json:"type"`
			Required   bool           `json:"required"`
			Default    any            `json:"default"`
			Allowed    []string       `json:"allowed"`
			Validation map[string]any `json:"validation"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &form); err != nil {
		t.Fatal(err)
	}
	if form.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d", form.SchemaVersion)
	}
	types := map[string]bool{"string": true, "string_list": true, "secret_string": true, "enum": true, "int": true, "duration": true}
	byKey := map[string]int{}
	for i, f := range form.Fields {
		byKey[f.Key] = i
		if !types[f.Type] {
			t.Fatalf("field %s has type %q outside D8", f.Key, f.Type)
		}
	}
	for _, k := range []string{"name", "transport", "command", "args", "url", "env", "bearer", "tool_allowlist", "tool_denylist", "max_concurrent_calls", "list_timeout"} {
		if _, ok := byKey[k]; !ok {
			t.Fatalf("form missing field %q", k)
		}
	}
	name := form.Fields[byKey["name"]]
	if !name.Required || name.Type != "string" || name.Validation["pattern"] != ConnectorNamePattern {
		t.Fatalf("name field = %+v", name)
	}
	tr := form.Fields[byKey["transport"]]
	if tr.Type != "enum" || len(tr.Allowed) != 2 || tr.Allowed[0] != "stdio" || tr.Allowed[1] != "http" {
		t.Fatalf("transport = %+v", tr)
	}
	if d := form.Fields[byKey["max_concurrent_calls"]].Default; d != float64(ConnectorDefaultMaxConcurrent) {
		t.Fatalf("max_concurrent default = %v", d)
	}
	if d := form.Fields[byKey["list_timeout"]].Default; d != ConnectorDefaultListTimeout {
		t.Fatalf("list_timeout default = %v", d)
	}
	if form.Fields[byKey["env"]].Type != "secret_string" || form.Fields[byKey["bearer"]].Type != "secret_string" {
		t.Fatal("env/bearer must be secret_string")
	}

	// No per-host data.
	body := w.Body.String()
	host, _ := os.Hostname()
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	for _, bad := range []string{host, wd, home, os.TempDir() + "/", e.tok} {
		if bad != "" && bad != "/" && strings.Contains(body, bad) {
			t.Fatalf("form leaks host-specific value %q", bad)
		}
	}
	// Static: two calls are byte-identical.
	if w2 := e.do(http.MethodGet, "/v1/mcp/connector-form", ""); w2.Body.String() != body {
		t.Fatal("form is not static")
	}
}

func TestConnectorAppState_Matrix(t *testing.T) {
	ro := []ConnectorTool{{Name: "a", ReadOnly: true}, {Name: "b", ReadOnly: true}}
	mixed := []ConnectorTool{{Name: "a", ReadOnly: true}, {Name: "b"}}
	cases := []struct {
		name    string
		state   string
		reason  string
		enabled bool
		tools   []ConnectorTool
		want    string
	}{
		{"starting", "starting", "", true, nil, AppStateNotConnected},
		{"backoff", "backoff", "spawn_failed", true, nil, AppStateNotConnected},
		{"backoff list_timeout", "backoff", "list_timeout", true, ro, AppStateNotConnected},
		{"pending approval", connStatePending, connReasonPendingAppvl, true, nil, AppStateNotConnected},
		{"name occupied", "disabled", "name_occupied", true, nil, AppStateNotConnected},
		{"credential latch", "disabled", "credential_unavailable", true, ro, AppStateNeedsReauth},
		{"credential reason", "backoff", "credential", true, nil, AppStateNeedsReauth},
		{"credential beats ready", "ready", "credential", true, ro, AppStateNeedsReauth},
		{"user disabled", "disabled", "user_disabled", false, ro, AppStateDisabled},
		{"ready all read-only", "ready", "ok", true, ro, AppStateReadOnly},
		{"ready no tools", "ready", "ok", true, nil, AppStateReadOnly},
		{"ready mixed", "ready", "ok", true, mixed, AppStateReadAct},
		{"ready unhinted", "ready", "ok", true, []ConnectorTool{{Name: "x"}}, AppStateReadAct},
	}
	for _, c := range cases {
		if got := ConnectorAppState(c.state, c.reason, c.enabled, c.tools); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	// connectorUserEnabled drives "enabled".
	if connectorUserEnabled("disabled", "user_disabled") || !connectorUserEnabled("disabled", "name_occupied") ||
		!connectorUserEnabled("ready", "ok") {
		t.Fatal("connectorUserEnabled wrong")
	}
	// flags parsing.
	tl := parseConnectorTool(ConnectorDoorTool{Name: "x", Flags: "read_only,destructive,open_world"})
	if !tl.ReadOnly || !tl.Destructive || !tl.OpenWorld {
		t.Fatalf("flags = %+v", tl)
	}
	if tl := parseConnectorTool(ConnectorDoorTool{Name: "x", Flags: "none"}); tl.ReadOnly {
		t.Fatal("none must not be read-only")
	}
}

func TestConnectors_SnapshotGrowth(t *testing.T) {
	e := setupConnEnv(t, true)
	used := "2026-11-01T08:00:00Z"
	e.door.set([]ConnectorDoorStatus{
		{Name: "alpha", State: "ready", Tools: 2, Restarts: 1, LastUsed: &used},
		{Name: "beta", State: "backoff", Reason: "spawn_failed"},
		{Name: "gamma", State: "disabled", Reason: "user_disabled"},
	}, map[string][]ConnectorDoorTool{"alpha": {{Name: "r", Flags: "read_only"}, {Name: "w", Flags: "none"}}})
	e.srv.connLife.mu.Lock()
	e.srv.connLife.meta = map[string]connectorMeta{"alpha": {Fingerprint: "fp1", URLOrCommand: "https://h/x", Env: []string{"A"}}}
	e.srv.connLife.mu.Unlock()

	w := e.do(http.MethodGet, "/v1/mcp/connectors", "")
	var raw struct {
		Connectors []map[string]any `json:"connectors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil || len(raw.Connectors) != 3 {
		t.Fatalf("inventory = %d %s", w.Code, w.Body.String())
	}
	a := raw.Connectors[0]
	for _, k := range []string{"name", "state", "tools", "restarts", "reason", "app_state", "enabled", "tool_list", "has_write_tools", "fingerprint", "last_used", "url_or_command", "env"} {
		if _, ok := a[k]; !ok && k != "reason" {
			t.Fatalf("row missing %q: %v", k, a)
		}
	}
	if a["tools"] != float64(2) || a["has_write_tools"] != true || a["last_used"] != used ||
		a["fingerprint"] != "fp1" || a["app_state"] != AppStateReadAct {
		t.Fatalf("alpha = %v", a)
	}
	b := raw.Connectors[1]
	if b["last_used"] != nil || b["app_state"] != AppStateNotConnected || b["fingerprint"] != "" {
		t.Fatalf("beta = %v", b)
	}
	if g := raw.Connectors[2]; g["enabled"] != false || g["app_state"] != AppStateDisabled {
		t.Fatalf("gamma = %v", g)
	}
	// env/tool_list are arrays, never null.
	if _, ok := b["env"].([]any); !ok {
		t.Fatalf("env not an array: %v", b["env"])
	}
	if _, ok := b["tool_list"].([]any); !ok {
		t.Fatalf("tool_list not an array: %v", b["tool_list"])
	}
}

func TestSummarizeURL_StripsUserinfo(t *testing.T) {
	cases := map[string]string{
		"scheme://user:pass@host/x":            "scheme://host/x",
		"https://u:p@api.example.com:8443/a/b": "https://api.example.com:8443/a/b",
		"http://127.0.0.1:1/x?token=abc#frag":  "http://127.0.0.1:1/x?…",
		"https://token@host/":                  "https://host/",
	}
	for in, want := range cases {
		if got := summarizeURL(in); got != want {
			t.Errorf("summarizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	args := redactArgs([]string{"--token", "SEKRIT", "--api-key=SEKRIT2", "--url", "https://u:SEKRIT3@h/x", "--verbose", "--password", "--flag"})
	joined := strings.Join(args, " ")
	for _, s := range []string{"SEKRIT", "SEKRIT2", "SEKRIT3"} {
		if strings.Contains(joined, s) {
			t.Fatalf("redactArgs leaked %s: %s", s, joined)
		}
	}
	if !strings.Contains(joined, "--verbose") || !strings.Contains(joined, "https://h/x") {
		t.Fatalf("redactArgs over-masked: %s", joined)
	}
	// vault refs stay visible.
	if got := redactArgs([]string{"--token=${vault:svc}"}); got[0] != "--token=${vault:svc}" {
		t.Fatalf("ref masked: %v", got)
	}
}

// readSSE streams events from url until ctx is done, delivering "event\ndata".
func readSSE(t *testing.T, ctx context.Context, url, tok string) <-chan [2]string {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan [2]string, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			case strings.HasPrefix(line, ": ping"):
				out <- [2]string{"ping", ""}
			}
		}
	}()
	return out
}

func nextSSE(t *testing.T, ch <-chan [2]string, skipPing bool) [2]string {
	t.Helper()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			if skipPing && ev[0] == "ping" {
				continue
			}
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("no SSE event")
		}
	}
}

func TestConnectors_SSE_StateUpsertAndGone(t *testing.T) {
	old := custosHeartbeatInterval
	custosHeartbeatInterval = 30 * time.Millisecond
	defer func() { custosHeartbeatInterval = old }()

	e := setupConnEnv(t, true)
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "starting"}}, nil)
	ts := httptest.NewServer(e.h)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := readSSE(t, ctx, ts.URL+"/v1/mcp/connectors/events", e.tok)

	// Silent bootstrap: the first thing on the wire is a heartbeat, not a row.
	if ev := nextSSE(t, ch, false); ev[0] != "ping" {
		t.Fatalf("first frame = %v, want ping", ev)
	}

	// Door status injection → full-row upsert.
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 1}},
		map[string][]ConnectorDoorTool{"alpha": {{Name: "t", Flags: "read_only"}}})
	ev := nextSSE(t, ch, true)
	if ev[0] != "connector_state" {
		t.Fatalf("event = %v", ev)
	}
	var row ConnectorCard
	if err := json.Unmarshal([]byte(ev[1]), &row); err != nil {
		t.Fatal(err)
	}
	if row.Name != "alpha" || row.State != "ready" || row.AppState != AppStateReadOnly || len(row.ToolList) != 1 {
		t.Fatalf("row = %+v", row)
	}

	// Tools-only change re-emits a full row too.
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 2}},
		map[string][]ConnectorDoorTool{"alpha": {{Name: "t", Flags: "read_only"}, {Name: "u", Flags: "none"}}})
	ev = nextSSE(t, ch, true)
	_ = json.Unmarshal([]byte(ev[1]), &row)
	if ev[0] != "connector_state" || row.AppState != AppStateReadAct || len(row.ToolList) != 2 {
		t.Fatalf("tools event = %v %+v", ev, row)
	}

	// Name vanishes → synthesized connector_gone.
	e.door.set(nil, nil)
	ev = nextSSE(t, ch, true)
	if ev[0] != "connector_gone" || ev[1] != `{"name":"alpha"}` {
		t.Fatalf("gone event = %v", ev)
	}
}

func TestConnectors_SSE_NoSecretsInPayload(t *testing.T) {
	e := setupConnEnv(t, true)
	ts := httptest.NewServer(e.h)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := readSSE(t, ctx, ts.URL+"/v1/mcp/connectors/events", e.tok)

	// Run a literal-secret add to approval; the pending row and committed
	// row both ride the rail.
	w := e.do(http.MethodPost, "/v1/mcp/connectors", secretCfg("alpha"))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	ev := nextSSE(t, ch, true)
	if ev[0] != "connector_state" || !strings.Contains(ev[1], `"pending"`) {
		t.Fatalf("pending event = %v", ev)
	}
	assertNoSecrets(t, "pending event", ev[1])
	e.do(http.MethodPost, "/v1/custos/cards/"+resp["card_id"]+"/resolve", `{"verdict":"once"}`)
	<-e.door.addDone
	e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: 2}},
		map[string][]ConnectorDoorTool{"alpha": e.door.probeRes.Tools})
	for i := 0; i < 3; i++ {
		select {
		case ev := <-ch:
			assertNoSecrets(t, "event "+ev[0], ev[1])
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// stallWriter blocks every Write until released, modelling a stalled client.
type stallWriter struct {
	hdr     http.Header
	release chan struct{}
}

func (w *stallWriter) Header() http.Header { return w.hdr }
func (w *stallWriter) WriteHeader(int)     {}
func (w *stallWriter) Flush()              {}
func (w *stallWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

func TestConnectors_SSE_OverflowDisconnects(t *testing.T) {
	e := setupConnEnv(t, true)
	sw := &stallWriter{hdr: http.Header{}, release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/connectors/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.srv.connectorEventsHandler(sw, req)
	}()
	waitFor(t, "subscribed", func() bool {
		e.door.mu.Lock()
		defer e.door.mu.Unlock()
		return len(e.door.pokes) > 0
	})
	// The first event wedges the handler in Write; later rows pile into the
	// 64-slot buffer and overflow it.
	for i := 0; i < 200; i++ {
		e.door.set([]ConnectorDoorStatus{{Name: "alpha", State: "ready", Tools: i}}, nil)
	}
	close(sw.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("overflow did not end the stream")
	}
}
