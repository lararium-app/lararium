package router

import (
	"net"
	"net/http"
	"time"
)

// refuseRedirect refuses every HTTP redirect (H8/F1): credentials posted
// or headed on a request (client_secret, refresh_token, bearer) must never
// be replayed to a redirect target, on any status. A 3xx response is
// terminal and surfaces to the caller as the final response.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// DefaultTransport creates an HTTP transport with M11 timeouts:
// 120s response header timeout and 10s dial timeout.
func DefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

// WrapClient ensures the H8 CheckRedirect law and M11 transport timeouts are intact.
// If c is nil, a default provider client is returned. If c is non-nil, a copy is
// returned with CheckRedirect set to refuseRedirect and transport timeouts configured
// (following the pattern established in oauth2.go).
func WrapClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{
			Transport:     DefaultTransport(),
			CheckRedirect: refuseRedirect,
		}
	}
	cp := *c
	cp.CheckRedirect = refuseRedirect
	if cp.Transport == nil {
		cp.Transport = DefaultTransport()
	} else if t, ok := cp.Transport.(*http.Transport); ok {
		tCopy := t.Clone()
		tCopy.ResponseHeaderTimeout = 120 * time.Second
		if tCopy.DialContext == nil {
			tCopy.DialContext = (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext
		}
		cp.Transport = tCopy
	}
	return &cp
}

// providerClient is the one HTTP client for calls that carry provider
// credentials (Authorization: Bearer <api key>). Redirects are refused
// (H8/F1 family) and transport timeouts are enforced (M11: 120s response header, 10s dial).
var providerClient = WrapClient(nil)
