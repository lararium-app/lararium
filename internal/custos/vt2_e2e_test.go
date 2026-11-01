package custos_test

// V-T2 suite (CONNECTOR-LIFE-SPEC "Exit criterion"): end-to-end proofs for
// connector lifecycle and dynamic management (T2). Real components only: real
// custosd, real DoorServer, real custosdoor client, real surface.Server, real
// in-process fixture children.
//
// Run all nine proofs together with:
//
//	go test -run 'VT2' ./internal/custos/ -count=1 -v

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

// vt2Needle is the distinctive fixture secret for V-T2 secret leak proofs.
const vt2Needle = "fx-liv-VT2SUITE-00000000000000000000-4321"

// ---------------------------------------------------------------------------
// Adapters and harness
// ---------------------------------------------------------------------------

// vt2DoorAdapter bridges *custosdoor.Client onto surface.CustosRegistry and
// surface.MCPConnectorsSource.
type vt2DoorAdapter struct {
	cl *custosdoor.Client
}

func (a *vt2DoorAdapter) Snapshot() []surface.CustosCard {
	snap := a.cl.Snapshot()
	out := make([]surface.CustosCard, 0, len(snap))
	for _, c := range snap {
		out = append(out, surface.CustosCard{
			ID:         c.ID,
			Cell:       c.Cell,
			Cred:       c.Cred,
			Tool:       c.Tool,
			Dest:       c.Dest,
			Review:     c.Review,
			AgeS:       c.AgeS,
			ExpiresInS: c.ExpiresInS,
		})
	}
	return out
}

func (a *vt2DoorAdapter) ConnectorSnapshot() []surface.ConnectorCard {
	statuses := a.cl.Statuses()
	out := make([]surface.ConnectorCard, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, surface.ConnectorCard{
			Name:     st.Name,
			State:    st.State,
			Tools:    st.Tools,
			Restarts: st.Restarts,
			Reason:   st.Reason,
		})
	}
	return out
}

func (a *vt2DoorAdapter) Subscribe(f func(card *surface.CustosCard, gone *surface.CustosGone)) func() {
	return a.cl.Subscribe(func(c *custosdoor.Card, g *custosdoor.Gone) {
		var sc *surface.CustosCard
		if c != nil {
			sc = &surface.CustosCard{
				ID:         c.ID,
				Cell:       c.Cell,
				Cred:       c.Cred,
				Tool:       c.Tool,
				Dest:       c.Dest,
				Review:     c.Review,
				AgeS:       c.AgeS,
				ExpiresInS: c.ExpiresInS,
			}
		}
		var sg *surface.CustosGone
		if g != nil {
			sg = &surface.CustosGone{ID: g.ID, State: g.State, Reason: g.Reason}
		}
		f(sc, sg)
	})
}

func (a *vt2DoorAdapter) Resolve(id, verdict string) (string, error) {
	st, err := a.cl.Resolve(id, verdict)
	switch {
	case err == nil:
		return st, nil
	case errors.Is(err, custosdoor.ErrNoSuchCard):
		return "", surface.ErrNoSuchCard
	case errors.Is(err, custosdoor.ErrCardAnswered):
		return "", surface.ErrCardAnswered
	case errors.Is(err, custosdoor.ErrStaleVerdict):
		return "", surface.ErrStaleVerdict
	case errors.Is(err, custosdoor.ErrLocked):
		return "", surface.ErrLocked
	case errors.Is(err, custosdoor.ErrIPAskOnly):
		return "", surface.ErrIPAskOnly
	default:
		return "", err
	}
}

func (a *vt2DoorAdapter) Attach() ([]surface.CustosCard, func(), <-chan surface.CustosEvent) {
	ch := make(chan surface.CustosEvent)
	close(ch)
	return a.Snapshot(), func() {}, ch
}

// vt2ConnectorDoorAdapter adapts *custosdoor.Client to surface.ConnectorDoor.
type vt2ConnectorDoorAdapter struct {
	client *custosdoor.Client
}

func (a *vt2ConnectorDoorAdapter) ConnectorStatuses() []surface.ConnectorDoorStatus {
	sts := a.client.Statuses()
	out := make([]surface.ConnectorDoorStatus, 0, len(sts))
	for _, st := range sts {
		out = append(out, surface.ConnectorDoorStatus{
			Name: st.Name, State: st.State, Reason: st.Reason,
			Tools: st.Tools, Restarts: st.Restarts, LastUsed: st.LastUsed,
		})
	}
	return out
}

func (a *vt2ConnectorDoorAdapter) ConnectorTools(name string) []surface.ConnectorDoorTool {
	tools := a.client.ToolsOf(name)
	out := make([]surface.ConnectorDoorTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, surface.ConnectorDoorTool{Name: t.Name, Flags: t.Flags})
	}
	return out
}

func (a *vt2ConnectorDoorAdapter) SubscribeConnectors(poke func()) func() {
	return a.client.SubscribeMCP(func([]custosdoor.ConnectorStatus, []custosdoor.ToolInfo) { poke() })
}

func (a *vt2ConnectorDoorAdapter) Probe(ctx context.Context, configJSON []byte) (surface.ConnectorProbe, error) {
	res, err := a.client.Probe(ctx, configJSON)
	if err != nil {
		return surface.ConnectorProbe{}, mapVT2DoorErr(err)
	}
	tools := make([]surface.ConnectorDoorTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		tools = append(tools, surface.ConnectorDoorTool{Name: t.Name, Flags: t.Flags})
	}
	return surface.ConnectorProbe{Fingerprint: res.Fingerprint, Tools: tools}, nil
}

