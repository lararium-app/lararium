package backup

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/lararium-app/lararium/internal/surface"
)

// OperatorStepStaleSocket is the exact operator remediation text when hearthd.sock
// is present but dead per PENATUS-SPEC §4.5.3.2 and §4.5.4 rule 3.
const OperatorStepStaleSocket = "confirm the daemon process is dead, remove hearthd.sock, retry"

// SwapHook is a package-level test-only crash injection seam (zero production behavior).
// Production never sets it. Tests set it to simulate crash mid-swap.
var SwapHook func(step string)

func runSwapHook(step string) {
	if SwapHook != nil {
		SwapHook(step)
	}
}

// PingSocketFunc allows overriding socket ping for tests.
var PingSocketFunc = surface.PingViaSocket

// RestoreMarker records swap metadata in a parent-directory dotfile per PENATUS-SPEC §4.5.4 rule 4.
type RestoreMarker struct {
	Staging string `json:"staging"`
	PreSwap string `json:"pre_swap"`
	Target  string `json:"target"`
	TS      string `json:"ts"`
	Safety  string `json:"safety,omitempty"`
}

// RestoreTo implements `hearthd restore <file> --to <dir>` (fresh root) per §4.5.4 rule 2.
// Refuse if dir exists. Mutual exclusion is structural: stage into unique sibling <dir>.staging-<ts>,
// fill from zip, fsync every file + dir, then single rename(staging, dir).
func RestoreTo(bundle, dir string) error {
	dir = filepath.Clean(dir)
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}

	// 1. Refuse if target dir exists (§4.5.4 rule 2)
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("target directory %s already exists: --to requires a fresh root", dir)
	}

	// 2. Version law & Verify FIRST (§4.5.4 rule 1)
	manifest, zr, err := verifyAndOpenBundle(bundle)
	if err != nil {
		return err
	}
	defer zr.Close()

	// 3. Pre-validate every member in bundle before ANY write
	if err := validateBundleMembers(&zr.Reader, manifest); err != nil {
		return err
	}

	parentDir := filepath.Dir(dir)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("create parent directory %s: %w", parentDir, err)
	}

	// Free space preflight on parent filesystem
	treeSize := computeStagedTreeSize(manifest)
	if parentFS, err := StatfsFunc(parentDir); err == nil {
		if parentFS.AvailableBytes < treeSize {
			return fmt.Errorf("insufficient free space on filesystem: required %d bytes, available %d bytes (PENATUS §4.5.4)", treeSize, parentFS.AvailableBytes)
		}
	}

	// 4. Staging into unique sibling <dir>.staging-<ts> (0700, same parent dir)
	ts := formatTimestamp()
	stagingDir := fmt.Sprintf("%s.staging-%s", dir, ts)
	if _, err := os.Stat(stagingDir); err == nil {
		stagingDir = fmt.Sprintf("%s-%d", stagingDir, time.Now().UnixNano())
	}

	if err := os.Mkdir(stagingDir, 0o700); err != nil {
		return fmt.Errorf("create staging dir %s: %w", stagingDir, err)
	}
	_ = os.Chmod(stagingDir, 0o700)

	// 5. Fill from zip and fsync every file + dir
	if err := extractTree(&zr.Reader, manifest, stagingDir); err != nil {
		cleanStaging(stagingDir)
		return err
	}

	// 6. Single rename(staging, dir). If rename fails because dir appeared, lose cleanly.
	if err := os.Rename(stagingDir, dir); err != nil {
		cleanStaging(stagingDir)
		return fmt.Errorf("restore failed: rename staging to target: %w", err)
	}
	fsyncDir(parentDir)

	return nil
}

