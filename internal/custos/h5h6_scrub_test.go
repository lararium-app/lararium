// H5/H6 regressions: response headers never carry the swapped secret,
// and a broken request-body stream closes instead of relaying past the
// scrub.

package custos_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

type scrubHarness struct {
	h           *proxyHarness
	setUpstream func(func(http.ResponseWriter, *http.Request))
}

func newScrubHarness(t *testing.T, credSecret string) *scrubHarness {
	t.Helper()
	h := setupProxyHarness(t, nil)
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["scrub-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"scrub-cred"}, nil
	}, false)
	_, _ = h.v.Policy().AddCredentialRule("scrub-cred", custos.VerdictAuto, false, "cli", "cli")
	_, _ = h.v.Policy().AddEgressRule("echo.example.com/*", "auto", false, "cli", "cli")

	var mu sync.Mutex
	var handler http.HandlerFunc
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hf := handler
		mu.Unlock()
		if hf != nil {
			hf(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	h.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", ts.Listener.Addr().String())
	})
	return &scrubHarness{
		h: h,
		setUpstream: func(f func(http.ResponseWriter, *http.Request)) {
			mu.Lock()
			handler = f
			mu.Unlock()
		},
	}
}

// H6: an upstream that reflects the credential in response headers —
// the WWW-Authenticate / Set-Cookie pattern — must not deliver it. The
// secret-carrying headers are stripped; any other header echo is
// needle-redacted like the body.
func TestH6_ResponseHeaderRedaction(t *testing.T) {
	const credSecret = "fake-header-secret-77aa" //nolint:gosec // test fixture
	sh := newScrubHarness(t, credSecret)
	surToken, _ := sh.h.v.AddSurrogate(sh.h.passphrase, "scrub-cred", "echo.example.com", 80, "/", false, "test")

	sh.setUpstream(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Set-Cookie", "session="+auth+"; Path=/")
		// Direct map write: the point is to emit the NON-canonical
		// wire spelling a hostile upstream would use.
		w.Header()["Www-Authenticate"] = []string{`Bearer error="bad realm="` + auth}
		w.Header().Set("X-Trace", "seen "+auth+" end")
		_, _ = fmt.Fprint(w, "ok")
	})

	conn, err := net.Dial("tcp", sh.h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write(fmt.Appendf(nil, "GET /h6 HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken))

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if _, ok := resp.Header["Set-Cookie"]; ok {
		t.Error("Set-Cookie survived the scrub lane")
	}
	if _, ok := resp.Header["Www-Authenticate"]; ok {
		t.Error("WWW-Authenticate survived the scrub lane")
	}
	if trace := resp.Header.Get("X-Trace"); strings.Contains(trace, credSecret) {
		t.Errorf("secret leaked in header echo: %q", trace)
	} else if !strings.Contains(trace, "[redacted]") {
		t.Errorf("header echo not redacted: %q", trace)
	}
}

// H5: Content-Length promises more body than the client sends before
// FIN. The body copy fails; the response — which echoes the secret —
// must never reach the client.
func TestH5_BodyCopyFailureClosesInsteadOfRelay(t *testing.T) {
	const credSecret = "fake-h5-secret-3c3d" //nolint:gosec // test fixture
	sh := newScrubHarness(t, credSecret)
	surToken, _ := sh.h.v.AddSurrogate(sh.h.passphrase, "scrub-cred", "echo.example.com", 80, "/", false, "test")

	sh.setUpstream(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		// #nosec G705 -- deliberate echo of the request credential in a
		// test-only fake upstream; no HTML context, no browser.
		_, _ = fmt.Fprint(w, "diagnostic: your key was "+auth)
	})

	conn, err := net.Dial("tcp", sh.h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Body promises 200 bytes; only 4 are sent, then FIN.
	_, _ = conn.Write(fmt.Appendf(nil, "POST /h5 HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\nContent-Length: 200\r\n\r\nabcd", surToken))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The old bypass relayed the raw diagnostic; the fixed path must
	// deliver nothing parseable (RST or EOF with no response).
	br := bufio.NewReader(conn)
	line, rerr := br.ReadString('\n')
	if rerr == nil && strings.Contains(line, "HTTP/") {
		rest, _ := io.ReadAll(br)
		if strings.Contains(line+string(rest), credSecret) {
			t.Fatalf("unscanned response with secret relayed: %s%s", line, rest)
		}
		t.Fatalf("response was relayed despite body-copy failure: %s", line)
	}
}
