package custos_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// V4: Binding matrix through the real proxy / comparator against target authority:
// - right host
// - wrong host
// - right host wrong port
// - /v1/ prefix vs /v1/../admin (normalized mismatch)
// - uppercase Host header
// - host:80 vs rule without port
// - punycode-homograph target (аpple.com matches only an xn--pple-43d.com rule, never apple.com)
// All per CUSTOS-SPEC §6.3, §10 V4.
func TestV4_BindingMatrix(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}

	// Add test credentials
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-test-123"}
		doc.Credentials["cyrillic"] = custos.Credential{Kind: "api_key", Secret: "sk-cyr-456"}
		return []string{"openai", "cyrillic"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate vault: %v", err)
	}

	// 1. Register surrogate for api.example.com with path prefix /v1/ and port 8080
	tok1, err := v.AddSurrogate(pass, "openai", "api.example.com", 8080, "/v1/", false, "cli")
	if err != nil {
		t.Fatalf("AddSurrogate tok1: %v", err)
	}

	// 2. Register surrogate for default port 80 on api.example.org
	tok2, err := v.AddSurrogate(pass, "openai", "api.example.org", 80, "/", false, "cli")
	if err != nil {
		t.Fatalf("AddSurrogate tok2: %v", err)
	}

	// 3. Register surrogate with explicit punycode host xn--pple-43d.com
	tokPuny, err := v.AddSurrogate(pass, "cyrillic", "xn--pple-43d.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatalf("AddSurrogate tokPuny: %v", err)
	}

	reg := v.Surrogates()
	if reg == nil {
		t.Fatal("expected non-nil surrogate registry")
	}

	// Case a: right host, right port, right path
	rec, err := reg.LookupAndMatch(tok1, "api.example.com:8080", "/v1/chat")
	if err != nil || rec == nil {
		t.Fatalf("right host match failed: %v", err)
	}

	// Case b: wrong host
	_, err = reg.LookupAndMatch(tok1, "evil.com:8080", "/v1/chat")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("wrong host expected mismatch, got %v", err)
	}

	// Case c: right host wrong port
	_, err = reg.LookupAndMatch(tok1, "api.example.com:9000", "/v1/chat")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("wrong port expected mismatch, got %v", err)
	}

	// Case d: /v1/ prefix vs /v1/../admin (normalized mismatch)
	_, err = reg.LookupAndMatch(tok1, "api.example.com:8080", "/v1/../admin")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("normalized path mismatch expected error, got %v", err)
	}

	// Case e: uppercase Host header (API.EXAMPLE.COM:8080 normalized to lowercase)
	rec, err = reg.LookupAndMatch(tok1, "API.EXAMPLE.COM:8080", "/v1/models")
	if err != nil || rec == nil {
		t.Fatalf("uppercase Host header match failed: %v", err)
	}

	// Case f: host:80 vs rule without port (api.example.org:80 stripped to api.example.org)
	rec, err = reg.LookupAndMatch(tok2, "api.example.org:80", "/anything")
	if err != nil || rec == nil {
		t.Fatalf("host:80 vs default port match failed: %v", err)
	}
	rec, err = reg.LookupAndMatch(tok2, "api.example.org", "/anything")
	if err != nil || rec == nil {
		t.Fatalf("bare host match failed: %v", err)
	}

	// Case g: punycode homograph target:
	// аpple.com (Cyrillic 'а', U+0430) normalizes to xn--pple-43d.com
	// Must match xn--pple-43d.com surrogate
	cyrillicTarget := "\u0430pple.com"
	rec, err = reg.LookupAndMatch(tokPuny, cyrillicTarget, "/")
	if err != nil || rec == nil {
		t.Fatalf("cyrillic homograph target match failed: %v", err)
	}

	// Must NOT match ASCII apple.com surrogate if one existed
	tokASCII, err := v.AddSurrogate(pass, "openai", "apple.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatalf("add ASCII apple.com surrogate: %v", err)
	}
	// Target аpple.com presented to ASCII apple.com surrogate fails host mismatch!
	reg = v.Surrogates()
	_, err = reg.LookupAndMatch(tokASCII, cyrillicTarget, "/")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("cyrillic target presented to ASCII surrogate expected mismatch, got %v", err)
	}

	// Drive the REAL comparator with punycode rules per §6.3, §10 V4:
	// Rule 1: xn--pple-43d.com auto
	// Rule 2: apple.com deny
	_, err = v.Policy().AddEgressRule("xn--pple-43d.com", custos.VerdictAuto, false, "cli", "cli")
	if err != nil {
		t.Fatalf("add punycode rule: %v", err)
	}
	_, err = v.Policy().AddEgressRule("apple.com", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatalf("add ascii rule: %v", err)
	}

	// Target аpple.com decides auto (matches xn--pple-43d.com rule only)
	verdict, matched := v.Policy().Decide(custos.DecideInput{
		Host: cyrillicTarget,
		Port: 80,
	})
	if verdict != custos.VerdictAuto || matched != "xn--pple-43d.com" {
		t.Errorf("homograph target decided %s (%s), want auto (xn--pple-43d.com)", verdict, matched)
	}

	// Target apple.com decides deny (matches apple.com rule)
	verdict, matched = v.Policy().Decide(custos.DecideInput{
		Host: "apple.com",
		Port: 80,
	})
	if verdict != custos.VerdictDeny || matched != "apple.com" {
		t.Errorf("ascii target decided %s (%s), want deny (apple.com)", verdict, matched)
	}
}

