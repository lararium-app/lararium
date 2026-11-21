package surface

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 §4.2.2 mandates SHA-1 for Sec-WebSocket-Accept
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// JSONRPCVersion is the required jsonrpc envelope version string (RPC-SPEC §3).
	JSONRPCVersion = "2.0"

	// RPCErrorCodeGeneric is the ratified generic application error code (RPC-SPEC §3, §7.1).
	// All domain signal lives in error.data.reason.
	RPCErrorCodeGeneric = -32000

	// WSCloseNormal is the standard RFC 6455 normal-closure code (1000).
	WSCloseNormal uint16 = 1000
	// WSCloseProtocolError is the RFC 6455 protocol-error close code (1002).
	WSCloseProtocolError uint16 = 1002
	// WSCloseUnsupportedData is the RFC 6455 unsupported-data close code (1003).
	WSCloseUnsupportedData uint16 = 1003
	// WSClosePolicyViolation is the RFC 6455 policy-violation close code (1008).
	WSClosePolicyViolation uint16 = 1008
	// WSCloseMessageTooBig is the RFC 6455 message-too-big close code (1009).
	WSCloseMessageTooBig uint16 = 1009
	// WSCloseAuthTimeout is the RPC-SPEC §2 close code for a missing auth.hello within the handshake deadline (4001).
	WSCloseAuthTimeout uint16 = 4001
	// WSCloseAlreadyAuthenticated is the RPC-SPEC §2 close code for a second auth.hello on an already-authenticated connection (4002).
	WSCloseAlreadyAuthenticated uint16 = 4002
	// WSCloseUnauthorized is the RPC-SPEC §2 close code for a bad/revoked token or a pre-auth method call (4003).
	WSCloseUnauthorized uint16 = 4003

	// WSReasonAuthTimeout is the close reason string paired with WSCloseAuthTimeout.
	WSReasonAuthTimeout = "auth_timeout"
	// WSReasonAlreadyAuthenticated is the close reason string paired with WSCloseAlreadyAuthenticated.
	WSReasonAlreadyAuthenticated = "already_authenticated"
	// WSReasonUnauthorized is the close reason string paired with WSCloseUnauthorized.
	WSReasonUnauthorized = "unauthorized"

	// DefaultWSAuthTimeout is the 5s deadline for the initial auth.hello frame (RPC-SPEC §2).
	DefaultWSAuthTimeout = 5 * time.Second

	// DefaultWSWriteTimeout bounds every server frame write (RPC-SPEC §2). A write blocks only
	// once the peer has stopped reading and both socket buffers are full; at the deadline the
	// connection is closed and torn down like any other disconnect.
	DefaultWSWriteTimeout = 10 * time.Second

	// wsCloseWriteTimeout bounds the best-effort close-frame write in CloseWithCode.
	wsCloseWriteTimeout = 2 * time.Second

	// maxWSFramePayload caps incoming WebSocket frame payloads at 1 MiB (SURFACE-SPEC §2 body limit).
	maxWSFramePayload = 1 << 20

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

// RPCRequest is a JSON-RPC 2.0 request envelope from client to server (RPC-SPEC §3).
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response envelope from server to client (RPC-SPEC §3).
// Exactly one of Result or Error is populated on outgoing responses.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error object inside an error RPCResponse (RPC-SPEC §3, §7.1).
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// RPCNotification is a server-push JSON-RPC 2.0 notification envelope with no id (RPC-SPEC §3).
type RPCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// EncodeRPCRequest marshals a client-to-server JSON-RPC 2.0 request envelope.
func EncodeRPCRequest(id int64, method string, params any) ([]byte, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal request params: %w", err)
		}
		rawParams = b
	} else {
		rawParams = json.RawMessage(`{}`)
	}
	req := RPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      &id,
		Method:  method,
		Params:  rawParams,
	}
	return json.Marshal(req)
}