// RestoreReplace implements `hearthd restore <file> --replace <dir> --yes [--no-safety]` per §4.5.4 rules 3–4.
func RestoreReplace(bundle, dir string, yes bool, noSafety bool) error {
	dir = filepath.Clean(dir)
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	parentDir := filepath.Dir(dir)
	baseName := filepath.Base(dir)

	// Top of restore: evaluate recovery table (§4.5.4 rule 4)
	if err := RecoverSwap(dir); err != nil {
		return err
	}

	// a. Requires yes
	if !yes {
		return errors.New("usage refusal: --replace requires --yes")
	}

	// Check if hearthd.sock answers ping (§4.5.4 rule 3, §4.5.3.2)
	sockPath := filepath.Join(dir, "hearthd.sock")
	if _, err := os.Stat(sockPath); err == nil {
		if err := PingSocketFunc(sockPath, 2*time.Second); err == nil {
			return errors.New("target daemon is running (hearthd.sock answers ping): --replace refuses")
		}
		return fmt.Errorf("hearthd.sock exists but daemon is not responding: %s", OperatorStepStaleSocket)
	}

	// Target directory must exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("target directory %s does not exist: use --to for a fresh root", dir)
	}

	// Verify FIRST, always (§4.5.4 rule 1)
	manifest, zr, err := verifyAndOpenBundle(bundle)
	if err != nil {
		return err
	}
	defer zr.Close()

	// Pre-validate every member in bundle before ANY write
	if err := validateBundleMembers(&zr.Reader, manifest); err != nil {
		return err
	}

	// b. Free-space check BEFORE any lock (§4.5.4 rule 3):
	// root fs needs ≥ staged tree size Σ plus, when safety is requested, another Σ for the safety bundle
	treeSize := computeStagedTreeSize(manifest)
	requiredSpace := treeSize
	if !noSafety {
		requiredSpace += treeSize
	}

	rootFS, err := StatfsFunc(dir)
	if err != nil {
		rootFS, err = StatfsFunc(parentDir)
		if err != nil {
			return fmt.Errorf("statfs %s: %w", dir, err)
		}
	}
	if rootFS.AvailableBytes < requiredSpace {
		return fmt.Errorf("insufficient free space: required %d bytes, available %d bytes; rule: root fs requires ≥ staged tree size Σ (%d bytes)%s (PENATUS §4.5.4)",
			requiredSpace, rootFS.AvailableBytes, treeSize,
			func() string {
				if !noSafety {
					return " plus safety bundle Σ"
				}
				return ""
			}(),
		)
	}

	ts := formatTimestamp()

	// c. Safety bundle law (§4.5.4 rule 3): unless noSafety, bundle existing
	// root first; on failure print the reason and the exact --no-safety retry.
	var safetyPath string
	if !noSafety {
		var err error
		safetyPath, err = createSafetyBundle(bundle, dir, parentDir, baseName, ts)
		if err != nil {
			return err
		}
	}

	// d. Swap protocol (§4.5.4 rule 4)
	// Acquire all three flocks (AcquireLocks — backup.lock first, LOCK_NB, create-and-open 0600)
	locks, err := AcquireLocks(dir)
	if err != nil {
		return fmt.Errorf("acquire locks on %s: %w", dir, err)
	}
	defer locks.Release()

	// Write staging <dir>.staging-<ts> (0700) next to the target, fill, fsync
	stagingDir := filepath.Join(parentDir, fmt.Sprintf("%s.staging-%s", baseName, ts))
	if _, err := os.Stat(stagingDir); err == nil {
		stagingDir = fmt.Sprintf("%s-%d", stagingDir, time.Now().UnixNano())
	}

	if err := os.Mkdir(stagingDir, 0o700); err != nil {
		return fmt.Errorf("create staging dir %s: %w", stagingDir, err)
	}
	_ = os.Chmod(stagingDir, 0o700)

	if err := extractTree(&zr.Reader, manifest, stagingDir); err != nil {
		cleanStaging(stagingDir)
		return err
	}

	runSwapHook("staging-complete")

	// Swap marker: parent-directory dotfile .restore-<ts>.json recording
	// {staging, pre_swap, target, ts, safety}, fsync it, fsync parent dir
	preSwapDir := filepath.Join(parentDir, fmt.Sprintf("%s.pre-swap-%s", baseName, ts))
	markerPath := filepath.Join(parentDir, fmt.Sprintf(".restore-%s.json", ts))

	marker := RestoreMarker{
		Staging: stagingDir,
		PreSwap: preSwapDir,
		Target:  dir,
		TS:      ts,
		Safety:  safetyPath,
	}
	markerBytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		cleanStaging(stagingDir)
		return fmt.Errorf("marshal restore marker: %w", err)
	}

	mf, err := os.OpenFile(markerPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanStaging(stagingDir)
		return fmt.Errorf("create marker file %s: %w", markerPath, err)
	}
	if _, err := mf.Write(markerBytes); err != nil {
		mf.Close()
		cleanStaging(stagingDir)
		return fmt.Errorf("write marker file: %w", err)
	}
	if err := mf.Sync(); err != nil {
		mf.Close()
		cleanStaging(stagingDir)
		return fmt.Errorf("sync marker file: %w", err)
	}
	if err := mf.Close(); err != nil {
		cleanStaging(stagingDir)
		return fmt.Errorf("close marker file: %w", err)
	}
	fsyncDir(parentDir)

	runSwapHook("post-marker/pre-rename-1")

	// rename-1: dir → <dir>.pre-swap-<ts>
	if err := os.Rename(dir, preSwapDir); err != nil {
		cleanStaging(stagingDir)
		_ = os.Remove(markerPath)
		fsyncDir(parentDir)
		return fmt.Errorf("swap rename-1 dir to pre-swap: %w", err)
	}

	runSwapHook("post-rename-1/pre-rename-2")

	// rename-2: staging → dir
	if err := os.Rename(stagingDir, dir); err != nil {
		return fmt.Errorf("swap rename-2 staging to dir: %w", err)
	}
	fsyncDir(parentDir)

	runSwapHook("post-rename-2/pre-marker-delete")

	// Delete marker LAST visible act
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("delete marker: %w", err)
	}
	fsyncDir(parentDir)

	// Release locks (defer will also do this, but explicit release keeps discipline)
	locks.Release()

	// f. Print pre-swap sibling path to stdout and leave deletion to operator
	fmt.Println(preSwapDir)

	return nil
}

