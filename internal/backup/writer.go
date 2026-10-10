package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

// StatMode formats the file or dir mode into a 4-digit octal string per §4.5.2.
func StatMode(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%04o", stat.Mode&07777)
	}
	perm := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		perm |= 04000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		perm |= 02000
	}
	if info.Mode()&os.ModeSticky != 0 {
		perm |= 01000
	}
	return fmt.Sprintf("%04o", perm)
}

// ParseOctalMode parses an octal mode string like "0755" or "4755" into os.FileMode.
func ParseOctalMode(modeStr string) (os.FileMode, error) {
	v, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return 0, err
	}
	perm := os.FileMode(v & 0777)
	if v&04000 != 0 {
		perm |= os.ModeSetuid
	}
	if v&02000 != 0 {
		perm |= os.ModeSetgid
	}
	if v&01000 != 0 {
		perm |= os.ModeSticky
	}
	return perm, nil
}

// WriteBundle takes the staged tree (and optional config) and compresses it into outAbs.
// It writes manifest.json as strictly the FIRST entry, paths NFC, with mode 0600.
func WriteBundle(stagingDir string, configPath string, noConfig bool, sourceRoot string, outAbs string) error {
	outDir := filepath.Dir(outAbs)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create out directory: %w", err)
	}

	// 1. Gather all entries from stagingDir
	var entries []ManifestEntry
	err := filepath.Walk(stagingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == stagingDir {
			return nil
		}
		rel, err := filepath.Rel(stagingDir, path)
		if err != nil {
			return err
		}
		relSlash := norm.NFC.String(filepath.ToSlash(rel))
		modeStr := StatMode(info)

		if info.IsDir() {
			entries = append(entries, ManifestEntry{
				Path: "tree/" + relSlash + "/",
				Dir:  true,
				Mode: modeStr,
			})
			return nil
		}

		// Regular file: compute size and sha256
		sz := info.Size()
		h, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("hash file %s: %w", rel, err)
		}
		entries = append(entries, ManifestEntry{
			Path:   "tree/" + relSlash,
			Mode:   modeStr,
			Size:   &sz,
			SHA256: h,
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk staging: %w", err)
	}

	// 2. Add config if requested (M13: !noConfig && read error → refuse with clear error)
	var configBytes []byte
	if !noConfig {
		if configPath == "" {
			return fmt.Errorf("config in force must be in bundle: no config path provided (M13)")
		}
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("read config file %s: %w (config in force must be in bundle; silent omission is illegal per M13)", configPath, err)
		}
		configBytes = raw
		sz := int64(len(raw))
		h := sha256.Sum256(raw)
		hashStr := hex.EncodeToString(h[:])
		entries = append(entries, ManifestEntry{
			Path:   "config/lararium.yaml",
			Mode:   "0600",
			Size:   &sz,
			SHA256: hashStr,
		})
	}

	// Sort entries deterministically by path
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	// 3. Compute counts
	counts, err := ComputeCounts(stagingDir)
	if err != nil {
		return fmt.Errorf("compute counts: %w", err)
	}

	absSource, err := filepath.Abs(sourceRoot)
	if err != nil {
		absSource = sourceRoot
	}
	hostname, _ := os.Hostname()
	manifest := NewManifest(absSource, hostname, entries, counts)
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	// 4. Create temp file next to outAbs
	tmpFile, err := os.CreateTemp(outDir, ".lararium-backup-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp backup: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		// Clean up partial temp if it still exists
		_ = os.Remove(tmpName)
	}()

	if err := os.Chmod(tmpName, 0o600); err != nil {
		tmpFile.Close()
		return fmt.Errorf("chmod temp backup: %w", err)
	}

	// 5. Stream ZIP with deflate
	zw := zip.NewWriter(tmpFile)

	// First entry: manifest.json (strictly FIRST entry per §4.5.2)
	mh := &zip.FileHeader{
		Name:   "manifest.json",
		Method: zip.Deflate,
	}
	mh.SetMode(0o600)
	mw, err := zw.CreateHeader(mh)
	if err != nil {
		zw.Close()
		tmpFile.Close()
		return fmt.Errorf("create manifest zip entry: %w", err)
	}
	if _, err := mw.Write(manifestData); err != nil {
		zw.Close()
		tmpFile.Close()
		return fmt.Errorf("write manifest zip entry: %w", err)
	}

	// Stream all entries
	for _, entry := range entries {
		mode, _ := ParseOctalMode(entry.Mode)
		fh := &zip.FileHeader{
			Name:   entry.Path,
			Method: zip.Deflate,
		}
		fh.SetMode(mode)
		if rawMode, err := strconv.ParseUint(entry.Mode, 8, 32); err == nil {
			typeBits := fh.ExternalAttrs >> 16 & 0xF000
			fh.ExternalAttrs = (uint32(typeBits|uint32(rawMode&07777)) << 16) | (fh.ExternalAttrs & 0xFFFF)
		}

		if entry.Dir {
			if _, err := zw.CreateHeader(fh); err != nil {
				zw.Close()
				tmpFile.Close()
				return fmt.Errorf("create dir zip entry %s: %w", entry.Path, err)
			}
			continue
		}

		w, err := zw.CreateHeader(fh)
		if err != nil {
			zw.Close()
			tmpFile.Close()
			return fmt.Errorf("create file zip entry %s: %w", entry.Path, err)
		}

		if entry.Path == "config/lararium.yaml" {
			if _, err := w.Write(configBytes); err != nil {
				zw.Close()
				tmpFile.Close()
				return fmt.Errorf("write config zip entry: %w", err)
			}
			continue
		}

		relPath := strings.TrimPrefix(entry.Path, "tree/")
		srcFile := filepath.Join(stagingDir, filepath.FromSlash(relPath))
		f, err := os.Open(srcFile)
		if err != nil {
			zw.Close()
			tmpFile.Close()
			return fmt.Errorf("open staged file %s: %w", srcFile, err)
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		if copyErr != nil {
			zw.Close()
			tmpFile.Close()
			return fmt.Errorf("copy staged file %s to zip: %w", srcFile, copyErr)
		}
	}

	if err := zw.Close(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("close zip writer: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// 6. Fsync parent directory
	fsyncDir(outDir)

	// 7. Atomic rename to outAbs
	if err := os.Rename(tmpName, outAbs); err != nil {
		return fmt.Errorf("rename temp backup to %s: %w", outAbs, err)
	}
	if err := os.Chmod(outAbs, 0o600); err != nil {
		return fmt.Errorf("chmod out backup: %w", err)
	}
	fsyncDir(outDir)

	return nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// CheckSelfInclusion verifies --out is not inside the hearth root (§4.5.3.4).
func CheckSelfInclusion(root, outAbs string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absOut, err := filepath.Abs(outAbs)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absOut)
	if err != nil {
		return fmt.Errorf("check self-inclusion: %w", err)
	}
	if !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("--out inside hearth root (self-inclusion refused): %s is inside %s", outAbs, absRoot)
	}
	return nil
}

// CreateOffline performs a full backup with the daemon stopped under flocks (§4.5.3.2).
func CreateOffline(root string, cfgPath string, outAbs string, noConfig bool, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	if err := CheckSelfInclusion(root, outAbs); err != nil {
		return err
	}

	// 1. Tree-shape check
	if _, err := CheckTreeShape(root); err != nil {
		return err
	}

	// 2. Preflight free space BEFORE any lock
	treeSize, err := ComputeTreeSize(root, cfgPath, !noConfig)
	if err != nil {
		return fmt.Errorf("compute tree size: %w", err)
	}
	if err := PreflightFreeSpace(root, outAbs, treeSize); err != nil {
		return err
	}

	// 3. Acquire flocks in order: backup.lock → custos.lock → keys.lock (all LOCK_NB)
	locks, err := AcquireLocks(root)
	if err != nil {
		return err
	}
	defer locks.Release()

	// 4. Fast staging copy onto root's own fs
	progress("staging tree")
	stagingDir, err := CreateStagingDir(root)
	if err != nil {
		return err
	}
	defer cleanStaging(stagingDir)

	if err := StageTree(root, stagingDir); err != nil {
		return fmt.Errorf("stage tree: %w", err)
	}

	// 5. Release locks before compression
	locks.Release()

	// 6. Compress staging → temp next to outAbs → rename
	progress("compressing archive")
	return WriteBundle(stagingDir, cfgPath, noConfig, root, outAbs)
}

// CreateFromDaemon performs a full backup from a running daemon (§4.5.3.1, B1).
func CreateFromDaemon(root string, cfgPath string, outAbs string, noConfig bool, singleWriterLock func() (func(), bool), progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	if err := CheckSelfInclusion(root, outAbs); err != nil {
		return err
	}

	// 1. Tree-shape check
	if _, err := CheckTreeShape(root); err != nil {
		return err
	}

	// 2. Preflight free space BEFORE any lock
	treeSize, err := ComputeTreeSize(root, cfgPath, !noConfig)
	if err != nil {
		return fmt.Errorf("compute tree size: %w", err)
	}
	if err := PreflightFreeSpace(root, outAbs, treeSize); err != nil {
		return err
	}

	// 3. Single-writer lock and flocks (B1: non-blocking, refuses with ErrBusy if held)
	var releaseSingleWriter func()
	if singleWriterLock != nil {
		rel, ok := singleWriterLock()
		if !ok {
			return fmt.Errorf("%w: single-writer lock held", ErrBusy)
		}
		releaseSingleWriter = rel
		defer func() {
			if releaseSingleWriter != nil {
				releaseSingleWriter()
				releaseSingleWriter = nil
			}
		}()
	}

	locks, err := AcquireLocks(root)
	if err != nil {
		return err
	}
	defer locks.Release()

	// 4. Fast staging copy
	progress("staging tree")
	stagingDir, err := CreateStagingDir(root)
	if err != nil {
		return err
	}
	defer cleanStaging(stagingDir)

	if err := StageTree(root, stagingDir); err != nil {
		return fmt.Errorf("stage tree: %w", err)
	}

	// 5. Release all locks before compression
	locks.Release()
	if releaseSingleWriter != nil {
		releaseSingleWriter()
		releaseSingleWriter = nil
	}

	// 6. Compress staging → temp next to outAbs → rename
	progress("compressing archive")
	return WriteBundle(stagingDir, cfgPath, noConfig, root, outAbs)
}

func cleanStaging(dir string) {
	// Restore write permissions on all dirs before deletion so os.RemoveAll succeeds on read-only dirs (M11)
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			_ = syscall.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
