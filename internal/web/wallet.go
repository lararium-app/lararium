package web

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/idna"

	"github.com/lararium-app/lararium/internal/keystore"
)

const (
	walletMagic   = "LRWS1"
	walletKeyName = "webwallet"
	nonceSize     = 12
)

// OriginKey converts a raw URL or origin host into a lowercased, punycoded,
// collision-safely escaped key suitable for file paths per WEB-LAYER-SPEC.
// Allowed characters without escape: [a-z0-9.-].
// Any other character is escaped as _<hex2>.
func OriginKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty origin")
	}

	var host string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parse origin URL: %w", err)
		}
		host = u.Hostname()
		if host == "" {
			host = u.Host
		}
	} else {
		// strip path if present
		if idx := strings.IndexByte(raw, '/'); idx >= 0 {
			raw = raw[:idx]
		}
		if h, _, err := net.SplitHostPort(raw); err == nil {
			host = h
		} else {
			host = raw
		}
	}

	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "" || host == "." || host == ".." {
		return "", errors.New("invalid origin host")
	}

	host = strings.ToLower(host)
	if ascii, err := idna.ToASCII(host); err == nil {
		host = ascii
	}

	var sb strings.Builder
	for i := range len(host) {
		b := host[i]
		switch {
		case (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9'):
			sb.WriteByte(b)
		case b == '.' || b == '-':
			// Prevent leading dot/dash or consecutive dots from causing traversal or hidden files
			if i == 0 || (b == '.' && i > 0 && host[i-1] == '.') {
				fmt.Fprintf(&sb, "_%02x", b)
			} else {
				sb.WriteByte(b)
			}
		default:
			fmt.Fprintf(&sb, "_%02x", b)
		}
	}

	res := sb.String()
	if res == "" {
		return "", errors.New("invalid origin key")
	}
	return res, nil
}

// Wallet manages encrypted browser profile sessions on disk under <home>/sessions/.
type Wallet struct {
	home   string
	ks     *keystore.Store
	mu     sync.Mutex
	active map[string]bool
}

// NewWallet creates a new Wallet instance rooted at home.
// If ks is nil, keystore.New(home) is used.
func NewWallet(home string, ks *keystore.Store) *Wallet {
	if ks == nil {
		ks = keystore.New(home)
	}
	return &Wallet{
		home:   home,
		ks:     ks,
		active: make(map[string]bool),
	}
}

// Home returns the wallet home directory.
func (w *Wallet) Home() string {
	return w.home
}

// Has reports whether an encrypted session entry exists for the given origin.
func (w *Wallet) Has(origin string) bool {
	originKey, err := OriginKey(origin)
	if err != nil {
		return false
	}
	lrwsPath := filepath.Join(w.home, "sessions", originKey+".lrws")
	fi, err := os.Stat(lrwsPath)
	return err == nil && !fi.IsDir()
}