// V11: Corruption postures:
// - unparseable policy.json => engine refuses, never defaults open; remedy custos policy reset
// - corrupt surrogate entries dropped with surrogate_rejected (unknown lane, invalid host, missing cred)
// - unknown lane dropped, never bearer-defaulted
// - status reports degraded with surrogates_dropped count
// All per CUSTOS-SPEC §6.6, §10 V11.
func TestV11_CorruptionPosture(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	// 1. Corrupt policy.json
	policyPath := filepath.Join(stateDir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"version": 1, "credential_actions": "corrupted`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock with corrupt policy should succeed into degraded/locked state: %v", err)
	}

	// Engine refuses, never defaults open per V11
	if !v.Policy().IsCorrupt() {
		t.Fatal("expected policy engine to be marked corrupt")
	}

	verdict, _ := v.Policy().Decide(custos.DecideInput{
		Host: "example.com",
	})
	if verdict != custos.VerdictDeny {
		t.Errorf("corrupt policy Decide() = %s, want deny (never defaults open)", verdict)
	}

	// Status reports degraded per §6.6, §11
	st := v.Status(false, 0)
	if st.State != custos.StateDegraded {
		t.Errorf("status = %s, want degraded", st.State)
	}

	// Reset clears corruption
	if err := v.Policy().Reset("cli"); err != nil {
		t.Fatalf("policy reset: %v", err)
	}
	if v.Policy().IsCorrupt() {
		t.Fatal("expected policy to no longer be corrupt after reset")
	}

	// 2. Corrupt surrogate entries in surrogates.age
	// Seed vault with credential "realcred"
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["realcred"] = custos.Credential{Kind: "api_key", Secret: "sk-real"}
		return []string{"realcred"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Add valid surrogate
	validTok, err := v.AddSurrogate(pass, "realcred", "valid.example.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatal(err)
	}

	// Tamper surrogates.age by decrypting, adding corrupt rows, and re-encrypting
	surPath := filepath.Join(stateDir, "surrogates.age")
	data, _ := os.ReadFile(surPath)
	doc, err := custos.DecryptSurrogates(data, pass)
	if err != nil {
		t.Fatal(err)
	}

	// Row a: unknown lane ("worker" or "oauth2" instead of "bearer")
	doc.Surrogates["sur_unknownlane123456789012"] = custos.SurrogateRecord{
		Token:      "sur_unknownlane123456789012",
		Credential: "realcred",
		Lane:       "oauth2", // unknown lane per §4.3
		Host:       "valid.example.com",
		Ports:      []int{80},
		PathPrefix: "/",
	}

	// Row b: unparseable host ("user@host")
	doc.Surrogates["sur_badhost1234567890123456"] = custos.SurrogateRecord{
		Token:      "sur_badhost1234567890123456",
		Credential: "realcred",
		Lane:       "bearer",
		Host:       "user@badhost.com", // invalid host per §6.3
		Ports:      []int{80},
		PathPrefix: "/",
	}

	// Row c: missing credential in vault
	doc.Surrogates["sur_missingcred123456789012"] = custos.SurrogateRecord{
		Token:      "sur_missingcred123456789012",
		Credential: "nonexistent_cred", // missing from vault per §6.6
		Lane:       "bearer",
		Host:       "valid.example.com",
		Ports:      []int{80},
		PathPrefix: "/",
	}

	if err := custos.WriteSurrogatesFile(stateDir, doc, pass); err != nil {
		t.Fatal(err)
	}

	// Lock and re-unlock to trigger LoadAndReconcileSurrogates
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after corrupt surrogate entries: %v", err)
	}

	// Assert degraded status and surrogates_dropped count = 3
	st = v.Status(false, 0)
	if st.State != custos.StateDegraded {
		t.Errorf("status = %s, want degraded", st.State)
	}
	if st.SurrogatesDropped != 3 {
		t.Errorf("surrogates_dropped = %d, want 3", st.SurrogatesDropped)
	}
	if st.Surrogates != 1 {
		t.Errorf("surrogates = %d, want 1 (only valid row survived)", st.Surrogates)
	}

	// Assert valid surrogate is still loaded and operational
	rec, err := v.Surrogates().LookupAndMatch(validTok, "valid.example.com", "/")
	if err != nil || rec == nil {
		t.Fatalf("valid surrogate lookup failed: %v", err)
	}

	// Assert audit log contains surrogate_rejected for all 3 dropped rows
	logFiles, _ := v.Audit().ListLogFiles()
	rejectedReasons := make(map[string]bool)
	for _, f := range logFiles {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var r custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &r); err == nil {
				if r.Kind == custos.AuditKindSurrogateRejected {
					rejectedReasons[r.Reason] = true
				}
			}
		}
	}

	if !rejectedReasons["unknown_lane"] {
		t.Error("audit missing surrogate_rejected for unknown_lane")
	}
	if !rejectedReasons["invalid_host"] {
		t.Error("audit missing surrogate_rejected for invalid_host")
	}
	if !rejectedReasons["missing_credential"] {
		t.Error("audit missing surrogate_rejected for missing_credential")
	}
}

