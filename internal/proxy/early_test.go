package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestForwardHTTPUpstreamEarlyAnswer: a server that answers the
// request line and hangs up BEFORE the body finishes (Cloudflare
// answers 405 to POST /example.com and closes) must have its
// response delivered to the client, not swallowed into an RST.
// Bench parity: direct curl saw 405; the proxy must not downgrade it.
func TestForwardHTTPUpstreamEarlyAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read nothing; answer, then close while the body is
		// still coming.
		_, _ = conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 2\r\nConnection: close\r\n\r\nno"))
		// Give the proxy time to READ the answer before the close
		// turns the socket into a RST (a real server's response
		// bytes are in flight; immediate close would discard them
		// and test nothing).
		time.Sleep(2 * time.Second)
	}()

	client, proxySide := net.Pipe()
	defer client.Close()
	br := bufio.NewReader(proxySide)
	u := &url.URL{Scheme: "http", Host: ln.Addr().String(), Path: "/"}

	payload := chunkWire(strings.Repeat("p", 4<<20), 65536) // 4 MiB, exceeds socket buffers
	go func() {
		client.Write([]byte(payload))
	}()
	done := make(chan string, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			done <- "read: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- resp.Status + " " + string(b)
	}()

	s := &Server{}
	// Permissive dial for the test only (upstream is loopback by
	// design; the production floor is exercised by the forbidden
	// destination tests, not this one).
	orig := dialUpstreamFn
	dialUpstreamFn = func(_ context.Context, hp string) (net.Conn, error) {
		return net.Dial("tcp", hp)
	}
	defer func() { dialUpstreamFn = orig }()
	go s.forwardHTTP(context.Background(), proxySide, br, u, http.MethodPost,
		[]string{"Host: " + ln.Addr().String(), "Transfer-Encoding: chunked"})

	select {
	case got := <-done:
		if got != "405 Method Not Allowed no" {
			t.Fatalf("client saw %q, want the upstream's early 405", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no response delivered (RST-swallow regression)")
	}
}