// DecodeRPCRequest unmarshals and validates a JSON-RPC 2.0 request envelope.
func DecodeRPCRequest(data []byte) (*RPCRequest, error) {
	var req RPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	if req.JSONRPC != JSONRPCVersion {
		return nil, errors.New("invalid jsonrpc version")
	}
	if req.ID == nil {
		return nil, errors.New("missing request id")
	}
	if req.Method == "" {
		return nil, errors.New("missing method")
	}
	return &req, nil
}

// EncodeRPCResponse marshals a successful server-to-client JSON-RPC 2.0 response envelope.
func EncodeRPCResponse(id int64, result any) ([]byte, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal response result: %w", err)
	}
	resp := RPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  b,
	}
	return json.Marshal(resp)
}

// EncodeRPCError marshals an error server-to-client JSON-RPC 2.0 response envelope (RPC-SPEC §3, §7.1).
// error.code is always -32000; error.data.reason carries the machine reason string, merged with any extra data fields.
func EncodeRPCError(id int64, message, reason string, extra map[string]any) ([]byte, error) {
	data := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		data[k] = v
	}
	data["reason"] = reason
	if message == "" {
		message = reason
	}
	resp := RPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &RPCError{
			Code:    RPCErrorCodeGeneric,
			Message: message,
			Data:    data,
		},
	}
	return json.Marshal(resp)
}

// DecodeRPCResponse unmarshals and validates a JSON-RPC 2.0 response envelope (success or error).
func DecodeRPCResponse(data []byte) (*RPCResponse, error) {
	var raw struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if raw.JSONRPC != JSONRPCVersion {
		return nil, errors.New("invalid jsonrpc version")
	}
	if raw.ID == nil {
		return nil, errors.New("missing response id")
	}
	hasResult := len(raw.Result) > 0
	hasError := raw.Error != nil
	if hasResult == hasError {
		return nil, errors.New("response must have exactly one of result or error")
	}
	return &RPCResponse{
		JSONRPC: raw.JSONRPC,
		ID:      *raw.ID,
		Result:  raw.Result,
		Error:   raw.Error,
	}, nil
}

// EncodeRPCNotification marshals a server-push JSON-RPC 2.0 notification envelope (no id).
func EncodeRPCNotification(method string, params any) ([]byte, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal notification params: %w", err)
		}
		rawParams = b
	} else {
		rawParams = json.RawMessage(`{}`)
	}
	notif := RPCNotification{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  rawParams,
	}
	return json.Marshal(notif)
}

// DecodeRPCNotification unmarshals and validates a JSON-RPC 2.0 notification envelope.
func DecodeRPCNotification(data []byte) (*RPCNotification, error) {
	var raw struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal notification: %w", err)
	}
	if raw.JSONRPC != JSONRPCVersion {
		return nil, errors.New("invalid jsonrpc version")
	}
	if len(raw.ID) > 0 {
		return nil, errors.New("notification must not carry an id")
	}
	if raw.Method == "" {
		return nil, errors.New("missing notification method")
	}
	return &RPCNotification{
		JSONRPC: raw.JSONRPC,
		Method:  raw.Method,
		Params:  raw.Params,
	}, nil
}

// RPCMethodHandler handles an authenticated JSON-RPC request on a connection.
type RPCMethodHandler func(ctx context.Context, conn *RPCConn, req *RPCRequest)

// RegisterRPCMethod registers a business method handler on the server.
func (s *Server) RegisterRPCMethod(method string, h RPCMethodHandler) {
	s.rpcMu.Lock()
	defer s.rpcMu.Unlock()
	if s.rpcMethods == nil {
		s.rpcMethods = make(map[string]RPCMethodHandler)
	}
	s.rpcMethods[method] = h
}

func (s *Server) lookupRPCMethod(method string) (RPCMethodHandler, bool) {
	s.rpcMu.RLock()
	defer s.rpcMu.RUnlock()
	if s.rpcMethods == nil {
		return nil, false
	}
	h, ok := s.rpcMethods[method]
	return h, ok
}

