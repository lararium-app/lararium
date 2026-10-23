package custos_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestM12_FingerprintsMACVerification tests that ReadFingerprints verifies the
// sidecar's MAC per CUSTOS-SPEC §4.6 (M12):
// - good mac passes.
// - flipped byte fails with ErrFingerprintsMAC.
// - absent file still returns empty map + nil.
func TestM12_FingerprintsMACVerification(t *testing.T) {
	stateDir := t.TempDir()

	// 1. Absent file returns empty map + nil
	fp, err := custos.ReadFingerprints(stateDir)
	if err != nil {
		t.Fatalf("absent file failed: %v", err)
	}
	if len(fp) != 0 {
		t.Fatalf("expected empty map for absent file, got %v", fp)
	}

	// 2. Good MAC passes
	instanceKey := make([]byte, 32)
	for i := range instanceKey {
		instanceKey[i] = byte(i + 1)
	}
	keyPath := filepath.Join(stateDir, "vault.key")
	if err := os.WriteFile(keyPath, instanceKey, 0o600); err != nil {
		t.Fatal(err)
	}

	creds := map[string]custos.Credential{
		"openai":    {Kind: "api_key", Secret: "dummy-token-1"},
		"anthropic": {Kind: "api_key", Secret: "dummy-token-2"}, //nolint:gosec // test cred
	}
	if err := custos.WriteFingerprints(stateDir, instanceKey, creds); err != nil {
		t.Fatalf("WriteFingerprints: %v", err)
	}

	fp, err = custos.ReadFingerprints(stateDir)
	if err != nil {
		t.Fatalf("good MAC failed: %v", err)
	}
	if len(fp.Names()) != 2 {
		t.Fatalf("expected 2 credential names, got %d", len(fp.Names()))
	}
	if fp.Sha256Hex8("openai") == "" || fp.Sha256Hex8("anthropic") == "" {
		t.Fatalf("missing expected credential hashes in mirror: %+v", fp)
	}

	// 3. Flipped byte in MAC fails with ErrFingerprintsMAC
	fpPath := custos.FingerprintsPath(stateDir)
	data, err := os.ReadFile(fpPath)
	if err != nil {
		t.Fatal(err)
	}
	var rawMap map[string]string
	if err := json.Unmarshal(data, &rawMap); err != nil {
		t.Fatal(err)
	}

	origMAC := rawMap["mac"]
	// Flip first hex character
	flippedFirst := 'a'
	if origMAC[0] == 'a' {
		flippedFirst = 'b'
	}
	rawMap["mac"] = string(flippedFirst) + origMAC[1:]

	tamperedJSON, err := json.Marshal(rawMap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpPath, tamperedJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = custos.ReadFingerprints(stateDir)
	if err == nil {
		t.Fatal("ReadFingerprints succeeded on tampered MAC")
	}
	if !errors.Is(err, custos.ErrFingerprintsMAC) {
		t.Fatalf("expected ErrFingerprintsMAC, got %v", err)
	}

	// 4. Flipped byte in a credential hash fails with ErrFingerprintsMAC
	rawMap["mac"] = origMAC
	origHash := rawMap["openai"]
	flippedHashFirst := '0'
	if origHash[0] == '0' {
		flippedHashFirst = '1'
	}
	rawMap["openai"] = string(flippedHashFirst) + origHash[1:]

	tamperedHashJSON, err := json.Marshal(rawMap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpPath, tamperedHashJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = custos.ReadFingerprints(stateDir)
	if err == nil {
		t.Fatal("ReadFingerprints succeeded on tampered payload hash")
	}
	if !errors.Is(err, custos.ErrFingerprintsMAC) {
		t.Fatalf("expected ErrFingerprintsMAC on tampered hash, got %v", err)
	}
}
