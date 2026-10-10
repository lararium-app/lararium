package backup

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const SecretsHonestyLine = "a bundle is your disk — store it where your disk would be unsafe."

// Finding represents one verification row: tamper or quarantine.
type Finding struct {
	Class  string // "tamper" or "quarantine"
	Path   string
	Detail string
}

// Verify runs the full §4.5.2 verification suite on bundlePath.
// Findings are written to stdout in TSV format (#columns: class path detail).
// Derived secrets list and disk-safety warning are written to stderr.
// Returns findings, and any fatal reader error.
func Verify(bundlePath string, stdout io.Writer, stderr io.Writer) ([]Finding, error) {
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open backup bundle: %w", err)
	}
	defer zr.Close()

	var findings []Finding

	// 1. Check first entry is manifest.json
	if len(zr.File) == 0 {
		findings = append(findings, Finding{
			Class:  "tamper",
			Path:   "manifest.json",
			Detail: "bundle zip archive is empty",
		})
		printVerifyOutput(findings, stdout, stderr, nil)
		return findings, nil
	}

	manifestFileIndex := -1
	for i, f := range zr.File {
		if f.Name == "manifest.json" {
			manifestFileIndex = i
			break
		}
	}

	if manifestFileIndex != 0 {
		detail := "manifest.json must be strictly the first zip entry"
		if manifestFileIndex == -1 {
			detail = "manifest.json missing from zip archive"
		} else {
			detail = fmt.Sprintf("manifest.json at index %d, must be strictly index 0", manifestFileIndex)
		}
		findings = append(findings, Finding{
			Class:  "tamper",
			Path:   "manifest.json",
			Detail: detail,
		})
		if manifestFileIndex == -1 {
			printVerifyOutput(findings, stdout, stderr, nil)
			return findings, nil
		}
	}

	// 2. Read manifest
	mf, err := zr.File[manifestFileIndex].Open()
	if err != nil {
		findings = append(findings, Finding{
			Class:  "tamper",
			Path:   "manifest.json",
			Detail: fmt.Sprintf("open manifest.json: %v", err),
		})
		printVerifyOutput(findings, stdout, stderr, nil)
		return findings, nil
	}
	manifestBytes, err := io.ReadAll(mf)
	mf.Close()
	if err != nil {
		findings = append(findings, Finding{
			Class:  "tamper",
			Path:   "manifest.json",
			Detail: fmt.Sprintf("read manifest.json: %v", err),
		})
		printVerifyOutput(findings, stdout, stderr, nil)
		return findings, nil
	}

	manifest, droppedFields, warnings, err := ParseManifest(manifestBytes)
	if err != nil {
		// Version law: major != 1 refuse with pointer, or unparseable json
		// If unparseable json or major != 1:
		if strings.Contains(err.Error(), "major version must be 1") || strings.Contains(err.Error(), "unsupported backup format") {
			// Fatal format refusal
			return nil, err
		}
		findings = append(findings, Finding{
			Class:  "tamper",
			Path:   "manifest.json",
			Detail: err.Error(),
		})
		printVerifyOutput(findings, stdout, stderr, nil)
		return findings, nil
	}

	// Report drops and warnings to stderr
	for _, drop := range droppedFields {
		fmt.Fprintf(stderr, "report drop: unknown manifest field %s\n", drop)
	}
	for _, warn := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warn)
	}

	// 3. Map zip files and manifest entries
	zipByName := make(map[string]*zip.File)
	for _, f := range zr.File {
		zipByName[f.Name] = f
	}

	manifestByName := make(map[string]ManifestEntry)
	for _, e := range manifest.Entries {
		manifestByName[e.Path] = e
	}

	// Track entries with tamper findings so they are never classified quarantine
	tamperedPaths := make(map[string]bool)

	// Check Zip -> Manifest (zip-slip & extra zip entries)
	for _, zf := range zr.File {
		if zf.Name == "manifest.json" {
			continue
		}

		// Bundle schema at verify (M15): every member path must start with "tree/" or be "config/lararium.yaml"
		if zf.Name != "config/lararium.yaml" && !strings.HasPrefix(zf.Name, "tree/") {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   zf.Name,
				Detail: "malformed bundle member path: must start with tree/ or be config/lararium.yaml or manifest.json",
			})
			tamperedPaths[zf.Name] = true
			continue
		}

		// Zip-slip law (§4.5.2, BK18): reject .., absolute, drive-shaped
		if IsZipSlip(zf.Name) {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   zf.Name,
				Detail: "illegal path: escapes target root (zip-slip law violation)",
			})
			tamperedPaths[zf.Name] = true
			continue
		}

		// NFC normalization law (§4.5.2, BK14)
		if norm.NFC.String(zf.Name) != zf.Name {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   zf.Name,
				Detail: "path is not NFC normalized",
			})
			tamperedPaths[zf.Name] = true
		}

		if _, ok := manifestByName[zf.Name]; !ok {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   zf.Name,
				Detail: "entry in zip archive but missing from manifest",
			})
			tamperedPaths[zf.Name] = true
		}
	}

	// Check Manifest -> Zip (hash, size, mode, missing from zip)
	for _, entry := range manifest.Entries {
		// Bundle schema check on manifest entries (M15)
		if entry.Path != "config/lararium.yaml" && !strings.HasPrefix(entry.Path, "tree/") {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: "malformed manifest member path: must start with tree/ or be config/lararium.yaml",
			})
			tamperedPaths[entry.Path] = true
		}

		zf, exists := zipByName[entry.Path]
		if !exists {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: "entry in manifest but missing from zip archive",
			})
			tamperedPaths[entry.Path] = true
			continue
		}

		// Check mode (B6: unparseable mode string in a manifest entry → tamper finding)
		expectedMode, err := ParseOctalMode(entry.Mode)
		if err != nil {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: fmt.Sprintf("unparseable mode string %q: %v", entry.Mode, err),
			})
			tamperedPaths[entry.Path] = true
		} else {
			zipPerm := zf.Mode() & 0o7777
			expectedPerm := expectedMode & 0o7777
			if zipPerm != expectedPerm {
				findings = append(findings, Finding{
					Class:  "tamper",
					Path:   entry.Path,
					Detail: fmt.Sprintf("mode mismatch: manifest %s, zip %04o", entry.Mode, zipPerm),
				})
				tamperedPaths[entry.Path] = true
			}
		}

		if entry.Dir {
			// Directory entry: must end in '/' and have no hash/size
			if !strings.HasSuffix(entry.Path, "/") || !zf.FileInfo().IsDir() {
				findings = append(findings, Finding{
					Class:  "tamper",
					Path:   entry.Path,
					Detail: "directory entry shape mismatch",
				})
				tamperedPaths[entry.Path] = true
			}
			if entry.SHA256 != "" || entry.Size != nil {
				findings = append(findings, Finding{
					Class:  "tamper",
					Path:   entry.Path,
					Detail: "directory entry must carry no sha256 or size",
				})
				tamperedPaths[entry.Path] = true
			}
			continue
		}

		// Regular file: verify hash and size
		rc, err := zf.Open()
		if err != nil {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: fmt.Sprintf("open zip member: %v", err),
			})
			tamperedPaths[entry.Path] = true
			continue
		}

		hasher := sha256.New()
		sz, copyErr := io.Copy(hasher, rc)
		rc.Close()
		if copyErr != nil {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: fmt.Sprintf("read zip member content: %v", copyErr),
			})
			tamperedPaths[entry.Path] = true
			continue
		}

		if entry.Size == nil || sz != *entry.Size {
			actualSize := sz
			expSize := int64(-1)
			if entry.Size != nil {
				expSize = *entry.Size
			}
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: fmt.Sprintf("size mismatch: manifest %d, zip %d", expSize, actualSize),
			})
			tamperedPaths[entry.Path] = true
		}

		actualHash := hex.EncodeToString(hasher.Sum(nil))
		if actualHash != entry.SHA256 {
			findings = append(findings, Finding{
				Class:  "tamper",
				Path:   entry.Path,
				Detail: fmt.Sprintf("hash mismatch: manifest %s, zip %s", entry.SHA256, actualHash),
			})
			tamperedPaths[entry.Path] = true
		}
	}

	// 4. Quarantine checks on session transcripts (§4.5.2, BK3):
	// Parse every tree/sessions/*/events.jsonl.
	// quarantine = logical damage the bytes themselves record: seq gap/duplicate,
	// or unparseable lines in a file that nonetheless matches its hash.
	// Content-level damage can never be tamper; manifest mismatch can never be quarantine.
	for _, entry := range manifest.Entries {
		if !isEventsJSONL(entry.Path) {
			continue
		}
		// If this file had any tamper findings (hash mismatch, missing, etc.), skip quarantine!
		if tamperedPaths[entry.Path] {
			continue
		}

		zf, ok := zipByName[entry.Path]
		if !ok {
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(rc)
		expectedSeq := int64(1)
		lineNum := 0
		hasCorrupt := false

		for scanner.Scan() {
			lineNum++
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var evt struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				findings = append(findings, Finding{
					Class:  "quarantine",
					Path:   entry.Path,
					Detail: fmt.Sprintf("unparseable line %d: %v", lineNum, err),
				})
				hasCorrupt = true
				break
			}

			if evt.Seq != expectedSeq {
				if evt.Seq < expectedSeq {
					findings = append(findings, Finding{
						Class:  "quarantine",
						Path:   entry.Path,
						Detail: fmt.Sprintf("sequence duplicate at line %d: got seq %d, expected %d", lineNum, evt.Seq, expectedSeq),
					})
				} else {
					findings = append(findings, Finding{
						Class:  "quarantine",
						Path:   entry.Path,
						Detail: fmt.Sprintf("sequence gap at line %d: got seq %d, expected %d", lineNum, evt.Seq, expectedSeq),
					})
				}
				hasCorrupt = true
				break
			}
			expectedSeq++
		}
		rc.Close()
		_ = hasCorrupt
	}

	// 5. Secret derivation:
	// Secrets derived from the frozen path patterns, never manifest (§4.5.2, BK2).
	var matchedSecrets []string
	for _, entry := range manifest.Entries {
		if IsSecretPath(entry.Path) {
			matchedSecrets = append(matchedSecrets, entry.Path)
		}
	}

	printVerifyOutput(findings, stdout, stderr, matchedSecrets)
	return findings, nil
}

func isEventsJSONL(p string) bool {
	// tree/sessions/<session-id>/events.jsonl
	parts := strings.Split(p, "/")
	return len(parts) == 4 && parts[0] == "tree" && parts[1] == "sessions" && parts[3] == "events.jsonl"
}

func printVerifyOutput(findings []Finding, stdout io.Writer, stderr io.Writer, secrets []string) {
	// Stdout: findings only! Header #columns: class path detail, exit 0 iff no rows (§4.5.2, rev-4 F4).
	if len(findings) > 0 {
		fmt.Fprintln(stdout, "#columns: class path detail")
		for _, f := range findings {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", f.Class, f.Path, f.Detail)
		}
	}

	// Stderr: derived-secrets list + disk-safety warning to stderr
	if secrets != nil {
		fmt.Fprintln(stderr, "derived secrets:")
		for _, s := range secrets {
			fmt.Fprintf(stderr, "  %s\n", s)
		}
	}
	fmt.Fprintln(stderr, SecretsHonestyLine)
}