// Atomic credential-revoke-removes-surrogates:
// - single generation for both mutations
// - one intent/commit pair
// - both audit kinds (credential_removed and surrogate_revoked)
// - all bound surrogates atomically removed from registry
// All per CUSTOS-SPEC §5.3, §4.2.
func TestAtomicCredentialRevokeRemovesSurrogates(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}

	// Seed vault with "gmail" and "openai"
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["gmail"] = custos.Credential{Kind: "api_key", Secret: "sk-gmail"}
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-openai"}
		return []string{"gmail", "openai"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Add 2 surrogates for gmail
	tokG1, err := v.AddSurrogate(pass, "gmail", "mail.google.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	tokG2, err := v.AddSurrogate(pass, "gmail", "gmail.googleapis.com", 80, "/v1/", false, "cli")
	if err != nil {
		t.Fatal(err)
	}

	// Add 1 surrogate for openai
	tokO1, err := v.AddSurrogate(pass, "openai", "api.openai.com", 80, "/v1/", false, "cli")
	if err != nil {
		t.Fatal(err)
	}

	if v.Surrogates().Count() != 3 {
		t.Fatalf("surrogates count = %d, want 3", v.Surrogates().Count())
	}

	genBefore := int64(4) // init=1, mutate=2, sur1=3, sur2=4, sur3=5
	_ = genBefore

	// Atomically revoke credential "gmail"
	if err := v.RevokeCredential(pass, "gmail", "cli"); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}

	// Assert in-memory registry: gmail surrogates removed, openai surrogate intact
	if v.Surrogates().Count() != 1 {
		t.Fatalf("surrogates count = %d, want 1", v.Surrogates().Count())
	}
	if _, ok := v.Surrogates().Lookup(tokG1); ok {
		t.Errorf("tokG1 still present after credential revoke")
	}
	if _, ok := v.Surrogates().Lookup(tokG2); ok {
		t.Errorf("tokG2 still present after credential revoke")
	}
	if _, ok := v.Surrogates().Lookup(tokO1); !ok {
		t.Errorf("tokO1 missing after unrelated credential revoke")
	}

	// Inspect audit log:
	logFiles, err := v.Audit().ListLogFiles()
	if err != nil {
		t.Fatal(err)
	}

	var records []custos.AuditRecord
	for _, f := range logFiles {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				records = append(records, rec)
			}
		}
	}

	// Find the final mutation commit and its events
	var finalCommit *custos.AuditRecord
	var intentGen int64
	var credRemovedGen int64
	var surRevokedGens []int64

	for i := range records {
		r := &records[i]
		switch r.Kind {
		case custos.AuditKindVaultMutation:
			finalCommit = r
		case custos.AuditKindVaultMutationIntent:
			if len(r.Names) > 0 && r.Names[0] == "gmail" {
				intentGen = r.Gen
			}
		case custos.AuditKindCredentialRemoved:
			if r.Cred == "gmail" {
				credRemovedGen = r.Gen
			}
		case custos.AuditKindSurrogateRevoked:
			if r.Cred == "gmail" {
				surRevokedGens = append(surRevokedGens, r.Gen)
			}
		}
	}

	if finalCommit == nil {
		t.Fatal("missing vault_mutation commit line")
	}
	targetGen := finalCommit.Gen

	// Assert single generation
	if intentGen != targetGen {
		t.Errorf("intent gen = %d, want %d", intentGen, targetGen)
	}
	if credRemovedGen != targetGen {
		t.Errorf("credential_removed gen = %d, want %d", credRemovedGen, targetGen)
	}
	if len(surRevokedGens) != 2 {
		t.Fatalf("expected 2 surrogate_revoked audit lines, got %d", len(surRevokedGens))
	}
	for _, g := range surRevokedGens {
		if g != targetGen {
			t.Errorf("surrogate_revoked gen = %d, want %d (single generation)", g, targetGen)
		}
	}

	// Re-lock and unlock to verify persistence on disk
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}

	if v.Surrogates().Count() != 1 {
		t.Fatalf("reloaded surrogates count = %d, want 1", v.Surrogates().Count())
	}
	if _, ok := v.Surrogates().Lookup(tokO1); !ok {
		t.Errorf("reloaded tokO1 missing")
	}
}

