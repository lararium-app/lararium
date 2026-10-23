package backup

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"

	"filippo.io/age"
	"golang.org/x/term"
)

// SealMagic is the standard age binary header magic string
// (PENATUS §4.5.2a, amendment A4b).
const SealMagic = "age-encryption.org/v1"

// sealMagic is the byte form of SealMagic.
var sealMagic = []byte(SealMagic)

// PlaintextBanner is the fixed stderr banner printed when creating an unencrypted bundle via --plaintext (§4.5.2a).
const PlaintextBanner = "WARNING: creating UNENCRYPTED bundle — secrets exposed at rest"

// UnsealedWarning is the exact warning printed to stderr when reading an unsealed bundle (§4.5.2a).
const UnsealedWarning = "UNENCRYPTED bundle — re-seal with backup create --to …"

// IsSealed performs a buffered sniff of r for the age sealMagic without byte loss (§4.5.2a).
// It returns a reconstructed reader containing all original bytes, whether the stream is sealed, and any read error.
func IsSealed(r io.Reader) (io.Reader, bool, error) {
	magicLen := len(sealMagic)
	buf := make([]byte, magicLen)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
		return bytes.NewReader(buf[:n]), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	isSealed := bytes.Equal(buf, sealMagic)
	return io.MultiReader(bytes.NewReader(buf), r), isSealed, nil
}

// Seal wraps age.Encrypt for the given recipients (§4.5.2a).
func Seal(dst io.Writer, recipients []age.Recipient) (io.WriteCloser, error) {
	if len(recipients) == 0 {
		return nil, errors.New("no recipients specified")
	}
	return age.Encrypt(dst, recipients...)
}

// Unseal wraps age.Decrypt for the given identities (§4.5.2a).
func Unseal(src io.Reader, ids []age.Identity) (io.Reader, error) {
	if len(ids) == 0 {
		return nil, errors.New("no identities specified")
	}
	return age.Decrypt(src, ids...)
}

// ParseRecipient parses an age1... recipient string per §4.5.2a.
func ParseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "age1") {
		return nil, fmt.Errorf("invalid recipient %q: must start with age1", s)
	}
	return age.ParseX25519Recipient(s)
}

// ParseRecipients parses a slice of age1... recipient strings.
func ParseRecipients(strs []string) ([]age.Recipient, error) {
	var recipients []age.Recipient
	for _, s := range strs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		r, err := ParseRecipient(s)
		if err != nil {
			return nil, err
		}
		recipients = append(recipients, r)
	}
	return recipients, nil
}

// NewScryptRecipient creates a password-based recipient using library default work factor.
func NewScryptRecipient(passphrase string) (age.Recipient, error) {
	return age.NewScryptRecipient(passphrase)
}

// NewScryptIdentity creates a password-based identity using library default work factor.
func NewScryptIdentity(passphrase string) (age.Identity, error) {
	return age.NewScryptIdentity(passphrase)
}

var warnOnceOS sync.Once

// LoadIdentityFile loads age identities from path enforcing the §4.5.2a mode-0600 law:
// group/other-readable files are refused; on operating systems without mode info, it warns once.
func LoadIdentityFile(path string) ([]age.Identity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat identity file %s: %w", path, err)
	}

	if runtime.GOOS == "windows" {
		warnOnceOS.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: cannot verify file permissions on %s\n", runtime.GOOS)
		})
	} else {
		perm := fi.Mode().Perm()
		if perm&0o077 != 0 {
			return nil, fmt.Errorf("identity file %s has mode %04o; must be 0600 (group/other-readable key refused per §4.5.2a)", path, perm)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open identity file %s: %w", path, err)
	}
	defer f.Close()

	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("parse identity file %s: %w", path, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("identity file %s contains no identities", path)
	}
	return ids, nil
}

// PromptPassphrase prompts for a passphrase via x/term on a TTY, or from stdin with the spec'd warning when piped (§4.5.2a).
func PromptPassphrase(prompt string, stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, prompt)
		passBytes, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		return string(passBytes), nil
	}

	fmt.Fprintln(stderr, "warning: reading passphrase from standard input")
	r := bufio.NewReader(stdin)
	line, err := r.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	return line, nil
}

// BundleReader wraps an opened backup zip reader and an optional closer.
type BundleReader struct {
	*zip.Reader

	closer io.Closer
}

// Close closes the underlying reader resources if applicable.
func (b *BundleReader) Close() error {
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

// OpenBundle opens a backup bundle from path, sniffing whether it is sealed or unsealed (§4.5.2a).
// For unsealed bundles, it prints UnsealedWarning to stderr and opens the zip.
// For sealed bundles, it decrypts via the supplied identities (or prompts for passphrase) and opens the unsealed zip stream in memory.
func OpenBundle(path string, identities ...age.Identity) (*BundleReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: %w", path, err)
	}

	r, isSealed, err := IsSealed(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("sniff bundle %s: %w", path, err)
	}

	if !isSealed {
		fmt.Fprintln(os.Stderr, UnsealedWarning)
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("stat bundle %s: %w", path, err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return nil, fmt.Errorf("seek bundle %s: %w", path, err)
		}
		zr, err := zip.NewReader(f, fi.Size())
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("open zip reader %s: %w", path, err)
		}
		return &BundleReader{Reader: zr, closer: f}, nil
	}

	// Sealed bundle
	defer f.Close()

	if len(identities) == 0 {
		pass, promptErr := PromptPassphrase("Enter passphrase: ", os.Stdin, os.Stderr)
		if promptErr == nil && pass != "" {
			scryptID, scryptErr := NewScryptIdentity(pass)
			if scryptErr == nil {
				identities = append(identities, scryptID)
			}
		}
	}

	if len(identities) == 0 {
		return nil, errors.New("sealed backup bundle: no identity provided (use --identity, config backup.identity, or passphrase)")
	}

	unsealed, err := Unseal(r, identities)
	if err != nil {
		return nil, fmt.Errorf("unseal bundle %s: %w", path, err)
	}

	zipBytes, err := io.ReadAll(unsealed)
	if err != nil {
		return nil, fmt.Errorf("read unsealed bundle %s: %w", path, err)
	}

	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("parse unsealed zip %s: %w", path, err)
	}

	return &BundleReader{Reader: zr}, nil
}

// Keygen generates a fresh X25519 identity, writes it to identityPath with
// mode 0600 (refusing to overwrite), and returns the recipient string —
// the operator-side key story of §4.5.2a (amendment A4b).
func Keygen(identityPath string) (string, error) {
	if _, err := os.Stat(identityPath); err == nil {
		return "", fmt.Errorf("identity file exists: %s (refusing to overwrite)", identityPath)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(identityPath, []byte(id.String()+"\n"), 0o600); err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}
