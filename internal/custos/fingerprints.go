package custos

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrFingerprintsMAC indicates that fingerprints.json exists with an invalid or tampered MAC (M12).
var ErrFingerprintsMAC = errors.New("fingerprints mac verification failed: tamper detected")

// Fingerprints represents the sidecar mirror in fingerprints.json per CUSTOS-SPEC §4.6.
// Maps credential names to 8-hex SHA-256 prefixes, plus a "mac" entry.
type Fingerprints map[string]string

// Names returns all credential names present in the mirror, excluding the "mac" key.
func (f Fingerprints) Names() []string {
	names := make([]string, 0, len(f))
	for k := range f {
		if k == "mac" {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Sha256Hex8 returns the 8-hex hash for name, or "" if not present.
func (f Fingerprints) Sha256Hex8(name string) string {
	if name == "mac" {
		return ""
	}
	return f[name]
}

// FingerprintsPath returns the path to fingerprints.json in stateDir.
func FingerprintsPath(stateDir string) string {
	return filepath.Join(stateDir, "fingerprints.json")
}

// ReadFingerprints reads and verifies the fingerprints sidecar from stateDir.
// If the file does not exist, an empty Fingerprints map is returned without error.
// If the file exists but has an invalid MAC, ErrFingerprintsMAC is returned (M12).
func ReadFingerprints(stateDir string, instanceKey ...[]byte) (Fingerprints, error) {
	path := FingerprintsPath(stateDir)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Fingerprints{}, nil
		}
		return nil, fmt.Errorf("read fingerprints: %w", err)
	}

	var key []byte
	if len(instanceKey) > 0 && len(instanceKey[0]) > 0 {
		key = instanceKey[0]
	} else {
		keyPath := filepath.Join(stateDir, "vault.key")
		k, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read vault.key for fingerprints: %w", err)
		}
		if len(k) != 32 {
			return nil, fmt.Errorf("invalid vault.key size: %d", len(k))
		}
		key = k
	}

	var m Fingerprints
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse fingerprints: %w", err)
	}

	if !VerifyFingerprintsMAC(key, m) {
		return nil, ErrFingerprintsMAC
	}

	return m, nil
}

// VerifyFingerprintsMAC checks if the "mac" entry in m matches HMAC under HKDF(instanceKey, "custos-fingerprints").
func VerifyFingerprintsMAC(instanceKey []byte, m map[string]string) bool {
	expectedMAC, ok := m["mac"]
	if !ok || expectedMAC == "" {
		return false
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "mac" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(":")
		sb.WriteString(m[k])
		sb.WriteString("\n")
	}

	macKey := DeriveHKDF(instanceKey, "custos-fingerprints")
	return VerifyMAC(macKey, []byte(sb.String()), expectedMAC)
}

// ComputeFingerprintsMAC calculates the MAC over sorted name:hash pairs using instanceKey.
func ComputeFingerprintsMAC(instanceKey []byte, m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "mac" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(":")
		sb.WriteString(m[k])
		sb.WriteString("\n")
	}

	macKey := DeriveHKDF(instanceKey, "custos-fingerprints")
	return ComputeMAC(macKey, []byte(sb.String()))
}

// WriteFingerprints atomically writes fingerprints.json in stateDir under 0600.
// CUSTOS §4.6: updated atomically inside every vault mutation.
func WriteFingerprints(stateDir string, instanceKey []byte, creds map[string]Credential) error {
	m := Fingerprints{}
	for name, cred := range creds {
		secret := cred.Secret
		if secret == "" && cred.Kind == "oauth2" {
			secret = cred.RefreshToken
			if secret == "" {
				secret = cred.ClientID
			}
		}
		m[name] = SHA256Hex8(secret)
	}

	if instanceKey != nil {
		m["mac"] = ComputeFingerprintsMAC(instanceKey, m)
	}

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fingerprints: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(stateDir, "fingerprints-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp fingerprints: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp fingerprints: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp fingerprints: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp fingerprints: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp fingerprints: %w", err)
	}

	target := FingerprintsPath(stateDir)
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename fingerprints: %w", err)
	}
	return nil
}
