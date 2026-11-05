package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSRFGuard_BlockedIPsAndHostnames(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		// Required by spec brief
		{name: "localhost", rawURL: "http://localhost", wantErr: "ssrf:"},
		{name: "localhost with port", rawURL: "http://localhost:8080/test", wantErr: "ssrf:"},
		{name: "127.0.0.1", rawURL: "http://127.0.0.1", wantErr: "ssrf:"},
		{name: "127.0.0.1 with path", rawURL: "http://127.0.0.1:9090/status", wantErr: "ssrf:"},
		{name: "::1", rawURL: "http://[::1]", wantErr: "ssrf:"},
		{name: "::1 with port", rawURL: "http://[::1]:8080/", wantErr: "ssrf:"},
		{name: "10.0.0.5", rawURL: "http://10.0.0.5", wantErr: "ssrf:"},
		{name: "172.16.0.1", rawURL: "http://172.16.0.1", wantErr: "ssrf:"},
		{name: "192.168.1.1", rawURL: "http://192.168.1.1", wantErr: "ssrf:"},
		{name: "169.254.169.254", rawURL: "http://169.254.169.254", wantErr: "ssrf:"},
		{name: "fe80::1", rawURL: "http://[fe80::1]", wantErr: "ssrf:"},

		// Additional boundaries: unspecified, multicast, CGNAT, broadcast, schemes
		{name: "0.0.0.0", rawURL: "http://0.0.0.0", wantErr: "ssrf:"},
		{name: ":: unspecified", rawURL: "http://[::]", wantErr: "ssrf:"},
		{name: "224.0.0.1 multicast", rawURL: "http://224.0.0.1", wantErr: "ssrf:"},
		{name: "100.64.0.1 cgnat", rawURL: "http://100.64.0.1", wantErr: "ssrf:"},
		{name: "6to4 wrapping loopback", rawURL: "http://[2002:7f00:0001::]", wantErr: "ssrf:"},
		{name: "teredo prefix", rawURL: "http://[2001:0000:4136:e378:8000:63bf:3fff:fdd2]", wantErr: "ssrf:"},
		{name: "255.255.255.255 broadcast", rawURL: "http://255.255.255.255", wantErr: "ssrf:"},
		{name: "file scheme", rawURL: "file:///etc/hosts", wantErr: "unsupported URL scheme"},
		{name: "ftp scheme", rawURL: "ftp://example.com/test", wantErr: "unsupported URL scheme"},
		{name: "gopher scheme", rawURL: "gopher://example.com", wantErr: "unsupported URL scheme"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ValidateURL(t.Context(), tc.rawURL)
			if err == nil {
				t.Fatalf("ValidateURL(%q) succeeded, want error containing %q", tc.rawURL, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateURL(%q) err = %v, want error containing %q", tc.rawURL, err, tc.wantErr)
			}
		})
	}
}

func TestSSRFGuard_MockResolverHostnameResolvesToLoopback(t *testing.T) {
	mockResolver := ResolverFunc(func(ctx context.Context, network, host string) ([]net.IP, error) {
		if host == "loopback.test" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		if host == "rebind-v6.test" {
			return []net.IP{net.ParseIP("::1")}, nil
		}
		if host == "metadata.test" {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{name: "hostname resolves to 127.0.0.1", rawURL: "http://loopback.test/secret", wantErr: "ssrf: host \"loopback.test\" resolves to blocked IP 127.0.0.1"},
		{name: "hostname resolves to ::1", rawURL: "http://rebind-v6.test/", wantErr: "ssrf: host \"rebind-v6.test\" resolves to blocked IP ::1"},
		{name: "hostname resolves to metadata", rawURL: "http://metadata.test/latest", wantErr: "ssrf: host \"metadata.test\" resolves to blocked IP 169.254.169.254"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ValidateURLWithResolver(context.Background(), tc.rawURL, mockResolver, "")
			if err == nil {
				t.Fatalf("ValidateURLWithResolver(%q) succeeded, want error %q", tc.rawURL, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateURLWithResolver(%q) err = %v, want %q", tc.rawURL, err, tc.wantErr)
			}
		})
	}

	// Valid public hostname resolution passes
	parsed, ips, err := ValidateURLWithResolver(context.Background(), "http://public.test/ok", mockResolver, "")
	if err != nil {
		t.Fatalf("ValidateURLWithResolver for public host failed: %v", err)
	}
	if parsed == nil || len(ips) == 0 {
		t.Fatalf("expected valid parsed URL and IPs, got parsed=%v ips=%v", parsed, ips)
	}
}

func TestSSRFGuard_RedirectToInternalBlocked(t *testing.T) {
	// httptest server that redirects to http://127.0.0.1/
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/", http.StatusFound)
	}))
	defer ts.Close()

	cfg := DefaultConfig()
	_, err := Fetch(context.Background(), ts.URL, cfg, WithAllowedTestHost(ts.Listener.Addr().String()))
	if err == nil {
		t.Fatal("expected redirect to 127.0.0.1 to be blocked, but fetch succeeded")
	}
	if !strings.Contains(err.Error(), "ssrf:") && !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected SSRF blocked error, got: %v", err)
	}
}
