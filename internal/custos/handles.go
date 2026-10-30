package custos

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Credential handles (CREDHANDLE-SPEC R1, R5): an opaque grant record that
// wraps an existing vault credential NAME plus a scope pattern, optional
// expiry and optional rate limit. The name stays the policy identity; handles
// never appear in policy rules. Records live inside the age-encrypted VaultDoc
// so the one mutation protocol (MutateWithRegistry) covers them.

// handlePrefix distinguishes handle tokens from surrogate tokens (sur_).
const handlePrefix = "hdl_"

// handleTokenLen is the number of base62 characters after the prefix.
const handleTokenLen = 22

// Handle denial reasons (closed set).
const (
	HandleDenyUnknown  = "unknown"
	HandleDenyExpired  = "expired"
	HandleDenyLocked   = "locked"
	HandleDenyNoCred   = "no_credential" //nolint:gosec // denial reason, not a credential
	HandleDenyBadScope = "bad_scope"
	HandleDenyRate     = "rate"
)

// Handle lookup and validation errors.
var (
	ErrHandleNotFound  = errors.New("handle not found")
	ErrHandleAmbiguous = errors.New("handle fingerprint is ambiguous")
	ErrHandleInvalid   = errors.New("invalid handle parameters")
)

// HandleRate is a per-handle token-bucket limit: N uses per Window seconds.
type HandleRate struct {
	N      int `json:"n"`
	Window int `json:"window_seconds"`
}

// HandleRecord is one credential handle grant.
type HandleRecord struct {
	Handle  string      `json:"handle"`  // opaque token, generated
	Name    string      `json:"name"`    // vault credential name
	Scope   string      `json:"scope"`   // policy credential-pattern grammar
	Created string      `json:"created"` // RFC3339 UTC
	Expires string      `json:"expires"` // RFC3339 UTC; "" = never
	Rate    *HandleRate `json:"rate,omitempty"`
	// Last4 is display-only (last 4 chars of the secret at create time).
	// It must never be written to the audit log.
	Last4 string `json:"last4"`
}

// Fingerprint returns the 8-hex sha256_8 prefix of the handle token, same
// construction as SurrogateRecord.Fingerprint. Raw tokens never reach audit.
func (r HandleRecord) Fingerprint() string {
	return SHA256Hex8(r.Handle)
}

// HandleGrant is the outcome of ResolveHandle.
type HandleGrant struct {
	Record HandleRecord
	Secret string // resolved secret; zero it at call-site when done
	Deny   string // "" on success, else one HandleDeny* value
}

// handleBuckets holds in-memory rate state per handle. Buckets reset on daemon
// restart (acceptable: the limit is a runaway guard, not a quota ledger).
// Accounting is per fixed window: N uses per full window, refilled when the
// window has elapsed. The mutex guards map access only; no I/O is performed
// while it is held.
type handleBuckets struct {
	mu sync.Mutex
	m  map[string]*handleBucket
}

type handleBucket struct {
	windowStart time.Time
	used        int
}

// take consumes one token for handle under rate at now; false when exhausted.
func (b *handleBuckets) take(handle string, rate HandleRate, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = make(map[string]*handleBucket)
	}
	bk, ok := b.m[handle]
	window := time.Duration(rate.Window) * time.Second
	if !ok || now.Sub(bk.windowStart) >= window {
		bk = &handleBucket{windowStart: now}
		b.m[handle] = bk
	}
	if bk.used >= rate.N {
		return false
	}
	bk.used++
	return true
}

// forget drops a handle's bucket (on revoke).
func (b *handleBuckets) forget(handle string) {
	b.mu.Lock()
	delete(b.m, handle)
	b.mu.Unlock()
}

// GenerateHandleToken creates a handle token: hdl_ + 22 base62 characters,
// using the same rejection sampling as GenerateSurrogateToken.
func GenerateHandleToken() (string, error) {
	body, err := generateBase62(handleTokenLen)
	if err != nil {
		return "", fmt.Errorf("generate handle token: %w", err)
	}
	return handlePrefix + body, nil
}

// credentialSecretValue picks the secret a handle resolves to.
func credentialSecretValue(c Credential) string {
	if c.Kind == "oauth2" && c.AccessToken != "" {
		return c.AccessToken
	}
	return c.Secret
}

