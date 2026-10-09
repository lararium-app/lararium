package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

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

// IsExcluded reports whether a relative path in the hearth root is excluded per §4.5.2.
// Always excluded: memory/index.db (rule 5), *.sock, *.lock.
func IsExcluded(rel string, info os.FileInfo) bool {
	cleanRel := filepath.ToSlash(filepath.Clean(rel))
	if cleanRel == "memory/index.db" {
		return true
	}
	base := filepath.Base(cleanRel)
	if strings.HasSuffix(base, ".sock") || strings.HasSuffix(base, ".lock") {
		return true
	}
	return false
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
	if includeConfig && configPath != "" {
		if fi, err := os.Stat(configPath); err == nil && !fi.IsDir() {
			total += uint64(fi.Size())
		}
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

// CreateStagingDir creates a 0700 temporary directory on the hearth root's own filesystem.
func CreateStagingDir(root string) (string, error) {
	parent := filepath.Dir(root)
	staging, err := os.MkdirTemp(parent, ".backup-staging-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("chmod staging directory: %w", err)
	}
	return staging, nil
}

// StageTree copies the included hearth root tree into stagingDir, preserving file and dir modes.
func StageTree(root, stagingDir string) error {
	return filepath.Walk(root, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
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

		dstPath := filepath.Join(stagingDir, norm.NFC.String(rel))
		if info.IsDir() {
			if err := os.MkdirAll(dstPath, info.Mode().Perm()); err != nil {
				return fmt.Errorf("create staging dir %s: %w", rel, err)
			}
			if err := os.Chmod(dstPath, info.Mode().Perm()); err != nil {
				return fmt.Errorf("chmod staging dir %s: %w", rel, err)
			}
			return nil
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", rel, err)
		}

		if err := copyFile(srcPath, dstPath, info.Mode()); err != nil {
			return fmt.Errorf("copy staging file %s: %w", rel, err)
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(dst, mode.Perm())
}
