package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// TestMCPWiring_CardsEndpointAndToolInjection verifies:
// 1. /v1/custos/cards returns connector cards from the door-client snapshot.
// 2. /v1/custos/cards returns empty array when no door client is wired.
// 3. Web-chat tool injection dynamically includes rail tools in Hub.
func TestMCPWiring_CardsEndpointAndToolInjection(t *testing.T) {
	dir := shortTmp(t)
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Test empty-array contract when no door client is wired
	tokenStore, err := surface.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	tok, err := tokenStore.Create("test-op")
	if err != nil {
		t.Fatalf("tokenStore.Create: %v", err)
	}

	srv := &surface.Server{
		Store: tokenStore,
		Cfg:   surface.ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	handler := surface.NewServer(srv)

	reqNoDoor := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
	reqNoDoor.Host = "127.0.0.1"
	reqNoDoor.Header.Set("Authorization", "Bearer "+tok)
	recNoDoor := httptest.NewRecorder()
	handler.ServeHTTP(recNoDoor, reqNoDoor)

	if recNoDoor.Code != http.StatusOK {
		t.Fatalf("GET /v1/custos/cards with no door client code = %d, want 200", recNoDoor.Code)
	}
	var respNoDoor struct {
		Cards []surface.CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(recNoDoor.Body.Bytes(), &respNoDoor); err != nil {
		t.Fatalf("unmarshal cards: %v", err)
	}
	if len(respNoDoor.Cards) != 0 {
		t.Fatalf("cards len = %d, want 0 when no door client is wired", len(respNoDoor.Cards))
	}

	// 2. Setup door server with a ready MCP connector
	v := custos.NewVault(stateDir, 5*time.Second)
	pass := "secret-mcp-passphrase"
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}

	hub := surface.NewApprovalHub(10 * time.Second)
	v.SetApprovalHub(hub)

	cfg := custos.MCPServerConfig{
		Name:      "github-rail",
		Transport: "stdio",
		Command:   "cat",
	}
	conn := custos.NewMCPConnector(cfg, stateDir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "github-rail",
		Tools: []custos.SnapshotTool{
			{Canonical: "create_issue", Title: "Create Issue"},
		},
		ToolInfos: []custos.ToolInfo{
			{Connector: "github-rail", Name: "create_issue", Title: "Create Issue"},
		},
	})

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, stateDir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(stateDir, hub, custos.WithVault(v), custos.WithRail(reg))
	if err != nil {
		t.Fatalf("StartDoorServer: %v", err)
	}
	defer doorSrv.Close()

	sockPath, tokenPath := custos.DoorPaths(stateDir)

	// Create surface Hub and Server
	staticTools := []loop.Tool{
		{
			Spec: routerToolSpec("static_tool", "Static Tool"),
		},
	}
	surfaceHub := surface.NewHub(surface.ServeConfig{}, dir, 1000, 80, staticTools, nil, hub)
	srv.Hub = surfaceHub

	custosCfg := CustosConfig{
		DoorsSock: sockPath,
		DoorToken: tokenPath,
	}

	stop := startCustosDoor(context.Background(), custosCfg, nil, srv)
	if stop == nil {
		t.Fatal("startCustosDoor returned nil stop")
	}
	defer stop()

	// Wait for door client to attach and populate snapshot
	deadline := time.Now().Add(5 * time.Second)
	var lastCards []surface.CustosCard
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Authorization", "Bearer "+tok)
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			var resp struct {
				Cards []surface.CustosCard `json:"cards"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil && len(resp.Cards) > 0 {
				lastCards = resp.Cards
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(lastCards) != 1 {
		t.Fatalf("cards = %+v, want 1 connector card", lastCards)
	}
	card := lastCards[0]
	if card.Name != "github-rail" || card.State != "ready" || card.Tools != 1 {
		t.Fatalf("unexpected connector card: %+v", card)
	}

	// 3. Verify web-chat tool injection into Hub
	hubTools := surfaceHub.Tools()
	foundMCPTool := false
	for _, t := range hubTools {
		if t.Spec.Name == "create_issue" && t.Spec.Description == "Create Issue" {
			foundMCPTool = true
			break
		}
	}
	if !foundMCPTool {
		t.Fatalf("hub tools missing injected MCP tool: %+v", hubTools)
	}
}

func routerToolSpec(name, desc string) router.ToolSpec {
	return router.ToolSpec{
		Name:             name,
		Description:      desc,
		ParamsJSONSchema: `{"type":"object"}`,
	}
}
