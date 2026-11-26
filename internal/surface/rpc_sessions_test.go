package surface

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/router"
)

func dialAuthenticatedWS(t *testing.T, tsURL, token string) *testWSClient {
	t.Helper()
	client := dialTestWS(t, tsURL+"/v1/ws")
	hello, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": token})
	if err != nil {
		t.Fatalf("EncodeRPCRequest(auth.hello): %v", err)
	}
	client.writeMaskedText(t, hello)
	op, payload := client.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("auth.hello response opcode = %d, want %d", op, wsOpText)
	}
	resp, err := DecodeRPCResponse(payload)
	if err != nil {
		t.Fatalf("DecodeRPCResponse(auth.hello): %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("auth.hello error: %+v", resp.Error)
	}
	return client
}

func (c *testWSClient) callRPC(t *testing.T, id int64, method string, params any) *RPCResponse {
	t.Helper()
	req, err := EncodeRPCRequest(id, method, params)
	if err != nil {
		t.Fatalf("EncodeRPCRequest(%s): %v", method, err)
	}
	c.writeMaskedText(t, req)
	for {
		op, payload := c.readServerFrame(t)
		if op != wsOpText {
			t.Fatalf("response opcode for %s = %d, want text", method, op)
		}
		// Unsolicited notifications (session.event frames from an in-flight
		// turn) interleave with responses on a live connection: the wire
		// law (RPC-SPEC §5 item 5) is 1:1 only for the response itself, so
		// skip past notifications as any real client does.
		if _, nerr := DecodeRPCNotification(payload); nerr == nil {
			continue
		}
		resp, err := DecodeRPCResponse(payload)
		if err != nil {
			t.Fatalf("DecodeRPCResponse(%s): %v (raw=%s)", method, err, string(payload))
		}
		if resp.ID != id {
			t.Fatalf("response id for %s = %d, want %d", method, resp.ID, id)
		}
		return resp
	}
}

func assertRPCErrorReason(t *testing.T, resp *RPCResponse, wantReason string) {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected RPC error with reason %q, got success result: %s", wantReason, string(resp.Result))
	}
	if resp.Error.Code != RPCErrorCodeGeneric {
		t.Errorf("error.code = %d, want %d", resp.Error.Code, RPCErrorCodeGeneric)
	}
	if got := resp.Error.Data["reason"]; got != wantReason {
		t.Errorf("error.data.reason = %v, want %q (message=%q)", got, wantReason, resp.Error.Message)
	}
}

func TestRPCSessionsList(t *testing.T) {
	t.Run("happy path returns newest-first sessions", func(t *testing.T) {
		srv, store, ts := newTestRPCServer(t, 2*time.Second)
		title := "Side Chat"
		srv.Sessions = &fakeSessionSource{
			sessions: []SessionInfo{
				{ID: "s_AAAAAAAAAAAAAAAAAAAAAAAAAA", Created: "2026-11-20T12:00:00Z", Kind: "side", Title: &title},
				{ID: "main", Created: "2026-11-20T10:00:00Z", Kind: "main"},
			},
		}
		tok, err := store.Create("list-test")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		resp := client.callRPC(t, 1, "sessions.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			Sessions []SessionInfo `json:"sessions"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("unmarshal sessions.list result: %v", err)
		}
		if len(out.Sessions) != 2 {
			t.Fatalf("sessions len = %d, want 2", len(out.Sessions))
		}
		if out.Sessions[0].ID != "s_AAAAAAAAAAAAAAAAAAAAAAAAAA" || out.Sessions[1].ID != "main" {
			t.Errorf("sessions order = [%s, %s], want side then main", out.Sessions[0].ID, out.Sessions[1].ID)
		}
	})

	t.Run("source error returns internal_error", func(t *testing.T) {
		srv, store, ts := newTestRPCServer(t, 2*time.Second)
		srv.Sessions = &fakeSessionSource{listErr: errors.New("disk read error")}
		tok, err := store.Create("list-err")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		resp := client.callRPC(t, 2, "sessions.list", map[string]any{})
		assertRPCErrorReason(t, resp, "internal_error")
	})
}

