package surface

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRPCEnvelopeCodecRoundTrip(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		raw, err := EncodeRPCRequest(42, "sessions.send", map[string]any{
			"id":   "main",
			"text": "hello hearth",
		})
		if err != nil {
			t.Fatalf("EncodeRPCRequest: %v", err)
		}

		// Verify wire shape matches RPC-SPEC §3: {"jsonrpc":"2.0","id":<int>,"method":"<namespace>.<verb>","params":{...}}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		if wire["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v, want 2.0", wire["jsonrpc"])
		}
		if wire["id"] != float64(42) {
			t.Errorf("id = %v, want 42", wire["id"])
		}
		if wire["method"] != "sessions.send" {
			t.Errorf("method = %v, want sessions.send", wire["method"])
		}

		dec, err := DecodeRPCRequest(raw)
		if err != nil {
			t.Fatalf("DecodeRPCRequest: %v", err)
		}
		if dec.JSONRPC != "2.0" {
			t.Errorf("JSONRPC = %q, want 2.0", dec.JSONRPC)
		}
		if dec.ID == nil || *dec.ID != 42 {
			t.Fatalf("ID = %v, want 42", dec.ID)
		}
		if dec.Method != "sessions.send" {
			t.Errorf("Method = %q, want sessions.send", dec.Method)
		}
		var params struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(dec.Params, &params); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if params.ID != "main" || params.Text != "hello hearth" {
			t.Errorf("params = %+v, want {id:main text:hello hearth}", params)
		}
	})

	t.Run("response success", func(t *testing.T) {
		raw, err := EncodeRPCResponse(7, map[string]any{
			"ok":      true,
			"version": "1.0.0-beta12",
		})
		if err != nil {
			t.Fatalf("EncodeRPCResponse: %v", err)
		}

		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		if wire["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v, want 2.0", wire["jsonrpc"])
		}
		if wire["id"] != float64(7) {
			t.Errorf("id = %v, want 7", wire["id"])
		}
		if _, hasErr := wire["error"]; hasErr {
			t.Errorf("success response must not contain error field: %s", string(raw))
		}

		dec, err := DecodeRPCResponse(raw)
		if err != nil {
			t.Fatalf("DecodeRPCResponse: %v", err)
		}
		if dec.ID != 7 {
			t.Errorf("ID = %d, want 7", dec.ID)
		}
		if dec.Error != nil {
			t.Errorf("Error = %+v, want nil", dec.Error)
		}
		var res struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(dec.Result, &res); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if !res.OK || res.Version != "1.0.0-beta12" {
			t.Errorf("result = %+v, want {ok:true version:1.0.0-beta12}", res)
		}
	})

	t.Run("response error", func(t *testing.T) {
		raw, err := EncodeRPCError(99, "invalid model pin", "invalid_model_pin", map[string]any{
			"field": "model_pin",
		})
		if err != nil {
			t.Fatalf("EncodeRPCError: %v", err)
		}

		// Verify wire shape matches RPC-SPEC §3 & §7.1:
		// {"jsonrpc":"2.0","id":<int>,"error":{"code":-32000,"message":"<human>","data":{"reason":"<machine string>",...}}}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		if _, hasRes := wire["result"]; hasRes {
			t.Errorf("error response must not contain result field: %s", string(raw))
		}

		dec, err := DecodeRPCResponse(raw)
		if err != nil {
			t.Fatalf("DecodeRPCResponse: %v", err)
		}
		if dec.ID != 99 {
			t.Errorf("ID = %d, want 99", dec.ID)
		}
		if len(dec.Result) != 0 {
			t.Errorf("Result = %s, want empty", string(dec.Result))
		}
		if dec.Error == nil {
			t.Fatal("Error is nil, want populated RPCError")
		}
		if dec.Error.Code != -32000 {
			t.Errorf("Error.Code = %d, want -32000 (RPC-SPEC §7.1)", dec.Error.Code)
		}
		if dec.Error.Message != "invalid model pin" {
			t.Errorf("Error.Message = %q, want %q", dec.Error.Message, "invalid model pin")
		}
		if dec.Error.Data["reason"] != "invalid_model_pin" {
			t.Errorf("Error.Data[reason] = %v, want invalid_model_pin", dec.Error.Data["reason"])
		}
		if dec.Error.Data["field"] != "model_pin" {
			t.Errorf("Error.Data[field] = %v, want model_pin", dec.Error.Data["field"])
		}
	})

	t.Run("notification", func(t *testing.T) {
		raw, err := EncodeRPCNotification("session.event", map[string]any{
			"session_id": "main",
			"event":      map[string]any{"t": "delta", "text": "hi"},
		})
		if err != nil {
			t.Fatalf("EncodeRPCNotification: %v", err)
		}

		// Verify wire shape matches RPC-SPEC §3: {"jsonrpc":"2.0","method":"<event>.<type>","params":{...}} (no id)
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		if _, hasID := wire["id"]; hasID {
			t.Errorf("notification must not contain id field: %s", string(raw))
		}
		if wire["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v, want 2.0", wire["jsonrpc"])
		}
		if wire["method"] != "session.event" {
			t.Errorf("method = %v, want session.event", wire["method"])
		}

		dec, err := DecodeRPCNotification(raw)
		if err != nil {
			t.Fatalf("DecodeRPCNotification: %v", err)
		}
		if dec.JSONRPC != "2.0" {
			t.Errorf("JSONRPC = %q, want 2.0", dec.JSONRPC)
		}
		if dec.Method != "session.event" {
			t.Errorf("Method = %q, want session.event", dec.Method)
		}
		var params struct {
			SessionID string         `json:"session_id"`
			Event     map[string]any `json:"event"`
		}
		if err := json.Unmarshal(dec.Params, &params); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if params.SessionID != "main" || params.Event["text"] != "hi" {
			t.Errorf("params = %+v, want session_id=main event.text=hi", params)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		if _, err := DecodeRPCRequest([]byte(`{"jsonrpc":"1.0","id":1,"method":"a.b"}`)); err == nil {
			t.Error("expected error for wrong jsonrpc version on request")
		}
		if _, err := DecodeRPCRequest([]byte(`{"jsonrpc":"2.0","method":"a.b"}`)); err == nil {
			t.Error("expected error for missing id on request")
		}
		if _, err := DecodeRPCRequest([]byte(`{"jsonrpc":"2.0","id":1}`)); err == nil {
			t.Error("expected error for missing method on request")
		}
		if _, err := DecodeRPCResponse([]byte(`{"jsonrpc":"2.0","id":1}`)); err == nil {
			t.Error("expected error for response with neither result nor error")
		}
		if _, err := DecodeRPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32000,"message":"x","data":{"reason":"x"}}}`)); err == nil {
			t.Error("expected error for response with both result and error")
		}
		if _, err := DecodeRPCNotification([]byte(`{"jsonrpc":"2.0","id":1,"method":"session.event","params":{}}`)); err == nil {
			t.Error("expected error for notification carrying an id")
		}
	})
}

// testWSClient is a minimal RFC 6455 client helper for testing the /v1/ws transport
// and inspecting exact close codes and close reasons.
type testWSClient struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

func dialTestWS(t *testing.T, rawURL string) *testWSClient {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var nonceBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, nonceBytes[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(nonceBytes[:])

	path := u.RequestURI()
	if path == "" {
		path = "/v1/ws"
	}

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)

	if _, err := bw.WriteString(reqStr); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush handshake: %v", err)
	}

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != computeWSAccept(key) {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, computeWSAccept(key))
	}

	return &testWSClient{conn: conn, br: br, bw: bw}
}

func (c *testWSClient) writeMaskedText(t *testing.T, payload []byte) {
	t.Helper()
	c.writeMaskedFrame(t, wsOpText, payload)
}

func (c *testWSClient) writeMaskedFrame(t *testing.T, opcode byte, payload []byte) {
	t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	var header [10]byte
	header[0] = 0x80 | (opcode & 0x0f)
	n := len(payload)
	hdrLen := 2
	switch {
	case n <= 125:
		header[1] = 0x80 | byte(n)
	case n <= 65535:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
		hdrLen = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
		hdrLen = 10
	}

	maskKey := [4]byte{0x12, 0x34, 0x56, 0x78}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}

	if _, err := c.bw.Write(header[:hdrLen]); err != nil {
		t.Fatalf("write frame header: %v", err)
	}
	if _, err := c.bw.Write(maskKey[:]); err != nil {
		t.Fatalf("write mask key: %v", err)
	}
	if _, err := c.bw.Write(masked); err != nil {
		t.Fatalf("write masked payload: %v", err)
	}
	if err := c.bw.Flush(); err != nil {
		t.Fatalf("flush frame: %v", err)
	}
}

func (c *testWSClient) readServerFrame(t *testing.T) (opcode byte, payload []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	b0, err := c.br.ReadByte()
	if err != nil {
		t.Fatalf("read b0: %v", err)
	}
	b1, err := c.br.ReadByte()
	if err != nil {
		t.Fatalf("read b1: %v", err)
	}

	opcode = b0 & 0x0f
	if (b1 & 0x80) != 0 {
		t.Fatalf("server frame must not be masked")
	}
	length := uint64(b1 & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatalf("read len16: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatalf("read len64: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return opcode, payload
}

func (c *testWSClient) expectCloseCode(t *testing.T, wantCode uint16, wantReason string) {
	t.Helper()
	op, payload := c.readServerFrame(t)
	if op != wsOpClose {
		t.Fatalf("expected close frame (opcode %d), got opcode %d with payload %q", wsOpClose, op, string(payload))
	}
	if len(payload) < 2 {
		t.Fatalf("close frame payload too short: %d bytes", len(payload))
	}
	gotCode := binary.BigEndian.Uint16(payload[:2])
	gotReason := string(payload[2:])
	if gotCode != wantCode {
		t.Errorf("close code = %d, want %d (reason=%q)", gotCode, wantCode, gotReason)
	}
	if wantReason != "" && gotReason != wantReason {
		t.Errorf("close reason = %q, want %q", gotReason, wantReason)
	}
}

func newTestRPCServer(t *testing.T, authTimeout time.Duration) (*Server, *TokenStore, *httptest.Server) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      &fakeSessionSource{},
		Hub:           &fakeHub{},
		wsAuthTimeout: authTimeout,
	}
	ts := httptest.NewUnstartedServer(NewServer(srv))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, store, ts
}

func TestWSHandshake_CloseCodes(t *testing.T) {
	t.Run("4001 auth_timeout when no auth.hello within deadline", func(t *testing.T) {
		_, _, ts := newTestRPCServer(t, 100*time.Millisecond)
		client := dialTestWS(t, ts.URL+"/v1/ws")

		// Do not send auth.hello; wait for server to close with 4001 auth_timeout.
		client.expectCloseCode(t, WSCloseAuthTimeout, WSReasonAuthTimeout)
	})

	t.Run("4003 unauthorized on bad or revoked token", func(t *testing.T) {
		_, store, ts := newTestRPCServer(t, 2*time.Second)

		// Case 1: completely invalid token
		client1 := dialTestWS(t, ts.URL+"/v1/ws")
		helloBad, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": "lar1_bad"})
		if err != nil {
			t.Fatal(err)
		}
		client1.writeMaskedText(t, helloBad)
		client1.expectCloseCode(t, WSCloseUnauthorized, WSReasonUnauthorized)

		// Case 2: revoked token
		tok, err := store.Create("revoke-test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.RevokeLabel("revoke-test"); err != nil {
			t.Fatal(err)
		}
		client2 := dialTestWS(t, ts.URL+"/v1/ws")
		helloRevoked, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": tok})
		if err != nil {
			t.Fatal(err)
		}
		client2.writeMaskedText(t, helloRevoked)
		client2.expectCloseCode(t, WSCloseUnauthorized, WSReasonUnauthorized)
	})

	t.Run("4003 unauthorized immediately on pre-auth method call without error reply", func(t *testing.T) {
		_, _, ts := newTestRPCServer(t, 2*time.Second)
		client := dialTestWS(t, ts.URL+"/v1/ws")

		req, err := EncodeRPCRequest(1, "sessions.list", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		client.writeMaskedText(t, req)

		// Server must close immediately with 4003 (no error reply frame first).
		client.expectCloseCode(t, WSCloseUnauthorized, WSReasonUnauthorized)
	})

	t.Run("4002 already_authenticated on second auth.hello", func(t *testing.T) {
		_, store, ts := newTestRPCServer(t, 2*time.Second)
		tok, err := store.Create("test-device")
		if err != nil {
			t.Fatal(err)
		}

		client := dialTestWS(t, ts.URL+"/v1/ws")
		hello1, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": tok})
		if err != nil {
			t.Fatal(err)
		}
		client.writeMaskedText(t, hello1)

		// First auth.hello succeeds.
		op, payload := client.readServerFrame(t)
		if op != wsOpText {
			t.Fatalf("opcode = %d, want text (%d)", op, wsOpText)
		}
		resp, err := DecodeRPCResponse(payload)
		if err != nil {
			t.Fatalf("DecodeRPCResponse: %v", err)
		}
		if resp.ID != 0 || resp.Error != nil {
			t.Fatalf("unexpected hello response: %+v", resp)
		}
		var helloRes struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(resp.Result, &helloRes); err != nil {
			t.Fatalf("unmarshal hello result: %v", err)
		}
		if !helloRes.OK || !strings.HasPrefix(helloRes.Version, "1.0.0-beta") {
			t.Errorf("hello result = %+v, want ok=true version=1.0.0-beta*", helloRes)
		}

		// Second auth.hello on the same connection closes with 4002 already_authenticated.
		hello2, err := EncodeRPCRequest(1, "auth.hello", map[string]string{"token": tok})
		if err != nil {
			t.Fatal(err)
		}
		client.writeMaskedText(t, hello2)
		client.expectCloseCode(t, WSCloseAlreadyAuthenticated, WSReasonAlreadyAuthenticated)
	})
}

func TestWSMultiplexingLifecycle(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	tok, err := store.Create("mux-client")
	if err != nil {
		t.Fatal(err)
	}

	// Register two test methods on the transport skeleton to verify out-of-order
	// multiplexing by request id and server-push notifications over one socket.
	blockSlow := make(chan struct{})
	slowEntered := make(chan struct{})

	srv.RegisterRPCMethod("test.slow", func(_ context.Context, conn *RPCConn, req *RPCRequest) {
		close(slowEntered)
		<-blockSlow
		_ = conn.SendResult(*req.ID, map[string]any{"which": "slow"})
	})
	srv.RegisterRPCMethod("test.fast", func(_ context.Context, conn *RPCConn, req *RPCRequest) {
		_ = conn.Notify("test.progress", map[string]any{"req_id": *req.ID})
		_ = conn.SendResult(*req.ID, map[string]any{"which": "fast"})
	})

	client := dialTestWS(t, ts.URL+"/v1/ws")

	// 1. Authenticate via auth.hello
	hello, err := EncodeRPCRequest(0, "auth.hello", map[string]string{"token": tok})
	if err != nil {
		t.Fatal(err)
	}
	client.writeMaskedText(t, hello)
	op, payload := client.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("hello response op = %d, want text", op)
	}
	if _, err := DecodeRPCResponse(payload); err != nil {
		t.Fatalf("decode hello response: %v", err)
	}

	// 2. Fire request id=10 (slow) and id=20 (fast) on the same socket.
	reqSlow, _ := EncodeRPCRequest(10, "test.slow", nil)
	client.writeMaskedText(t, reqSlow)

	select {
	case <-slowEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("test.slow did not start")
	}

	reqFast, _ := EncodeRPCRequest(20, "test.fast", nil)
	client.writeMaskedText(t, reqFast)

	// Expect notification and id=20 response while id=10 is still in flight.
	_, frame1 := client.readServerFrame(t)
	notif, err := DecodeRPCNotification(frame1)
	if err != nil {
		t.Fatalf("expected notification frame first, got %s: %v", string(frame1), err)
	}
	if notif.Method != "test.progress" {
		t.Errorf("notif.Method = %q, want test.progress", notif.Method)
	}

	_, frame2 := client.readServerFrame(t)
	respFast, err := DecodeRPCResponse(frame2)
	if err != nil {
		t.Fatalf("DecodeRPCResponse(fast): %v", err)
	}
	if respFast.ID != 20 {
		t.Errorf("respFast.ID = %d, want 20", respFast.ID)
	}

	// Unblock id=10 and verify it arrives correlated by id=10.
	close(blockSlow)
	_, frame3 := client.readServerFrame(t)
	respSlow, err := DecodeRPCResponse(frame3)
	if err != nil {
		t.Fatalf("DecodeRPCResponse(slow): %v", err)
	}
	if respSlow.ID != 10 {
		t.Errorf("respSlow.ID = %d, want 10", respSlow.ID)
	}
}
