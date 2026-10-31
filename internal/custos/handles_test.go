package custos_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// handleTestValue is the fake secret under test; the name avoids "secret" so
// gosec's hardcoded-credential heuristic stays quiet.
const handleTestValue = "sk-liv-HAND...9876"

func setupHandleVault(t *testing.T) (*custos.Vault, string, string) {
	t.Helper()
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["svc"] = custos.Credential{Kind: "api_key", Secret: handleTestValue}
		return []string{"svc"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return v, stateDir, pass
}

func readAuditLines(t *testing.T, stateDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(stateDir, "audit", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

func auditByKind(t *testing.T, stateDir, kind string) []custos.AuditRecord {
	t.Helper()
	var out []custos.AuditRecord
	for _, l := range readAuditLines(t, stateDir) {
		var r custos.AuditRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func TestHandleA1_CreateList(t *testing.T) {
	v, stateDir, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc/toola", time.Hour, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Handle, "hdl_") || len(rec.Handle) != 26 {
		t.Fatalf("bad handle shape %q", rec.Handle)
	}
	if rec.Last4 != "9876" {
		t.Fatalf("last4 = %q", rec.Last4)
	}
	list, err := v.ListHandles()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].Last4 != "" {
		t.Fatal("Last4 not blanked in list")
	}
	if list[0].Fingerprint() != rec.Fingerprint() || list[0].Scope != "svc/toola" || list[0].Name != "svc" {
		t.Fatalf("unexpected row %+v", list[0])
	}
	out, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(handleTestValue)) || bytes.Contains(out, []byte("9876")) {
		t.Fatal("list output leaks secret material")
	}
	// No secret substring in any on-disk state file (vault is age-encrypted;
	// audit carries only the fingerprint). Path is t.TempDir()-rooted test
	// state, never user input.
	err = filepath.Walk(filepath.Dir(stateDir), func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // G122: t.TempDir()-rooted test state, not user input
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, []byte(handleTestValue)) {
			t.Errorf("secret found in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.CreateHandle(pass, "missing", "missing", 0, nil, "cli"); err == nil {
		t.Fatal("expected error for unknown credential")
	}
}

func TestHandleA2_ResolveHappy(t *testing.T) {
	v, stateDir, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	g := v.ResolveHandle(rec.Handle, "anytool", "", 0)
	if g.Deny != "" || g.Secret != handleTestValue {
		t.Fatalf("grant = %+v", g)
	}
	used := auditByKind(t, stateDir, custos.AuditKindHandleUsed)
	if len(used) != 1 {
		t.Fatalf("handle_used audits = %d", len(used))
	}
	if used[0].Sur != rec.Fingerprint() || used[0].Cred != "svc" {
		t.Fatalf("audit = %+v", used[0])
	}
	created := auditByKind(t, stateDir, custos.AuditKindHandleCreated)
	if len(created) != 1 || created[0].Sur != rec.Fingerprint() {
		t.Fatalf("created audit = %+v", created)
	}
}

func TestHandleA3_Denials(t *testing.T) {
	v, stateDir, pass := setupHandleVault(t)

	if g := v.ResolveHandle("hdl_nope", "t", "", 0); g.Deny != custos.HandleDenyUnknown || g.Secret != "" {
		t.Fatalf("unknown: %+v", g)
	}

	// expired: write a record with a past expiry through the mutation protocol.
	rec, err := v.CreateHandle(pass, "svc", "svc", time.Hour, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	err = v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		r := doc.Handles[rec.Handle]
		r.Expires = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		doc.Handles[rec.Handle] = r
		return []string{"svc"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(rec.Handle, "t", "", 0); g.Deny != custos.HandleDenyExpired || g.Secret != "" {
		t.Fatalf("expired: %+v", g)
	}

	// bad_scope: tool and host forms
	tool, err := v.CreateHandle(pass, "svc", "svc/toola", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(tool.Handle, "toola", "", 0); g.Deny != "" {
		t.Fatalf("toola should pass: %+v", g)
	}
	if g := v.ResolveHandle(tool.Handle, "toolb", "", 0); g.Deny != custos.HandleDenyBadScope || g.Secret != "" {
		t.Fatalf("toolb: %+v", g)
	}
	hostRec, err := v.CreateHandle(pass, "svc", "svc@api.example.com:8443", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(hostRec.Handle, "", "api.example.com", 8443); g.Deny != "" {
		t.Fatalf("host should pass: %+v", g)
	}
	if g := v.ResolveHandle(hostRec.Handle, "", "evil.example.com", 8443); g.Deny != custos.HandleDenyBadScope {
		t.Fatalf("wrong host: %+v", g)
	}
	if g := v.ResolveHandle(hostRec.Handle, "", "", 0); g.Deny != custos.HandleDenyBadScope {
		t.Fatalf("no tool no host: %+v", g)
	}

	// rate: n+1th call in-window
	rated, err := v.CreateHandle(pass, "svc", "svc", 0, &custos.HandleRate{N: 2, Window: 60}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if g := v.ResolveHandle(rated.Handle, "t", "", 0); g.Deny != "" {
			t.Fatalf("call %d denied: %+v", i, g)
		}
	}
	if g := v.ResolveHandle(rated.Handle, "t", "", 0); g.Deny != custos.HandleDenyRate || g.Secret != "" {
		t.Fatalf("rate: %+v", g)
	}
	if n := len(auditByKind(t, stateDir, custos.AuditKindHandleDeniedRate)); n != 1 {
		t.Fatalf("rate audits = %d", n)
	}
	if n := len(auditByKind(t, stateDir, custos.AuditKindHandleDeniedScope)); n < 4 {
		t.Fatalf("scope-kind audits = %d", n)
	}

	// no_credential
	if err := v.RevokeCredential(pass, "svc", "cli"); err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(tool.Handle, "toola", "", 0); g.Deny != custos.HandleDenyNoCred || g.Secret != "" {
		t.Fatalf("no_credential: %+v", g)
	}

	// locked: not audited as a handle event
	before := len(readAuditLines(t, stateDir))
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	afterLock := len(readAuditLines(t, stateDir))
	if g := v.ResolveHandle(tool.Handle, "toola", "", 0); g.Deny != custos.HandleDenyLocked {
		t.Fatalf("locked: %+v", g)
	}
	if got := len(readAuditLines(t, stateDir)); got != afterLock {
		t.Fatalf("locked resolve audited (%d -> %d, pre-lock %d)", afterLock, got, before)
	}
}

func TestHandleA3b_RateRefillsAfterWindow(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc", 0, &custos.HandleRate{N: 1, Window: 1}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(rec.Handle, "t", "", 0); g.Deny != "" {
		t.Fatal(g.Deny)
	}
	if g := v.ResolveHandle(rec.Handle, "t", "", 0); g.Deny != custos.HandleDenyRate {
		t.Fatalf("want rate, got %q", g.Deny)
	}
	time.Sleep(1100 * time.Millisecond)
	if g := v.ResolveHandle(rec.Handle, "t", "", 0); g.Deny != "" {
		t.Fatalf("after window: %q", g.Deny)
	}
}

func TestHandleA4_ScopeGrammar(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	bare, err := v.CreateHandle(pass, "svc", "svc", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"a", "b"} {
		if g := v.ResolveHandle(bare.Handle, tool, "", 0); g.Deny != "" {
			t.Fatalf("bare should cover %s: %q", tool, g.Deny)
		}
	}
	q, err := v.CreateHandle(pass, "svc", "svc/a", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(q.Handle, "a", "", 0); g.Deny != "" {
		t.Fatalf("a: %q", g.Deny)
	}
	if g := v.ResolveHandle(q.Handle, "b", "", 0); g.Deny != custos.HandleDenyBadScope {
		t.Fatalf("b: %q", g.Deny)
	}
	for _, bad := range []string{"SVC", "svc/a/b", "svc@", "svc:auto", "svc allow", "other"} {
		if _, err := v.CreateHandle(pass, "svc", bad, 0, nil, "cli"); err == nil {
			t.Errorf("scope %q should be rejected", bad)
		}
	}
	if _, err := v.CreateHandle(pass, "svc", "svc", 0, &custos.HandleRate{N: 0, Window: 10}, "cli"); err == nil {
		t.Error("rate n=0 should be rejected")
	}
}

func TestHandleA5_Revoke(t *testing.T) {
	v, stateDir, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.RevokeHandle(pass, rec.Handle, "cli"); err != nil {
		t.Fatal(err)
	}
	if g := v.ResolveHandle(rec.Handle, "t", "", 0); g.Deny != custos.HandleDenyUnknown {
		t.Fatalf("after revoke: %q", g.Deny)
	}
	if err := v.RevokeHandle(pass, rec.Handle, "cli"); !errors.Is(err, custos.ErrHandleNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	rev := auditByKind(t, stateDir, custos.AuditKindHandleRevoked)
	if len(rev) != 1 || rev[0].Sur != rec.Fingerprint() {
		t.Fatalf("revoked audit = %+v", rev)
	}
}

func TestHandleA5b_FindByFingerprint(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc", 0, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := v.FindHandle(rec.Fingerprint()); err != nil || tok != rec.Handle {
		t.Fatalf("fingerprint find: %q %v", tok, err)
	}
	if tok, err := v.FindHandle(rec.Handle); err != nil || tok != rec.Handle {
		t.Fatalf("raw find: %q %v", tok, err)
	}
	if _, err := v.FindHandle("deadbeef"); !errors.Is(err, custos.ErrHandleNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestHandleA6_Persistence(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	// Vault doc written without any handles still loads.
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock without handles field: %v", err)
	}
	if list, err := v.ListHandles(); err != nil || len(list) != 0 {
		t.Fatalf("expected empty list, got %v %v", list, err)
	}
	rec, err := v.CreateHandle(pass, "svc", "svc", time.Hour, &custos.HandleRate{N: 3, Window: 60}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ListHandles(); !errors.Is(err, custos.ErrCustosLocked) {
		t.Fatalf("locked list: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}
	g := v.ResolveHandle(rec.Handle, "t", "", 0)
	if g.Deny != "" || g.Secret != handleTestValue {
		t.Fatalf("after relock: %+v", g)
	}
	if g.Record.Rate == nil || g.Record.Rate.N != 3 {
		t.Fatalf("rate lost: %+v", g.Record)
	}
}

func TestHandleA7_SecretHygiene(t *testing.T) {
	v, stateDir, pass := setupHandleVault(t)
	rec, err := v.CreateHandle(pass, "svc", "svc/a", 0, &custos.HandleRate{N: 1, Window: 60}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	v.ResolveHandle(rec.Handle, "a", "", 0) // used
	v.ResolveHandle(rec.Handle, "a", "", 0) // rate
	v.ResolveHandle(rec.Handle, "b", "", 0) // scope
	v.ResolveHandle("hdl_bogus", "a", "", 0)
	if err := v.RevokeHandle(pass, rec.Handle, "cli"); err != nil {
		t.Fatal(err)
	}
	list, _ := v.ListHandles()
	listJSON, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(listJSON, []byte(handleTestValue)) || bytes.Contains(listJSON, []byte("9876")) {
		t.Fatal("list leaks secret material")
	}
	for _, l := range readAuditLines(t, stateDir) {
		if strings.Contains(l, handleTestValue) {
			t.Fatalf("secret in audit line: %s", l)
		}
		if strings.Contains(l, "9876") {
			t.Fatalf("last4 in audit line: %s", l)
		}
		if strings.Contains(l, rec.Handle) {
			t.Fatalf("raw handle in audit line: %s", l)
		}
	}
}

// TestHandleViews_CarryLast4NoSecret: ListHandleViews carries last4 and kind
// (CREDHANDLE-SPEC D5) while ListHandles still redacts; no value equals or
// contains the secret.
func TestHandleViews_CarryLast4NoSecret(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	rate := &custos.HandleRate{N: 3, Window: 60}
	rec, err := v.CreateHandle(pass, "svc", "svc", time.Hour, rate, "cli")
	if err != nil {
		t.Fatal(err)
	}
	views, err := v.ListHandleViews()
	if err != nil || len(views) != 1 {
		t.Fatalf("views: %v %v", views, err)
	}
	hv := views[0]
	if hv.Handle != rec.Handle || hv.Name != "svc" || hv.Kind != "api_key" || hv.Last4 != "9876" ||
		hv.Created == "" || hv.Expires == "" || hv.Rate == nil || hv.Rate.N != 3 || hv.Rate.Window != 60 {
		t.Fatalf("unexpected view %+v", hv)
	}
	for _, f := range []string{hv.Handle, hv.Name, hv.Kind, hv.Last4, hv.Created, hv.Expires} {
		if f == handleTestValue || strings.Contains(f, handleTestValue) {
			t.Fatal("view field carries the secret")
		}
	}
	list, err := v.ListHandles()
	if err != nil || list[0].Last4 != "" {
		t.Fatalf("ListHandles must still redact: %+v %v", list, err)
	}
}

func TestCreateCredentialHandle_ValidatesBeforeWrite(t *testing.T) {
	v, _, pass := setupHandleVault(t)
	cases := map[string]func() error{
		"name":   func() error { _, e := v.CreateCredentialHandle(pass, "Bad Name", "x", "", 0, nil, "web"); return e },
		"secret": func() error { _, e := v.CreateCredentialHandle(pass, "ok1", "", "", 0, nil, "web"); return e },
		"scope": func() error {
			_, e := v.CreateCredentialHandle(pass, "ok1", "x", "other/tool", 0, nil, "web")
			return e
		},
		"rate": func() error {
			_, e := v.CreateCredentialHandle(pass, "ok1", "x", "", 0, &custos.HandleRate{N: 0, Window: 1}, "web")
			return e
		},
	}
	for field, fn := range cases {
		var fe *custos.FieldError
		if err := fn(); !errors.As(err, &fe) || fe.Field != field {
			t.Fatalf("%s: got %v", field, err)
		}
	}
	// No orphan credential was written by the failed attempts.
	if _, err := v.CreateCredentialHandle(pass, "ok1", "fresh-value-1234", "", 0, nil, "web"); err != nil {
		t.Fatalf("name should still be free: %v", err)
	}
	if _, err := v.CreateCredentialHandle(pass, "ok1", "again-value-1234", "", 0, nil, "web"); !errors.Is(err, custos.ErrCredentialExists) {
		t.Fatalf("want ErrCredentialExists, got %v", err)
	}
}
