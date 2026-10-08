package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

func init() {
	custos.SetTestAgeWorkFactor(10)
}

func TestCustosWebAdapter_FieldMapping(t *testing.T) {
	c := custosdoor.Card{
		ID:         "card-123",
		Cell:       "cell-abc",
		Cred:       "cred-def",
		Tool:       "tool-ghi",
		Dest:       "dest.example.com:443",
		Review:     "review-summary",
		AgeS:       15,
		ExpiresInS: 45,
	}
	sc := toSurfaceCard(c)
	if sc.ID != c.ID || sc.Cell != c.Cell || sc.Cred != c.Cred || sc.Tool != c.Tool ||
		sc.Dest != c.Dest || sc.Review != c.Review || sc.AgeS != c.AgeS || sc.ExpiresInS != c.ExpiresInS {
		t.Fatalf("toSurfaceCard mapped %+v to %+v", c, sc)
	}

	g := custosdoor.Gone{
		ID:     "card-123",
		State:  "approved",
		Reason: "web",
	}
	sg := toSurfaceGone(g)
	if sg.ID != g.ID || sg.State != g.State || sg.Reason != g.Reason {
		t.Fatalf("toSurfaceGone mapped %+v to %+v", g, sg)
	}

	evCard := toSurfaceEvent(custosdoor.Event{Card: &c})
	if evCard.Card == nil || evCard.Gone != nil || evCard.Card.ID != c.ID {
		t.Fatalf("toSurfaceEvent(card) mapped to %+v", evCard)
	}

	evGone := toSurfaceEvent(custosdoor.Event{Gone: &g})
	if evGone.Gone == nil || evGone.Card != nil || evGone.Gone.ID != g.ID {
		t.Fatalf("toSurfaceEvent(gone) mapped to %+v", evGone)
	}
}

func TestCustosWebAdapter_SentinelMapping(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"no_such_card", custosdoor.ErrNoSuchCard, surface.ErrNoSuchCard},
		{"already_answered", custosdoor.ErrCardAnswered, surface.ErrCardAnswered},
		{"stale_verdict", custosdoor.ErrStaleVerdict, surface.ErrStaleVerdict},
		{"locked", custosdoor.ErrLocked, surface.ErrLocked},
		{"ip_ask_only", custosdoor.ErrIPAskOnly, surface.ErrIPAskOnly},
		{"door_down_unchanged", custosdoor.ErrDoorDown, custosdoor.ErrDoorDown},
		{"bad_token_unchanged", custosdoor.ErrBadToken, custosdoor.ErrBadToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapDoorError(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapDoorError(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	unknown := errors.New("arbitrary_door_internal_error")
	gotUnknown := mapDoorError(unknown)
	if !errors.Is(gotUnknown, unknown) {
		t.Fatalf("unknown error = %v, want %v", gotUnknown, unknown)
	}
}

func TestCustosWebAdapter_AttachAndSnapshot(t *testing.T) {
	dir := shortTmp(t)
	sock := filepath.Join(dir, "doors.sock")
	tok := filepath.Join(dir, "door.token")
	if err := os.WriteFile(tok, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client, err := custosdoor.New(custosdoor.Options{
		SockPath:  sock,
		TokenPath: tok,
	})
	if err != nil || client == nil {
		t.Fatalf("New: %v", err)
	}

	adapter := NewCustosWebAdapter(client)

	snap := adapter.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("snapshot len = %d, want 0", len(snap))
	}

	cards, cancel, events := adapter.Attach()
	if len(cards) != 0 {
		t.Fatalf("attach cards len = %d, want 0", len(cards))
	}

	cancel()
	cancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("events channel not closed on cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("events channel did not close")
	}
}

func TestCustosWebWiring_Integration(t *testing.T) {
	dir := shortTmp(t)
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	v := custos.NewVault(stateDir, 5*time.Second)
	pass := "secret-integration-passphrase"
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}

	hub := surface.NewApprovalHub(10 * time.Second)
	v.SetApprovalHub(hub)

	doorSrv, err := custos.StartDoorServer(stateDir, hub, custos.WithVault(v))
	if err != nil {
		t.Fatalf("StartDoorServer: %v", err)
	}
	defer doorSrv.Close()

	cardID, _ := hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "to:integration@example.com", 30*time.Second, nil)

	sockPath, tokenPath := custos.DoorPaths(stateDir)

	tokenStore, err := surface.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	tok, err := tokenStore.Create("integration")
	if err != nil {
		t.Fatalf("tokenStore.Create: %v", err)
	}

	srv := &surface.Server{
		Store: tokenStore,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	handler := surface.NewServer(srv)

	stopDoor := startCustosDoor(t.Context(), CustosConfig{DoorsSock: sockPath, DoorToken: tokenPath}, nil, srv)
	if stopDoor == nil {
		t.Fatal("startCustosDoor returned nil stop")
	}
	defer stopDoor()

	deadline := time.Now().Add(5 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			lastBody = rec.Body.String()
			var resp struct {
				Cards []surface.CustosCard `json:"cards"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil && len(resp.Cards) > 0 {
				if resp.Cards[0].ID == cardID {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1. GET /v1/custos/cards lists the card
	reqList := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
	reqList.Host = "127.0.0.1"
	reqList.Header.Set("Authorization", "Bearer "+tok)
	recList := httptest.NewRecorder()
	handler.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("GET /v1/custos/cards code = %d, want 200; body = %s", recList.Code, recList.Body.String())
	}

	var listResp struct {
		Cards []surface.CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(recList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal cards: %v; body = %s", err, lastBody)
	}
	if len(listResp.Cards) != 1 || listResp.Cards[0].ID != cardID {
		t.Fatalf("cards = %+v, want 1 card with id %s", listResp.Cards, cardID)
	}

	// 2. POST resolve verdict once returns 200 approved
	resolveBody := `{"verdict":"once"}`
	reqResolve := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(resolveBody))
	reqResolve.Host = "127.0.0.1"
	reqResolve.Header.Set("Authorization", "Bearer "+tok)
	recResolve := httptest.NewRecorder()
	handler.ServeHTTP(recResolve, reqResolve)

	if recResolve.Code != http.StatusOK {
		t.Fatalf("POST resolve code = %d, want 200; body = %s", recResolve.Code, recResolve.Body.String())
	}
	var resolveResp map[string]string
	if err := json.Unmarshal(recResolve.Body.Bytes(), &resolveResp); err != nil {
		t.Fatalf("unmarshal resolve resp: %v", err)
	}
	if resolveResp["state"] != "approved" {
		t.Fatalf("resolve state = %q, want approved", resolveResp["state"])
	}

	// 3. Second resolve returns 409 already_answered
	reqResolve2 := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(resolveBody))
	reqResolve2.Host = "127.0.0.1"
	reqResolve2.Header.Set("Authorization", "Bearer "+tok)
	recResolve2 := httptest.NewRecorder()
	handler.ServeHTTP(recResolve2, reqResolve2)

	if recResolve2.Code != http.StatusConflict {
		t.Fatalf("POST second resolve code = %d, want 409; body = %s", recResolve2.Code, recResolve2.Body.String())
	}
	var resolveResp2 map[string]string
	if err := json.Unmarshal(recResolve2.Body.Bytes(), &resolveResp2); err != nil {
		t.Fatalf("unmarshal second resolve resp: %v", err)
	}
	if resolveResp2["error"] != "already_answered" {
		t.Fatalf("second resolve error = %q, want already_answered", resolveResp2["error"])
	}
}