// D3 regression test: CUSTOS-SPEC §4.3 token format matches ^sur_[0-9A-Za-z]{22}$, 10k mints in-alphabet and unique.
func TestD3_SurrogateTokenFormat_10kMints(t *testing.T) {
	re := regexp.MustCompile(`^sur_[0-9A-Za-z]{22}$`)
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		tok, err := custos.GenerateSurrogateToken()
		if err != nil {
			t.Fatalf("mint %d failed: %v", i, err)
		}
		if !re.MatchString(tok) {
			t.Fatalf("mint %d: token %q does not match ^sur_[0-9A-Za-z]{22}$", i, tok)
		}
		if seen[tok] {
			t.Fatalf("mint %d: collision on token %q", i, tok)
		}
		seen[tok] = true
	}
}

// D4 regression test: CUSTOS-SPEC §6.6, §8.1: boot reconcile on first unlock passes isRecovery=true
// and emits registry_reconciled for orphan surrogate entries.
func TestD4_FirstUnlockReconcileEmitsRegistryReconciled(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["orphan_cred"] = custos.Credential{Kind: "api_key", Secret: "secret-orphan"}
		return []string{"orphan_cred"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	tok, err := v.AddSurrogate(pass, "orphan_cred", "api.example.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatal(err)
	}

	// Remove credential from vault document directly to leave an orphan surrogate entry in surrogates.age
	err = v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		delete(doc.Credentials, "orphan_cred")
		return []string{"orphan_cred"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh Vault instance representing a new process boot (firstUnlockDone == false)
	bootVault := custos.NewVault(stateDir, 5*time.Second)
	if err := bootVault.Unlock(pass, false); err != nil {
		t.Fatalf("boot unlock: %v", err)
	}

	// Verify that registry_reconciled was emitted in the audit log for the orphan surrogate
	files, _ := bootVault.Audit().ListLogFiles()
	foundReconciled := false
	id8 := custos.SHA256Hex8(tok)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			var rec custos.AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Kind == custos.AuditKindRegistryReconciled && rec.Cred == "orphan_cred" && rec.Sur == id8 {
					foundReconciled = true
				}
			}
		}
	}
	if !foundReconciled {
		t.Errorf("expected audit log to record registry_reconciled for orphan %s", id8)
	}
}

