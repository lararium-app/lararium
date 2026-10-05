package custos

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockFile manages the flock serialization on custos.lock per CUSTOS-SPEC §4.2, §C2.
type LockFile struct {
	path    string
	timeout time.Duration
}

// NewLockFile creates a LockFile for the given state root directory.
func NewLockFile(stateDir string, timeout time.Duration) *LockFile {
	if timeout <= 0 {
		timeout = DefaultLockWaitTimeout
	}
	return &LockFile{
		path:    filepath.Join(stateDir, "custos.lock"),
		timeout: timeout,
	}
}

// Lock acquires an exclusive lock on custos.lock with timeout per CUSTOS-SPEC §4.2.
// Acquisition timeout returns typed ErrLockTimeout ("lock_timeout").
func (l *LockFile) Lock() (func(), error) {
	deadline := time.Now().Add(l.timeout)
	pollInterval := 10 * time.Millisecond

	for {
		// OpenFile 0600 per CUSTOS §C2
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file %s: %w", l.path, err)
		}
		_ = os.Chmod(l.path, 0o600)

		// Try non-blocking flock
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case flockErr == nil:
			// Verify file inode hasn't changed under us
			fi1, err1 := f.Stat()
			fi2, err2 := os.Stat(l.path)
			if err1 == nil && err2 == nil && os.SameFile(fi1, fi2) {
				unlock := func() {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
				}
				return unlock, nil
			}
			// File was replaced, release and retry
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		case flockErr != syscall.EWOULDBLOCK && flockErr != syscall.EAGAIN:
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", l.path, flockErr)
		default:
			_ = f.Close()
		}

		if time.Now().After(deadline) {
			// CUSTOS §4.2: typed lock_timeout error on expiry
			return nil, ErrLockTimeout
		}

		time.Sleep(pollInterval)
		if pollInterval < 50*time.Millisecond {
			pollInterval += 5 * time.Millisecond
		}
	}
}
