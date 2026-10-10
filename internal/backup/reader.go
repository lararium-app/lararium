package backup

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// IsZipSlip reports whether a relative path in a bundle violates the zip-slip law (§4.5.2, BK18):
// absolute path, contains '..', drive-shaped (e.g. C:).
func IsZipSlip(p string) bool {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return true
	}
	// Drive letter check (e.g. C:)
	if len(p) >= 2 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) && p[1] == ':' {
		return true
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return true
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == ".." {
			return true
		}
	}
	return false
}

// IsSecretPath matches the frozen secrets patterns per PENATUS-SPEC §4.5.2:
// tree/keys.json, tree/tokens.json, tree/*.token, tree/custos/vault.*,
// tree/custos/surrogates.age, tree/custos/fingerprints.json,
// tree/custos/snapshots/**, config/lararium.yaml.
func IsSecretPath(p string) bool {
	switch p {
	case "tree/keys.json", "tree/tokens.json", "tree/custos/surrogates.age", "tree/custos/fingerprints.json", "config/lararium.yaml":
		return true
	}
	if strings.HasPrefix(p, "tree/") && strings.HasSuffix(p, ".token") {
		rest := strings.TrimPrefix(p, "tree/")
		if !strings.Contains(rest, "/") {
			return true
		}
	}
	if strings.HasPrefix(p, "tree/custos/vault.") {
		rest := strings.TrimPrefix(p, "tree/custos/")
		if !strings.Contains(rest, "/") {
			return true
		}
	}
	// m17: snapshots/** matches children only, not the directory entry itself.
	if strings.HasPrefix(p, "tree/custos/snapshots/") && p != "tree/custos/snapshots/" {
		return true
	}
	return false
}

// ExtractionMode maps recorded source mode to extraction mode per §4.5.2 (B5, BK13):
// recorded modes are masked & 0777 after stripping setuid/setgid/sticky bits.
func ExtractionMode(mode os.FileMode) os.FileMode {
	return mode & 0o777
}

// List implements `hearthd backup list <file>`: TSV `#columns: path size sha256 mode` (dirs: size/sha256 = "-").
func List(bundlePath string, w io.Writer) error {
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return fmt.Errorf("open backup bundle %s: %w", bundlePath, err)
	}
	defer zr.Close()

	if len(zr.File) == 0 || zr.File[0].Name != "manifest.json" {
		return fmt.Errorf("invalid bundle: manifest.json must be strictly the first zip entry")
	}

	mf, err := zr.File[0].Open()
	if err != nil {
		return fmt.Errorf("read manifest.json: %w", err)
	}
	manifestData, err := io.ReadAll(mf)
	mf.Close()
	if err != nil {
		return fmt.Errorf("read manifest.json content: %w", err)
	}

	manifest, _, _, err := ParseManifest(manifestData)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "#columns: path size sha256 mode")
	for _, entry := range manifest.Entries {
		if entry.Dir {
			fmt.Fprintf(w, "%s\t-\t-\t%s\n", entry.Path, entry.Mode)
		} else {
			sz := int64(0)
			if entry.Size != nil {
				sz = *entry.Size
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", entry.Path, sz, entry.SHA256, entry.Mode)
		}
	}
	return nil
}

// ExtractConfig implements `hearthd backup extract-config <file> <path>` (§4.5.4, BK16):
// writes bundled yaml mode 0600, refuses existing <path>, clear error on --no-config bundles.
func ExtractConfig(bundlePath string, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("destination path %s already exists: extract-config refuses to overwrite", destPath)
	}

	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return fmt.Errorf("open backup bundle: %w", err)
	}
	defer zr.Close()

	var configZF *zip.File
	for _, f := range zr.File {
		if f.Name == "config/lararium.yaml" {
			configZF = f
			break
		}
	}
	if configZF == nil {
		return fmt.Errorf("backup bundle was created with --no-config: config/lararium.yaml is not present")
	}

	rc, err := configZF.Open()
	if err != nil {
		return fmt.Errorf("open config in bundle: %w", err)
	}
	defer rc.Close()

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create config file %s: %w", destPath, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		os.Remove(destPath)
		return fmt.Errorf("write config file: %w", err)
	}
	return os.Chmod(destPath, 0o600)
}