// D6 regression test: CUSTOS-SPEC §6.6 corruption posture: a present-but-undecryptable surrogates.age
// must abort the mutation with the corruption error, leaving file bytes unchanged.
func TestD6_CorruptSurrogatesAbortsMutation(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["mycred"] = custos.Credential{Kind: "api_key", Secret: "mysecret"}
		return []string{"mycred"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	surPath := filepath.Join(stateDir, "surrogates.age")
	corruptBytes := []byte("this-is-corrupt-surrogates-age-payload-not-decryptable")
	if err := os.WriteFile(surPath, corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// AddSurrogate must return an error because surrogates.age is corrupt
	_, err = v.AddSurrogate(pass, "mycred", "api.example.com", 80, "/", false, "cli")
	if err == nil {
		t.Fatal("expected AddSurrogate to fail on corrupt surrogates.age, got nil")
	}

	// Verify file bytes on disk are completely unchanged
	afterBytes, err := os.ReadFile(surPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, corruptBytes) {
		t.Errorf("surrogates.age bytes changed! got %q, want %q", afterBytes, corruptBytes)
	}
}

// D2 regression test: CUSTOS-SPEC §C3: cleartext passphrase is never retained,
// mutations use the retained derived secret when unlocked, and Lock zeroes the derived key.
func TestD2_SecretRetention_DerivedKeyZeroedOnLock(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatal(err)
	}

	// Seed credential
	err := v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["api"] = custos.Credential{Kind: "api_key", Secret: "s3cr3t"}
		return []string{"api"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// While unlocked, AddSurrogate with empty passphrase succeeds using retained derived key (daemon ctl path)
	tok, err := v.AddSurrogate("", "api", "api.example.com", 80, "/", false, "cli")
	if err != nil {
		t.Fatalf("AddSurrogate with empty passphrase failed while unlocked: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}

	// Lock the vault
	if err := v.Lock(); err != nil {
		t.Fatal(err)
	}

	// While locked, AddSurrogate with empty passphrase must fail with ErrCustosLocked
	_, err = v.AddSurrogate("", "api", "api.example.com", 80, "/", false, "cli")
	if !errors.Is(err, custos.ErrCustosLocked) {
		t.Fatalf("AddSurrogate while locked got %v, want ErrCustosLocked", err)
	}
}
