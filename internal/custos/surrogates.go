package custos

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Frozen failure strings and errors for surrogates per CUSTOS-SPEC §5.2, §5.3, §10 V4, V11.
var (
	ErrSurrogateNotFound      = errors.New("surrogate not found")
	ErrSurrogateHostMismatch  = errors.New("surrogate host mismatch")
	ErrSurrogatePortMismatch  = errors.New("surrogate port mismatch")
	ErrSurrogatePathMismatch  = errors.New("surrogate path mismatch")
	ErrSurrogateLaneMismatch  = errors.New("surrogate lane mismatch")
	ErrSurrogateBinaryRefused = errors.New("surrogate binary refused")
)

// SurrogateRecord represents one registered surrogate per CUSTOS-SPEC §4.3, §5.3.
type SurrogateRecord struct {
	Token       string `json:"token"`
	Credential  string `json:"credential"`
	Lane        string `json:"lane"` // "bearer" in v1
	Host        string `json:"host"` // canonical host or IP (brackets stripped)
	Ports       []int  `json:"ports"`
	PathPrefix  string `json:"path_prefix"`
	AllowBinary bool   `json:"allow_binary,omitempty"`
	AddedAt     string `json:"added_at"`
}

// Fingerprint returns the 8-hex sha256_8 prefix of the token per CUSTOS-SPEC §8.1.
// Tokens NEVER appear in audit in cleartext.
func (r *SurrogateRecord) Fingerprint() string {
	return SHA256Hex8(r.Token)
}

// BindingString formats host, port, path for CLI list display per CUSTOS-SPEC §11.
func (r *SurrogateRecord) BindingString() string {
	var portPart string
	if len(r.Ports) > 0 && r.Ports[0] != 80 {
		portPart = fmt.Sprintf(":%d", r.Ports[0])
	}

	pfx := r.PathPrefix
	if pfx == "" {
		pfx = "/"
	}

	var hostWithPort string
	if strings.Contains(r.Host, ":") && portPart != "" {
		hostWithPort = fmt.Sprintf("[%s]%s", r.Host, portPart)
	} else {
		hostWithPort = fmt.Sprintf("%s%s", r.Host, portPart)
	}

	if pfx == "/" {
		if r.AllowBinary {
			return hostWithPort + " (allow-binary)"
		}
		return hostWithPort
	}

	if r.AllowBinary {
		return hostWithPort + pfx + " (allow-binary)"
	}
	return hostWithPort + pfx
}

// MatchesAuthority checks if request host and port match the surrogate's binding per §5.2, §6.3.
func (r *SurrogateRecord) MatchesAuthority(targetHost string, targetPort int) bool {
	if r.Host != targetHost {
		return false
	}

	// Port check: default is 80 if unspecified
	allowedPorts := r.Ports
	if len(allowedPorts) == 0 {
		allowedPorts = []int{80}
	}

	for _, p := range allowedPorts {
		if p == targetPort {
			return true
		}
	}
	return false
}

// MatchesPath checks if request path matches the surrogate's path prefix per §5.2.
// Encoded dot-slices must be decoded and cleaned before match.
func (r *SurrogateRecord) MatchesPath(reqPath string) bool {
	pfx := r.PathPrefix
	if pfx == "" || pfx == "/" {
		return true
	}

	cleaned := path.Clean(reqPath)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}

	// Ensure prefix trailing slash semantics
	if strings.HasSuffix(pfx, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned = cleaned + "/"
	}

	return strings.HasPrefix(cleaned, pfx)
}

// SurrogateDoc represents the JSON document inside surrogates.age per CUSTOS-SPEC §4.3.
type SurrogateDoc struct {
	Version    int                        `json:"version"`
	Surrogates map[string]SurrogateRecord `json:"surrogates"`
}

// SurrogateRegistry manages surrogate records in memory and on disk.
type SurrogateRegistry struct {
	records map[string]SurrogateRecord
}