// RPCConn represents one authenticated-or-handshaking WebSocket client connection.
// A client holds one socket and multiplexes concurrent requests by request id (RPC-SPEC §0, §2).
type RPCConn struct {
	conn         net.Conn
	br           *bufio.Reader
	bw           *bufio.Writer
	writeMu      sync.Mutex
	closeOnce    sync.Once
	writeTimeout time.Duration
}

func newRPCConn(conn net.Conn, br *bufio.Reader, bw *bufio.Writer) *RPCConn {
	return &RPCConn{
		conn:         conn,
		br:           br,
		bw:           bw,
		writeTimeout: DefaultWSWriteTimeout,
	}
}

// SendResult sends a JSON-RPC 2.0 success response for request id.
func (c *RPCConn) SendResult(id int64, result any) error {
	payload, err := EncodeRPCResponse(id, result)
	if err != nil {
		return err
	}
	return c.writeFrame(wsOpText, payload)
}

// SendError sends a JSON-RPC 2.0 error response for request id (code -32000, data.reason = reason).
func (c *RPCConn) SendError(id int64, message, reason string, extra map[string]any) error {
	payload, err := EncodeRPCError(id, message, reason, extra)
	if err != nil {
		return err
	}
	return c.writeFrame(wsOpText, payload)
}

// Notify pushes a JSON-RPC 2.0 notification (no id) to the client.
func (c *RPCConn) Notify(method string, params any) error {
	payload, err := EncodeRPCNotification(method, params)
	if err != nil {
		return err
	}
	return c.writeFrame(wsOpText, payload)
}

// CloseWithCode sends a WebSocket Close control frame with the 2-byte status code and UTF-8 reason,
// then closes the underlying network connection.
func (c *RPCConn) CloseWithCode(code uint16, reason string) error {
	var writeErr error
	c.closeOnce.Do(func() {
		reasonBytes := []byte(reason)
		if len(reasonBytes) > 123 {
			reasonBytes = reasonBytes[:123]
		}
		payload := make([]byte, 2+len(reasonBytes))
		binary.BigEndian.PutUint16(payload[:2], code)
		copy(payload[2:], reasonBytes)
		writeErr = c.writeFrameWithin(wsOpClose, payload, wsCloseWriteTimeout)
		_ = c.conn.Close()
	})
	return writeErr
}

// Close closes the underlying connection without sending a close frame if not already closed.
func (c *RPCConn) Close() error {
	return c.conn.Close()
}

func (c *RPCConn) writeFrame(opcode byte, payload []byte) error {
	return c.writeFrameWithin(opcode, payload, c.writeTimeout)
}

// writeFrameWithin writes one frame under a write deadline of d. Any write
// error, including the deadline, closes the connection: the frame may have
// been cut off mid-write, which corrupts the stream for every later frame.
// Closing also ends the read loop, so the connection is torn down like any
// other disconnect instead of leaving a writer blocked forever.
func (c *RPCConn) writeFrameWithin(opcode byte, payload []byte, d time.Duration) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if d > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(d))
	}
	if err := c.writeFrameLocked(opcode, payload); err != nil {
		_ = c.conn.Close()
		return err
	}
	return nil
}