// createSafetyBundle bundles the existing root to <dir>.pre-restore-<ts>.lararium-backup
// via slice-1 CreateOffline (§4.5.4 rule 3). On failure it prints the reason and
// the exact retry command ending --no-safety, so a damaged source cannot lock out
// recovery, then returns the wrapped error.
func createSafetyBundle(bundle, dir, parentDir, baseName, ts string) (string, error) {
	safetyPath := filepath.Join(parentDir, fmt.Sprintf("%s.pre-restore-%s.lararium-backup", baseName, ts))

	cfgPath := ""
	noConfig := true
	for _, candidate := range []string{
		filepath.Join(dir, "lararium.yaml"),
		filepath.Join(parentDir, "lararium.yaml"),
		"lararium.yaml",
	} {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			cfgPath = candidate
			noConfig = false
			break
		}
	}

	if err := CreateOffline(dir, cfgPath, safetyPath, noConfig, nil); err != nil {
		_ = os.Remove(safetyPath)
		fmt.Fprintf(os.Stderr, "safety bundle failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "hearthd restore %s --replace %s --yes --no-safety\n", bundle, dir)
		return "", fmt.Errorf("safety bundle failed: %w", err)
	}
	return safetyPath, nil
}

// RecoverSwap evaluates the §4.5.4 rule 4 recovery table against root's parent directory.
// Implement the six table rows EXACTLY as written.
func RecoverSwap(root string) error {
	root = filepath.Clean(root)
	absRoot, err := filepath.Abs(root)
	if err == nil {
		root = absRoot
	}
	parentDir := filepath.Dir(root)

	matches, err := filepath.Glob(filepath.Join(parentDir, ".restore-*.json"))
	if err != nil {
		return fmt.Errorf("glob restore markers: %w", err)
	}

	// Row 1: no marker | nothing interrupted | normal operation; siblings ignored
	if len(matches) == 0 {
		return nil
	}

	for _, markerFile := range matches {
		data, err := os.ReadFile(markerFile)
		if err != nil {
			// Marker unreadable → refuse, print contents + rm <marker> escape hatch
			fmt.Fprintf(os.Stderr, "unreadable restore marker %s: %v\nescape hatch: rm %s\n", markerFile, err, markerFile)
			return fmt.Errorf("unreadable restore marker %s: remove with 'rm %s' to proceed", markerFile, markerFile)
		}

		var marker RestoreMarker
		if err := json.Unmarshal(data, &marker); err != nil {
			// Marker corrupt → refuse, print contents + rm <marker> escape hatch
			fmt.Fprintf(os.Stderr, "corrupt restore marker %s contents:\n%s\nescape hatch: rm %s\n", markerFile, string(data), markerFile)
			return fmt.Errorf("corrupt restore marker %s: remove with 'rm %s' to proceed", markerFile, markerFile)
		}

		// Only process markers whose target matches root
		absTarget, err := filepath.Abs(marker.Target)
		if err != nil {
			absTarget = marker.Target
		}
		if filepath.Clean(absTarget) != filepath.Clean(root) && filepath.Clean(marker.Target) != filepath.Clean(root) {
			continue
		}

		_, errDir := os.Lstat(root)
		dirExists := (errDir == nil)

		_, errStaging := os.Lstat(marker.Staging)
		stagingExists := (errStaging == nil)

		_, errPreSwap := os.Lstat(marker.PreSwap)
		preSwapExists := (errPreSwap == nil)

		// Row 2: marker, pre_swap absent (whether or not dir and staging exist)
		// died before rename-1 — swap never touched the root
		// action: delete staging + marker, proceed normally
		if !preSwapExists {
			if stagingExists {
				cleanStaging(marker.Staging)
			}
			_ = os.Remove(markerFile)
			fsyncDir(parentDir)
			continue
		}

		// Row 3: marker, dir absent, staging + pre_swap present
		// died in the rename window
		// action: complete: rename staging→dir, fsync parent, report, delete marker
		if !dirExists && stagingExists && preSwapExists {
			if err := os.Rename(marker.Staging, root); err != nil {
				return fmt.Errorf("recover swap complete: rename staging to dir: %w", err)
			}
			fsyncDir(parentDir)
			fmt.Fprintf(os.Stderr, "recovered swap: renamed %s to %s\n", marker.Staging, root)
			if err := os.Remove(markerFile); err != nil {
				return fmt.Errorf("recover swap complete: delete marker: %w", err)
			}
			fsyncDir(parentDir)
			continue
		}

		// Row 4: marker, dir present, staging absent
		// died after rename-2, before marker delete
		// action: delete marker; done (pre_swap remains as pre-restore root)
		if dirExists && !stagingExists {
			if err := os.Remove(markerFile); err != nil {
				return fmt.Errorf("recover swap: delete marker: %w", err)
			}
			fsyncDir(parentDir)
			continue
		}

		// Row 5: marker, dir + staging present, pre_swap present
		// impossible — pre_swap + dir together means rename-1 was undone by an operator; staging present means rename-2 hadn't run
		// action: refuse, print marker contents, print rm <marker> escape hatch
		if dirExists && stagingExists && preSwapExists {
			fmt.Fprintf(os.Stderr, "impossible swap recovery state: dir, staging, and pre_swap all present\nmarker contents (%s):\n%s\nescape hatch: rm %s\n", markerFile, string(data), markerFile)
			return fmt.Errorf("impossible swap recovery state in marker %s: remove with 'rm %s' to proceed", markerFile, markerFile)
		}

		// Row 6: marker, dir absent, staging absent (pre_swap either)
		// operator deleted staging mid-flight, or crash before staging completed — no claimable payload exists
		// action: refuse, print marker contents + rm <marker> escape; never resurrect silently
		if !dirExists && !stagingExists {
			fmt.Fprintf(os.Stderr, "unrecoverable swap recovery state: dir and staging both absent\nmarker contents (%s):\n%s\nescape hatch: rm %s\n", markerFile, string(data), markerFile)
			return fmt.Errorf("unrecoverable swap recovery state in marker %s: remove with 'rm %s' to proceed", markerFile, markerFile)
		}
	}

	return nil
}

