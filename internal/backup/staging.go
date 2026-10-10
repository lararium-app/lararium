package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

// Writer inventory (PENATUS §4.5.3.1, M9):
// Every hearth-root writer and its serialization protocol:
// 1. tokens.json: uses flock on the tokens file itself (TokenStore.lockTokenFile) — NOT keys.lock.
// 2. session create PenatusSource.Create: creates session dir and renames session.json outside
//    single-writer (documented honest gap: transient session file, atomic rename, no torn reads).
// 3. nuntius inbox/state/owners.jsonl: tmp+rename under its own mutexes, NOT fs locks
//    (transient file sets vary, atomicity per file, no torn reads).
// 4. events.jsonl: appended under Hub singleWriterMu (SingleWriterLock/TrySingleWriterLock).
// 5. memory/: atomic tmp+rename under Hub single-writer lock.
// 6. custody state root (vault.*, surrogates.age, fingerprints.json, snapshots/):
//    serialized under custos.lock flock protocol (CUSTOS-SPEC §4.2, AcquireLocks).
// 7. keys.json: serialized under keys.lock flock protocol.

// FSInfo holds filesystem availability information.
type FSInfo struct {
	AvailableBytes uint64
	Fsid           uint64
}

// StatfsFunc allows overriding statfs for testing (BK12).
var StatfsFunc = defaultStatfs

func defaultStatfs(path string) (FSInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FSInfo{}, err
	}
	fsid := uint64(stat.Fsid.X__val[0])<<32 | uint64(uint32(stat.Fsid.X__val[1]))
	avail := stat.Bavail * uint64(stat.Bsize)
	return FSInfo{AvailableBytes: avail, Fsid: fsid}, nil
}