func (a *vt2ConnectorDoorAdapter) Add(ctx context.Context, name string, configJSON []byte) error {
	return mapVT2DoorErr(a.client.Add(ctx, name, configJSON))
}

func (a *vt2ConnectorDoorAdapter) Remove(ctx context.Context, name string) error {
	return mapVT2DoorErr(a.client.Remove(ctx, name))
}

func (a *vt2ConnectorDoorAdapter) SetState(ctx context.Context, name string, enabled bool) error {
	return mapVT2DoorErr(a.client.SetState(ctx, name, enabled))
}

func (a *vt2ConnectorDoorAdapter) Refresh(ctx context.Context, name string) error {
	return mapVT2DoorErr(a.client.Refresh(ctx, name))
}

var vt2ReasonRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func mapVT2DoorErr(err error) error {
	var le *custosdoor.LifecycleError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, custosdoor.ErrDoorDown), errors.Is(err, custosdoor.ErrBadToken):
		return surface.ErrConnectorDoorDown
	case errors.Is(err, custosdoor.ErrVerbUnsupported):
		return surface.ErrConnectorDaemonTooOld
	case errors.Is(err, custosdoor.ErrLocked):
		return surface.ErrLocked
	case errors.As(err, &le):
		return &surface.ConnectorReasonError{Reason: le.Reason}
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "connector ") && strings.HasSuffix(msg, " not found"):
		return &surface.ConnectorReasonError{Reason: "not_found"}
	case vt2ReasonRe.MatchString(msg):
		return &surface.ConnectorReasonError{Reason: msg}
	}
	return err
}

// vt2World is the hermetic, in-process environment for V-T2 proofs.
type vt2World struct {
	t          *testing.T
	dir        string
	stateDir   string
	d          *custos.Daemon
	hub        *surface.ApprovalHub
	doorClient *custosdoor.Client
	doorCancel context.CancelFunc
	token      string
	srv        *surface.Server
	tsrv       *httptest.Server
	client     *http.Client
	logbuf     *safeBuf
}

