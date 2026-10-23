package surface

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostGate(t *testing.T) {
	loopbackAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7717}
	lanAddr := &net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 7717}

	tests := []struct {
		name       string
		allowed    []string
		host       string
		localAddr  net.Addr
		wantStatus int
	}{
		{"localhost loopback", nil, "localhost:7717", loopbackAddr, http.StatusOK},
		{"ipv6 bracketed", nil, "[::1]:7717", loopbackAddr, http.StatusOK},
		{"127.0.0.1", nil, "127.0.0.1:7717", loopbackAddr, http.StatusOK},
		{"evil host", nil, "evil.example", loopbackAddr, http.StatusForbidden},
		{"allowed host", []string{"hearth.example.com"}, "hearth.example.com", loopbackAddr, http.StatusOK},
		{"not in allowed", []string{"hearth.example.com"}, "evil.example", loopbackAddr, http.StatusForbidden},
		{"case insensitive", []string{"HEARTH.EXAMPLE.COM"}, "hearth.example.com", loopbackAddr, http.StatusOK},
		{"with port", []string{"example.com"}, "example.com:8080", loopbackAddr, http.StatusOK},

		// M18 finding tests:
		// (a) allowed_hosts with host:port matches Host header with port and bare host
		{"M18: host.example:8443 entry matches Host with port", []string{"host.example:8443"}, "host.example:8443", lanAddr, http.StatusOK},
		{"M18: host.example:8443 entry matches bare Host", []string{"host.example:8443"}, "host.example", lanAddr, http.StatusOK},
		{"M18: unknown host still 403", []string{"host.example:8443"}, "other.example", lanAddr, http.StatusForbidden},

		// (b) localhost verification against connection local address
		{"M18: Host localhost accepted on loopback local addr", nil, "localhost", loopbackAddr, http.StatusOK},
		{"M18: Host localhost rejected with spoofed-localhost on LAN local addr", nil, "localhost", lanAddr, http.StatusForbidden},
		{"M18: Host localhost rejected when local addr unavailable", nil, "localhost", nil, http.StatusForbidden},
		{"M18: 127.0.0.1 rejected on LAN local addr", nil, "127.0.0.1", lanAddr, http.StatusForbidden},
		{"M18: sub.localhost rejected on LAN local addr", nil, "sub.localhost", lanAddr, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := HostGate(tc.allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			if tc.localAddr != nil {
				req = req.WithContext(WithLocalAddr(req.Context(), tc.localAddr))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusForbidden {
				if w.Header().Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
				}
				if w.Body.String() != `{"error":"forbidden"}` {
					t.Errorf("body = %q, want {\"error\":\"forbidden\"}", w.Body.String())
				}
			}
		})
	}
}

type testVerifier struct {
	validTokens map[string]bool
}

func (v *testVerifier) Verify(token string) bool {
	return v.validTokens[token]
}

func TestBearerAuth(t *testing.T) {
	v := &testVerifier{validTokens: map[string]bool{"lar1_valid": true}}

	handler := BearerAuth(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme bearer", "bearer lar1_valid", http.StatusUnauthorized},
		{"wrong scheme Token", "Token lar1_valid", http.StatusUnauthorized},
		{"invalid token", "Bearer lar1_invalid", http.StatusUnauthorized},
		{"valid token", "Bearer lar1_valid", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if w.Header().Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
				}
				if w.Body.String() != `{"error":"unauthorized"}` {
					t.Errorf("body = %q, want {\"error\":\"unauthorized\"}", w.Body.String())
				}
			}
		})
	}
}

func TestValidSessionID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"main", true},
		{"s_ABCDEFGHIJKLMNOPQRSTUVWXYZ", true},
		{"s_0123456789ABCDEFGHIJKLMNOP", true},
		{"", false},
		{"s_", false},
		{"s_abcdefghijklmnopqrstuvwxyz", false},  // lowercase
		{"s_ABCDEFGHIJKLMNOPQRSTUVWXY", false},   // 25 chars
		{"s_ABCDEFGHIJKLMNOPQRSTUVWXYZ1", false}, // 27 chars
		{"s_../..", false},
		{"../..", false},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			got := ValidSessionID(tc.id)
			if got != tc.want {
				t.Errorf("ValidSessionID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestValidApprovalID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"a_abcdefghij", true},                   // 10 chars
		{"a_abcdefghijklmnopqrstuvwxyz12", true}, // 32 chars
		{"a_ABCDEFGHIJ", true},                   // uppercase
		{"", false},
		{"a_", false},
		{"a_abcdefghi", false},                         // 9 chars
		{"a_abcdefghijklmnopqrstuvwxyz1234567", false}, // 33 chars after a_
		{"b_abcdefghij", false},
		{"a_abcdefghi@", false},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			got := ValidApprovalID(tc.id)
			if got != tc.want {
				t.Errorf("ValidApprovalID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