func last4Of(s string) string {
	if len(s) <= 4 {
		// Short secrets would be fully revealed; show nothing.
		return ""
	}
	return s[len(s)-4:]
}

// findHandleConstTime looks a token up with a constant-time compare over all
// records (no early exit), mirroring the surrogate lookup posture.
func findHandleConstTime(m map[string]HandleRecord, token string) (HandleRecord, bool) {
	var found HandleRecord
	ok := 0
	for k, r := range m {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			found = r
			ok = 1
		}
	}
	return found, ok == 1
}

// CreateHandle mints a handle over an existing credential name.
func (v *Vault) CreateHandle(passphrase, name, scope string, ttl time.Duration, rate *HandleRate, actor string) (HandleRecord, error) {
	v.mu.RLock()
	hasKey := len(v.derivedKey) > 0
	isUnlocked := v.isUnlocked
	v.mu.RUnlock()
	if !isUnlocked && passphrase == "" && !hasKey {
		return HandleRecord{}, ErrCustosLocked
	}
	if name == "" {
		return HandleRecord{}, fmt.Errorf("%w: empty credential name", ErrHandleInvalid)
	}
	if scope == "" {
		scope = name
	}
	parsed, err := ValidateCredentialPattern(scope, VerdictAuto, false)
	if err != nil {
		return HandleRecord{}, fmt.Errorf("invalid scope %q: %w", scope, err)
	}
	if parsed.Name != name {
		return HandleRecord{}, fmt.Errorf("%w: scope %q does not name credential %q", ErrHandleInvalid, scope, name)
	}
	if rate != nil {
		if rate.N <= 0 || rate.Window <= 0 {
			return HandleRecord{}, fmt.Errorf("%w: rate needs n>0 and window>0", ErrHandleInvalid)
		}
		rc := *rate
		rate = &rc
	}
	token, err := GenerateHandleToken()
	if err != nil {
		return HandleRecord{}, err
	}

	var rec HandleRecord
	err = v.MutateWithRegistry(passphrase, func(doc *VaultDoc, _ *SurrogateDoc) ([]string, []AuditRecord, error) {
		cred, exists := doc.Credentials[name]
		if !exists {
			return nil, nil, fmt.Errorf("credential %q not found", name)
		}
		now := time.Now().UTC()
		rec = HandleRecord{
			Handle:  token,
			Name:    name,
			Scope:   parsed.Raw,
			Created: now.Format(time.RFC3339),
			Rate:    rate,
			Last4:   last4Of(credentialSecretValue(cred)),
		}
		if ttl > 0 {
			rec.Expires = now.Add(ttl).Format(time.RFC3339)
		}
		if doc.Handles == nil {
			doc.Handles = make(map[string]HandleRecord)
		}
		doc.Handles[token] = rec
		ev := AuditRecord{
			Kind:  AuditKindHandleCreated,
			Cred:  name,
			Sur:   rec.Fingerprint(),
			Actor: actor,
			Via:   "handle",
		}
		return []string{name}, []AuditRecord{ev}, nil
	}, false)
	if err != nil {
		return HandleRecord{}, err
	}
	return rec, nil
}

// ListHandles returns all handle records sorted by Created (then fingerprint).
// Last4 is blanked in the returned copies.
func (v *Vault) ListHandles() ([]HandleRecord, error) {
	v.mu.RLock()
	if !v.isUnlocked || v.loadedDoc == nil {
		v.mu.RUnlock()
		return nil, ErrCustosLocked
	}
	out := make([]HandleRecord, 0, len(v.loadedDoc.Handles))
	for _, r := range v.loadedDoc.Handles {
		r.Last4 = ""
		out = append(out, r)
	}
	v.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].Fingerprint() < out[j].Fingerprint()
	})
	return out, nil
}

// FindHandle resolves an 8-hex fingerprint (unique match) or a raw handle to
// the full handle token. Ambiguity yields ErrHandleAmbiguous.
func (v *Vault) FindHandle(fingerprintOrHandle string) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.isUnlocked || v.loadedDoc == nil {
		return "", ErrCustosLocked
	}
	var matches []string
	for tok, r := range v.loadedDoc.Handles {
		if r.Fingerprint() == fingerprintOrHandle {
			matches = append(matches, tok)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
	default:
		return "", ErrHandleAmbiguous
	}
	if r, ok := findHandleConstTime(v.loadedDoc.Handles, fingerprintOrHandle); ok {
		return r.Handle, nil
	}
	return "", ErrHandleNotFound
}

