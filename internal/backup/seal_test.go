package backup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestSeal_IsSealed_NoByteLoss(t *testing.T) {
	// 1. Empty stream
	r1, sealed, err := IsSealed(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("IsSealed(empty): %v", err)
	}
	if sealed {
		t.Fatal("empty stream reported as sealed")
	}
	b1, _ := io.ReadAll(r1)
	if len(b1) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(b1))
	}

	// 2. Short stream (< len(sealMagic))
	shortInput := []byte("short")
	r2, sealed, err := IsSealed(bytes.NewReader(shortInput))
	if err != nil {
		t.Fatalf("IsSealed(short): %v", err)
	}
	if sealed {
		t.Fatal("short stream reported as sealed")
	}
	b2, _ := io.ReadAll(r2)
	if !bytes.Equal(b2, shortInput) {
		t.Fatalf("byte loss on short stream: got %q, want %q", b2, shortInput)
	}

	// 3. Unsealed stream (zip magic PK\x03\x04 + extra data)
	zipInput := []byte("PK\x03\x04restofzipfilebyteshere")
	r3, sealed, err := IsSealed(bytes.NewReader(zipInput))
	if err != nil {
		t.Fatalf("IsSealed(zip): %v", err)
	}
	if sealed {
		t.Fatal("zip stream reported as sealed")
	}
	b3, _ := io.ReadAll(r3)
	if !bytes.Equal(b3, zipInput) {
		t.Fatalf("byte loss on zip stream: got %q, want %q", b3, zipInput)
	}

	// 4. Sealed stream
	sealedInput := append([]byte{}, sealMagic...)
	sealedInput = append(sealedInput, []byte("\nrest of age encrypted file")...)
	r4, sealed, err := IsSealed(bytes.NewReader(sealedInput))
	if err != nil {
		t.Fatalf("IsSealed(sealed): %v", err)
	}
	if !sealed {
		t.Fatal("sealed stream reported as unsealed")
	}
	b4, _ := io.ReadAll(r4)
	if !bytes.Equal(b4, sealedInput) {
		t.Fatalf("byte loss on sealed stream: got %q, want %q", b4, sealedInput)
	}
}

func TestSeal_RoundTrip_X25519(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	recipient := id.Recipient()

	plaintext := []byte("hello sealed backup world 1234567890")
	var buf bytes.Buffer

	w, err := Seal(&buf, []age.Recipient{recipient})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close seal writer: %v", err)
	}

	// Verify magic at offset 0
	if !bytes.HasPrefix(buf.Bytes(), sealMagic) {
		t.Fatalf("sealed bytes do not start with sealMagic")
	}
	if bytes.HasPrefix(buf.Bytes(), []byte("PK\x03\x04")) {
		t.Fatal("sealed bytes contain PK header")
	}

	// Unseal with right identity
	decReader, err := Unseal(bytes.NewReader(buf.Bytes()), []age.Identity{id})
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	decBytes, err := io.ReadAll(decReader)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(decBytes, plaintext) {
		t.Fatalf("decrypted bytes mismatch: got %q, want %q", decBytes, plaintext)
	}

	// Unseal with wrong identity -> refusal
	wrongId, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate wrong identity: %v", err)
	}
	_, err = Unseal(bytes.NewReader(buf.Bytes()), []age.Identity{wrongId})
	if err == nil {
		t.Fatal("expected refusal with wrong identity, got nil error")
	}
}

func TestSeal_RoundTrip_Scrypt(t *testing.T) {
	passphrase := "my-secret-passphrase-2026"
	recipient, err := NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}

	plaintext := []byte("confidential backup payload")
	var buf bytes.Buffer

	w, err := Seal(&buf, []age.Recipient{recipient})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close seal writer: %v", err)
	}

	// Unseal with correct passphrase
	id, err := NewScryptIdentity(passphrase)
	if err != nil {
		t.Fatalf("NewScryptIdentity: %v", err)
	}
	decReader, err := Unseal(bytes.NewReader(buf.Bytes()), []age.Identity{id})
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	decBytes, err := io.ReadAll(decReader)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(decBytes, plaintext) {
		t.Fatalf("decrypted mismatch: got %q, want %q", decBytes, plaintext)
	}

	// Unseal with wrong passphrase -> refusal
	wrongId, _ := NewScryptIdentity("wrong-password")
	_, err = Unseal(bytes.NewReader(buf.Bytes()), []age.Identity{wrongId})
	if err == nil {
		t.Fatal("expected refusal with wrong password, got nil error")
	}
}

func TestSeal_ParseRecipient(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipStr := id.Recipient().String()

	r, err := ParseRecipient(recipStr)
	if err != nil {
		t.Fatalf("ParseRecipient(%s): %v", recipStr, err)
	}
	if r == nil {
		t.Fatal("ParseRecipient returned nil")
	}

	// Invalid prefix
	if _, err := ParseRecipient("not-an-age-key"); err == nil {
		t.Fatal("expected error for non-age recipient string")
	}

	// Parse multiple
	recips, err := ParseRecipients([]string{recipStr, "   "})
	if err != nil {
		t.Fatalf("ParseRecipients: %v", err)
	}
	if len(recips) != 1 {
		t.Fatalf("expected 1 recipient, got %d", len(recips))
	}
}

func TestSeal_LoadIdentityFile_Permissions(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	idPath := filepath.Join(tmpDir, "test.key")
	if err := os.WriteFile(idPath, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Mode 0600 should succeed
	ids, err := LoadIdentityFile(idPath)
	if err != nil {
		t.Fatalf("LoadIdentityFile(0600): %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(ids))
	}

	// Mode 0644 must be refused per §4.5.2a
	if err := os.Chmod(idPath, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadIdentityFile(idPath)
	if err == nil || !strings.Contains(err.Error(), "group/other-readable") {
		t.Fatalf("expected group/other-readable refusal for 0644, got %v", err)
	}

	// Mode 0666 must be refused
	if err := os.Chmod(idPath, 0o666); err != nil {
		t.Fatal(err)
	}
	_, err = LoadIdentityFile(idPath)
	if err == nil || !strings.Contains(err.Error(), "group/other-readable") {
		t.Fatalf("expected group/other-readable refusal for 0666, got %v", err)
	}
}

func TestSeal_PromptPassphrase_Stdin(t *testing.T) {
	var stderr bytes.Buffer
	stdin := strings.NewReader("secret-pass-from-pipe\n")

	pass, err := PromptPassphrase("Prompt: ", stdin, &stderr)
	if err != nil {
		t.Fatalf("PromptPassphrase: %v", err)
	}
	if pass != "secret-pass-from-pipe" {
		t.Fatalf("passphrase = %q, want 'secret-pass-from-pipe'", pass)
	}
	if !strings.Contains(stderr.String(), "warning: reading passphrase from standard input") {
		t.Fatalf("expected warning on stderr, got %q", stderr.String())
	}
}