// IsExcluded reports whether a relative path in the hearth root is excluded per §4.5.2, B4, M8, and F1.
// Excluded:
// - memory/index.db (rule 5)
// - *.sock, *.lock
// - atomic-rename temporary files according to the precise writer law (F1):
//  1. base has suffix ".tmp":
//     Covers root TokenStore (tokens-*.tmp), Keystore (keys-*.tmp),
//     and Custos (*-mut-*.tmp, *-init-*.tmp, etc.), where all os.CreateTemp
//     patterns end with ".tmp".
//  2. rel starts with "nuntius/" AND base matches "<anything>.tmp<any digits>":
//     Covers nuntius state.go (internal/nuntius/state.go:76) which uses
//     os.CreateTemp(dir, name+".tmp*") where random digits replace the star,
//     producing e.g. "state.tmp4182" (".tmp" followed by ONLY digits at end).
//
// - temporary staging directories (.backup-staging-*) per B4
func IsExcluded(rel string, info os.FileInfo) bool {
	cleanRel := filepath.ToSlash(filepath.Clean(rel))
	if cleanRel == "memory/index.db" {
		return true
	}
	base := filepath.Base(cleanRel)
	if strings.HasSuffix(base, ".sock") || strings.HasSuffix(base, ".lock") {
		return true
	}
	if strings.HasPrefix(base, ".backup-staging-") {
		return true
	}

	// Clause 1: base has suffix ".tmp"
	// Writers covered:
	// - surface TokenStore: tokens-*.tmp at root
	// - keystore: keys-*.tmp
	// - custos: vault-mut-*.tmp, vaultmac-mut-*.tmp, surrogates-mut-*.tmp,
	//   vault-init-*.tmp, vaultmac-init-*.tmp, etc.
	// All these writers use os.CreateTemp patterns ending in ".tmp".
	if strings.HasSuffix(base, ".tmp") {
		return true
	}

	// Clause 2: rel starts with "nuntius/" AND base matches "<anything>.tmp<any digits>"
	// Writers covered:
	// - nuntius state.go (internal/nuntius/state.go:76): uses CreateTemp(dir, name+".tmp*")
	//   where random digits replace the star, producing e.g. "state.tmp4182".
	//   Matches ".tmp" followed by ONLY digits at end.
	if strings.HasPrefix(cleanRel, "nuntius/") {
		if idx := strings.LastIndex(base, ".tmp"); idx != -1 {
			digits := base[idx+len(".tmp"):]
			if len(digits) > 0 && isAllDigits(digits) {
				return true
			}
		}
	}

	return false
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// CheckTreeShape enforces the tree-shape law (§4.5.2):
// Regular files + directories only. Symlinks, hardlinks, devices, FIFOs
// anywhere in the root → backup refuses, listing offenders.
func CheckTreeShape(root string) ([]string, error) {
	var offenders []string
	type devIno struct {
		dev uint64
		ino uint64
	}
	seenInos := make(map[devIno]string)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if IsExcluded(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		mode := info.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
			offenders = append(offenders, rel)
			return nil
		}

		if !info.IsDir() {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				key := devIno{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
				if stat.Nlink > 1 {
					offenders = append(offenders, rel)
				} else if _, seen := seenInos[key]; seen {
					offenders = append(offenders, rel)
				} else {
					seenInos[key] = rel
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(offenders) > 0 {
		return offenders, fmt.Errorf("tree-shape law violation: backup refuses on symlink/hardlink/device/FIFO listing offenders: %s", strings.Join(offenders, ", "))
	}
	return nil, nil
}

// ComputeTreeSize sums the sizes of all regular files included in the backup.
func ComputeTreeSize(root string, configPath string, includeConfig bool) (uint64, error) {
	var total uint64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if IsExcluded(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if includeConfig {
		if configPath == "" {
			return 0, fmt.Errorf("config in force must be in bundle: no config path provided (M13)")
		}
		fi, err := os.Stat(configPath)
		if err != nil {
			return 0, fmt.Errorf("stat config file %s: %w (config in force must be in bundle; silent omission is illegal per M13)", configPath, err)
		}
		if fi.IsDir() {
			return 0, fmt.Errorf("config path %s is a directory", configPath)
		}
		total += uint64(fi.Size())
	}
	return total, nil
}

// PreflightFreeSpace enforces the free-space precondition (§4.5.3.1, BK12) BEFORE any lock:
// Root fs needs ≥ Σ for staging; --out fs needs ≥ Σ for output.
// If both live on one filesystem (f_fsid equal), require ≥ 2Σ.
// Short → refuse with the numbers and the rule printed.
func PreflightFreeSpace(root string, outPath string, treeSize uint64) error {
	rootFS, err := StatfsFunc(root)
	if err != nil {
		return fmt.Errorf("statfs root %s: %w", root, err)
	}

	outDir := filepath.Dir(outPath)
	// Walk up if outDir doesn't exist yet
	for {
		if _, err := os.Stat(outDir); err == nil {
			break
		}
		parent := filepath.Dir(outDir)
		if parent == outDir {
			break
		}
		outDir = parent
	}

	outFS, err := StatfsFunc(outDir)
	if err != nil {
		return fmt.Errorf("statfs out directory %s: %w", outDir, err)
	}

	if rootFS.Fsid == outFS.Fsid {
		required := 2 * treeSize
		if rootFS.AvailableBytes < required {
			return fmt.Errorf("insufficient free space: required %d bytes (2Σ), available %d bytes; rule: root fs and out fs share filesystem (f_fsid %d), require ≥ 2Σ for staging and output coexistence (PENATUS §4.5.3.1, BK12)", required, rootFS.AvailableBytes, rootFS.Fsid)
		}
	} else {
		if rootFS.AvailableBytes < treeSize {
			return fmt.Errorf("insufficient free space on root filesystem: required %d bytes (Σ), available %d bytes; rule: root fs requires ≥ Σ for staging (PENATUS §4.5.3.1, BK12)", treeSize, rootFS.AvailableBytes)
		}
		if outFS.AvailableBytes < treeSize {
			return fmt.Errorf("insufficient free space on output filesystem: required %d bytes (Σ), available %d bytes; rule: out fs requires ≥ Σ for output (PENATUS §4.5.3.1, BK12)", treeSize, outFS.AvailableBytes)
		}
	}
	return nil
}

// CreateStagingDir creates a 0700 temporary directory on the hearth root's own filesystem (B4).
// If parent filesystem differs from root's fs, falls back to creating inside root.
func CreateStagingDir(root string) (string, error) {
	rootFS, err := StatfsFunc(root)
	if err != nil {
		return "", fmt.Errorf("statfs root %s: %w", root, err)
	}

	parent := filepath.Dir(root)
	targetDir := parent
	parentFS, err := StatfsFunc(parent)
	if err != nil || parentFS.Fsid != rootFS.Fsid {
		targetDir = root
	}

	staging, err := os.MkdirTemp(targetDir, ".backup-staging-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("chmod staging directory: %w", err)
	}
	return staging, nil
}

type dirTarget struct {
	path string
	mode uint32
}

// StagePreCopyHook is an optional hook invoked before inspecting and copying each path during StageTree.
// Tests can use this seam to simulate a file vanishing (ENOENT) between walk-visit and copy.
var StagePreCopyHook func(srcPath string) error

// StageCopyFileFunc allows overriding file copying during staging (test seam).
var StageCopyFileFunc = copyFile

// StageTree copies the included hearth root tree into stagingDir, preserving file and dir modes (M10).
// While holding flocks, lstat each entry during StageTree; symlink/device/FIFO encountered → abort (M12).
// Never follow; open with O_NOFOLLOW (M12).
// Do not pre-order chmod dirs; copy children first, then chmod dirs last in post-order (M11).
// If a file vanishes mid-copy (ENOENT) → benign skip, not failure (M8).
func StageTree(root, stagingDir string) error {
	var dirTargets []dirTarget

	err := filepath.Walk(root, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // M8: benign skip if file vanishes mid-walk
			}
			return err
		}
		if srcPath == root {
			return nil
		}
		rel, err := filepath.Rel(root, srcPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if IsExcluded(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if StagePreCopyHook != nil {
			if err := StagePreCopyHook(srcPath); err != nil {
				return err
			}
		}

		// M12: staging-time shape re-check while holding flocks via lstat
		lfi, err := os.Lstat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // M8 benign skip
			}
			return err
		}
		mode := lfi.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
			return fmt.Errorf("tree-shape law violation during staging: encountered illegal entry %s (%v)", rel, mode)
		}

		rawMode := uint32(mode.Perm())
		if stat, ok := lfi.Sys().(*syscall.Stat_t); ok {
			rawMode = uint32(stat.Mode & 0o7777)
		}

		dstPath := filepath.Join(stagingDir, norm.NFC.String(rel))
		if info.IsDir() {
			// M11: do not pre-order chmod dirs; create 0700 for children, chmod post-order
			if err := os.MkdirAll(dstPath, 0o700); err != nil {
				return fmt.Errorf("create staging dir %s: %w", rel, err)
			}
			dirTargets = append(dirTargets, dirTarget{path: dstPath, mode: rawMode})
			return nil
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", rel, err)
		}

		if err := StageCopyFileFunc(srcPath, dstPath, lfi.Mode()); err != nil {
			if os.IsNotExist(err) {
				return nil // M8: benign skip
			}
			return fmt.Errorf("copy staging file %s: %w", rel, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// M11: apply dir modes in post-order (deepest directories first)
	sort.Slice(dirTargets, func(i, j int) bool {
		return len(dirTargets[i].path) > len(dirTargets[j].path)
	})
	for _, dt := range dirTargets {
		if err := syscall.Chmod(dt.path, dt.mode); err != nil {
			return fmt.Errorf("post-order chmod dir %s: %w", dt.path, err)
		}
	}

	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	// M12: Never follow; open with O_NOFOLLOW
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist // M8 benign skip
		}
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// M10: preserve source stat mode (incl. setuid/setgid/sticky bits) into staging
	rawMode := uint32(mode.Perm())
	if fi, err := os.Lstat(src); err == nil {
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			rawMode = uint32(stat.Mode & 0o7777)
		}
	}
	return syscall.Chmod(dst, rawMode)
}