// NewSurrogateRegistry returns an empty SurrogateRegistry.
func NewSurrogateRegistry() *SurrogateRegistry {
	return &SurrogateRegistry{
		records: make(map[string]SurrogateRecord),
	}
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// GenerateSurrogateToken creates a surrogate token with sur_ prefix followed by
// exactly 22 base62 characters via uniform rejection sampling over crypto/rand
// per CUSTOS-SPEC §4.3 (a byte is acceptable if < 62 * floor(256/62) = 248).
func GenerateSurrogateToken() (string, error) {
	const tokenLen = 22
	const maxAcceptable = 248 // 62 * floor(256/62) = 248; uniform without modulo bias
	out := make([]byte, tokenLen)
	var generated int
	buf := make([]byte, 32)
	for generated < tokenLen {
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return "", fmt.Errorf("generate surrogate token: %w", err)
		}
		for _, b := range buf {
			if b < maxAcceptable {
				out[generated] = base62Alphabet[b%62]
				generated++
				if generated == tokenLen {
					break
				}
			}
		}
	}
	return "sur_" + string(out), nil
}

// DecryptSurrogates decrypts surrogates.age using age scrypt under passphrase per CUSTOS §4.3.
func DecryptSurrogates(ciphertext []byte, passphrase string) (*SurrogateDoc, error) {
	if len(ciphertext) == 0 {
		// Empty placeholder file from vault.Init
		return &SurrogateDoc{
			Version:    1,
			Surrogates: make(map[string]SurrogateRecord),
		}, nil
	}

	data, err := DecryptAge(ciphertext, passphrase)
	if err != nil {
		return nil, err
	}

	// Support both { "version": 1, "surrogates": { ... } } and map[string]SurrogateRecord
	var doc SurrogateDoc
	if err := json.Unmarshal(data, &doc); err == nil && doc.Surrogates != nil {
		return &doc, nil
	}

	var rawMap map[string]SurrogateRecord
	if err := json.Unmarshal(data, &rawMap); err == nil {
		return &SurrogateDoc{
			Version:    1,
			Surrogates: rawMap,
		}, nil
	}

	return nil, errors.New("unmarshal surrogates document failed")
}

