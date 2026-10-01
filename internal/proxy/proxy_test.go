package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readConn adapts an io.Reader to net.Conn for copyRequestBody's
// conn parameter (only Read matters; deadlines are no-ops).
type readConn struct{ io.Reader }

func (readConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (readConn) Close() error                     { return nil }
func (readConn) LocalAddr() net.Addr              { return nil }
func (readConn) RemoteAddr() net.Addr             { return nil }
func (readConn) SetDeadline(time.Time) error      { return nil }
func (readConn) SetReadDeadline(time.Time) error  { return nil }
func (readConn) SetWriteDeadline(time.Time) error { return nil }

// chunkedReaderOf decodes HTTP/1.1 chunked framing with the STDLIB
// reader (net/http via ReadResponse) — the reference implementation
// our re-framed output must satisfy.
func decodeChunked(framing io.Reader) (string, error) {
	src := io.MultiReader(strings.NewReader(
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"), framing)
	resp, err := http.ReadResponse(bufio.NewReader(src),
		&http.Request{Method: http.MethodPost})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// chunkWire renders a body as HTTP/1.1 chunked transfer-encoding with
// the given chunk size — byte-exact wire format including chunk-size
// lines, CRLF terminators and the zero-chunk trailer.
func chunkWire(payload string, chunkSize int) string {
	var b strings.Builder
	for i := 0; i < len(payload); i += chunkSize {
		end := i + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		fmt.Fprintf(&b, "%x\r\n%s\r\n", end-i, payload[i:end])
	}
	b.WriteString("0\r\n\r\n")
	return b.String()
}

// TestCopyRequestBodyChunkedReframes is the regression for the F1 fix
// round (agy r3 F1) ITSELF: the first cut of copyChunked forgot the
// CRLF terminating each chunk's data, so every chunked POST with a
// body died mid-stream (live bench: "Recv failure: Connection reset
// by peer" after the last chunk). The output must be byte-identical
// chunk framing that net/http's own reader accepts.
func TestCopyRequestBodyChunkedReframes(t *testing.T) {
	payload := strings.Repeat("Lararium-Chunk-Payload-0123456789-", 5000) // ~175 KiB
	wire := chunkWire(payload, 65536)

	// Split the wire across the br/conn seam to prove the buffered
	// tail is drained first (agy r1 F2 invariant).
	br := bufio.NewReader(strings.NewReader(wire[:70000]))
	rest := strings.NewReader(wire[70000:])

	var out bytes.Buffer
	hdrs := []string{"Host: x", "Transfer-Encoding: chunked"}
	if err := copyRequestBody(readConn{rest}, br, &out, hdrs); err != nil {
		t.Fatalf("copyRequestBody: %v", err)
	}

	// Parse the re-framed output with the stdlib's chunked reader.
	got, err := decodeChunked(&out)
	if err != nil {
		t.Fatalf("re-framed output rejected by net/http chunked reader: %v", err)
	}
	if got != payload {
		t.Fatalf("reassembled body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestCopyRequestBodyChunkedExtensionsAndTrailers: chunk extensions
// are accepted and stripped from the size line; trailers after the
// zero chunk are consumed, not forwarded as body.
func TestCopyRequestBodyChunkedExtensionsAndTrailers(t *testing.T) {
	wire := "a;foo=bar\r\n0123456789\r\n0\r\nX-Trail: nope\r\n\r\n"
	var out bytes.Buffer
	if err := copyRequestBody(readConn{strings.NewReader("")}, bufio.NewReader(strings.NewReader(wire)), &out,
		[]string{"Transfer-Encoding: chunked"}); err != nil {
		t.Fatalf("copyRequestBody: %v", err)
	}
	got, err := decodeChunked(&out)
	if err != nil {
		t.Fatalf("framing invalid: %v", err)
	}
	if got != "0123456789" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(out.String(), "X-Trail") {
		t.Fatal("trailer header leaked into forwarded framing")
	}
}

// TestCopyRequestBodyChunkedMalformed: garbage where a chunk size
// belongs must ERROR (caller RSTs), never hang or forward garbage.
func TestCopyRequestBodyChunkedMalformed(t *testing.T) {
	for _, wire := range []string{
		"nothex\r\nzzz\r\n",
		"\r\n",         // empty size line
		"5\r\nabc",     // truncated chunk data
		"5\r\nabcdeXX", // missing CRLF terminator
		"20000001\r\n", // size beyond budget, then EOF
	} {
		done := make(chan error, 1)
		go func(w string) {
			var out bytes.Buffer
			done <- copyRequestBody(readConn{strings.NewReader("")}, bufio.NewReader(strings.NewReader(w)), &out,
				[]string{"Transfer-Encoding: chunked"})
		}(wire)
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("wire %q accepted, want error", wire)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("wire %q hung", wire)
		}
	}
}

// TestCopyRequestBodyChunkedOverBudget: a chunked stream larger than
// maxRelayBody is cut off with an error, not streamed unbounded.
func TestCopyRequestBodyChunkedOverBudget(t *testing.T) {
	var big strings.Builder
	for i := 0; i <= maxRelayBody/65536+1; i++ {
		payload := strings.Repeat("z", 65536)
		fmt.Fprintf(&big, "%x\r\n%s\r\n", len(payload), payload)
	}
	big.WriteString("0\r\n\r\n")
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- copyRequestBody(readConn{strings.NewReader("")}, bufio.NewReader(strings.NewReader(big.String())), &out,
			[]string{"Transfer-Encoding: chunked"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("over-budget chunked body accepted")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("over-budget chunked body hung")
	}
}

// TestCopyRequestBodyContentLength: exact Content-Length copy,
// oversized rejected, no body headers mean zero bytes copied.
func TestCopyRequestBodyContentLength(t *testing.T) {
	var out bytes.Buffer
	if err := copyRequestBody(readConn{strings.NewReader("hello")}, bufio.NewReader(strings.NewReader("")), &out,
		[]string{"Content-Length: 5"}); err != nil {
		t.Fatalf("small body: %v", err)
	}
	if out.String() != "hello" {
		t.Fatalf("got %q", out.String())
	}
	out.Reset()
	if err := copyRequestBody(readConn{strings.NewReader("")}, bufio.NewReader(strings.NewReader("")), &out,
		[]string{"Content-Length: " + strconv.Itoa(maxRelayBody+1)}); err == nil {
		t.Fatal("oversized Content-Length accepted")
	}
}

// TestReadLineLimitedBounded: oversized lines error instead of
// growing memory without limit (agy r2 F3).
func TestReadLineLimitedBounded(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("A", maxRequestLine+10)))
	if _, err := readLineLimited(br); err == nil {
		t.Fatal("oversized line accepted")
	}
	br = bufio.NewReader(strings.NewReader("short\r\nrest"))
	line, err := readLineLimited(br)
	if err != nil || line != "short\r\n" {
		t.Fatalf("got %q, %v", line, err)
	}
}

// TestCopyRequestBodyChunkedDeclaredOverflow is the regression for
// agy r4 F1: a declared chunk near MaxInt64 used to wrap the int
// accumulator negative (reachable with real data on 32-bit, and it
// also let the copy block waiting for exabytes of data). The budget
// check must fire on the DECLARATION, in int64, before any copy.
func TestCopyRequestBodyChunkedDeclaredOverflow(t *testing.T) {
	framing := "1\r\nA\r\n7FFFFFFFFFFFFFFF\r\n" // 1 byte, then 8 EiB declared
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- copyRequestBody(readConn{strings.NewReader("")},
			bufio.NewReader(strings.NewReader(framing)), &out,
			[]string{"Transfer-Encoding: chunked"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("declared 8 EiB chunk accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("declared-size overflow blocked instead of rejecting")
	}
}

// TestCopyRequestBodyChunkedTrailerTruncated is the regression for
// agy r4 F2: EOF mid-trailer used to break the trailer loop and
// commit the request upstream with a synthesized terminator,
// delivering an aborted-but-final request. Truncation is a framing
// error and must propagate.
func TestCopyRequestBodyChunkedTrailerTruncated(t *testing.T) {
	framing := "3\r\nabc\r\n0\r\n" // zero chunk, then EOF inside trailers
	var out bytes.Buffer
	err := copyRequestBody(readConn{strings.NewReader("")},
		bufio.NewReader(strings.NewReader(framing)), &out,
		[]string{"Transfer-Encoding: chunked"})
	if err == nil {
		t.Fatal("truncated trailer committed the request")
	}
	if !strings.Contains(err.Error(), "trailer") {
		t.Fatalf("want trailer error, got %v", err)
	}
}
