package custos

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
	"golang.org/x/crypto/hkdf"
	"os"
	"unsafe"
)

// HKDF info labels per CUSTOS-SPEC §4.1, §8.3.
const (
	HKDFInfoCustosMac      = "custos-mac"      // Deprecated: superseded by HKDFInfoCustosEnv per CUSTOS-SPEC §4.1
	HKDFInfoCustosEnv      = "custos-env"      // CUSTOS-SPEC §4.1: vault.age.mac
	HKDFInfoCustosEnvelope = "custos-envelope" // Deprecated
	HKDFInfoCustosAnchor   = "custos-anchor"   // CUSTOS-SPEC §8.3: anchor MAC
)

// ageWorkFactor allows tests to lower scrypt work factor for execution speed.
// Default 0 uses age's default work factor (18 = 2^18).
var ageWorkFactor = 0

// SetTestAgeWorkFactor sets scrypt work factor logN for testing.
func SetTestAgeWorkFactor(logN int) {
	ageWorkFactor = logN
}

// GenerateInstanceKey creates 32 random bytes for vault.key per CUSTOS §4.1.
func GenerateInstanceKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate instance key: %w", err)
	}
	return key, nil
}

// DeriveHKDF expands secret using HKDF-SHA256 with the given info label.
func DeriveHKDF(secret []byte, info string) []byte {
	r := hkdf.New(sha256.New, secret, nil, []byte(info))
	out := make([]byte, 32)
	if _, err := io.ReadFull(r, out); err != nil {
		panic(fmt.Sprintf("hkdf read: %v", err))
	}
	return out
}

// ComputeMAC returns the hex-encoded HMAC-SHA256 over data using key.
func ComputeMAC(key []byte, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyMAC constant-time verifies whether expectedHex matches HMAC-SHA256 over data.
func VerifyMAC(key []byte, data []byte, expectedHex string) bool {
	expected, err := hex.DecodeString(expectedHex)
	if err != nil || len(expected) != 32 {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	actual := mac.Sum(nil)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// ComputeEnvelopeMAC generates vault.age.mac under HKDF(vault.key, "custos-env") per CUSTOS-SPEC §4.1.
func ComputeEnvelopeMAC(instanceKey []byte, envelopeBytes []byte) string {
	// CUSTOS-SPEC §4.1: HMAC-SHA256 over envelope bytes under HKDF(vault.key, "custos-env")
	macKey := DeriveHKDF(instanceKey, HKDFInfoCustosEnv)
	return ComputeMAC(macKey, envelopeBytes)
}

// VerifyEnvelopeMAC verifies vault.age.mac under HKDF(vault.key, "custos-env") per CUSTOS-SPEC §4.1.
// Accepts ONLY "custos-env" (no dual-label leniency).
func VerifyEnvelopeMAC(instanceKey []byte, envelopeBytes []byte, expectedMAC string) bool {
	macKey := DeriveHKDF(instanceKey, HKDFInfoCustosEnv)
	return VerifyMAC(macKey, envelopeBytes, expectedMAC)
}

// EncryptAge encrypts plaintext under passphrase using age scrypt recipient per CUSTOS §4.1.
func EncryptAge(plaintext []byte, passphrase string) ([]byte, error) {
	// CUSTOS §4.1: scrypt parameters are fixed versioned constants
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("age scrypt recipient: %w", err)
	}
	if ageWorkFactor > 0 {
		r.SetWorkFactor(ageWorkFactor)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age close: %w", err)
	}
	return buf.Bytes(), nil
}

// DecryptAge decrypts age ciphertext under passphrase using age scrypt identity per CUSTOS §4.1.
func DecryptAge(ciphertext []byte, passphrase string) ([]byte, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("age scrypt identity: %w", err)
	}
	if ageWorkFactor > 0 {
		id.SetMaxWorkFactor(ageWorkFactor)
	}

	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		var noIDMatch *age.NoIdentityMatchError
		if errors.Is(err, age.ErrIncorrectIdentity) || errors.As(err, &noIDMatch) {
			return nil, ErrIncorrectPassphrase
		}
		return nil, fmt.Errorf("age decrypt: %w", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		var noIDMatch *age.NoIdentityMatchError
		if errors.Is(err, age.ErrIncorrectIdentity) || errors.As(err, &noIDMatch) {
			return nil, ErrIncorrectPassphrase
		}
		return nil, fmt.Errorf("age read: %w", err)
	}
	return out, nil
}

// EncryptAgeWithKey encrypts plaintext using the retained derived secret key per CUSTOS-SPEC §4.1, §C3.
func EncryptAgeWithKey(plaintext []byte, key []byte) ([]byte, error) {
	return EncryptAge(plaintext, string(key))
}

// DecryptAgeWithKey decrypts age ciphertext using the retained derived secret key per CUSTOS-SPEC §4.1, §C3.
func DecryptAgeWithKey(ciphertext []byte, key []byte) ([]byte, error) {
	return DecryptAge(ciphertext, string(key))
}

var devZero *os.File

func init() {
	if f, err := os.Open("/dev/zero"); err == nil {
		devZero = f
	}
}

// zeroString attempts best-effort overwrite of the underlying bytes of a string per CUSTOS-SPEC §C3.
// If /dev/zero is unavailable the probe cannot run, so we never write blindly (a read-only
// string page would be a fatal SIGSEGV): the zeroing is skipped instead.
func zeroString(s string) {
	if len(s) == 0 || devZero == nil {
		return
	}
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	// Probe writability using /dev/zero read to avoid fatal SIGSEGV on rodata
	if _, err := devZero.Read(b[:1]); err != nil {
		return // memory is not writable (e.g. rodata string constant)
	}
	for i := range b {
		b[i] = 0
	}
}

// zeroBytes overwrites a byte slice with zeros per CUSTOS-SPEC §C3.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// SHA256Hex8 returns the first 8 hex characters of the SHA-256 hash of value.
func SHA256Hex8(val string) string {
	sum := sha256.Sum256([]byte(val))
	return hex.EncodeToString(sum[:])[:8]
}
