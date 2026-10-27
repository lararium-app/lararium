// H2 regression: MCP-CALL carries the worker-lane enforcement gate
// (vault lock, worker policy verdict, approval hold, audit) on every
// socket, so a door.token or ctl.token holder can never execute a rail
// tool with a locked vault or an ask/deny verdict set.

package custos_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

// guardFanout captures the approval IDs the hub fans out so a test can
// resolve a custos-source card (observers only see turn cards).
type guardFanout struct{ ids chan string }

func (g guardFanout) ApprovalPending(id, sessionID, name, argsSummary string) {
	if sessionID == "guardtest" {
		select {
		case g.ids <- id:
		default:
		}
	}
}

func (g guardFanout) ApprovalTerminal(id, sessionID, state, reason, source string) {}

func newGuardVault(t *testing.T) *custos.Vault {
	t.Helper()
	v := custos.NewVault(t.TempDir(), 5*time.Second)
	if err := v.Init("test-passphrase-9f3k"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock("test-passphrase-9f3k", false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}
	return v
}

func dispatchCall(t *testing.T, reg *custos.RailRegistry, tool string, g custos.MCPCallGuard) string {
	t.Helper()
	var got string
	if !custos.DispatchMCPCall(nil, reg, "alpha", tool, `{"message":"hi"}`, captureWrite(t, &got), g) {
		t.Fatal("DispatchMCPCall did not handle the call")
	}
	return got
}

func TestMCPCallGuard_LockedVaultRefused(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	if err := v.Lock(); err != nil {
		t.Fatalf("lock: %v", err)
	}
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR locked\n" {
		t.Fatalf("locked vault frame = %q, want ERR locked", got)
	}
}

func TestMCPCallGuard_DenyVerdictRefused(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	if _, err := v.Policy().AddCredentialRule("alpha/ping", "deny", false, "test", "guardtest"); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR denied\n" {
		t.Fatalf("deny frame = %q, want ERR denied", got)
	}
}

func TestMCPCallGuard_AskWithoutHubFailsClosed(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{Vault: v, Actor: "guardtest"})
	if got != "ERR approval_required\n" {
		t.Fatalf("ask-no-hub frame = %q, want ERR approval_required", got)
	}
}

