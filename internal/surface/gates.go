package surface

import (
	"net"
	"net/http"
	"regexp"
	"strings"
)

var sessionIDRe = regexp.MustCompile(`^(s_[0-9A-Z]{26}|main)$`)
var approvalIDRe = regexp.MustCompile(`^a_[0-9A-Za-z]{10,32}$`)

func ValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

func ValidApprovalID(id string) bool {
	return approvalIDRe.MatchString(id)
}

func HostGate(allowed []string) func(http.Handler) http.Handler {
	allowedMap := make(map[string]bool)
	for _, h := range allowed {
		allowedMap[normalizeHost(h)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if host == "" {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}

			h, _, err := net.SplitHostPort(host)
			if err != nil {
				h = host
			}

			h = normalizeHost(h)

			if h == "localhost" || h == "127.0.0.1" || h == "::1" || allowedMap[h] {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
		})
	}
}

func BearerAuth(v interface{ Verify(string) bool }) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}

			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}

			token := auth[len(prefix):]
			if !v.Verify(token) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return h
}
