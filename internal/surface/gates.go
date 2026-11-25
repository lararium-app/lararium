package surface

import (
	"context"
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

// WithLocalAddr returns a child context carrying the local network address (M18).
func WithLocalAddr(ctx context.Context, addr net.Addr) context.Context {
	return context.WithValue(ctx, http.LocalAddrContextKey, addr)
}

func stripHostPort(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		h = hostPort
	}
	return normalizeHost(h)
}

func isLoopbackHostName(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	return IsLoopbackLiteral(h)
}

func isRequestLocalAddrLoopback(r *http.Request) bool {
	val := r.Context().Value(http.LocalAddrContextKey)
	if val == nil {
		return false
	}
	addr, ok := val.(net.Addr)
	if !ok || addr == nil {
		return false
	}
	var ip net.IP
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip = a.IP
	case *net.UDPAddr:
		ip = a.IP
	case *net.IPAddr:
		ip = a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			host = addr.String()
		}
		host = strings.Trim(host, "[]")
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// HostGate rejects requests whose Host header is not loopback or an
// entry of the serve.allowed_hosts allowlist (DNS-rebinding defense,
// spec §5, M18). Ports are stripped from both allowed_hosts entries and
// the incoming Host header before matching. Loopback host names are only
// accepted when the underlying connection's local address is proven loopback.
func HostGate(allowed []string) func(http.Handler) http.Handler {
	allowedMap := make(map[string]bool)
	for _, h := range allowed {
		allowedMap[stripHostPort(h)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if host == "" {
				writeJSONBody(w, http.StatusForbidden, `{"error":"forbidden"}`)
				return
			}

			h := stripHostPort(host)

			if allowedMap[h] {
				next.ServeHTTP(w, r)
				return
			}

			if isLoopbackHostName(h) {
				if isRequestLocalAddrLoopback(r) {
					next.ServeHTTP(w, r)
					return
				}
				writeJSONBody(w, http.StatusForbidden, `{"error":"forbidden"}`)
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

// IdentityVerifier is a Verifier that also names the caller a token belongs
// to (TokenStore implements it). BearerAuth binds that identity into the
// request context for session audience checks; a plain Verifier leaves the
// caller anonymous, which sees only sessions shared with every device.
type IdentityVerifier interface {
	VerifyIdentity(token string) (string, bool)
}

// KindVerifier is an IdentityVerifier that also reports what kind of
// client the credential was minted for (TokenStore implements it).
// BearerAuth binds that kind into the request context so a verdict can be
// attributed to the channel it actually came from instead of being
// recorded as the web dashboard (CUSTOS-SPEC §6.4b amendment v14). A
// verifier that does not implement this leaves the kind empty, which
// means the web channel.
type KindVerifier interface {
	VerifyKind(token string) (caller string, kind string, ok bool)
}

// BearerAuth gates /v1/ behind a bearer token verified by v (the
// TokenStore). Static routes are registered outside this middleware
// (spec §4: the page itself carries no secret).
func BearerAuth(v Verifier) func(http.Handler) http.Handler {
	iv, _ := v.(IdentityVerifier)
	kv, _ := v.(KindVerifier)
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
			if kv != nil {
				caller, kind, ok := kv.VerifyKind(token)
				if !ok {
					writeJSONBody(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
					return
				}
				ctx := withCallerKind(withCaller(r.Context(), caller), kind)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if iv != nil {
				caller, ok := iv.VerifyIdentity(token)
				if !ok {
					writeJSONBody(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
					return
				}
				next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), caller)))
				return
			}
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
