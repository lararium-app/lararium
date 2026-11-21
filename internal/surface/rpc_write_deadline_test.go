package surface

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A peer that stops reading must not block a server write forever. The write
// fails at the deadline and the connection is closed: the timed-out frame may
// have been cut off mid-write, so the stream can never be reused.
func TestRPCConnWriteDeadlineClosesStalledPeer(t *testing.T) {
	server, peer := net.Pipe() // unbuffered: every write blocks until the peer reads
	t.Cleanup(func() { _ = peer.Close() })
	rc := newRPCConn(server, bufio.NewReader(server), bufio.NewWriter(server))
	rc.writeTimeout = 50 * time.Millisecond

	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- rc.Notify("session.event", map[string]any{"n": 1}) }()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(3 * time.Second):
		_ = server.Close() // release the stuck writer before failing
		<-errCh
		t.Fatal("Notify to a stalled peer still blocked after 3s: no write deadline")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Notify to a stalled peer = %v, want os.ErrDeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Notify returned after %v, want about the 50ms deadline", elapsed)
	}

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after the timeout = %v, want io.EOF (server side closed)", err)
	}
	if err := rc.Notify("session.event", map[string]any{"n": 2}); err == nil {
		t.Fatal("write after a timed-out write succeeded; the connection must stay closed")
	}
}

const stalledSessionID = "s_STALLEDCLIENTSESSION000000"

// End to end: a client that authenticates, subscribes, then stops reading
// while live events keep coming is disconnected at the write deadline, and
// the connection's teardown runs instead of hanging on a write forever.
func TestWSStalledClientDisconnectedAtWriteDeadline(t *testing.T) {
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:          store,
		Sessions:       &fakeSessionSource{inFlight: true},
		Hub:            &fakeHub{},
		Cfg:            ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
		wsAuthTimeout:  2 * time.Second,
		wsWriteTimeout: 200 * time.Millisecond,
	}
	handler := NewServer(srv)
	wsDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ws" {
			defer close(wsDone)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	tok, err := store.Create("stalled-client")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)
	if resp := client.callRPC(t, 1, "sessions.subscribe", map[string]any{"id": stalledSessionID}); resp.Error != nil {
		t.Fatalf("subscribe error: %+v", resp.Error)
	}

	// From here on the client never reads. Large events fill both socket
	// buffers, after which the next server write stalls.
	big := buildRPCSessionEventJSON("delta", `{"text":"`+strings.Repeat("x", 256<<10)+`"}`)
	stopFlood := make(chan struct{})
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for {
			select {
			case <-stopFlood:
				return
			case <-wsDone:
				return
			default:
			}
			srv.notifyRPCSessionEvent(stalledSessionID, big)
		}
	}()

	select {
	case <-wsDone:
	case <-time.After(10 * time.Second):
		close(stopFlood)
		_ = client.conn.Close() // release the stuck server write before failing
		<-floodDone
		t.Fatal("connection still open 10s after the client stopped reading: writes have no deadline")
	}
	<-floodDone

	srv.rpcSubsMu.RLock()
	left := len(srv.rpcSubs[stalledSessionID])
	srv.rpcSubsMu.RUnlock()
	if left != 0 {
		t.Fatalf("subscriptions left after teardown = %d, want 0", left)
	}
}