func (c *RPCConn) writeFrameLocked(opcode byte, payload []byte) error {
	var header [10]byte
	header[0] = 0x80 | (opcode & 0x0f) // FIN=1
	n := len(payload)
	hdrLen := 2
	switch {
	case n <= 125:
		header[1] = byte(n)
	case n <= 65535:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
		hdrLen = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
		hdrLen = 10
	}

	if _, err := c.bw.Write(header[:hdrLen]); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// readMessage reads the next complete data message (text or binary, reassembling fragments),
// transparently answering Ping control frames and handling Close frames.
func (c *RPCConn) readMessage() (byte, []byte, error) {
	var msgOpcode byte
	var msgBuf []byte
	inFragment := false

	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch opcode {
		case wsOpClose:
			code := WSCloseNormal
			if len(payload) >= 2 {
				code = binary.BigEndian.Uint16(payload[:2])
			}
			_ = c.CloseWithCode(code, "")
			return wsOpClose, payload, io.EOF

		case wsOpPing:
			_ = c.writeFrame(wsOpPong, payload)
			continue

		case wsOpPong:
			continue

		case wsOpText, wsOpBinary:
			if inFragment {
				_ = c.CloseWithCode(WSCloseProtocolError, "unexpected data opcode during fragmentation")
				return 0, nil, errors.New("websocket protocol error: unexpected data frame")
			}
			if fin {
				return opcode, payload, nil
			}
			inFragment = true
			msgOpcode = opcode
			msgBuf = append(msgBuf[:0], payload...)

		case wsOpContinuation:
			if !inFragment {
				_ = c.CloseWithCode(WSCloseProtocolError, "unexpected continuation frame")
				return 0, nil, errors.New("websocket protocol error: unexpected continuation")
			}
			if len(msgBuf)+len(payload) > maxWSFramePayload {
				_ = c.CloseWithCode(WSCloseMessageTooBig, "message too big")
				return 0, nil, errors.New("websocket frame payload exceeds limit")
			}
			msgBuf = append(msgBuf, payload...)
			if fin {
				return msgOpcode, msgBuf, nil
			}

		default:
			_ = c.CloseWithCode(WSCloseProtocolError, "unsupported opcode")
			return 0, nil, fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
	}
}

func (c *RPCConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var b0, b1 byte
	b0, err = c.br.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	b1, err = c.br.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}

	fin = (b0 & 0x80) != 0
	rsv := (b0 & 0x70)
	if rsv != 0 {
		_ = c.CloseWithCode(WSCloseProtocolError, "rsv bits must be zero")
		return false, 0, nil, errors.New("websocket rsv bits set")
	}
	opcode = b0 & 0x0f

	masked := (b1 & 0x80) != 0
	if !masked {
		// RFC 6455 §5.1: server MUST close connection if client frame is unmasked.
		_ = c.CloseWithCode(WSCloseProtocolError, "client frame not masked")
		return false, 0, nil, errors.New("client frame not masked")
	}

	length := uint64(b1 & 0x7f)
	isControl := (opcode & 0x08) != 0
	if isControl && (!fin || length > 125) {
		_ = c.CloseWithCode(WSCloseProtocolError, "invalid control frame")
		return false, 0, nil, errors.New("invalid control frame")
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	if length > maxWSFramePayload {
		_ = c.CloseWithCode(WSCloseMessageTooBig, "frame too big")
		return false, 0, nil, errors.New("websocket frame too big")
	}

	var maskKey [4]byte
	if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
		return false, 0, nil, err
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}

	return fin, opcode, payload, nil
}

