package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBusy is returned on lock conflict per §4.5.3.3 (all LOCK_NB).
var ErrBusy = errors.New("busy: lock held by another process")

// Locks manages the flock ordering per PENATUS-SPEC §4.5.3.3:
// backup.lock → custos.lock → keys.lock, ALL LOCK_NB, create-and-open 0600, never deleted.
type Locks struct {
	backupFile *os.File
	custosFile *os.File
	keysFile   *os.File
}

// AcquireLocks acquires backup.lock, custos.lock, and keys.lock in strict order with LOCK_NB.
func AcquireLocks(root string) (*Locks, error) {
	backupPath := filepath.Join(root, "backup.lock")
	custosPath := filepath.Join(root, "custos.lock")
	keysPath := filepath.Join(root, "keys.lock")

	bFile, err := openAndFlock(backupPath)
	if err != nil {
		return nil, err
	}

	cFile, err := openAndFlock(custosPath)
	if err != nil {
		unlockAndClose(bFile)
		return nil, err
	}

	kFile, err := openAndFlock(keysPath)
	if err != nil {
		unlockAndClose(cFile)
		unlockAndClose(bFile)
		return nil, err
	}

	return &Locks{
		backupFile: bFile,
		custosFile: cFile,
		keysFile:   kFile,
	}, nil
}

func openAndFlock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", filepath.Base(path), err)
	}
	_ = os.Chmod(path, 0o600)

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("%w: %s", ErrBusy, filepath.Base(path))
		}
		return nil, fmt.Errorf("flock %s: %w", filepath.Base(path), err)
	}
	return f, nil
}

func unlockAndClose(f *os.File) {
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// Release releases all held locks in reverse order (keys.lock → custos.lock → backup.lock)
// and closes file descriptors. Per §4.5.3.3 lock files are NEVER deleted.
func (l *Locks) Release() {
	if l == nil {
		return
	}
	unlockAndClose(l.keysFile)
	l.keysFile = nil
	unlockAndClose(l.custosFile)
	l.custosFile = nil
	unlockAndClose(l.backupFile)
	l.backupFile = nil
}