// ask + card denied → ERR denied, and the tool never executed (an
// executed echo would answer OK …pong).
func TestMCPCallGuard_AskCardDenied(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	hub := surface.NewApprovalHub(30 * time.Second)
	ids := make(chan string, 4)
	hub.SetFanout(guardFanout{ids: ids})

	guard := custos.MCPCallGuard{Vault: v, Hub: hub, Actor: "guardtest", Hold: 20 * time.Second}
	done := make(chan string, 1)
	go func() { done <- dispatchCall(t, reg, "ping", guard) }()

	select {
	case id := <-ids:
		if code := hub.ResolveFrom("guardtest", id, false, "test"); code != 200 {
			t.Fatalf("resolve deny code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no approval card raised for ask verdict")
	}

	select {
	case got := <-done:
		if got != "ERR denied\n" {
			t.Fatalf("card-denied frame = %q, want ERR denied", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch did not return after card denial")
	}
}

func TestMCPCallGuard_AskCardApproved(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	hub := surface.NewApprovalHub(30 * time.Second)
	ids := make(chan string, 4)
	hub.SetFanout(guardFanout{ids: ids})

	guard := custos.MCPCallGuard{Vault: v, Hub: hub, Actor: "guardtest", Hold: 20 * time.Second}
	done := make(chan string, 1)
	go func() { done <- dispatchCall(t, reg, "echo", guard) }()

	select {
	case id := <-ids:
		if code := hub.ResolveFrom("guardtest", id, true, "test"); code != 200 {
			t.Fatalf("resolve approve code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no approval card raised for ask verdict")
	}

	select {
	case got := <-done:
		if !strings.HasPrefix(got, "OK ") || !strings.Contains(got, "echo:hi") {
			t.Fatalf("card-approved frame = %q, want OK echo result", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("dispatch did not return after card approval")
	}
}

// shortGuardDir returns a short UDS-safe directory under /tmp so unix socket
// paths never exceed the 104-byte sun_path limit.
func shortGuardDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cg") //nolint:usetesting // UDS path cap
	if err != nil {
		d = t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// doorSurfaceAdapter bridges *custosdoor.Client onto surface.CustosRegistry
// inside package custos_test so A-2 and A-3 exercise the real surface HTTP
// handlers (/v1/custos/cards and /v1/custos/cards/{id}/resolve).
type doorSurfaceAdapter struct {
	cl *custosdoor.Client
}

func (a *doorSurfaceAdapter) Snapshot() []surface.CustosCard {
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

func (a *doorSurfaceAdapter) Subscribe(f func(card *surface.CustosCard, gone *surface.CustosGone)) func() {
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

func (a *doorSurfaceAdapter) Resolve(id, verdict string) (string, error) {
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

func (a *doorSurfaceAdapter) Attach() ([]surface.CustosCard, func(), <-chan surface.CustosEvent) {
	ch := make(chan surface.CustosEvent)
	close(ch)
	return a.Snapshot(), func() {}, ch
}

// TestA1_MCPCallGuard_UsesConfiguredHoldAndExecTimeout verifies that:
//  1. NewMCPCallGuard populates Hold from cfg.AskHoldTimeout (e.g. 7 s, not 60 s).
//  2. WorkerServer.BindRailRegistry threads both cfg.AskHoldTimeout and
//     cfg.WorkerExecTimeout onto the MCP connector so a zero-Hold guard
//     inherits the configured hold and parks for that duration (not 60 s).
func TestA1_MCPCallGuard_UsesConfiguredHoldAndExecTimeout(t *testing.T) {
	reg := newCallTestRegistry(t)
	v := newGuardVault(t)
	hub := surface.NewApprovalHub(7 * time.Second)

	cfg7 := &custos.Config{
		AskHoldTimeout:    7 * time.Second,
		WorkerExecTimeout: 9 * time.Second,
	}
	g7 := custos.NewMCPCallGuard(cfg7, v, hub, "guardtest")
	if g7.Hold != 7*time.Second {
		t.Fatalf("NewMCPCallGuard Hold = %v, want 7s", g7.Hold)
	}

	// Verify that a short configured hold (120 ms) wired via WorkerServer +
	// BindRailRegistry is inherited by a guard whose Hold field is left 0
	// (matching the ctl_mcp.go and doors.go call sites) and expires in ~120 ms
	// instead of 60 s.
	shortHold := 120 * time.Millisecond
	cfgShort := &custos.Config{
		AskHoldTimeout:    shortHold,
		WorkerExecTimeout: 9 * time.Second,
	}
	shortHub := surface.NewApprovalHub(shortHold)
	ws := custos.NewWorkerServer(v, shortHub, cfgShort, custos.WithRailRegistry(reg))
	ws.BindRailRegistry(reg)

	conn := reg.Connector("alpha")
	if conn.ExecTimeout() != 9*time.Second {
		t.Fatalf("connector ExecTimeout = %v, want 9s", conn.ExecTimeout())
	}
	if conn.AskHoldTimeout() != shortHold {
		t.Fatalf("connector AskHoldTimeout = %v, want %v", conn.AskHoldTimeout(), shortHold)
	}

	parkedCh := make(chan string, 1)
	start := time.Now()
	got := dispatchCall(t, reg, "ping", custos.MCPCallGuard{
		Vault: v,
		Hub:   shortHub,
		Actor: "guardtest",
		OnParked: func(cardID string) {
			parkedCh <- cardID
		},
	})
	elapsed := time.Since(start)

	select {
	case id := <-parkedCh:
		if id == "" {
			t.Fatal("expected non-empty card id on park")
		}
	default:
		t.Fatal("guard did not park on approval card")
	}

	if got != "ERR denied\n" {
		t.Fatalf("expired hold frame = %q, want ERR denied", got)
	}
	if elapsed < shortHold/2 || elapsed > 2*time.Second {
		t.Fatalf("hold duration = %v, want ~%v (not 60s)", elapsed, shortHold)
	}
}

// TestA2_MCPRailCall_HumanResolutionWinsRace verifies that with a 10 s hold
// configured, an ask-gated MCP-CALL over doors.sock does not block concurrent
// card resolution: resolving via the real surface -> custosdoor.Client ->
// doors.sock path settles promptly, delivers approved=true to the waiting
// decision channel, and lets the MCP-CALL succeed.
func TestA2_MCPRailCall_HumanResolutionWinsRace(t *testing.T) {
	dir := shortGuardDir(t)
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	reg := newCallTestRegistry(t)
	cfg := &custos.Config{
		AskHoldTimeout:    10 * time.Second,
		WorkerExecTimeout: 10 * time.Second,
	}
	cfg.Normalize()

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init("test-passphrase-a2"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock("test-passphrase-a2", false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	v.SetApprovalHub(hub)

	ws := custos.NewWorkerServer(v, hub, cfg, custos.WithRailRegistry(reg))
	ws.BindRailRegistry(reg)

	doorSrv, err := custos.StartDoorServer(stateDir, hub,
		custos.WithVault(v),
		custos.WithWorkers(ws),
		custos.WithRail(reg),
	)
	if err != nil {
		t.Fatalf("StartDoorServer: %v", err)
	}
	defer doorSrv.Close()

	sockPath, tokenPath := custos.DoorPaths(stateDir)
	attachedCh := make(chan struct{}, 1)
	doorClient, err := custosdoor.New(custosdoor.Options{
		SockPath:   sockPath,
		TokenPath:  tokenPath,
		Name:       "hearthd",
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("custosdoor.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cardCh := make(chan string, 4)
	unsubCards := doorClient.Subscribe(func(card *custosdoor.Card, _ *custosdoor.Gone) {
		if card != nil && card.ID != "" {
			select {
			case cardCh <- card.ID:
			default:
			}
		}
	})
	defer unsubCards()

	unsubMCP := doorClient.SubscribeMCP(func(st []custosdoor.ConnectorStatus, _ []custosdoor.ToolInfo) {
		if len(st) > 0 {
			select {
			case attachedCh <- struct{}{}:
			default:
			}
		}
	})
	defer unsubMCP()

	go doorClient.Run(ctx)

	select {
	case <-attachedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("doorClient did not attach to DoorServer")
	}

	tokenStore, err := surface.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	bearerTok, err := tokenStore.Create("a2-web")
	if err != nil {
		t.Fatal(err)
	}
	srv := &surface.Server{
		Store: tokenStore,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetCustosRegistry(&doorSurfaceAdapter{cl: doorClient})
	srv.SetApprovalHub(hub)
	handler := surface.NewServer(srv)

	type callOutcome struct {
		res json.RawMessage
		err error
	}
	callDone := make(chan callOutcome, 1)
	go func() {
		res, callErr := doorClient.Call(ctx, "alpha", "echo", json.RawMessage(`{"message":"race-won"}`))
		callDone <- callOutcome{res: res, err: callErr}
	}()

	var cardID string
	select {
	case cardID = <-cardCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ask card from MCP-CALL")
	}

	resolveStart := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(`{"verdict":"once"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer "+bearerTok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resolveElapsed := time.Since(resolveStart)

	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, body = %s (elapsed %v)", rec.Code, rec.Body.String(), resolveElapsed)
	}
	if !strings.Contains(rec.Body.String(), `"approved"`) {
		t.Fatalf("resolve body = %s, want state approved", rec.Body.String())
	}
	if resolveElapsed > 2*time.Second {
		t.Fatalf("resolve blocked for %v, must settle promptly inside 10s hold", resolveElapsed)
	}

	select {
	case out := <-callDone:
		if out.err != nil {
			t.Fatalf("doorClient.Call error = %v, want nil", out.err)
		}
		if !strings.Contains(string(out.res), "echo:race-won") {
			t.Fatalf("doorClient.Call result = %s, want echo:race-won", string(out.res))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MCP-CALL did not complete after card approval")
	}
}

// TestA3_CLILaneAskCard_SurfacesAndResolvesEndToEnd verifies that a parked
// CLI-lane ask call (via ctl.sock MCP-CALL) surfaces in GET /v1/custos/cards
// (and ctlClient.Cards) with its connector/tool metadata, and resolving it
// via POST /v1/custos/cards/{id}/resolve wakes the waiting CLI call.
func TestA3_CLILaneAskCard_SurfacesAndResolvesEndToEnd(t *testing.T) {
	dir := shortGuardDir(t)
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	reg := newCallTestRegistry(t)
	cfg := &custos.Config{
		AskHoldTimeout:    15 * time.Second,
		WorkerExecTimeout: 10 * time.Second,
	}
	cfg.Normalize()

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init("test-passphrase-a3"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock("test-passphrase-a3", false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	v.SetApprovalHub(hub)

	ws := custos.NewWorkerServer(v, hub, cfg, custos.WithRailRegistry(reg))
	ws.BindRailRegistry(reg)

	ctlSrv, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		t.Fatalf("StartCtlServer: %v", err)
	}
	defer ctlSrv.Close()
	ctlSrv.SetWorkers(ws)
	ctlSrv.SetHub(hub)
	ctlSrv.SetRailRegistry(reg)

	doorSrv, err := custos.StartDoorServer(stateDir, hub,
		custos.WithVault(v),
		custos.WithWorkers(ws),
		custos.WithRail(reg),
	)
	if err != nil {
		t.Fatalf("StartDoorServer: %v", err)
	}
	defer doorSrv.Close()

	sockPath, tokenPath := custos.DoorPaths(stateDir)
	attachedCh := make(chan struct{}, 1)
	doorClient, err := custosdoor.New(custosdoor.Options{
		SockPath:   sockPath,
		TokenPath:  tokenPath,
		Name:       "hearthd",
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("custosdoor.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cardCh := make(chan custosdoor.Card, 4)
	unsubCards := doorClient.Subscribe(func(card *custosdoor.Card, _ *custosdoor.Gone) {
		if card != nil && card.Tool == "echo" {
			select {
			case cardCh <- *card:
			default:
			}
		}
	})
	defer unsubCards()

	unsubMCP := doorClient.SubscribeMCP(func(st []custosdoor.ConnectorStatus, _ []custosdoor.ToolInfo) {
		if len(st) > 0 {
			select {
			case attachedCh <- struct{}{}:
			default:
			}
		}
	})
	defer unsubMCP()

	go doorClient.Run(ctx)

	select {
	case <-attachedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("doorClient did not attach")
	}

	tokenStore, err := surface.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	bearerTok, err := tokenStore.Create("a3-web")
	if err != nil {
		t.Fatal(err)
	}
	srv := &surface.Server{
		Store: tokenStore,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetCustosRegistry(&doorSurfaceAdapter{cl: doorClient})
	srv.SetApprovalHub(hub)
	handler := surface.NewServer(srv)

	ctlClient, err := custos.NewCtlClient(stateDir, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCtlClient: %v", err)
	}
	var parkedBuf bytes.Buffer
	ctlClient.SetParkedWriter(&parkedBuf)

	type cliResult struct {
		out string
		err error
	}
	cliDone := make(chan cliResult, 1)
	go func() {
		out, callErr := ctlClient.RoundTripWithArgTimeout("MCP-CALL", `alpha echo {"message":"from-cli"}`, 10*time.Second)
		cliDone <- cliResult{out: out, err: callErr}
	}()

	var pushedCard custosdoor.Card
	select {
	case pushedCard = <-cardCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for CLI ask card to surface on door registry")
	}

	// Assert GET /v1/custos/cards returns the parked CLI card with full metadata.
	reqList := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
	reqList.Host = "127.0.0.1"
	reqList.Header.Set("Authorization", "Bearer "+bearerTok)
	recList := httptest.NewRecorder()
	handler.ServeHTTP(recList, reqList)
	if recList.Code != http.StatusOK {
		t.Fatalf("GET /v1/custos/cards status = %d, body = %s", recList.Code, recList.Body.String())
	}
	var listResp struct {
		Cards []surface.CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(recList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal cards: %v", err)
	}
	if len(listResp.Cards) != 1 {
		t.Fatalf("surface cards = %+v, want 1 card", listResp.Cards)
	}
	sc := listResp.Cards[0]
	if sc.ID != pushedCard.ID || sc.Cred != "alpha" || sc.Tool != "echo" || sc.ExpiresInS <= 0 {
		t.Fatalf("unexpected surface card: %+v", sc)
	}

	// Also assert ctlClient.Cards() (CardSnapshot) includes the same card.
	ctlCards, err := ctlClient.Cards()
	if err != nil {
		t.Fatalf("ctlClient.Cards: %v", err)
	}
	if len(ctlCards) != 1 || ctlCards[0].ID != sc.ID || ctlCards[0].Tool != "echo" {
		t.Fatalf("ctlClient.Cards = %+v, want card %s", ctlCards, sc.ID)
	}

	// Resolve via POST /v1/custos/cards/{id}/resolve and assert CLI call proceeds.
	reqResolve := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+sc.ID+"/resolve", strings.NewReader(`{"verdict":"once"}`))
	reqResolve.Host = "127.0.0.1"
	reqResolve.Header.Set("Authorization", "Bearer "+bearerTok)
	recResolve := httptest.NewRecorder()
	handler.ServeHTTP(recResolve, reqResolve)
	if recResolve.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, body = %s", recResolve.Code, recResolve.Body.String())
	}

	select {
	case res := <-cliDone:
		if res.err != nil {
			t.Fatalf("CLI MCP-CALL error = %v, want nil", res.err)
		}
		if !strings.Contains(res.out, "echo:from-cli") {
			t.Fatalf("CLI MCP-CALL output = %q, want echo:from-cli", res.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CLI MCP-CALL did not complete after surface resolution")
	}

	if !strings.Contains(parkedBuf.String(), "call parked pending approval (id "+sc.ID+")") {
		t.Fatalf("parked notice = %q, want card id %s", parkedBuf.String(), sc.ID)
	}
}

type notifyBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	ch  chan string
}

func (n *notifyBuffer) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	nw, err := n.buf.Write(p)
	select {
	case n.ch <- n.buf.String():
	default:
	}
	return nw, err
}

func (n *notifyBuffer) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buf.String()
}

// TestA4_CLI_ParkedThenDeniedVsDeadSocketClassification verifies:
// (a) A parked-then-denied MCP-CALL over ctl.sock prints the parked notice
//
//	with the card id and returns an approval verdict error (not "read response: EOF").
//
// (b) A dead socket that closes before parking still yields a "read response: EOF"
//
//	transport error.
func TestA4_CLI_ParkedThenDeniedVsDeadSocketClassification(t *testing.T) {
	t.Run("parked_then_denied", func(t *testing.T) {
		dir := shortGuardDir(t)
		stateDir := filepath.Join(dir, "custos")
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}

		reg := newCallTestRegistry(t)
		cfg := &custos.Config{
			AskHoldTimeout:    15 * time.Second,
			WorkerExecTimeout: 10 * time.Second,
		}
		cfg.Normalize()

		v := custos.NewVault(stateDir, 5*time.Second)
		if err := v.Init("test-passphrase-a4"); err != nil {
			t.Fatal(err)
		}
		if err := v.Unlock("test-passphrase-a4", false); err != nil {
			t.Fatal(err)
		}
		hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
		v.SetApprovalHub(hub)

		ws := custos.NewWorkerServer(v, hub, cfg, custos.WithRailRegistry(reg))
		ws.BindRailRegistry(reg)

		ctlSrv, err := custos.StartCtlServer(stateDir, v, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ctlSrv.Close()
		ctlSrv.SetWorkers(ws)
		ctlSrv.SetHub(hub)
		ctlSrv.SetRailRegistry(reg)

		ctlClient, err := custos.NewCtlClient(stateDir, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		nb := &notifyBuffer{ch: make(chan string, 4)}
		ctlClient.SetParkedWriter(nb)

		errCh := make(chan error, 1)
		go func() {
			_, callErr := ctlClient.RoundTripWithArgTimeout("MCP-CALL", `alpha echo {"message":"deny-me"}`, 10*time.Second)
			errCh <- callErr
		}()

		var notice string
		select {
		case notice = <-nb.ch:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for CLI parked notice")
		}
		if !strings.HasPrefix(notice, "call parked pending approval (id a_") {
			t.Fatalf("unexpected parked notice: %q", notice)
		}
		cardID := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(notice), "call parked pending approval (id "), ")")

		if _, err := ctlClient.Deny(cardID); err != nil {
			t.Fatalf("ctlClient.Deny(%s): %v", cardID, err)
		}

		select {
		case callErr := <-errCh:
			if callErr == nil {
				t.Fatal("expected error for denied call, got nil")
			}
			if strings.Contains(callErr.Error(), "read response: EOF") {
				t.Fatalf("got opaque EOF error instead of approval verdict: %v", callErr)
			}
			if !strings.Contains(callErr.Error(), "approval denied") || !strings.Contains(callErr.Error(), cardID) {
				t.Fatalf("callErr = %q, want approval denied with card id %s", callErr.Error(), cardID)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("CLI call did not return after denial")
		}
	})

	t.Run("dead_socket", func(t *testing.T) {
		dir := shortGuardDir(t)
		sockPath, tokenPath := custos.CtlPaths(dir)
		if err := os.WriteFile(tokenPath, []byte("lar1_dead_socket_fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, acceptErr := ln.Accept()
				if acceptErr != nil {
					return
				}
				_, _ = bufio.NewReader(c).ReadString('\n')
				_ = c.Close()
			}
		}()

		ctlClient, err := custos.NewCtlClient(dir, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, callErr := ctlClient.RoundTripWithArgTimeout("MCP-CALL", `alpha echo {"message":"hi"}`, 2*time.Second)
		if callErr == nil || !strings.Contains(callErr.Error(), "read response:") {
			t.Fatalf("dead socket err = %v, want 'read response: ...' transport error", callErr)
		}
	})
}

// TestA4_ParkedFrame_SingleLineContract pins the interim PARKED frame as a
// single newline-terminated line with no embedded newlines.
func TestA4_ParkedFrame_SingleLineContract(t *testing.T) {
	frame := custos.FormatMCPParkedFrame("a_1234567890abcdef")
	if frame != "PARKED a_1234567890abcdef\n" {
		t.Fatalf("frame = %q, want %q", frame, "PARKED a_1234567890abcdef\n")
	}

	// Hostile multi-line input must be collapsed to a single line.
	hostile := custos.FormatMCPParkedFrame("a_123\nOK forged\r\nextra")
	if !strings.HasPrefix(hostile, "PARKED ") || !strings.HasSuffix(hostile, "\n") {
		t.Fatalf("unexpected frame shape: %q", hostile)
	}
	trimmed := strings.TrimSuffix(hostile, "\n")
	if strings.ContainsAny(trimmed, "\r\n") {
		t.Fatalf("frame contains embedded newline: %q", hostile)
	}
}