// verifyAndOpenBundle runs full Verify FIRST, enforces version law (§4.5.4 rule 1),
// and returns the manifest and opened zip.Reader.
func verifyAndOpenBundle(bundlePath string) (*Manifest, *zip.ReadCloser, error) {
	// Full slice-1 Verify; warnings and dropped fields go to os.Stderr
	findings, err := Verify(bundlePath, io.Discard, os.Stderr)
	if err != nil {
		return nil, nil, err
	}

	hasTamper := false
	for _, f := range findings {
		switch f.Class {
		case "tamper":
			hasTamper = true
		case "quarantine":
			fmt.Fprintf(os.Stderr, "warning: bundle quarantine finding: %s: %s\n", f.Path, f.Detail)
		}
	}
	if hasTamper {
		return nil, nil, errors.New("backup bundle verification failed: tamper detected")
	}

	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open backup bundle: %w", err)
	}

	if len(zr.File) == 0 || zr.File[0].Name != "manifest.json" {
		zr.Close()
		return nil, nil, errors.New("invalid bundle: manifest.json must be strictly the first zip entry")
	}

	mf, err := zr.File[0].Open()
	if err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("open manifest.json: %w", err)
	}
	manifestBytes, err := io.ReadAll(mf)
	mf.Close()
	if err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("read manifest.json: %w", err)
	}

	manifest, _, _, err := ParseManifest(manifestBytes)
	if err != nil {
		zr.Close()
		return nil, nil, err
	}

	return manifest, zr, nil
}