// Delete removes an encrypted session entry and any stale active directory.
func (w *Wallet) Delete(origin string) error {
	originKey, err := OriginKey(origin)
	if err != nil {
		return fmt.Errorf("invalid origin: %w", err)
	}

	lrwsPath := filepath.Join(w.home, "sessions", originKey+".lrws")
	activeDir := filepath.Join(w.home, "sessions", ".active", originKey)
	lockPath := filepath.Join(w.home, "sessions", ".active", originKey+".lock")

	_ = os.RemoveAll(activeDir)
	_ = os.Remove(lockPath)

	if err := os.Remove(lrwsPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Load unpacks the encrypted session for origin into an active directory, locking it.
// It returns the active directory path and a release function.
// When released, the active directory is re-tarred, sealed, written atomically,
// and the active directory is removed.
// If the session file is tampered or truncated, Load returns a clear error and cleans up.
func (w *Wallet) Load(origin string) (string, func() error, error) {
	originKey, err := OriginKey(origin)
	if err != nil {
		return "", nil, fmt.Errorf("invalid origin %q: %w", origin, err)
	}

	w.mu.Lock()
	if w.active[originKey] {
		w.mu.Unlock()
		return "", nil, fmt.Errorf("session for origin %q is already active", origin)
	}
	w.active[originKey] = true
	w.mu.Unlock()

	cleanupInMem := func() {
		w.mu.Lock()
		delete(w.active, originKey)
		w.mu.Unlock()
	}

	sessionsDir := filepath.Join(w.home, "sessions")
	activeParent := filepath.Join(sessionsDir, ".active")
	if err := os.MkdirAll(activeParent, 0o700); err != nil {
		cleanupInMem()
		return "", nil, fmt.Errorf("create active dir: %w", err)
	}

	lockPath := filepath.Join(activeParent, originKey+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		cleanupInMem()
		return "", nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		cleanupInMem()
		return "", nil, fmt.Errorf("session for origin %q is already active (flock): %w", origin, err)
	}

	activeDir := filepath.Join(activeParent, originKey)
	if err := os.MkdirAll(activeDir, 0o700); err != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		cleanupInMem()
		return "", nil, fmt.Errorf("create origin active dir: %w", err)
	}

	lrwsPath := filepath.Join(sessionsDir, originKey+".lrws")
	if data, err := os.ReadFile(lrwsPath); err == nil {
		key, kerr := getOrGenerateWalletKey(w.ks)
		if kerr != nil {
			_ = os.RemoveAll(activeDir)
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			cleanupInMem()
			return "", nil, kerr
		}
		tarData, derr := unsealProfile(key, data)
		if derr != nil {
			_ = os.RemoveAll(activeDir)
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			cleanupInMem()
			return "", nil, derr
		}
		if uerr := untarProfile(tarData, activeDir); uerr != nil {
			_ = os.RemoveAll(activeDir)
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			cleanupInMem()
			return "", nil, fmt.Errorf("tampered or truncated session file: untar failed: %w", uerr)
		}
	} else if !os.IsNotExist(err) {
		_ = os.RemoveAll(activeDir)
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		cleanupInMem()
		return "", nil, fmt.Errorf("read session file: %w", err)
	}

	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			defer func() {
				_ = os.RemoveAll(activeDir)
				_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
				_ = lockFile.Close()
				cleanupInMem()
			}()

			tarData, err := tarProfile(activeDir)
			if err != nil {
				releaseErr = fmt.Errorf("tar active dir: %w", err)
				return
			}

			key, err := getOrGenerateWalletKey(w.ks)
			if err != nil {
				releaseErr = err
				return
			}

			sealed, err := sealProfile(key, tarData)
			if err != nil {
				releaseErr = fmt.Errorf("seal session data: %w", err)
				return
			}

			tmpFile, err := os.CreateTemp(sessionsDir, ".tmp-"+originKey+"-*.lrws")
			if err != nil {
				releaseErr = fmt.Errorf("create temp session file: %w", err)
				return
			}
			tmpName := tmpFile.Name()
			defer func() {
				if tmpName != "" {
					_ = os.Remove(tmpName)
				}
			}()

			if _, err := tmpFile.Write(sealed); err != nil {
				_ = tmpFile.Close()
				releaseErr = fmt.Errorf("write temp session file: %w", err)
				return
			}
			if err := tmpFile.Sync(); err != nil {
				_ = tmpFile.Close()
				releaseErr = fmt.Errorf("sync temp session file: %w", err)
				return
			}
			if err := tmpFile.Close(); err != nil {
				releaseErr = fmt.Errorf("close temp session file: %w", err)
				return
			}
			if err := os.Rename(tmpName, lrwsPath); err != nil {
				releaseErr = fmt.Errorf("rename temp session file: %w", err)
				return
			}
			tmpName = "" // rename succeeded
		})
		return releaseErr
	}

	return activeDir, release, nil
}

func getOrGenerateWalletKey(ks *keystore.Store) ([]byte, error) {
	m, err := ks.Read()
	if err != nil {
		return nil, fmt.Errorf("read keystore: %w", err)
	}
	if val, ok := m[walletKeyName]; ok && strings.TrimSpace(val) != "" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(val))
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate wallet key: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(key)
	if err := ks.Set(walletKeyName, b64); err != nil {
		return nil, fmt.Errorf("store wallet key: %w", err)
	}
	return key, nil
}

func sealProfile(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	buf := make([]byte, 0, len(walletMagic)+len(nonce)+len(ciphertext))
	buf = append(buf, walletMagic...)
	buf = append(buf, nonce...)
	buf = append(buf, ciphertext...)
	return buf, nil
}

func unsealProfile(key, sealed []byte) ([]byte, error) {
	if len(sealed) < len(walletMagic)+nonceSize+16 {
		return nil, errors.New("tampered or truncated session file: too short")
	}
	if string(sealed[:len(walletMagic)]) != walletMagic {
		return nil, errors.New("tampered or truncated session file: invalid magic")
	}
	nonce := sealed[len(walletMagic) : len(walletMagic)+nonceSize]
	ciphertext := sealed[len(walletMagic)+nonceSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("tampered or truncated session file: %w", err)
	}
	return plaintext, nil
}

func tarProfile(srcDir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Mode = 0o700
		} else {
			hdr.Mode = 0o600
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path) //nolint:gosec // G122: local profile directory walk
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func untarProfile(data []byte, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		cleanPath := filepath.Clean(filepath.FromSlash(hdr.Name))
		target := filepath.Join(destDir, cleanPath)
		rel, err := filepath.Rel(destDir, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("invalid path in session tar: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil { //nolint:gosec // G703: path is cleaned and validated
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil { //nolint:gosec // G703: path is cleaned and validated
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G703: path is cleaned and validated
			if err != nil {
				return err
			}
			// Read one byte past the cap: an entry that overflows is an
			// integrity error, never a silent truncation.
			n, err := io.Copy(f, io.LimitReader(tr, 100<<20+1))
			if err != nil {
				_ = f.Close()
				return err
			}
			if n > 100<<20 {
				_ = f.Close()
				return fmt.Errorf("profile entry %q exceeds 100 MiB cap", hdr.Name)
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
