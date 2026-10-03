package surface

import (
	"net"
	"net/http"
	"regexp"
	"strings"
)

var (
	sessionIDRe  = regexp.MustCompile(`^(s_[0-9A-Z]{26}|main)$`)
	approvalIDRe = regexp.MustCompile(`^a_[0-9A-Za-z]{10,32}$`)
)

// ValidSessionID accepts exactly "main" or a penatus s_<26 base32> id
// (spec §5: ids are validated before touching the filesystem).
func ValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

// ValidApprovalID accepts the a_<id> shape this package mints.
func ValidApprovalID(id string) bool {
	return approvalIDRe.MatchString(id)
}

// HostGate rejects requests whose Host header is not loopback or an
// entry of the serve.allowed_hosts allowlist (DNS-rebinding defense,
// spec §5). Port is stripped before matching.
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

			writeJSONBody(w, http.StatusForbidden, `{"error":"forbidden"}`)
		})
	}
}

// Verifier is the bearer-token check (TokenStore implements it).
type Verifier interface {
	Verify(token string) bool
}

// BearerAuth gates /v1/ behind a bearer token verified by v (the
// TokenStore). Static routes are registered outside this middleware
// (spec §4: the page itself carries no secret).
func BearerAuth(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeJSONBody(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
				return
			}

			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				writeJSONBody(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
				return
			}

			token := auth[len(prefix):]
			if !v.Verify(token) {
				writeJSONBody(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeJSONBody answers with a fixed JSON body (errors are terminal
// for the request; a write failure means the client is gone).
func writeJSONBody(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return h
}