// validateBundleMembers pre-validates every member in the bundle against zip-slip,
// bundle schema, NFC normalization, and manifest presence BEFORE writing ANY files (§4.5.4 rule 2, BK18).
func validateBundleMembers(zr *zip.Reader, manifest *Manifest) error {
	manifestByName := make(map[string]ManifestEntry, len(manifest.Entries))
	for _, e := range manifest.Entries {
		manifestByName[e.Path] = e
	}

	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			continue
		}
		if f.Name != "config/lararium.yaml" && !strings.HasPrefix(f.Name, "tree/") {
			return fmt.Errorf("crafted bundle refusal: member %q violates schema (must start with tree/ or be config/lararium.yaml)", f.Name)
		}
		if IsZipSlip(f.Name) {
			return fmt.Errorf("crafted bundle refusal: zip-slip violation in member %q", f.Name)
		}
		if norm.NFC.String(f.Name) != f.Name {
			return fmt.Errorf("crafted bundle refusal: member path %q is not NFC normalized", f.Name)
		}
		if _, ok := manifestByName[f.Name]; !ok {
			return fmt.Errorf("crafted bundle refusal: member %q in zip archive but missing from manifest", f.Name)
		}
	}
	return nil
}

func computeStagedTreeSize(manifest *Manifest) uint64 {
	var total uint64
	for _, e := range manifest.Entries {
		if !e.Dir && e.Size != nil && strings.HasPrefix(e.Path, "tree/") {
			total += uint64(*e.Size) //nolint:gosec // G115: manifest sizes are non-negative
		}
	}
	return total
}

// extractTree extracts all tree/ entries from manifest into stagingDir,
// preserving recorded modes, stripping privilege bits on files (04755 → 0755),
// round-tripping directory modes in post-order, and fsyncing all files and dirs.
func extractTree(zr *zip.Reader, manifest *Manifest, stagingDir string) error {
	zipByName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		zipByName[f.Name] = f
	}

	var dirTargets []dirTarget

	for _, entry := range manifest.Entries {
		if !strings.HasPrefix(entry.Path, "tree/") {
			// config/lararium.yaml is never extracted into the root (§4.5.2)
			continue
		}

		// NFC: restored paths are the manifest's NFC paths; never re-derive from zip header.
		rel := strings.TrimPrefix(entry.Path, "tree/")
		destPath := filepath.Join(stagingDir, filepath.FromSlash(rel))

		rawMode, err := strconv.ParseUint(entry.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("unparseable mode for %s: %w", entry.Path, err)
		}

		if entry.Dir {
			if err := os.MkdirAll(destPath, 0o700); err != nil {
				return fmt.Errorf("mkdir dir %s: %w", destPath, err)
			}
			dirTargets = append(dirTargets, dirTarget{
				path: destPath,
				mode: uint32(rawMode & 0o7777),
			})
			continue
		}

		// Regular file
		if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
			return fmt.Errorf("mkdir parent for %s: %w", destPath, err)
		}

		zf, ok := zipByName[entry.Path]
		if !ok {
			return fmt.Errorf("member %s missing from zip archive", entry.Path)
		}

		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("open zip member %s: %w", entry.Path, err)
		}

		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create file %s: %w", destPath, err)
		}

		maxSize := int64(64 << 30) // 64 GiB ceiling
		if entry.Size != nil {
			maxSize = *entry.Size + (1 << 20)
		}
		_, copyErr := io.Copy(out, io.LimitReader(rc, maxSize))
		rc.Close()
		if copyErr != nil {
			out.Close()
			return fmt.Errorf("copy zip member %s: %w", entry.Path, copyErr)
		}

		if err := out.Sync(); err != nil {
			out.Close()
			return fmt.Errorf("sync file %s: %w", destPath, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close file %s: %w", destPath, err)
		}

		// Mode law (§4.5.2, BK13): recorded modes are masked & 0777 after stripping setuid/setgid
		fileMode := ExtractionMode(os.FileMode(rawMode))
		if err := syscall.Chmod(destPath, uint32(fileMode)); err != nil {
			return fmt.Errorf("chmod file %s: %w", destPath, err)
		}
	}

	// Apply directory modes in post-order (deepest directories first)
	sort.Slice(dirTargets, func(i, j int) bool {
		return len(dirTargets[i].path) > len(dirTargets[j].path)
	})
	for _, dt := range dirTargets {
		if err := syscall.Chmod(dt.path, dt.mode); err != nil {
			return fmt.Errorf("post-order chmod dir %s: %w", dt.path, err)
		}
	}

	// Fsync every directory in stagingDir
	_ = filepath.Walk(stagingDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			fsyncDir(p)
		}
		return nil
	})
	fsyncDir(stagingDir)

	// Root dir ends 0700 (§4.5.4 rule 2, 5)
	return os.Chmod(stagingDir, 0o700)
}

func formatTimestamp() string {
	return fmt.Sprintf("%s-%06d", time.Now().UTC().Format("20060102T150405Z"), time.Now().Nanosecond()/1000)
}