func TestRPCSessionsCreate(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	cat := sampleCatalog()
	srv.Models = cat
	tok, err := store.Create("create-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("happy path with valid model_pin", func(t *testing.T) {
		sessions := &trackingSessionSource{}
		srv.Sessions = sessions
		srv.Models = cat

		resp := client.callRPC(t, 10, "sessions.create", map[string]any{
			"title":     "Research",
			"model_pin": "openrouter/anthropic/claude-3.5-sonnet",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var got SessionInfo
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal SessionInfo: %v", err)
		}
		if got.ID == "" {
			t.Errorf("expected non-empty session ID")
		}
		if sessions.createCalls != 1 || sessions.lastPin != "openrouter/anthropic/claude-3.5-sonnet" {
			t.Errorf("createCalls=%d lastPin=%q", sessions.createCalls, sessions.lastPin)
		}
	})

	t.Run("invalid model_pin rejected with invalid_model_pin", func(t *testing.T) {
		sessions := &trackingSessionSource{}
		srv.Sessions = sessions
		srv.Models = cat

		resp := client.callRPC(t, 11, "sessions.create", map[string]any{
			"title":     "Bad Pin",
			"model_pin": "unknown/model",
		})
		assertRPCErrorReason(t, resp, "invalid_model_pin")
		if sessions.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0", sessions.createCalls)
		}
	})

	t.Run("no_key model_pin rejected with invalid_model_pin", func(t *testing.T) {
		sessions := &trackingSessionSource{}
		srv.Sessions = sessions
		srv.Models = cat

		resp := client.callRPC(t, 12, "sessions.create", map[string]any{
			"title":     "No Key Pin",
			"model_pin": "openai/gpt-4o",
		})
		assertRPCErrorReason(t, resp, "invalid_model_pin")
	})

	t.Run("nil catalog with model_pin rejected with model_catalog_unavailable", func(t *testing.T) {
		sessions := &trackingSessionSource{}
		srv.Sessions = sessions
		srv.Models = nil

		resp := client.callRPC(t, 13, "sessions.create", map[string]any{
			"title":     "Nil Catalog",
			"model_pin": "openrouter/anthropic/claude-3.5-sonnet",
		})
		assertRPCErrorReason(t, resp, "model_catalog_unavailable")
		if sessions.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0", sessions.createCalls)
		}
	})

	t.Run("source error returns internal_error", func(t *testing.T) {
		srv.Sessions = &fakeSessionSource{createErr: errors.New("disk full")}
		srv.Models = cat

		resp := client.callRPC(t, 14, "sessions.create", map[string]any{
			"title": "Fail",
		})
		assertRPCErrorReason(t, resp, "internal_error")
	})
}

func TestRPCSessionsDelete(t *testing.T) {
	const sideID = "s_AAAAAAAAAAAAAAAAAAAAAAAAAA"
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	tok, err := store.Create("delete-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("happy path deletes side session", func(t *testing.T) {
		sessions := &fakeSessionSource{}
		srv.Sessions = sessions
		srv.Hub = &fakeHub{}

		resp := client.callRPC(t, 20, "sessions.delete", map[string]any{"id": sideID})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK {
			t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
		}
		if len(sessions.deleted) != 1 || sessions.deleted[0] != sideID {
			t.Errorf("deleted = %v, want [%s]", sessions.deleted, sideID)
		}
	})

	t.Run("409 session_busy when turn in flight", func(t *testing.T) {
		sessions := &fakeSessionSource{}
		srv.Sessions = sessions
		srv.Hub = &fakeHub{inFlight: map[string]bool{sideID: true}}

		resp := client.callRPC(t, 21, "sessions.delete", map[string]any{"id": sideID})
		assertRPCErrorReason(t, resp, "session_busy")
		if len(sessions.deleted) != 0 {
			t.Errorf("busy session must not be deleted: %v", sessions.deleted)
		}
	})

	t.Run("403 session_is_protected for main session", func(t *testing.T) {
		srv.Sessions = &fakeSessionSource{deleteErr: ErrProtectedSession}
		srv.Hub = &fakeHub{}

		resp := client.callRPC(t, 22, "sessions.delete", map[string]any{"id": "main"})
		assertRPCErrorReason(t, resp, "session_is_protected")
	})

	t.Run("404 no_such_session for unknown or malformed id", func(t *testing.T) {
		srv.Sessions = &fakeSessionSource{deleteErr: ErrNoSession}
		srv.Hub = &fakeHub{}

		resp1 := client.callRPC(t, 23, "sessions.delete", map[string]any{"id": sideID})
		assertRPCErrorReason(t, resp1, "no_such_session")

		resp2 := client.callRPC(t, 24, "sessions.delete", map[string]any{"id": "not-a-valid-id"})
		assertRPCErrorReason(t, resp2, "no_such_session")
	})
}

