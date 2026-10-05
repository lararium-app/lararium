// Package custos implements the credential custody vault core per CUSTOS-SPEC.
package custos

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AuditRecord represents one event in the hash-chained audit log per CUSTOS-SPEC §8.1.
type AuditRecord struct {
	T        string   `json:"t"`
	Kind     string   `json:"kind"`
	Cred     string   `json:"cred,omitempty"`
	Sur      string   `json:"sur,omitempty"`
	Actor    string   `json:"actor,omitempty"`
	Host     string   `json:"host,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Verdict  string   `json:"verdict,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Gen      int64    `json:"gen,omitempty"`
	PrevHash string   `json:"prev_hash"`
	Nonce    string   `json:"nonce,omitempty"`
	Names    []string `json:"names,omitempty"`
}

// AnchorEntry represents one checkpoint in audit/anchors.json per CUSTOS-SPEC §8.3.
type AnchorEntry struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Hash      string `json:"hash"`
	AnchorMAC string `json:"anchor_mac"`
}

// AuditLogger manages the append-only hash-chained audit log.
type AuditLogger struct {
	stateDir string
	auditDir string
}

// NewAuditLogger returns an AuditLogger rooted at <hearth>/custos.
func NewAuditLogger(stateDir string) *AuditLogger {
	return &AuditLogger{
		stateDir: stateDir,
		auditDir: filepath.Join(stateDir, "audit"),
	}
}

// EnsureAuditDir creates <hearth>/custos/audit/ with mode 0700 per CUSTOS §C2.
func (a *AuditLogger) EnsureAuditDir() error {
	return os.MkdirAll(a.auditDir, 0o700)
}

// CurrentLogFileName returns the filename custos-YYYYMMDD.jsonl for t.
func CurrentLogFileName(t time.Time) string {
	return fmt.Sprintf("custos-%s.jsonl", t.UTC().Format("20060102"))
}

