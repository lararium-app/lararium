package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

// fakeDoor stands up a unix socket that speaks enough of the doors protocol
// (CUSTOS-SPEC §6.4b) to let the real custosdoor client attach: HELLO →
// "OK []" snapshot, then one MCP-STATUS push. This keeps the hearthd wiring
// test off any custos-internal test seams.
func fakeDoor(t *testing.T, sockPath, statusFrame string) func() {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake door listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "HELLO ") {
					return
				}
				_, _ = c.Write([]byte("OK []\n"))
				if statusFrame != "" {
					_, _ = c.Write([]byte(statusFrame + "\n"))
				}
				// Drain until the client hangs up or the test ends.
				for {
					select {
					case <-done:
						return
					default:
					}
					_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					if _, err := br.ReadString('\n'); err != nil {
						var ne net.Error
						if errors.As(err, &ne) && ne.Timeout() {
							continue
						}
						return
					}
				}
			}(conn)
		}
	}()
	return func() { close(done); ln.Close() }
}

// TestMCPWiring_ConnectorsEndpoint verifies the additive web surface for the
// MCP rail (issue #86):
//  1. GET /v1/mcp/connectors answers {"connectors":[]} with no door client.
//  2. With a live door client it reports rail connector state pushed over
//     the doors protocol.
//  3. GET /v1/custos/cards stays frozen to approval cards — connector state
//     never rides the CUSTOS-SPEC §6.4a wire shape.
func TestMCPWiring_ConnectorsEndpoint(t *testing.T) {
	dir := shortTmp(t)

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

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. No door client wired: empty list, 200 (the UI polls unconditionally).
	rec := get("/v1/mcp/connectors")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/mcp/connectors (no door) code = %d, want 200", rec.Code)
	}
	var empty struct {
		Connectors []surface.ConnectorCard `json:"connectors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(empty.Connectors) != 0 {
		t.Fatalf("connectors = %v, want empty", empty.Connectors)
	}

	// 2. Attach through a fake door server that pushes one ready connector.
	sockPath := filepath.Join(dir, "doors.sock")
	tokenPath := filepath.Join(dir, "door.token")
	if err := os.WriteFile(tokenPath, []byte("test-door-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	frame := `MCP-STATUS [{"name":"github-rail","state":"ready","tools":1,"restarts":0,"reason":"ok"}]`
	cleanUp := fakeDoor(t, sockPath, frame)
	defer cleanUp()

	stop := startCustosDoor(context.Background(), CustosConfig{
		DoorsSock: sockPath,
		DoorToken: tokenPath,
	}, nil, srv)
	if stop == nil {
		t.Fatal("startCustosDoor returned nil stop")
	}
	defer stop()

	var got []surface.ConnectorCard
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec = get("/v1/mcp/connectors")
		if rec.Code == http.StatusOK {
			var resp struct {
				Connectors []surface.ConnectorCard `json:"connectors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil && len(resp.Connectors) > 0 {
				got = resp.Connectors
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("connectors = %v, want one github-rail row", got)
	}
	c := got[0]
	if c.Name != "github-rail" || c.State != "ready" || c.Tools != 1 {
		t.Fatalf("unexpected connector row: %+v", c)
	}

	// 3. Cards endpoint stays frozen: only pending approvals live there.
	rec = get("/v1/custos/cards")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/custos/cards code = %d, want 200", rec.Code)
	}
	var cards struct {
		Cards []surface.CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cards); err != nil {
		t.Fatalf("unmarshal cards: %v", err)
	}
	if len(cards.Cards) != 0 {
		t.Fatalf("cards = %v, want none (rail state must not ride the card feed)", cards.Cards)
	}
}