func TestRPCSessionsSubscribe(t *testing.T) {
	const validID = "s_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	events := []json.RawMessage{
		json.RawMessage(`{"seq":1,"t":"msg","role":"user","text":"hi"}`),
		json.RawMessage(`{"seq":2,"t":"msg","role":"assistant","text":"hello"}`),
	}
	srv.Sessions = &fakeSessionSource{events: events, inFlight: true}
	tok, err := store.Create("sub-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("catch-up default and after_seq", func(t *testing.T) {
		resp := client.callRPC(t, 30, "sessions.subscribe", map[string]any{"id": validID})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			Events   []json.RawMessage `json:"events"`
			InFlight bool              `json:"in_flight"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if len(out.Events) != 2 || !out.InFlight {
			t.Errorf("got len=%d in_flight=%v, want len=2 in_flight=true", len(out.Events), out.InFlight)
		}

		// With after_seq=1
		resp2 := client.callRPC(t, 31, "sessions.subscribe", map[string]any{
			"id":        validID,
			"after_seq": 1,
			"limit":     5000, // clamped to 1000
		})
		if resp2.Error != nil {
			t.Fatalf("unexpected error: %+v", resp2.Error)
		}
		var out2 struct {
			Events   []json.RawMessage `json:"events"`
			InFlight bool              `json:"in_flight"`
		}
		if err := json.Unmarshal(resp2.Result, &out2); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if len(out2.Events) != 1 {
			t.Errorf("got len=%d, want 1", len(out2.Events))
		}
	})

	t.Run("validation errors on after_seq, limit, and session id", func(t *testing.T) {
		respNegSeq := client.callRPC(t, 32, "sessions.subscribe", map[string]any{
			"id":        validID,
			"after_seq": -1,
		})
		assertRPCErrorReason(t, respNegSeq, "invalid_after_seq")

		respBadSeq := client.callRPC(t, 33, "sessions.subscribe", map[string]any{
			"id":        validID,
			"after_seq": "abc",
		})
		assertRPCErrorReason(t, respBadSeq, "invalid_after_seq")

		respZeroLim := client.callRPC(t, 34, "sessions.subscribe", map[string]any{
			"id":    validID,
			"limit": 0,
		})
		assertRPCErrorReason(t, respZeroLim, "invalid_limit")

		respNegLim := client.callRPC(t, 35, "sessions.subscribe", map[string]any{
			"id":    validID,
			"limit": -5,
		})
		assertRPCErrorReason(t, respNegLim, "invalid_limit")

		respBadID := client.callRPC(t, 36, "sessions.subscribe", map[string]any{
			"id": "invalid_id",
		})
		assertRPCErrorReason(t, respBadID, "not_found")

		srv.Sessions = &fakeSessionSource{eventsErr: ErrNoSession}
		respNoSess := client.callRPC(t, 37, "sessions.subscribe", map[string]any{
			"id": validID,
		})
		assertRPCErrorReason(t, respNoSess, "not_found")
	})
}

func TestRPCSessionsCancel(t *testing.T) {
	const validID = "s_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	tok, err := store.Create("cancel-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("cancels in-flight turn", func(t *testing.T) {
		srv.Hub = &fakeHub{
			inFlight: map[string]bool{validID: true},
			cancel:   map[string]bool{validID: true},
		}
		resp := client.callRPC(t, 40, "sessions.cancel", map[string]any{"id": validID})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK {
			t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
		}
	})

	t.Run("404 no_turn_in_flight when idle", func(t *testing.T) {
		srv.Hub = &fakeHub{
			inFlight: map[string]bool{},
			cancel:   map[string]bool{},
		}
		resp := client.callRPC(t, 41, "sessions.cancel", map[string]any{"id": validID})
		assertRPCErrorReason(t, resp, "no_turn_in_flight")
	})

	t.Run("invalid session id returns not_found", func(t *testing.T) {
		resp := client.callRPC(t, 42, "sessions.cancel", map[string]any{"id": "bad-id"})
		assertRPCErrorReason(t, resp, "not_found")
	})
}

func TestRPCSessionsApprovalsResolve(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(_ int, _ []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}
	hub := setupHub(t, sp)

	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      NewPenatusSource(hub.hearthHome, hub),
		Hub:           hub,
		wsAuthTimeout: 2 * time.Second,
	}
	ts := httptest.NewUnstartedServer(NewServer(srv))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
	}
	ts.Start()
	t.Cleanup(ts.Close)

	tok, err := store.Create("approvals-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	t.Run("happy path allow and 410 already_resolved on second resolve", func(t *testing.T) {
		aid, ch := hub.ap.Register("main", "echo", `{"v":"hi"}`, func(string) {})

		resp := client.callRPC(t, 50, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": aid,
			"decision":    "allow",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK {
			t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
		}
		select {
		case dec := <-ch:
			if !dec {
				t.Fatal("expected allow decision=true")
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for decision")
		}

		// Second resolve -> 410 already_resolved
		resp2 := client.callRPC(t, 51, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": aid,
			"decision":    "allow",
		})
		assertRPCErrorReason(t, resp2, "already_resolved")
	})

	t.Run("scope validation and connector trust minting", func(t *testing.T) {
		// 1. deny + scope -> 400 invalid_request
		aid1, _ := hub.ap.Register("main", "mcp__tool__one", `{}`, func(string) {})
		resp1 := client.callRPC(t, 52, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": aid1,
			"decision":    "deny",
			"scope":       "connector",
		})
		assertRPCErrorReason(t, resp1, "invalid_request")

		// 2. invalid scope value -> 400 invalid_request
		aid2, _ := hub.ap.Register("main", "mcp__tool__two", `{}`, func(string) {})
		resp2 := client.callRPC(t, 53, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": aid2,
			"decision":    "allow",
			"scope":       "global",
		})
		assertRPCErrorReason(t, resp2, "invalid_request")

		// 3. valid scope "connector" with decision "allow"
		var minted string
		hub.SetConnectorTrustHook(func(toolName string) error {
			minted = toolName
			return nil
		})
		aid3, ch3 := hub.ap.Register("main", "mcp__tool__three", `{}`, func(string) {})
		resp3 := client.callRPC(t, 54, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": aid3,
			"decision":    "allow",
			"scope":       "connector",
		})
		if resp3.Error != nil {
			t.Fatalf("unexpected error: %+v", resp3.Error)
		}
		if minted != "mcp__tool__three" {
			t.Errorf("minted = %q, want mcp__tool__three", minted)
		}
		select {
		case dec := <-ch3:
			if !dec {
				t.Fatal("expected allow")
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for ch3")
		}
	})

	t.Run("404 not_found for unknown approval or foreign session", func(t *testing.T) {
		aid, _ := hub.ap.Register("main", "echo", `{}`, func(string) {})
		foreignID := "s_BBBBBBBBBBBBBBBBBBBBBBBBBB"

		respForeign := client.callRPC(t, 55, "sessions.approvals.resolve", map[string]any{
			"session_id":  foreignID,
			"approval_id": aid,
			"decision":    "allow",
		})
		assertRPCErrorReason(t, respForeign, "not_found")

		respUnknown := client.callRPC(t, 56, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": "a_0000000000000000",
			"decision":    "allow",
		})
		assertRPCErrorReason(t, respUnknown, "not_found")
	})
}

func TestRPCSessionsSend_ValidationAndGates(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(_ int, _ []router.Message) (*router.Completion, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return &router.Completion{Text: "hello back", Model: "stub"}, nil
		},
	}
	hub := setupHub(t, sp)
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      NewPenatusSource(hub.hearthHome, hub),
		Hub:           hub,
		wsAuthTimeout: 2 * time.Second,
	}
	ts := httptest.NewUnstartedServer(NewServer(srv))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
	}
	ts.Start()
	t.Cleanup(ts.Close)

	tok, err := store.Create("send-gates")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	// 1. Not subscribed yet -> 409 not_subscribed (RPC-SPEC §7.2)
	respUnsub := client.callRPC(t, 60, "sessions.send", map[string]any{
		"id":   "main",
		"text": "hello before subscribe",
	})
	assertRPCErrorReason(t, respUnsub, "not_subscribed")

	// 2. Empty/whitespace text -> 400 empty_message_text
	respEmpty := client.callRPC(t, 61, "sessions.send", map[string]any{
		"id":   "main",
		"text": "   \t\n ",
	})
	assertRPCErrorReason(t, respEmpty, "empty_message_text")

	// 3. Invalid session ID -> 404 not_found
	respBadID := client.callRPC(t, 62, "sessions.send", map[string]any{
		"id":   "bad_id",
		"text": "hello",
	})
	assertRPCErrorReason(t, respBadID, "not_found")

	// 4. Subscribe to main
	respSub := client.callRPC(t, 63, "sessions.subscribe", map[string]any{"id": "main"})
	if respSub.Error != nil {
		t.Fatalf("subscribe failed: %+v", respSub.Error)
	}

	// 5. First sessions.send -> accepted:true immediately
	respSend1 := client.callRPC(t, 64, "sessions.send", map[string]any{
		"id":   "main",
		"text": "first turn",
	})
	if respSend1.Error != nil {
		t.Fatalf("first send failed: %+v", respSend1.Error)
	}
	var ack struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(respSend1.Result, &ack); err != nil || !ack.Accepted {
		t.Fatalf("send result = %s, want {\"accepted\":true}", string(respSend1.Result))
	}

	// Wait until turn is inside provider
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not start")
	}

	// 6. Second concurrent sessions.send on same session -> 409 turn_in_flight
	respSend2 := client.callRPC(t, 65, "sessions.send", map[string]any{
		"id":   "main",
		"text": "second concurrent turn",
	})
	assertRPCErrorReason(t, respSend2, "turn_in_flight")

	// Release first turn and drain notifications until turn_done
	close(release)
	for {
		_, payload := client.readServerFrame(t)
		notif, err := DecodeRPCNotification(payload)
		if err != nil {
			t.Fatalf("expected notification frame, got %s: %v", string(payload), err)
		}
		if notif.Method != "session.event" {
			t.Fatalf("notif.Method = %q, want session.event", notif.Method)
		}
		var p struct {
			SessionID string         `json:"session_id"`
			Event     map[string]any `json:"event"`
		}
		if err := json.Unmarshal(notif.Params, &p); err != nil {
			t.Fatalf("unmarshal notif params: %v", err)
		}
		if p.Event["type"] == "turn_done" {
			break
		}
	}
}

func TestRPCSessionsMultiplexingInterleave(t *testing.T) {
	// Proves an in-flight turn's session.event notifications interleave
	// cleanly with other method calls multiplexed on the same connection.
	step1Ready := make(chan struct{})
	allowStep2 := make(chan struct{})

	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, _ []router.Message) (*router.Completion, error) {
			if n == 1 {
				return &router.Completion{
					Text:      "thinking...",
					ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"interleave"}`}},
					Model:     "stub",
				}, nil
			}
			close(step1Ready)
			<-allowStep2
			return &router.Completion{Text: "final answer", Model: "stub"}, nil
		},
	}
	hub := setupHub(t, sp)
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      NewPenatusSource(hub.hearthHome, hub),
		Hub:           hub,
		wsAuthTimeout: 2 * time.Second,
	}
	ts := httptest.NewUnstartedServer(NewServer(srv))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
	}
	ts.Start()
	t.Cleanup(ts.Close)

	tok, err := store.Create("mux-interleave")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	// 1. Subscribe to main
	subResp := client.callRPC(t, 100, "sessions.subscribe", map[string]any{"id": "main"})
	if subResp.Error != nil {
		t.Fatalf("subscribe error: %+v", subResp.Error)
	}

	// 2. Start turn via sessions.send
	sendReq, _ := EncodeRPCRequest(101, "sessions.send", map[string]any{
		"id":   "main",
		"text": "run echo tool",
	})
	client.writeMaskedText(t, sendReq)

	// Read frames until we have both the sessions.send response (id=101)
	// and the approval_request session.event notification.
	var gotSendAck bool
	var approvalID string
	var seenEvents []string

	readNext := func() {
		_, payload := client.readServerFrame(t)
		if notif, err := DecodeRPCNotification(payload); err == nil {
			if notif.Method != "session.event" {
				t.Fatalf("unexpected notification method %q", notif.Method)
			}
			var p struct {
				SessionID string         `json:"session_id"`
				Event     map[string]any `json:"event"`
			}
			if err := json.Unmarshal(notif.Params, &p); err != nil {
				t.Fatalf("unmarshal notif: %v", err)
			}
			if p.SessionID != "main" {
				t.Errorf("notif session_id = %q, want main", p.SessionID)
			}
			evType, _ := p.Event["type"].(string)
			seenEvents = append(seenEvents, evType)
			if evType == "approval_request" {
				approvalID, _ = p.Event["approval_id"].(string)
			}
			return
		}
		resp, err := DecodeRPCResponse(payload)
		if err != nil {
			t.Fatalf("frame neither notification nor response: %s (%v)", string(payload), err)
		}
		if resp.ID == 101 {
			if resp.Error != nil {
				t.Fatalf("sessions.send error: %+v", resp.Error)
			}
			gotSendAck = true
		} else {
			t.Fatalf("unexpected response id %d", resp.ID)
		}
	}

	for !gotSendAck || approvalID == "" {
		readNext()
	}

	// 3. While the turn is paused awaiting approval, multiplex a sessions.list call
	// and a sessions.approvals.resolve call on the SAME WebSocket connection.
	listReq, _ := EncodeRPCRequest(102, "sessions.list", map[string]any{})
	client.writeMaskedText(t, listReq)

	listRespFrameOp, listRespPayload := client.readServerFrame(t)
	if listRespFrameOp != wsOpText {
		t.Fatalf("expected text frame, got %d", listRespFrameOp)
	}
	listResp, err := DecodeRPCResponse(listRespPayload)
	if err != nil || listResp.ID != 102 || listResp.Error != nil {
		t.Fatalf("sessions.list mid-turn failed: %+v (err=%v)", listResp, err)
	}

	// Resolve the pending approval over the same connection
	resolveReq, _ := EncodeRPCRequest(103, "sessions.approvals.resolve", map[string]any{
		"session_id":  "main",
		"approval_id": approvalID,
		"decision":    "allow",
	})
	client.writeMaskedText(t, resolveReq)

	// Wait for round 2 in provider, then unblock final completion
	select {
	case <-step1Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not advance after approval resolution")
	}
	close(allowStep2)

	var gotResolveAck bool
	var gotTurnDone bool
	for !gotResolveAck || !gotTurnDone {
		_, payload := client.readServerFrame(t)
		if notif, err := DecodeRPCNotification(payload); err == nil {
			var p struct {
				SessionID string         `json:"session_id"`
				Event     map[string]any `json:"event"`
			}
			if err := json.Unmarshal(notif.Params, &p); err != nil {
				t.Fatalf("unmarshal notif: %v", err)
			}
			evType, _ := p.Event["type"].(string)
			seenEvents = append(seenEvents, evType)
			if evType == "turn_done" {
				gotTurnDone = true
			}
			continue
		}
		resp, err := DecodeRPCResponse(payload)
		if err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.ID == 103 {
			if resp.Error != nil {
				t.Fatalf("resolve error: %+v", resp.Error)
			}
			gotResolveAck = true
		}
	}

	// Verify exact notification sequence for the turn:
	// turn_start -> delta -> approval_request -> tool_start -> tool_done -> delta -> turn_done
	// (turn_start leads since #345: the client must learn the turn id
	// before it can scope a cancel to this exact turn.)
	wantSeq := []string{"turn_start", "delta", "approval_request", "tool_start", "tool_done", "delta", "turn_done"}
	if len(seenEvents) != len(wantSeq) {
		t.Fatalf("seenEvents = %v, want %v", seenEvents, wantSeq)
	}
	for i, w := range wantSeq {
		if seenEvents[i] != w {
			t.Errorf("seenEvents[%d] = %q, want %q (all=%v)", i, seenEvents[i], w, seenEvents)
		}
	}
}