// ListLogFiles returns all *.jsonl files in audit/ sorted by filename.
func (a *AuditLogger) ListLogFiles() ([]string, error) {
	if err := a.EnsureAuditDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(a.auditDir)
	if err != nil {
		return nil, fmt.Errorf("read audit dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "custos-") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(a.auditDir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// ReadTailLine finds the last line across all audit log files and returns (lineBytes, filePath, lineNum, error).
// CUSTOS §8.1: each appender re-reads current tail line under flock to compute prev_hash.
func (a *AuditLogger) ReadTailLine() ([]byte, string, int, error) {
	files, err := a.ListLogFiles()
	if err != nil {
		return nil, "", 0, err
	}
	if len(files) == 0 {
		return nil, "", 0, nil
	}

	for i := len(files) - 1; i >= 0; i-- {
		filePath := files[i]
		lines, err := readNonEmptyLines(filePath)
		if err != nil {
			return nil, "", 0, err
		}
		if len(lines) > 0 {
			last := lines[len(lines)-1]
			return last.raw, filePath, last.num, nil
		}
	}
	return nil, "", 0, nil
}

// ReadTailRecord returns the unmarshaled last record in the audit log, or nil if empty.
func (a *AuditLogger) ReadTailRecord() (*AuditRecord, error) {
	tailBytes, _, _, err := a.ReadTailLine()
	if err != nil || len(tailBytes) == 0 {
		return nil, err
	}
	var rec AuditRecord
	if err := json.Unmarshal(tailBytes, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

type lineInfo struct {
	num int
	raw []byte
}

func readNonEmptyLines(filePath string) ([]lineInfo, error) {
	f, err := os.OpenFile(filePath, os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var result []lineInfo
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		b := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(b) > 0 {
			copied := make([]byte, len(b))
			copy(copied, b)
			result = append(result, lineInfo{num: lineNum, raw: copied})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Append appends a record to today's audit log file under the already-held custos.lock.
// CUSTOS §8.1: chain-writer serialization under custos.lock; re-read tail to compute prev_hash.
func (a *AuditLogger) Append(rec AuditRecord) error {
	if err := a.EnsureAuditDir(); err != nil {
		return err
	}

	tailBytes, _, _, err := a.ReadTailLine()
	if err != nil {
		return fmt.Errorf("read audit tail: %w", err)
	}

	if len(tailBytes) == 0 {
		// Genesis line per CUSTOS §8.1
		rec.PrevHash = ""
	} else {
		// CUSTOS §8.1: hash-chained (previous-line SHA-256)
		sum := sha256.Sum256(tailBytes)
		rec.PrevHash = hex.EncodeToString(sum[:])
	}

	if rec.T == "" {
		rec.T = time.Now().UTC().Format(time.RFC3339Nano)
	}

	data, err := json.Marshal(rec) //nolint:gosec // AuditRecord.Cred carries provider name, never secret material
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	data = append(data, '\n')

	todayFile := filepath.Join(a.auditDir, CurrentLogFileName(time.Now()))
	f, err := os.OpenFile(todayFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit file %s: %w", todayFile, err)
	}
	defer f.Close()
	_ = os.Chmod(todayFile, 0o600)

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	// Flushed and fsynced
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync audit file: %w", err)
	}
	return nil
}

// ComputeAnchorMAC computes HMAC-SHA256 over "file:line:hash" under HKDF(vault.key, "custos-anchor").
func ComputeAnchorMAC(instanceKey []byte, file string, line int, hash string) string {
	anchorKey := DeriveHKDF(instanceKey, HKDFInfoCustosAnchor)
	data := fmt.Sprintf("%s:%d:%s", file, line, hash)
	return ComputeMAC(anchorKey, []byte(data))
}

// VerifyAnchorMAC verifies the MAC of an AnchorEntry under HKDF(vault.key, "custos-anchor").
func VerifyAnchorMAC(instanceKey []byte, entry AnchorEntry) bool {
	expected := ComputeAnchorMAC(instanceKey, entry.File, entry.Line, entry.Hash)
	return subtleConstantTimeHexCompare(entry.AnchorMAC, expected)
}

func subtleConstantTimeHexCompare(a, b string) bool {
	ab, err1 := hex.DecodeString(a)
	bb, err2 := hex.DecodeString(b)
	if err1 != nil || err2 != nil || len(ab) != len(bb) {
		return false
	}
	return subtle.ConstantTimeCompare(ab, bb) == 1
}

// ReadAnchors loads audit/anchors.json. Missing or empty file returns empty slice without error.
func (a *AuditLogger) ReadAnchors() ([]AnchorEntry, error) {
	path := filepath.Join(a.auditDir, "anchors.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read anchors.json: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var anchors []AnchorEntry
	if err := json.Unmarshal(b, &anchors); err != nil {
		return nil, fmt.Errorf("parse anchors.json: %w", err)
	}
	return anchors, nil
}

// WriteAnchors writes entries to audit/anchors.json atomically (temp + 0600 + rename).
func (a *AuditLogger) WriteAnchors(anchors []AnchorEntry) error {
	if err := a.EnsureAuditDir(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(anchors, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal anchors: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(a.auditDir, "anchors-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp anchors: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp anchors: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp anchors: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp anchors: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp anchors: %w", err)
	}

	target := filepath.Join(a.auditDir, "anchors.json")
	return os.Rename(tmpName, target)
}

// BootstrapGenesisAnchor creates an anchor for the genesis line if anchors.json is missing or empty.
// CUSTOS §8.4: if anchors.json missing or empty, restore bootstraps it from genesis line.
func (a *AuditLogger) BootstrapGenesisAnchor(instanceKey []byte) error {
	anchors, err := a.ReadAnchors()
	if err != nil {
		return err
	}
	if len(anchors) > 0 {
		return nil // already has anchors
	}

	files, err := a.ListLogFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil // no log files yet
	}

	genesisFile := files[0]
	lines, err := readNonEmptyLines(genesisFile)
	if err != nil || len(lines) == 0 {
		return err
	}

	genesisLine := lines[0]
	sum := sha256.Sum256(genesisLine.raw)
	hashHex := hex.EncodeToString(sum[:])
	baseFile := filepath.Base(genesisFile)

	entry := AnchorEntry{
		File:      baseFile,
		Line:      genesisLine.num,
		Hash:      hashHex,
		AnchorMAC: ComputeAnchorMAC(instanceKey, baseFile, genesisLine.num, hashHex),
	}
	return a.WriteAnchors([]AnchorEntry{entry})
}

// VerifyResult reports the status of audit verification per CUSTOS-SPEC §8.3, §8.1a.
type VerifyResult struct {
	TotalRecords    int
	BrokenFile      string
	BrokenLine      int
	DanglingFile    string
	DanglingLine    int
	IsBroken        bool
	IsDangling      bool
	UncommittedLine string
}

// Format returns the exact frozen output string per CUSTOS-SPEC §8.3, §8.1a.
func (vr *VerifyResult) Format() string {
	if vr.IsBroken {
		// Frozen first-break file:line (§8.3)
		return fmt.Sprintf("%s:%d", vr.BrokenFile, vr.BrokenLine)
	}
	if vr.IsDangling {
		// Frozen diagnostic per §8.1a, §8.3: "uncommitted mutation at <file:line>"
		return fmt.Sprintf("uncommitted mutation at %s:%d", vr.DanglingFile, vr.DanglingLine)
	}
	// Frozen output per §8.3: "chain ok (<n> records)"
	return fmt.Sprintf("chain ok (%d records)", vr.TotalRecords)
}

type rawRecord struct {
	file string
	line int
	raw  []byte
	rec  AuditRecord
}

// Verify walks the audit chain from the newest anchor (or genesis) and checks hash continuity
// and uncommitted WAL intents per CUSTOS-SPEC §8.3, §8.1a.
// Can run file-read-only while locked without decrypting vault secrets.
func (a *AuditLogger) Verify(instanceKey []byte) (*VerifyResult, error) {
	files, err := a.ListLogFiles()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return &VerifyResult{TotalRecords: 0}, nil
	}

	// Read all records across files
	var allRecords []rawRecord
	for _, fpath := range files {
		lines, err := readNonEmptyLines(fpath)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(fpath)
		for _, li := range lines {
			var rec AuditRecord
			if err := json.Unmarshal(li.raw, &rec); err != nil {
				// Unparseable line is a break
				return &VerifyResult{ //nolint:nilerr // unparseable audit line is a chain break reported via VerifyResult
					IsBroken:   true,
					BrokenFile: base,
					BrokenLine: li.num,
				}, nil
			}
			allRecords = append(allRecords, rawRecord{
				file: base,
				line: li.num,
				raw:  li.raw,
				rec:  rec,
			})
		}
	}

	if len(allRecords) == 0 {
		return &VerifyResult{TotalRecords: 0}, nil
	}

	// Check anchors if available
	anchors, err := a.ReadAnchors()
	if err != nil {
		return nil, err
	}

	startIndex := 0
	if len(anchors) > 0 {
		newest := anchors[len(anchors)-1]
		// CUSTOS §8.3: verify newest anchor MAC first
		if instanceKey != nil && !VerifyAnchorMAC(instanceKey, newest) {
			// Anchor MAC invalid: reject
			return &VerifyResult{
				IsBroken:   true,
				BrokenFile: "audit/anchors.json",
				BrokenLine: len(anchors),
			}, nil
		}

		// Find the anchor point in allRecords
		found := false
		for i, r := range allRecords {
			if r.file == newest.File && r.line == newest.Line {
				sum := sha256.Sum256(r.raw)
				if hex.EncodeToString(sum[:]) != newest.Hash {
					return &VerifyResult{
						IsBroken:   true,
						BrokenFile: r.file,
						BrokenLine: r.line,
					}, nil
				}
				startIndex = i
				found = true
				break
			}
		}
		if !found {
			// Anchor points to nonexistent line
			return &VerifyResult{
				IsBroken:   true,
				BrokenFile: newest.File,
				BrokenLine: newest.Line,
			}, nil
		}
	}

	// Walk from startIndex (or 0) to end
	// If starting at 0, genesis record must have rec.PrevHash == ""
	if startIndex == 0 {
		if allRecords[0].rec.PrevHash != "" {
			return &VerifyResult{
				IsBroken:   true,
				BrokenFile: allRecords[0].file,
				BrokenLine: allRecords[0].line,
			}, nil
		}
	}

	// Track intents and resolution nonces
	type intentInfo struct {
		file string
		line int
	}
	intentsByNonce := make(map[string]intentInfo)
	resolvedNonces := make(map[string]bool)

	// First pass for intent tracking across the entire chain
	for _, r := range allRecords {
		rec := r.rec
		if rec.Nonce != "" {
			switch rec.Kind {
			case AuditKindVaultMutationIntent:
				intentsByNonce[rec.Nonce] = intentInfo{file: r.file, line: r.line}
			case AuditKindVaultMutation, AuditKindVaultMutationRecovered,
				AuditKindVaultMutationAborted, AuditKindVaultMutationSuperseded:
				resolvedNonces[rec.Nonce] = true
			}
		}
	}

	// Verify chain hashes from startIndex
	for i := startIndex + 1; i < len(allRecords); i++ {
		prev := allRecords[i-1]
		curr := allRecords[i]

		sum := sha256.Sum256(prev.raw)
		expectedPrevHash := hex.EncodeToString(sum[:])
		if curr.rec.PrevHash != expectedPrevHash {
			// First-break file:line per §8.3
			return &VerifyResult{
				IsBroken:   true,
				BrokenFile: curr.file,
				BrokenLine: curr.line,
			}, nil
		}
	}

	// Check for dangling intents (§8.1a, §8.3)
	for nonce, info := range intentsByNonce {
		if !resolvedNonces[nonce] {
			return &VerifyResult{
				IsDangling:      true,
				DanglingFile:    info.file,
				DanglingLine:    info.line,
				TotalRecords:    len(allRecords),
				UncommittedLine: fmt.Sprintf("%s:%d", info.file, info.line),
			}, nil
		}
	}

	return &VerifyResult{
		TotalRecords: len(allRecords),
	}, nil
}