func computeWSAccept(key string) string {
	h := sha1.New() //nolint:gosec // RFC 6455 §4.2.2
	_, _ = io.WriteString(h, key)
	_, _ = io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContainsToken(h http.Header, key, token string) bool {
	for _, v := range h[http.CanonicalHeaderKey(key)] {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// upgradeWebSocket performs the RFC 6455 HTTP/1.1 upgrade handshake and returns an RPCConn.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*RPCConn, error) {
	if r.Method != http.MethodGet {
		writeJSONBody(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return nil, errors.New("method not allowed")
	}
	if !headerContainsToken(r.Header, "Connection", "Upgrade") ||
		!headerContainsToken(r.Header, "Upgrade", "websocket") {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"bad upgrade"}`)
		return nil, errors.New("missing upgrade headers")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		writeJSONBody(w, http.StatusBadRequest, `{"error":"unsupported websocket version"}`)
		return nil, errors.New("bad websocket version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"missing websocket key"}`)
		return nil, errors.New("missing websocket key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"internal error"}`)
		return nil, errors.New("hijacking not supported")
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	// Clear any HTTP server read/write deadlines inherited from net/http before running the WS loop.
	_ = conn.SetDeadline(time.Time{})

	accept := computeWSAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return newRPCConn(conn, rw.Reader, rw.Writer), nil
}

// wsHandler handles GET /v1/ws (RPC-SPEC §2, §3).
// It upgrades the connection, enforces the 5s auth.hello handshake with close codes
// 4001/4002/4003, and then multiplexes JSON-RPC 2.0 requests by request id over the socket.
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	rc, err := upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer rc.Close()
	if s.wsWriteTimeout > 0 {
		rc.writeTimeout = s.wsWriteTimeout
	}

	authTimeout := s.wsAuthTimeout
	if authTimeout <= 0 {
		authTimeout = DefaultWSAuthTimeout
	}

	// Enforce the 5s auth.hello window (RPC-SPEC §2: no auth.hello within 5s -> close 4001 auth_timeout).
	authTimer := time.AfterFunc(authTimeout, func() {
		_ = rc.CloseWithCode(WSCloseAuthTimeout, WSReasonAuthTimeout)
	})

	opcode, payload, err := rc.readMessage()
	if !authTimer.Stop() {
		// Timer already fired and closed the socket with 4001.
		return
	}
	if err != nil {
		return
	}
	if opcode != wsOpText {
		_ = rc.CloseWithCode(WSCloseUnauthorized, WSReasonUnauthorized)
		return
	}

	req, err := DecodeRPCRequest(payload)
	if err != nil {
		_ = rc.CloseWithCode(WSCloseUnauthorized, WSReasonUnauthorized)
		return
	}

	// Any method other than auth.hello before a successful auth.hello -> close 4003 immediately
	// with no error reply first (RPC-SPEC §2: don't let an unauthenticated peer probe the method catalog).
	if req.Method != "auth.hello" {
		_ = rc.CloseWithCode(WSCloseUnauthorized, WSReasonUnauthorized)
		return
	}

	var helloParams struct {
		Token string `json:"token"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &helloParams) != nil {
		_ = rc.CloseWithCode(WSCloseUnauthorized, WSReasonUnauthorized)
		return
	}

	if s.Store == nil || !s.Store.Verify(helloParams.Token) {
		_ = rc.CloseWithCode(WSCloseUnauthorized, WSReasonUnauthorized)
		return
	}

	// Handshake succeeded: reply {"jsonrpc":"2.0","id":<id>,"result":{"ok":true,"version":"1.0.0-beta12"}}
	if err := rc.SendResult(*req.ID, map[string]any{
		"ok":      true,
		"version": "1.0.0-beta12",
	}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		cancel()
		s.unsubscribeAllRPCConn(rc)
	}()

	var reqWg sync.WaitGroup
	defer reqWg.Wait()

	// Post-auth loop: one socket per client, multiplexing requests concurrently by request id.
	for {
		op, msg, err := rc.readMessage()
		if err != nil {
			return
		}
		if op != wsOpText {
			_ = rc.CloseWithCode(WSCloseUnsupportedData, "text frames only")
			return
		}

		rpcReq, err := DecodeRPCRequest(msg)
		if err != nil {
			continue
		}

		// Exactly one live auth.hello per connection; a second one closes 4002 already_authenticated (RPC-SPEC §2).
		if rpcReq.Method == "auth.hello" {
			_ = rc.CloseWithCode(WSCloseAlreadyAuthenticated, WSReasonAlreadyAuthenticated)
			return
		}

		handler, ok := s.lookupRPCMethod(rpcReq.Method)
		if !ok {
			_ = rc.SendError(*rpcReq.ID, "method not found", "method_not_found", nil)
			continue
		}

		reqWg.Add(1)
		go func(rReq *RPCRequest, h RPCMethodHandler) {
			defer reqWg.Done()
			h(ctx, rc, rReq)
		}(rpcReq, handler)
	}
}