// EncryptSurrogates serializes and encrypts SurrogateDoc using age scrypt under passphrase per CUSTOS §4.3.
func EncryptSurrogates(doc *SurrogateDoc, passphrase string) ([]byte, error) {
	if doc == nil {
		doc = &SurrogateDoc{
			Version:    1,
			Surrogates: make(map[string]SurrogateRecord),
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return EncryptAge(b, passphrase)
}

// DecryptSurrogatesWithKey decrypts surrogates.age using the retained derived key per CUSTOS-SPEC §4.3, §C3.
func DecryptSurrogatesWithKey(ciphertext []byte, key []byte) (*SurrogateDoc, error) {
	return DecryptSurrogates(ciphertext, string(key))
}

// EncryptSurrogatesWithKey serializes and encrypts SurrogateDoc using the retained derived key per CUSTOS-SPEC §4.3, §C3.
func EncryptSurrogatesWithKey(doc *SurrogateDoc, key []byte) ([]byte, error) {
	return EncryptSurrogates(doc, string(key))
}

// LoadAndReconcile reads surrogates.age, reconciles with vault credentials, and filters corrupt entries.
// CUSTOS §4.3, §6.6, §10 V11:
// - Unknown lane: dropped with surrogate_rejected, never bearer-defaulted
// - Unparseable host: dropped with surrogate_rejected
// - Credential missing from vault: dropped with surrogate_rejected (or registry_reconciled)
// - Dropped count returned for status degraded reporting
func LoadAndReconcileSurrogates(stateDir, passphrase string, vaultCreds map[string]Credential, audit *AuditLogger, isRecovery bool) (*SurrogateRegistry, int, error) {
	regPath := filepath.Join(stateDir, "surrogates.age")
	data, err := os.ReadFile(regPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewSurrogateRegistry(), 0, nil
		}
		return nil, 0, err
	}

	doc, err := DecryptSurrogates(data, passphrase)
	if err != nil {
		return nil, 0, err
	}

	reg := NewSurrogateRegistry()
	droppedCount := 0
	orphansDropped := false

	for token, rec := range doc.Surrogates {
		rec.Token = token

		// 1. Unknown lane validation per CUSTOS §4.3, §10 V11
		if rec.Lane != "bearer" {
			droppedCount++
			if audit != nil {
				_ = audit.Append(AuditRecord{
					Kind:   AuditKindSurrogateRejected,
					Sur:    rec.Fingerprint(),
					Cred:   rec.Credential,
					Reason: "unknown_lane",
					Actor:  "cli",
				})
			}
			continue
		}

		// 2. Validate host per §6.3
		canonHost, _, err := NormalizeHost(rec.Host)
		if err != nil {
			droppedCount++
			if audit != nil {
				_ = audit.Append(AuditRecord{
					Kind:   AuditKindSurrogateRejected,
					Sur:    rec.Fingerprint(),
					Cred:   rec.Credential,
					Reason: "invalid_host",
					Actor:  "cli",
				})
			}
			continue
		}
		rec.Host = canonHost

		// 3. Validate credential existence in vault per §6.6, §4.2
		if _, exists := vaultCreds[rec.Credential]; !exists {
			droppedCount++
			orphansDropped = true
			if isRecovery {
				if audit != nil {
					_ = audit.Append(AuditRecord{
						Kind:   AuditKindRegistryReconciled,
						Sur:    rec.Fingerprint(),
						Cred:   rec.Credential,
						Reason: "credential_removed",
						Actor:  "cli",
					})
				}
			} else {
				if audit != nil {
					_ = audit.Append(AuditRecord{
						Kind:   AuditKindSurrogateRejected,
						Sur:    rec.Fingerprint(),
						Cred:   rec.Credential,
						Reason: "missing_credential",
						Actor:  "cli",
					})
				}
			}
			continue
		}

		reg.records[token] = rec
	}

	// If orphans were dropped during recovery/load, write back updated file
	if orphansDropped {
		updatedDoc := &SurrogateDoc{
			Version:    1,
			Surrogates: reg.records,
		}
		if enc, err := EncryptSurrogates(updatedDoc, passphrase); err == nil {
			tmp, err := os.CreateTemp(stateDir, "surr-reconcile-*.tmp")
			if err == nil {
				_, _ = tmp.Write(enc)
				_ = tmp.Sync()
				_ = tmp.Close()
				_ = os.Chmod(tmp.Name(), 0o600)
				_ = os.Rename(tmp.Name(), regPath)
				if df, err := os.Open(stateDir); err == nil {
					_ = df.Sync()
					_ = df.Close()
				}
			}
		}
	}

	return reg, droppedCount, nil
}

// WriteSurrogatesFile atomically writes surrogates.age using temp+rename under flock.
func WriteSurrogatesFile(stateDir string, doc *SurrogateDoc, passphrase string) error {
	enc, err := EncryptSurrogates(doc, passphrase)
	if err != nil {
		return fmt.Errorf("encrypt surrogates: %w", err)
	}

	tmp, err := os.CreateTemp(stateDir, "surrogates-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}

	regTarget := filepath.Join(stateDir, "surrogates.age")
	if err := os.Rename(tmp.Name(), regTarget); err != nil {
		return err
	}

	// fsync dir
	if df, err := os.Open(stateDir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// Lookup finds a record by exact token.
func (r *SurrogateRegistry) Lookup(token string) (*SurrogateRecord, bool) {
	if r == nil {
		return nil, false
	}
	rec, ok := r.records[token]
	if !ok {
		return nil, false
	}
	return &rec, true
}

// LookupAndMatch checks exact token, host, port, path per CUSTOS §5.2.
// Forward-time normalized authority is matched per §6.3 canonical rules.
func (r *SurrogateRegistry) LookupAndMatch(token, authority, reqPath string) (*SurrogateRecord, error) {
	rec, ok := r.Lookup(token)
	if !ok {
		return nil, ErrSurrogateNotFound
	}

	if rec.Lane != "bearer" {
		return nil, ErrSurrogateLaneMismatch
	}

	// Normalize request authority
	targetHost, targetPort, _, err := NormalizeAuthority(authority)
	if err != nil {
		return nil, ErrInvalidPattern
	}

	if !rec.MatchesAuthority(targetHost, targetPort) {
		if rec.Host != targetHost {
			return nil, ErrSurrogateHostMismatch
		}
		return nil, ErrSurrogatePortMismatch
	}

	if !rec.MatchesPath(reqPath) {
		return nil, ErrSurrogatePathMismatch
	}

	return rec, nil
}

// List returns all surrogate records sorted by credential name and added_at.
func (r *SurrogateRegistry) List() []SurrogateRecord {
	if r == nil {
		return nil
	}
	var out []SurrogateRecord
	for _, rec := range r.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credential != out[j].Credential {
			return out[i].Credential < out[j].Credential
		}
		return out[i].Fingerprint() < out[j].Fingerprint()
	})
	return out
}

// FindByFingerprint finds a surrogate by its 8-hex fingerprint.
func (r *SurrogateRegistry) FindByFingerprint(id8 string) (*SurrogateRecord, bool) {
	if r == nil {
		return nil, false
	}
	id8 = strings.ToLower(id8)
	for _, rec := range r.records {
		if strings.EqualFold(rec.Fingerprint(), id8) {
			return &rec, true
		}
	}
	return nil, false
}

// Count returns the number of registered surrogates.
func (r *SurrogateRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.records)
}

// ValidateSurrogateBinding validates host, port, and path for surrogate issuance per CUSTOS §5.3, §6.3, §6.5.
func ValidateSurrogateBinding(host string, port int, pfx string) (canonHost string, ports []int, canonPfx string, err error) {
	if host == "" {
		return "", nil, "", fmt.Errorf("%w: empty host", ErrInvalidPattern)
	}

	// Refuse uppercase host at write per §6.3
	for i := 0; i < len(host); i++ {
		if host[i] >= 'A' && host[i] <= 'Z' {
			return "", nil, "", fmt.Errorf("%w: uppercase host", ErrInvalidPattern)
		}
	}

	// Normalize host (brackets stripped from IPv6)
	h, isIP, err := NormalizeHost(host)
	if err != nil {
		return "", nil, "", err
	}

	// Floor check (unsatisfiable binding per §6.5)
	if isIP {
		addr, err := netip.ParseAddr(h)
		if err == nil && IsFloorAddress(addr) {
			return "", nil, "", ErrUnsatisfiableBinding
		}
	} else {
		if h == "localhost" || strings.HasSuffix(h, ".localhost") {
			return "", nil, "", ErrUnsatisfiableBinding
		}
	}

	// Port grammar: 1-65535 per §5.3. 0, out of range, non-numeric refused at issuance.
	if port < 1 || port > 65535 {
		return "", nil, "", fmt.Errorf("%w: invalid port %d", ErrInvalidPattern, port)
	}

	// Path prefix default "/"
	if pfx == "" {
		pfx = "/"
	}
	if strings.Contains(pfx, "..") {
		return "", nil, "", fmt.Errorf("%w: path prefix cannot contain ..", ErrInvalidPattern)
	}
	if !strings.HasPrefix(pfx, "/") {
		pfx = "/" + pfx
	}
	pfx = path.Clean(pfx)
	if !strings.HasSuffix(pfx, "/") {
		pfx = pfx + "/"
	}

	return h, []int{port}, pfx, nil
}