func newVT2World(t *testing.T, seed ...custos.MCPServerConfig) *vt2World {
	t.Helper()
	w := &vt2World{t: t, logbuf: &safeBuf{}}

	prevW, prevF := log.Writer(), log.Flags()
	log.SetOutput(w.logbuf)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevF) })

	w.dir = t.TempDir()
	w.stateDir = filepath.Join(w.dir, "custos")
	if err := os.MkdirAll(w.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &custos.Config{
		AskHoldTimeout: 30 * time.Second,
	}
	cfg.Connectors.MCP = seed
	w.d = custos.NewDaemon(w.stateDir, cfg)
	v := w.d.Vault()
	if !v.Exists() {
		if err := v.Init(lcPass); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Unlock(lcPass, false); err != nil {
		t.Fatal(err)
	}
	if err := w.d.Start(); err != nil {
		t.Fatal(err)
	}
	w.hub = w.d.Hub()

	sockPath, tokenPath := custos.DoorPaths(w.stateDir)
	attachedCh := make(chan struct{}, 1)
	var err error
	w.doorClient, err = custosdoor.New(custosdoor.Options{
		SockPath:   sockPath,
		TokenPath:  tokenPath,
		Name:       "hearthd",
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	doorCtx, doorCancel := context.WithCancel(context.Background())
	w.doorCancel = doorCancel

	unsubMCP := w.doorClient.SubscribeMCP(func([]custosdoor.ConnectorStatus, []custosdoor.ToolInfo) {
		select {
		case attachedCh <- struct{}{}:
		default:
		}
	})
	defer unsubMCP()

	go w.doorClient.Run(doorCtx)

	select {
	case <-attachedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("doorClient did not attach to DoorServer")
	}

	tokenStore, err := surface.OpenTokenStore(filepath.Join(w.dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	w.token, err = tokenStore.Create("vt2-admin")
	if err != nil {
		t.Fatal(err)
	}

	w.srv = &surface.Server{
		Store: tokenStore,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	w.srv.SetApprovalHub(w.hub)
	w.srv.SetCustosRegistry(&vt2DoorAdapter{cl: w.doorClient})
	w.srv.SetConnectorDoor(&vt2ConnectorDoorAdapter{client: w.doorClient})
	w.srv.SetCredentialSource(vt1CredAdapter{v: v})

	w.tsrv = httptest.NewServer(surface.NewServer(w.srv))
	t.Cleanup(w.tsrv.Close)
	w.client = w.tsrv.Client()

	t.Cleanup(w.cleanup)
	return w
}

func (w *vt2World) cleanup() {
	if w.doorCancel != nil {
		w.doorCancel()
	}
	if w.d != nil {
		w.d.Stop()
		for _, c := range w.d.Rail().Connectors() {
			c.Shutdown(false)
		}
	}
}

func (w *vt2World) stopDaemon() {
	if w.doorCancel != nil {
		w.doorCancel()
	}
	if w.d != nil {
		w.d.Stop()
		for _, c := range w.d.Rail().Connectors() {
			c.Shutdown(false)
		}
	}
}

func (w *vt2World) restartDaemon(seed ...custos.MCPServerConfig) {
	cfg := &custos.Config{
		AskHoldTimeout: 30 * time.Second,
	}
	cfg.Connectors.MCP = seed
	w.d = custos.NewDaemon(w.stateDir, cfg)
	v := w.d.Vault()
	if err := v.Unlock(lcPass, false); err != nil {
		w.t.Fatalf("restart vault unlock: %v", err)
	}
	if err := w.d.Start(); err != nil {
		w.t.Fatalf("restart daemon start: %v", err)
	}
	w.hub = w.d.Hub()

	sockPath, tokenPath := custos.DoorPaths(w.stateDir)
	attachedCh := make(chan struct{}, 1)
	var err error
	w.doorClient, err = custosdoor.New(custosdoor.Options{
		SockPath:   sockPath,
		TokenPath:  tokenPath,
		Name:       "hearthd",
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		w.t.Fatalf("restart doorClient: %v", err)
	}
	doorCtx, doorCancel := context.WithCancel(context.Background())
	w.doorCancel = doorCancel

	unsubMCP := w.doorClient.SubscribeMCP(func([]custosdoor.ConnectorStatus, []custosdoor.ToolInfo) {
		select {
		case attachedCh <- struct{}{}:
		default:
		}
	})
	defer unsubMCP()

	go w.doorClient.Run(doorCtx)

	select {
	case <-attachedCh:
	case <-time.After(5 * time.Second):
		w.t.Fatal("restart doorClient did not attach")
	}

	w.srv.SetApprovalHub(w.hub)
	w.srv.SetCustosRegistry(&vt2DoorAdapter{cl: w.doorClient})
	w.srv.SetConnectorDoor(&vt2ConnectorDoorAdapter{client: w.doorClient})
	w.srv.SetCredentialSource(vt1CredAdapter{v: v})
}

func (w *vt2World) postJSON(path, body string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.tsrv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt2World) patchJSON(path, body string) (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPatch, w.tsrv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt2World) deleteReq(path string) int {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, w.tsrv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (w *vt2World) getJSON() (string, int) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.tsrv.URL+"/v1/mcp/connectors", nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String(), resp.StatusCode
}

func (w *vt2World) waitForReady(name string) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range w.doorClient.Statuses() {
			if st.Name == name && st.State == "ready" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("timed out waiting for connector %q to become ready", name)
}

// ---------------------------------------------------------------------------
// SSE stream test helper
// ---------------------------------------------------------------------------

type vt2SSEEvent struct {
	Kind string
	Data string
}

type vt2SSEReader struct {
	resp   *http.Response
	r      *bufio.Reader
	events chan vt2SSEEvent
	errs   chan error
	done   chan struct{}
}

func (w *vt2World) openSSE(t *testing.T, path string) *vt2SSEReader {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.tsrv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("SSE GET %s = %d", path, resp.StatusCode)
	}

	reader := &vt2SSEReader{
		resp:   resp,
		r:      bufio.NewReader(resp.Body),
		events: make(chan vt2SSEEvent, 64),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}
	t.Cleanup(func() {
		close(reader.done)
		_ = resp.Body.Close()
	})

	go func() {
		var curEvent, curData string
		for {
			select {
			case <-reader.done:
				return
			default:
			}
			line, err := reader.r.ReadString('\n')
			if err != nil {
				select {
				case <-reader.done:
				case reader.errs <- err:
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, ":") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				curEvent = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				curData = strings.TrimPrefix(line, "data: ")
			case line == "" && curEvent != "":
				ev := vt2SSEEvent{Kind: curEvent, Data: curData}
				curEvent, curData = "", ""
				select {
				case <-reader.done:
					return
				case reader.events <- ev:
				}
			}
		}
	}()

	return reader
}

func (r *vt2SSEReader) nextEvent(t *testing.T) vt2SSEEvent {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	case err := <-r.errs:
		t.Fatalf("SSE stream error: %v", err)
		return vt2SSEEvent{}
	case <-time.After(5 * time.Second):
		t.Fatalf("SSE read timed out")
		return vt2SSEEvent{}
	}
}

// assertNoNeedle asserts neither raw needle nor standard/raw/URL base64 forms
// appear in the payload.
func assertNoNeedle(t *testing.T, label, data string) {
	t.Helper()
	needle := vt2Needle
	if strings.Contains(data, needle) {
		t.Errorf("%s contains secret needle", label)
	}
	encs := []string{
		base64.StdEncoding.EncodeToString([]byte(needle)),
		base64.RawStdEncoding.EncodeToString([]byte(needle)),
		base64.URLEncoding.EncodeToString([]byte(needle)),
		base64.RawURLEncoding.EncodeToString([]byte(needle)),
	}
	// Alignment variants (same trick as mcp_lifecycle_verbs_test.go): the
	// needle hidden inside a longer base64 blob still leaves a body
	// fragment that must not appear anywhere.
	for off := 1; off < 3; off++ {
		enc := base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("x", off) + needle))
		encs = append(encs, enc[4:len(enc)-4])
	}
	for _, enc := range encs {
		if strings.Contains(data, enc) {
			t.Errorf("%s contains base64-encoded needle (%s)", label, enc)
		}
	}
}

// ---------------------------------------------------------------------------
// V-T2 exit suite: TestVT2a through TestVT2i
// ---------------------------------------------------------------------------

// TestVT2a proves that POST /v1/mcp/connectors (fixture connector) raises a
// custody card on the phone approvals rail; approving flips state to ready,
// tools count updates in GET /v1/mcp/connectors, AND a connector_state SSE
// event arrives on /v1/mcp/connectors/events without test polling.
func TestVT2a(t *testing.T) {
	w := newVT2World(t)

	// Open the SSE stream BEFORE acting.
	sse := w.openSSE(t, "/v1/mcp/connectors/events")
	cardSSE := w.openSSE(t, "/v1/custos/cards/events")

	cfgJSON := lcConfig(t, "vt2a", nil, nil)
	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", code, body)
	}
	var postRes struct {
		Name   string `json:"name"`
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(body), &postRes); err != nil || postRes.CardID == "" {
		t.Fatalf("bad 202 response %s: %v", body, err)
	}

	// Custody card arrives on the phone stream without polling.
	cardEv := cardSSE.nextEvent(t)
	if cardEv.Kind != "custos_card" || !strings.Contains(cardEv.Data, postRes.CardID) {
		t.Fatalf("unexpected card event: kind=%s data=%s", cardEv.Kind, cardEv.Data)
	}

	// Approve the custody card.
	resolveBody, resolveCode := w.postJSON("/v1/custos/cards/"+postRes.CardID+"/resolve", `{"verdict":"once"}`)
	if resolveCode != http.StatusOK {
		t.Fatalf("resolve = %d: %s", resolveCode, resolveBody)
	}

	// Read connector_state SSE event with deadline, no polling.
	var cardRow surface.ConnectorCard
	for {
		stateEv := sse.nextEvent(t)
		if stateEv.Kind == "connector_state" {
			var row surface.ConnectorCard
			if err := json.Unmarshal([]byte(stateEv.Data), &row); err == nil && row.Name == "vt2a" {
				cardRow = row
				if row.State == "ready" {
					break
				}
			}
		}
	}
	if cardRow.Tools != 2 {
		t.Fatalf("SSE connector_state ready but tools = %d, want 2", cardRow.Tools)
	}

	// GET /v1/mcp/connectors: inventory reflects ready and tool count.
	listBody, listCode := w.getJSON()
	if listCode != http.StatusOK {
		t.Fatalf("GET /v1/mcp/connectors = %d: %s", listCode, listBody)
	}
	var listRes struct {
		Connectors []surface.ConnectorCard `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(listBody), &listRes); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	var found *surface.ConnectorCard
	for i := range listRes.Connectors {
		if listRes.Connectors[i].Name == "vt2a" {
			found = &listRes.Connectors[i]
			break
		}
	}
	if found == nil || found.State != "ready" || found.Tools != 2 {
		t.Fatalf("GET connectors missing ready vt2a with 2 tools: %+v", found)
	}
}

// TestVT2b proves that denying the custody card reaps the probe child (no orphan
// OS process), leaves the store untouched, leaves GET listing no such connector,
// and appends mcp_add_denied to the audit stream with the denying via.
func TestVT2b(t *testing.T) {
	w := newVT2World(t)

	pidFile := filepath.Join(t.TempDir(), "vt2b.pid")
	cfgJSON := lcConfig(t, "vt2b", map[string]string{"LC_PIDFILE": pidFile}, nil)

	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", code, body)
	}
	var res struct {
		Name   string `json:"name"`
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.CardID == "" {
		t.Fatalf("bad 202: %s", body)
	}

	// Read probe child pid recorded by fixture.
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read probe pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad probe pid: %s", pidBytes)
	}

	// Deny the custody card via web surface.
	resolveBody, resolveCode := w.postJSON("/v1/custos/cards/"+res.CardID+"/resolve", `{"verdict":"deny"}`)
	if resolveCode != http.StatusOK {
		t.Fatalf("resolve deny = %d: %s", resolveCode, resolveBody)
	}

	// Assert probe child is reaped (no orphan process).
	reaped := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			reaped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reaped {
		t.Fatalf("probe child OS process %d was not reaped", pid)
	}

	// Assert store untouched: connectors.json either absent or has no vt2b.
	if raw, rerr := os.ReadFile(filepath.Join(w.stateDir, "connectors.json")); rerr == nil {
		if strings.Contains(string(raw), "vt2b") {
			t.Fatalf("store contains denied connector vt2b: %s", raw)
		}
	}

	// GET lists no such connector.
	listBody, listCode := w.getJSON()
	if listCode != http.StatusOK {
		t.Fatalf("GET /v1/mcp/connectors = %d: %s", listCode, listBody)
	}
	if strings.Contains(listBody, "vt2b") {
		t.Fatalf("GET listed denied connector vt2b: %s", listBody)
	}

	// Audit stream carries mcp_add_denied with the denying via ("web").
	deniedAudits := auditByKind(t, w.stateDir, custos.AuditKindMCPAddDenied)
	if len(deniedAudits) == 0 {
		t.Fatalf("audit stream has no %s records", custos.AuditKindMCPAddDenied)
	}
	last := deniedAudits[len(deniedAudits)-1]
	if last.Cred != "vt2b" {
		t.Fatalf("mcp_add_denied cred = %q, want vt2b", last.Cred)
	}
	if last.Via != "web" {
		t.Fatalf("mcp_add_denied via = %q, want web", last.Via)
	}
}

// TestVT2c proves that a second POST of an existing name yields 409 duplicate_name,
// and a POST while that same name's add is still pending yields 409 pending_add.
func TestVT2c(t *testing.T) {
	w := newVT2World(t)

	cfgJSON := lcConfig(t, "vt2c", nil, nil)

	// Step 1: First POST raises custody card (pending).
	body1, code1 := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code1 != http.StatusAccepted {
		t.Fatalf("first POST = %d: %s", code1, body1)
	}
	var res1 struct {
		Name   string `json:"name"`
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(body1), &res1); err != nil || res1.CardID == "" {
		t.Fatalf("bad 202: %s", body1)
	}

	// Step 2: POST while add is pending -> 409 pending_add.
	body2, code2 := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code2 != http.StatusConflict {
		t.Fatalf("pending POST want 409, got %d: %s", code2, body2)
	}
	if !strings.Contains(body2, "pending_add") {
		t.Fatalf("want reason pending_add, got: %s", body2)
	}

	// Step 3: Approve the card so the connector commits.
	resBody, resCode := w.postJSON("/v1/custos/cards/"+res1.CardID+"/resolve", `{"verdict":"once"}`)
	if resCode != http.StatusOK {
		t.Fatalf("resolve = %d: %s", resCode, resBody)
	}
	w.waitForReady("vt2c")

	// Step 4: Second POST of already-committed name -> 409 duplicate_name.
	body3, code3 := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code3 != http.StatusConflict {
		t.Fatalf("duplicate POST want 409, got %d: %s", code3, body3)
	}
	if !strings.Contains(body3, "duplicate_name") {
		t.Fatalf("want reason duplicate_name, got: %s", body3)
	}
}

// TestVT2d proves that PATCH deny-listing one tool renders the removed tool in
// the custody card review text, and upon approval, the snapshot tools count drops
// and MCP-TOOLS no longer lists the deny-listed tool.
func TestVT2d(t *testing.T) {
	w := newVT2World(t)

	// Add initial connector with tools alpha (read-only) and beta (write).
	cfgJSON := lcConfig(t, "vt2d", nil, nil)
	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("add = %d: %s", code, body)
	}
	var addRes struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(body), &addRes)
	_, _ = w.postJSON("/v1/custos/cards/"+addRes.CardID+"/resolve", `{"verdict":"once"}`)

	// Verify tools count is 2 initially.
	t0 := time.Now()
	for {
		if tools := w.doorClient.ToolsOf("vt2d"); len(tools) == 2 {
			break
		}
		if time.Since(t0) > 3*time.Second {
			t.Fatal("timed out waiting for initial 2 tools")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Open card stream to inspect card review text.
	cardSSE := w.openSSE(t, "/v1/custos/cards/events")

	// PATCH deny-listing "beta".
	patchJSON := lcConfig(t, "vt2d", nil, map[string]any{"tool_denylist": []string{"beta"}})
	patchBody, patchCode := w.patchJSON("/v1/mcp/connectors/vt2d", patchJSON)
	if patchCode != http.StatusAccepted {
		t.Fatalf("PATCH = %d: %s", patchCode, patchBody)
	}
	var patchRes struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(patchBody), &patchRes)

	// Card review text shows the removed tool.
	cardEv := cardSSE.nextEvent(t)
	if !strings.Contains(cardEv.Data, "beta") || !strings.Contains(cardEv.Data, "tools removed") {
		t.Fatalf("card review text missing removed tool beta: %s", cardEv.Data)
	}

	// Approve the edit card.
	_, resCode := w.postJSON("/v1/custos/cards/"+patchRes.CardID+"/resolve", `{"verdict":"once"}`)
	if resCode != http.StatusOK {
		t.Fatalf("approve edit = %d", resCode)
	}

	// Snapshot tools count drops in GET /v1/mcp/connectors.
	t1 := time.Now()
	for {
		listBody, _ := w.getJSON()
		var listRes struct {
			Connectors []surface.ConnectorCard `json:"connectors"`
		}
		_ = json.Unmarshal([]byte(listBody), &listRes)
		for _, c := range listRes.Connectors {
			if c.Name == "vt2d" && c.Tools == 1 {
				goto toolsDropped
			}
		}
		if time.Since(t1) > 3*time.Second {
			t.Fatal("tools count did not drop to 1 in snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}

toolsDropped:
	// Door mirror MCP-TOOLS no longer lists beta. MCP-STATUS (count) and
	// MCP-TOOLS (list) are separate frames; wait for the list frame to
	// land too before asserting it.
	{
		tm := time.Now()
		for {
			tools := w.doorClient.ToolsOf("vt2d")
			if len(tools) == 1 && tools[0].Name == "alpha" {
				break
			}
			if time.Since(tm) > 3*time.Second {
				t.Fatalf("tools should only contain alpha, got: %+v", tools)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestVT2e proves that DELETE removes the connector from GET, emits a
// connector_gone event on the SSE rail, and after a daemon restart (fresh
// process, same state dir) it stays gone (tombstone-over-YAML-seed proven).
func TestVT2e(t *testing.T) {
	seedName := "seeded-vt2e"
	seedConfig := custos.MCPServerConfig{
		Name:      seedName,
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestLifecycleHelperProcess", "--"},
		Env:       map[string]string{"GO_WANT_LC_HELPER": "1"},
	}

	w := newVT2World(t, seedConfig)

	// Verify seeded connector exists at boot.
	t0 := time.Now()
	for {
		body, _ := w.getJSON()
		if strings.Contains(body, seedName) {
			break
		}
		if time.Since(t0) > 3*time.Second {
			t.Fatalf("seeded connector %s not listed initially", seedName)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Open SSE stream before deleting.
	sse := w.openSSE(t, "/v1/mcp/connectors/events")

	// DELETE /v1/mcp/connectors/seeded-vt2e -> 204 No Content.
	delCode := w.deleteReq("/v1/mcp/connectors/" + seedName)
	if delCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", delCode)
	}

	// Gone from GET /v1/mcp/connectors immediately.
	listBody, _ := w.getJSON()
	if strings.Contains(listBody, seedName) {
		t.Fatalf("GET still lists %s after delete: %s", seedName, listBody)
	}

	// connector_gone event arrives on SSE rail.
	goneEv := sse.nextEvent(t)
	if goneEv.Kind != "connector_gone" || !strings.Contains(goneEv.Data, seedName) {
		t.Fatalf("want connector_gone for %s, got kind=%s data=%s", seedName, goneEv.Kind, goneEv.Data)
	}

	// Restart daemon: fresh process, same state dir, same YAML seed.
	w.stopDaemon()
	w.restartDaemon(seedConfig)

	// Tombstone-over-YAML-seed is proven: stays gone after cold restart.
	recheckBody, recheckCode := w.getJSON()
	if recheckCode != http.StatusOK {
		t.Fatalf("GET after restart = %d: %s", recheckCode, recheckBody)
	}
	if strings.Contains(recheckBody, seedName) {
		t.Fatalf("tombstone failed: %s reappeared from YAML seed after restart: %s", seedName, recheckBody)
	}
}

// TestVT2f proves that a POST embedding a literal secret needle never leaks the
// needle into any response body, SSE event, audit line, or daemon log (raw and
// base64 forms); the store holds only ${vault:...}; and the child process
// receives the resolved secret on the wire.
func TestVT2f(t *testing.T) {
	w := newVT2World(t)

	outFile := filepath.Join(t.TempDir(), "vt2f_secret.out")
	sse := w.openSSE(t, "/v1/mcp/connectors/events")
	cardSSE := w.openSSE(t, "/v1/custos/cards/events")

	// Config embeds literal secret needle.
	cfgJSON := lcConfig(t, "vt2f", map[string]string{
		"LC_SECRET":  vt2Needle,
		"LC_OUTFILE": outFile,
	}, nil)

	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", code, body)
	}
	assertNoNeedle(t, "POST response body", body)

	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(body), &res)

	// Custody card review text does not leak secret.
	cardEv := cardSSE.nextEvent(t)
	assertNoNeedle(t, "custos_card SSE event", cardEv.Data)

	// Approve card.
	resBody, resCode := w.postJSON("/v1/custos/cards/"+res.CardID+"/resolve", `{"verdict":"once"}`)
	if resCode != http.StatusOK {
		t.Fatalf("resolve = %d: %s", resCode, resBody)
	}
	assertNoNeedle(t, "resolve response body", resBody)

	// Wait for connector to commit and become ready.
	w.waitForReady("vt2f")

	// Assert connector_state SSE events do not leak secret.
	for {
		select {
		case stateEv := <-sse.events:
			assertNoNeedle(t, "connector_state SSE event", stateEv.Data)
		default:
			goto sseChecked
		}
	}
sseChecked:

	// GET response body does not leak secret.
	listBody, _ := w.getJSON()
	assertNoNeedle(t, "GET connectors response body", listBody)

	// Stored config has only ${vault:...} reference, no literal.
	storeData, err := os.ReadFile(filepath.Join(w.stateDir, "connectors.json"))
	if err != nil {
		t.Fatalf("read connectors.json: %v", err)
	}
	assertNoNeedle(t, "connectors.json", string(storeData))
	if !strings.Contains(string(storeData), "${vault:") {
		t.Fatalf("connectors.json lacks ${vault:...} reference: %s", storeData)
	}

	// Daemon logs do not leak secret.
	assertNoNeedle(t, "daemon log", w.logbuf.String())

	// Audit lines do not leak secret.
	auditLines := strings.Join(readAuditLines(t, w.stateDir), "\n")
	assertNoNeedle(t, "audit lines", auditLines)

	// Wire authentication proof: child received real secret and echoed it.
	t0 := time.Now()
	for {
		echoed, err := os.ReadFile(outFile)
		if err == nil && len(echoed) > 0 {
			if string(echoed) != vt2Needle {
				t.Fatalf("child process received %q, want %q", echoed, vt2Needle)
			}
			break
		}
		if time.Since(t0) > 3*time.Second {
			t.Fatal("child process did not write secret echo file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestVT2g proves disable then enable round-trips through the API with
// connector_state events in both directions and no custody card raised.
func TestVT2g(t *testing.T) {
	w := newVT2World(t)

	// Setup ready connector.
	cfgJSON := lcConfig(t, "vt2g", nil, nil)
	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", code, body)
	}
	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	_, _ = w.postJSON("/v1/custos/cards/"+res.CardID+"/resolve", `{"verdict":"once"}`)
	w.waitForReady("vt2g")

	// Open status and custody card SSE streams.
	sse := w.openSSE(t, "/v1/mcp/connectors/events")
	cardSSE := w.openSSE(t, "/v1/custos/cards/events")

	// Disable -> connector_state event with disabled state.
	disBody, disCode := w.postJSON("/v1/mcp/connectors/vt2g/disable", "")
	if disCode != http.StatusOK {
		t.Fatalf("disable = %d: %s", disCode, disBody)
	}
	var ev1 vt2SSEEvent
	for {
		ev := sse.nextEvent(t)
		if ev.Kind == "connector_state" && strings.Contains(ev.Data, "disabled") {
			ev1 = ev
			break
		}
	}
	if !strings.Contains(ev1.Data, "disabled") {
		t.Fatalf("want disabled event, got %s: %s", ev1.Kind, ev1.Data)
	}

	// Verify no approval card was raised.
	if len(w.hub.Pending()) != 0 {
		t.Fatalf("approval card raised on disable: %+v", w.hub.Pending())
	}
	select {
	case ev := <-cardSSE.events:
		t.Fatalf("unexpected card event on disable: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// Enable -> connector_state event with ready state.
	enBody, enCode := w.postJSON("/v1/mcp/connectors/vt2g/enable", "")
	if enCode != http.StatusOK {
		t.Fatalf("enable = %d: %s", enCode, enBody)
	}
	var ev2 vt2SSEEvent
	for {
		ev := sse.nextEvent(t)
		if ev.Kind == "connector_state" && (strings.Contains(ev.Data, "read+act") || strings.Contains(ev.Data, "ready")) {
			ev2 = ev
			break
		}
	}
	if !strings.Contains(ev2.Data, "read+act") && !strings.Contains(ev2.Data, "ready") {
		t.Fatalf("want read+act event, got %s: %s", ev2.Kind, ev2.Data)
	}

	// Verify no approval card was raised.
	if len(w.hub.Pending()) != 0 {
		t.Fatalf("approval card raised on enable: %+v", w.hub.Pending())
	}
	select {
	case ev := <-cardSSE.events:
		t.Fatalf("unexpected card event on enable: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestVT2h proves deleting a referenced vault credential moves the connector to
// needs-re-auth (reason credential), writing a new value under the same name via
// the T1 endpoint and calling POST .../reauth restores ready, with connector_state
// events arriving in strict order at each hop.
func TestVT2h(t *testing.T) {
	w := newVT2World(t)

	credName := "cred-vt2h" //nolint:gosec // G101: fixture canary name

	// 1. Write initial credential via T1 admin endpoint.
	initCred := `{"name":"` + credName + `","secret":"init-secret-1234"}`
	body, code := w.postJSON("/v1/credentials", initCred)
	if code != http.StatusCreated {
		t.Fatalf("create credential = %d: %s", code, body)
	}

	// 2. Add connector referencing ${vault:cred-vt2h}.
	cfgJSON := lcConfig(t, "vt2h", map[string]string{
		"LC_SECRET": "${vault:" + credName + "}",
	}, nil)
	body, code = w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST connector = %d: %s", code, body)
	}
	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	_, _ = w.postJSON("/v1/custos/cards/"+res.CardID+"/resolve", `{"verdict":"once"}`)
	w.waitForReady("vt2h")

	// Open status SSE stream before mutating credential.
	sse := w.openSSE(t, "/v1/mcp/connectors/events")

	// 3. Delete referenced vault credential and notify rail.
	if err := w.d.Vault().RevokeCredential("", credName, "cli"); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	w.d.Rail().NotifyCredentialGone(credName)

	// Hop 1: state becomes needs-re-auth with reason credential.
	var cardHop1 surface.ConnectorCard
	for {
		hop1 := sse.nextEvent(t)
		if hop1.Kind == "connector_state" {
			_ = json.Unmarshal([]byte(hop1.Data), &cardHop1)
			if cardHop1.AppState == "needs-re-auth" {
				break
			}
		}
	}
	if !strings.Contains(cardHop1.Reason, "credential") {
		t.Fatalf("hop 1 expected needs-re-auth with credential reason: %+v", cardHop1)
	}

	// 4. Write new value under same name via T1 /v1/credentials endpoint.
	newCred := `{"name":"` + credName + `","secret":"new-secret-5678"}`
	body, code = w.postJSON("/v1/credentials", newCred)
	if code != http.StatusCreated {
		t.Fatalf("re-create credential = %d: %s", code, body)
	}

	// 5. POST /v1/mcp/connectors/vt2h/reauth -> ready.
	reauthBody, reauthCode := w.postJSON("/v1/mcp/connectors/vt2h/reauth", "")
	if reauthCode != http.StatusOK {
		t.Fatalf("reauth = %d: %s", reauthCode, reauthBody)
	}

	// Hop 2: state becomes ready (read+act). Assert strict event order.
	var cardHop2 surface.ConnectorCard
	for {
		hop2 := sse.nextEvent(t)
		if hop2.Kind == "connector_state" {
			_ = json.Unmarshal([]byte(hop2.Data), &cardHop2)
			if cardHop2.AppState == "read+act" || cardHop2.State == "ready" {
				break
			}
		}
	}
	if cardHop2.AppState != "read+act" || cardHop2.State != "ready" {
		t.Fatalf("hop 2 expected ready (read+act): %+v", cardHop2)
	}
}

// TestVT2i proves that an old door client (speaking only pre-amendment verbs
// MCP-STATUS, MCP-TOOLS, MCP-REFRESH) connects to the new custosd and works;
// sending an unknown verb answers an error without dropping the connection;
// and all new frames are additive.
func TestVT2i(t *testing.T) {
	w := newVT2World(t)

	// Add live connector so tools and status queries return data.
	cfgJSON := lcConfig(t, "vt2i", nil, nil)
	body, code := w.postJSON("/v1/mcp/connectors", cfgJSON)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", code, body)
	}
	var res struct {
		CardID string `json:"card_id"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	_, _ = w.postJSON("/v1/custos/cards/"+res.CardID+"/resolve", `{"verdict":"once"}`)
	w.waitForReady("vt2i")

	// Raw unix-socket client speaking old door protocol.
	sockPath, tokenPath := custos.DoorPaths(w.stateDir)
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r := bufio.NewReader(conn)

	// Send HELLO.
	if _, err := fmt.Fprintf(conn, "HELLO %s old-client-v1\n", token); err != nil {
		t.Fatal(err)
	}

	readLine := func() string {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		return strings.TrimRight(line, "\r\n")
	}

	// Read initial attach frames: OK <cards>, and snapshot MCP-STATUS / MCP-TOOLS.
	helloAck := readLine()
	if !strings.HasPrefix(helloAck, "OK ") {
		t.Fatalf("helloAck = %q", helloAck)
	}
	var gotStatus, gotTools bool
	for !gotStatus || !gotTools {
		line := readLine()
		switch {
		case strings.HasPrefix(line, "MCP-STATUS "):
			gotStatus = true
		case strings.HasPrefix(line, "MCP-TOOLS vt2i "):
			gotTools = true
		default:
			t.Fatalf("unexpected attach frame: %q", line)
		}
	}

	// Pre-amendment verb 1: MCP-STATUS.
	if _, err := fmt.Fprint(conn, "MCP-STATUS\n"); err != nil {
		t.Fatal(err)
	}
	statusReply := readLine()
	if !strings.HasPrefix(statusReply, "OK ") || !strings.Contains(statusReply, "vt2i") {
		t.Fatalf("MCP-STATUS reply = %q", statusReply)
	}

	// Pre-amendment verb 2: MCP-TOOLS vt2i.
	if _, err := fmt.Fprint(conn, "MCP-TOOLS vt2i\n"); err != nil {
		t.Fatal(err)
	}
	toolsReply := readLine()
	if !strings.HasPrefix(toolsReply, "OK ") || !strings.Contains(toolsReply, "alpha") {
		t.Fatalf("MCP-TOOLS reply = %q", toolsReply)
	}

	// Pre-amendment verb 3: MCP-REFRESH vt2i.
	if _, err := fmt.Fprint(conn, "MCP-REFRESH vt2i\n"); err != nil {
		t.Fatal(err)
	}
	refreshReply := readLine()
	if !strings.HasPrefix(refreshReply, "OK") {
		t.Fatalf("MCP-REFRESH reply = %q", refreshReply)
	}

	// Unknown verb: answers an error without dropping the connection.
	if _, err := fmt.Fprint(conn, "UNKNOWN-OLD-VERB\n"); err != nil {
		t.Fatal(err)
	}
	var errReply string
	for {
		line := readLine()
		if strings.HasPrefix(line, "ERR ") {
			errReply = line
			break
		}
		// Intervening broadcast frames (e.g. from preceding refresh) are valid.
		if !strings.HasPrefix(line, "MCP-STATUS ") && !strings.HasPrefix(line, "MCP-TOOLS ") {
			t.Fatalf("unexpected line waiting for error reply: %q", line)
		}
	}
	if !strings.HasPrefix(errReply, "ERR ") {
		t.Fatalf("unknown verb reply want ERR, got %q", errReply)
	}

	// Verify connection is alive: send another MCP-STATUS and get valid reply.
	if _, err := fmt.Fprint(conn, "MCP-STATUS\n"); err != nil {
		t.Fatal(err)
	}
	var statusReply2 string
	for {
		line := readLine()
		if strings.HasPrefix(line, "OK ") {
			statusReply2 = line
			break
		}
		// Intervening broadcast frames are valid and expected.
		if !strings.HasPrefix(line, "MCP-STATUS ") && !strings.HasPrefix(line, "MCP-TOOLS ") {
			t.Fatalf("unexpected line waiting for MCP-STATUS reply: %q", line)
		}
	}
	if !strings.Contains(statusReply2, "vt2i") {
		t.Fatalf("subsequent MCP-STATUS reply missing vt2i: %q", statusReply2)
	}

	// Additive frames check: disable vt2i via surface, old client receives
	// pushed broadcast frame without crashing.
	_, _ = w.postJSON("/v1/mcp/connectors/vt2i/disable", "")
	var broadcastLine string
	for {
		line := readLine()
		if strings.HasPrefix(line, "MCP-STATUS ") {
			broadcastLine = line
			break
		}
	}
	if !strings.Contains(broadcastLine, "vt2i") {
		t.Fatalf("want pushed MCP-STATUS broadcast for vt2i, got: %q", broadcastLine)
	}
}