func TestRPCSessionsDisconnectSemantics_TranslationNote1(t *testing.T) {
	t.Run("mid-turn WebSocket disconnect denies pending approval immediately", func(t *testing.T) {
		sp := &stubProvider{
			name: "stub",
			caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
			replies: func(n int, _ []router.Message) (*router.Completion, error) {
				if n == 1 {
					return &router.Completion{
						ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"drop"}`}},
						Model:     "stub",
					}, nil
				}
				return &router.Completion{Text: "after denial", Model: "stub"}, nil
			},
		}
		hub := setupHub(t, sp)
		store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
		if err != nil {
			t.Fatal(err)
		}
		srv := &Server{
			Store:         store,
			Sessions:      NewPenatusSource(hub.hearthHome, hub),
			Hub:           hub,
			wsAuthTimeout: 2 * time.Second,
		}
		ts := httptest.NewUnstartedServer(NewServer(srv))
		ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
		}
		ts.Start()
		t.Cleanup(ts.Close)

		tok, err := store.Create("disconnect-deny")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		// Subscribe and start turn
		if resp := client.callRPC(t, 200, "sessions.subscribe", map[string]any{"id": "main"}); resp.Error != nil {
			t.Fatalf("subscribe: %+v", resp.Error)
		}
		sendReq, _ := EncodeRPCRequest(201, "sessions.send", map[string]any{
			"id":   "main",
			"text": "trigger approval then drop",
		})
		client.writeMaskedText(t, sendReq)

		// Wait until we receive the approval_request notification
		var approvalID string
		for approvalID == "" {
			_, payload := client.readServerFrame(t)
			if notif, err := DecodeRPCNotification(payload); err == nil {
				var p struct {
					Event map[string]any `json:"event"`
				}
				_ = json.Unmarshal(notif.Params, &p)
				if p.Event["type"] == "approval_request" {
					approvalID, _ = p.Event["approval_id"].(string)
				}
			}
		}

		if hub.ap.PendingCount("main") != 1 {
			t.Fatalf("PendingCount = %d, want 1 while socket is open", hub.ap.PendingCount("main"))
		}

		// Close the WebSocket connection mid-turn while approval is pending
		_ = client.conn.Close()

		// Confirm the pending approval is denied with reason "disconnected" (not left hanging)
		deadline := time.Now().Add(2 * time.Second)
		for hub.ap.PendingCount("main") != 0 || hub.InFlight("main") {
			if time.Now().After(deadline) {
				t.Fatalf("approval or turn left hanging after WS disconnect: pending=%d inFlight=%v",
					hub.ap.PendingCount("main"), hub.InFlight("main"))
			}
			time.Sleep(10 * time.Millisecond)
		}

		if gotReason := hub.ap.Reason("main", approvalID); gotReason != "disconnected" {
			t.Errorf("approval reason after WS disconnect = %q, want \"disconnected\"", gotReason)
		}
	})

	t.Run("live connection held open after sessions.send returns does NOT wrongly deny approval", func(t *testing.T) {
		sp := &stubProvider{
			name: "stub",
			caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
			replies: func(n int, _ []router.Message) (*router.Completion, error) {
				if n == 1 {
					// Small delay so sessions.send handler goroutine has definitely returned
					// before the engine reaches the approval gate.
					time.Sleep(40 * time.Millisecond)
					return &router.Completion{
						ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"hold-open"}`}},
						Model:     "stub",
					}, nil
				}
				return &router.Completion{Text: "completed normally", Model: "stub"}, nil
			},
		}
		hub := setupHub(t, sp)
		store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
		if err != nil {
			t.Fatal(err)
		}
		srv := &Server{
			Store:         store,
			Sessions:      NewPenatusSource(hub.hearthHome, hub),
			Hub:           hub,
			wsAuthTimeout: 2 * time.Second,
		}
		ts := httptest.NewUnstartedServer(NewServer(srv))
		ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
		}
		ts.Start()
		t.Cleanup(ts.Close)

		tok, err := store.Create("stay-open")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		if resp := client.callRPC(t, 300, "sessions.subscribe", map[string]any{"id": "main"}); resp.Error != nil {
			t.Fatalf("subscribe: %+v", resp.Error)
		}

		// sessions.send returns {"accepted":true} immediately
		sendResp := client.callRPC(t, 301, "sessions.send", map[string]any{
			"id":   "main",
			"text": "run tool after send returns",
		})
		if sendResp.Error != nil {
			t.Fatalf("send: %+v", sendResp.Error)
		}

		// The approval_request arrives AFTER sessions.send has already returned.
		// Because the WebSocket connection is still open, it must remain pending
		// (not auto-denied by sessions.send returning) and be resolvable with allow.
		var approvalID string
		for approvalID == "" {
			_, payload := client.readServerFrame(t)
			notif, err := DecodeRPCNotification(payload)
			if err != nil {
				t.Fatalf("expected notification, got %s: %v", string(payload), err)
			}
			var p struct {
				Event map[string]any `json:"event"`
			}
			_ = json.Unmarshal(notif.Params, &p)
			if p.Event["type"] == "approval_request" {
				approvalID, _ = p.Event["approval_id"].(string)
			}
		}

		if hub.ap.PendingCount("main") != 1 {
			t.Fatalf("approval should be pending (not auto-denied), got PendingCount=%d reason=%q",
				hub.ap.PendingCount("main"), hub.ap.Reason("main", approvalID))
		}

		// Resolve with allow and confirm tool_done has ok:true and approval:"approved:<id>"
		resolveReq, _ := EncodeRPCRequest(302, "sessions.approvals.resolve", map[string]any{
			"session_id":  "main",
			"approval_id": approvalID,
			"decision":    "allow",
		})
		client.writeMaskedText(t, resolveReq)

		var sawToolDoneOK bool
		var sawTurnDone bool
		for !sawTurnDone {
			_, payload := client.readServerFrame(t)
			if notif, err := DecodeRPCNotification(payload); err == nil {
				var p struct {
					Event map[string]any `json:"event"`
				}
				_ = json.Unmarshal(notif.Params, &p)
				switch p.Event["type"] {
				case "tool_done":
					okVal, _ := p.Event["ok"].(bool)
					appStr, _ := p.Event["approval"].(string)
					if !okVal || !strings.HasPrefix(appStr, "approved:") {
						t.Fatalf("tool_done = %+v, want ok=true approval=approved:*", p.Event)
					}
					sawToolDoneOK = true
				case "turn_done":
					sawTurnDone = true
				}
			}
		}
		if !sawToolDoneOK {
			t.Fatal("did not see tool_done with ok=true")
		}
	})
}
