package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Issue #338 / CUSTOS-SPEC §6.4b amendment v14: a verdict decided on a
// paired device must be recorded as the device channel, not as a
// dashboard click. The only honest discriminator is the kind of
// credential the daemon itself minted, so these tests pin that chain:
// mint -> verify -> context -> via.

func newKindTestStore(t *testing.T) *TokenStore {
	t.Helper()
	st, err := OpenTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}
	return st
}

func TestViaForKind(t *testing.T) {
	tests := []struct {
		name string
		kind string
		want string
	}{
		{"device credential yields the device channel", TokenKindDevice, viaDevice},
		{"unmarked credential yields the web channel", "", viaWeb},
		{"unrecognised kind is never trusted into the device channel", "phone", viaWeb},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := viaForKind(tc.kind); got != tc.want {
				t.Fatalf("viaForKind(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestTokenStoreCreateKindRoundTrip(t *testing.T) {
	st := newKindTestStore(t)

	devTok, err := st.CreateKind("paired:app:phone", TokenKindDevice)
	if err != nil {
		t.Fatalf("CreateKind: %v", err)
	}
	opTok, err := st.Create("operator-minted")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	caller, kind, ok := st.VerifyKind(devTok)
	if !ok || kind != TokenKindDevice {
		t.Fatalf("VerifyKind(device) = %q/%q/%v, want caller/device/true", caller, kind, ok)
	}
	if caller == "" {
		t.Fatal("VerifyKind(device) returned an empty caller identity")
	}

	// An operator-minted credential must never claim the device channel,
	// no matter what label the operator typed.
	if _, kind, ok := st.VerifyKind(opTok); !ok || kind != "" {
		t.Fatalf("VerifyKind(operator) kind = %q (ok=%v), want empty", kind, ok)
	}

	// VerifyIdentity must keep reporting exactly what it did before.
	id, ok := st.VerifyIdentity(devTok)
	if !ok || id != caller {
		t.Fatalf("VerifyIdentity = %q/%v, want %q/true", id, ok, caller)
	}
	if _, _, ok := st.VerifyKind("not-a-token"); ok {
		t.Fatal("VerifyKind accepted an unknown token")
	}
}

// TestTokenStoreKindBackCompat pins that entries written before the kind
// field existed keep working and report the web channel, so the change
// cannot silently re-attribute historical credentials.
func TestTokenStoreKindBackCompat(t *testing.T) {
	st := newKindTestStore(t)
	tok, err := st.Create("legacy")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Rewrite the store exactly as a pre-v14 daemon would have: no kind key.
	raw, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	delete(entries[0], "kind")
	stripped, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(st.path, stripped, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, kind, ok := st.VerifyKind(tok)
	if !ok {
		t.Fatal("VerifyKind rejected a pre-v14 entry")
	}
	if kind != "" {
		t.Fatalf("pre-v14 entry kind = %q, want empty", kind)
	}
	if got := viaForKind(kind); got != viaWeb {
		t.Fatalf("pre-v14 entry via = %q, want %q", got, viaWeb)
	}
}

// kindVerifierDouble implements both Verifier and KindVerifier.
type kindVerifierDouble struct{ kind string }

func (k kindVerifierDouble) Verify(token string) bool { return token == "good" }

func (k kindVerifierDouble) VerifyIdentity(token string) (string, bool) {
	return "t_abc", token == "good"
}

func (k kindVerifierDouble) VerifyKind(token string) (string, string, bool) {
	return "t_abc", k.kind, token == "good"
}

// identityOnlyDouble implements Verifier and IdentityVerifier but NOT
// KindVerifier, standing in for any verifier that predates the kind field.
type identityOnlyDouble struct{}

func (identityOnlyDouble) Verify(token string) bool { return token == "good" }

func (identityOnlyDouble) VerifyIdentity(token string) (string, bool) {
	return "t_abc", token == "good"
}

func TestBearerAuthBindsCallerKind(t *testing.T) {
	tests := []struct {
		name     string
		verifier Verifier
		wantKind string
		wantVia  string
	}{
		{"device credential", kindVerifierDouble{kind: TokenKindDevice}, TokenKindDevice, viaDevice},
		{"unmarked credential", kindVerifierDouble{kind: ""}, "", viaWeb},
		{"verifier without kind support", identityOnlyDouble{}, "", viaWeb},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotCaller, gotKind, gotVia string
			h := BearerAuth(tc.verifier)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCaller = callerFrom(r.Context())
				gotKind = callerKindFrom(r.Context())
				gotVia = viaForKind(gotKind)
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/a_x/resolve", nil)
			req.Header.Set("Authorization", "Bearer good")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotCaller != "t_abc" {
				t.Fatalf("caller = %q, want t_abc", gotCaller)
			}
			if gotKind != tc.wantKind {
				t.Fatalf("caller kind = %q, want %q", gotKind, tc.wantKind)
			}
			if gotVia != tc.wantVia {
				t.Fatalf("via = %q, want %q", gotVia, tc.wantVia)
			}
		})
	}
}

func TestBearerAuthKindVerifierStillRejectsBadToken(t *testing.T) {
	called := false
	h := BearerAuth(kindVerifierDouble{kind: TokenKindDevice})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("handler ran despite an unauthorized credential")
	}
}
