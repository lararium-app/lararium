package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FormatVersion is the bundle format identifier per PENATUS-SPEC §4.5.2.
const FormatVersion = "backup/1.0"

// Version is the daemon version (can be set via ldflags), matching daemon /health (m16).
var Version = "0.8.0"

// Product is hearthd/<version> per §4.5.2 (m16).
var Product = "hearthd/" + Version

// Manifest is the manifest.json model per PENATUS-SPEC §4.5.2.
type Manifest struct {
	Format  string          `json:"format"`
	Created string          `json:"created"`
	Product string          `json:"product"`
	Source  SourceInfo      `json:"source"`
	Entries []ManifestEntry `json:"entries"`
	Counts  CountsInfo      `json:"counts"`
}

// SourceInfo records the origin of a bundle per §4.5.2.
type SourceInfo struct {
	AbsPath  string `json:"abs_path"`
	Hostname string `json:"hostname"`
}

// ManifestEntry describes one bundle member per §4.5.2.
type ManifestEntry struct {
	Path   string `json:"path"`
	Dir    bool   `json:"dir,omitempty"`
	Mode   string `json:"mode"`
	Size   *int64 `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// CountsInfo carries manifest summary counts per §4.5.2.
type CountsInfo struct {
	Sessions    int `json:"sessions"`
	MemoryFiles int `json:"memory_files"`
}

// ComputeCounts computes the counts per §4.5.2:
// sessions = dirs under tree/sessions/,
// memory_files = tree/memory/**/*.md — recursive.
func ComputeCounts(root string) (CountsInfo, error) {
	var counts CountsInfo

	sessDir := filepath.Join(root, "sessions")
	if entries, err := os.ReadDir(sessDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				counts.Sessions++
			}
		}
	}

	memDir := filepath.Join(root, "memory")
	if _, err := os.Stat(memDir); err == nil {
		_ = filepath.Walk(memDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				// Best-effort count: unreadable subtrees simply contribute zero.
				return nil //nolint:nilerr // walk errors are swallowed by design
			}
			if strings.HasSuffix(info.Name(), ".md") {
				counts.MemoryFiles++
			}
			return nil
		})
	}

	return counts, nil
}

// NewManifest constructs a Manifest with current metadata and computed counts.
func NewManifest(absPath string, hostname string, entries []ManifestEntry, counts CountsInfo) *Manifest {
	if hostname == "" {
		h, err := os.Hostname()
		if err == nil {
			hostname = h
		} else {
			hostname = "localhost"
		}
	}
	return &Manifest{
		Format:  FormatVersion,
		Created: time.Now().UTC().Format(time.RFC3339),
		Product: Product,
		Source: SourceInfo{
			AbsPath:  absPath,
			Hostname: hostname,
		},
		Entries: entries,
		Counts:  counts,
	}
}

// ParseManifest decodes manifest.json, enforces format-version law (§4.5.2, §4.5.4, BK7),
// and returns the manifest along with any dropped unknown field reports and warnings.
func ParseManifest(data []byte) (*Manifest, []string, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, nil, fmt.Errorf("manifest json invalid: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, nil, fmt.Errorf("manifest unmarshal: %w", err)
	}

	// Version law (§4.5.2, §4.5.4, BK7):
	// format must be "backup/<major>.<minor>"
	if !strings.HasPrefix(m.Format, "backup/") {
		return nil, nil, nil, fmt.Errorf("unsupported backup format %q: missing backup/ prefix (see docs/PENATUS-SPEC.md §4.5)", m.Format)
	}
	verStr := strings.TrimPrefix(m.Format, "backup/")
	parts := strings.Split(verStr, ".")
	if len(parts) != 2 {
		return nil, nil, nil, fmt.Errorf("unsupported backup format %q: invalid major.minor version (see docs/PENATUS-SPEC.md §4.5)", m.Format)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major != 1 {
		return nil, nil, nil, fmt.Errorf("unsupported backup format %q: major version must be 1 (see docs/PENATUS-SPEC.md §4.5 for migration)", m.Format)
	}

	var droppedFields []string
	var warnings []string

	// Detect top-level unknown fields to report drops
	knownTop := map[string]bool{
		"format":  true,
		"created": true,
		"product": true,
		"source":  true,
		"entries": true,
		"counts":  true,
	}
	for k := range raw {
		if !knownTop[k] {
			droppedFields = append(droppedFields, fmt.Sprintf("top-level field %q", k))
		}
	}

	// Detect unknown fields in raw entries
	if rawEntries, ok := raw["entries"]; ok {
		var entryList []map[string]json.RawMessage
		if err := json.Unmarshal(rawEntries, &entryList); err == nil {
			knownEntry := map[string]bool{
				"path":   true,
				"dir":    true,
				"mode":   true,
				"size":   true,
				"sha256": true,
			}
			for i, e := range entryList {
				entryPath := ""
				if pRaw, ok := e["path"]; ok {
					_ = json.Unmarshal(pRaw, &entryPath)
				}
				if entryPath == "" {
					entryPath = fmt.Sprintf("index %d", i)
				}
				for ek := range e {
					if !knownEntry[ek] {
						droppedFields = append(droppedFields, fmt.Sprintf("entry field %q in %s", ek, entryPath))
					}
				}
			}
		}
	}

	// Check product version: if newer than binary, warn
	if isProductNewer(m.Product, Product) {
		warnings = append(warnings, fmt.Sprintf("bundle product %q is newer than binary %q", m.Product, Product))
	}

	return &m, droppedFields, warnings, nil
}

func isProductNewer(bundleProd, binaryProd string) bool {
	// e.g. "hearthd/0.7.3" vs "hearthd/0.8.0"
	bp := strings.TrimPrefix(bundleProd, "hearthd/")
	cp := strings.TrimPrefix(binaryProd, "hearthd/")
	if bp == cp {
		return false
	}
	// Parse semver if possible
	bParts := strings.Split(bp, ".")
	cParts := strings.Split(cp, ".")
	for i := 0; i < len(bParts) && i < len(cParts); i++ {
		bNum, bErr := strconv.Atoi(bParts[i])
		cNum, cErr := strconv.Atoi(cParts[i])
		if bErr != nil || cErr != nil {
			return false // m18: on parse failure, NO warning (skip comparison)
		}
		if bNum > cNum {
			return true
		}
		if bNum < cNum {
			return false
		}
	}
	return false
}