func (v *Vault) auditHandleDenial(rec HandleRecord, presented, reason string) {
	kind := AuditKindHandleDeniedScope
	if reason == HandleDenyRate {
		kind = AuditKindHandleDeniedRate
	}
	fp := rec.Fingerprint()
	if rec.Handle == "" {
		fp = SHA256Hex8(presented)
	}
	// Closed-set mapping: unknown/expired/no_credential/bad_scope share the
	// scope-denial kind; Reason carries the specific denial value.
	_ = v.audit.Append(AuditRecord{
		Kind:   kind,
		Cred:   rec.Name,
		Sur:    fp,
		Actor:  "handle",
		Via:    "handle",
		Reason: reason,
	})
}

// ResolveHandle resolves a handle for a call. toolName != "" selects the
// worker-lane comparator; otherwise host/port select the bearer comparator.
func (v *Vault) ResolveHandle(handle, toolName string, host string, port int) HandleGrant {
	v.mu.RLock()
	if !v.isUnlocked || v.loadedDoc == nil {
		v.mu.RUnlock()
		// Locked is deliberately not audited as a handle event.
		return HandleGrant{Deny: HandleDenyLocked}
	}
	rec, found := findHandleConstTime(v.loadedDoc.Handles, handle)
	var cred Credential
	var credOK bool
	if found {
		cred, credOK = v.loadedDoc.Credentials[rec.Name]
	}
	v.mu.RUnlock()

	deny := func(reason string) HandleGrant {
		v.auditHandleDenial(rec, handle, reason)
		return HandleGrant{Record: redactHandle(rec), Deny: reason}
	}
	if !found {
		return deny(HandleDenyUnknown)
	}
	now := time.Now().UTC()
	if rec.Expires != "" {
		exp, err := time.Parse(time.RFC3339, rec.Expires)
		if err != nil || !now.Before(exp) {
			return deny(HandleDenyExpired)
		}
	}
	if !credOK {
		return deny(HandleDenyNoCred)
	}
	parsed, err := ValidateCredentialPattern(rec.Scope, VerdictAuto, false)
	if err != nil {
		return deny(HandleDenyBadScope)
	}
	var covered bool
	switch {
	case toolName != "":
		covered = parsed.MatchesWorkerCall(rec.Name, toolName)
	case host != "":
		covered = parsed.MatchesBearerAt(rec.Name, host, port)
	}
	if !covered {
		return deny(HandleDenyBadScope)
	}
	if rec.Rate != nil && !v.handleBuckets.take(rec.Handle, *rec.Rate, now) {
		return deny(HandleDenyRate)
	}

	_ = v.audit.Append(AuditRecord{
		Kind:  AuditKindHandleUsed,
		Cred:  rec.Name,
		Sur:   rec.Fingerprint(),
		Actor: "handle",
		Via:   "handle",
		Host:  host,
		Tool:  toolName,
	})
	return HandleGrant{Record: rec, Secret: credentialSecretValue(cred)}
}

// redactHandle strips display-only data from a record echoed on denial.
func redactHandle(r HandleRecord) HandleRecord {
	r.Last4 = ""
	return r
}

// RevokeHandle deletes a handle by its raw token.
func (v *Vault) RevokeHandle(passphrase, handle, actor string) error {
	v.mu.RLock()
	hasKey := len(v.derivedKey) > 0
	isUnlocked := v.isUnlocked
	v.mu.RUnlock()
	if !isUnlocked && passphrase == "" && !hasKey {
		return ErrCustosLocked
	}
	err := v.MutateWithRegistry(passphrase, func(doc *VaultDoc, _ *SurrogateDoc) ([]string, []AuditRecord, error) {
		rec, ok := findHandleConstTime(doc.Handles, handle)
		if !ok {
			return nil, nil, ErrHandleNotFound
		}
		delete(doc.Handles, rec.Handle)
		ev := AuditRecord{
			Kind:  AuditKindHandleRevoked,
			Cred:  rec.Name,
			Sur:   rec.Fingerprint(),
			Actor: actor,
			Via:   "handle",
		}
		return []string{rec.Name}, []AuditRecord{ev}, nil
	}, false)
	if err != nil {
		return err
	}
	v.handleBuckets.forget(handle)
	return nil
}
