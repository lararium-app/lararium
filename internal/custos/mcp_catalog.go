// MCP tool catalog types, naming normalization, text sanitization,
// RFC 8785 (JCS) canonicalization, and snapshot management per MCP-SPEC
// §4.2, §4.3, §4.5, §11, and CUSTOS-SPEC §5.1 / §6.5 / §8.1.

package custos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP rail audit kinds per MCP-SPEC A8 and CUSTOS-SPEC §8.1.
const (
	AuditKindMCPConfigLiteralEnv = "mcp_config_literal_env"
	AuditKindMCPToolDropped      = "mcp_tool_dropped"
	AuditKindMCPCatalogChanged   = "mcp_catalog_changed"
	AuditKindMCPResultTruncated  = "mcp_result_truncated"
	AuditKindMCPAlwaysRevoked    = "mcp_always_revoked"
	AuditKindMCPRefresh          = "mcp_refresh"
)

// Truncation audit token names per MCP-SPEC §4.5 / amendment A8.
const (
	TruncationTokenBlocksCollapsed       = "blocks_collapsed"
	TruncationTokenTextTruncated         = "text_truncated" //nolint:gosec // audit token name, not a credential
	TruncationTokenStructuredContentDrop = "structuredContent_dropped"
)

// ToolInfo is one connector tool description row per MCP-SPEC A1 / §4.2.
type ToolInfo struct {
	Connector string `json:"connector"`
	Name      string `json:"name"`
	Title     string `json:"title"`
}

// ToolLister is the discovery interface per MCP-SPEC A1. Rail handles
// and GmailConnector implement it to expose their catalog without schemas.
type ToolLister interface {
	ListTools() []ToolInfo
}

// NormalizeToolName applies the normalization pipeline per MCP-SPEC §4.2:
// 1. lowercase
// 2. non-[a-z0-9_-] replaced with _
// 3. if the result is empty, drop (checked before prefix)
// 4. while the first character is not [a-z0-9], prefix "t-"
// 5. truncate to 64 runes.
// Returns normalized name and true if kept, or ("", false) if dropped.
func NormalizeToolName(raw string) (string, bool) {
	lower := strings.ToLower(raw)

	var b strings.Builder
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()

	// Emptiness check runs BEFORE t- prefix: only raw "" drops.
	if s == "" {
		return "", false
	}

	for len(s) > 0 {
		c := s[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			break
		}
		s = "t-" + s
	}

	runes := []rune(s)
	if len(runes) > 64 {
		s = string(runes[:64])
	}
	return s, true
}

// SanitizeText sanitizes title or description text per MCP-SPEC §4.2:
// CR/LF/TAB -> single space, C0/C1 control characters removed,
// whitespace runs collapsed, credential-scrubbed, then truncated to
// maxRunes with '…' appended if it exceeds maxRunes (total <= maxRunes runes).
// Title/description use maxRunes = 120 (119 runes + '…').
func SanitizeText(s string, needles []string, maxRunes int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteRune(' ')
		case (r >= 0x00 && r <= 0x1F) || r == 0x7F || (r >= 0x80 && r <= 0x9F) || r == utf8.RuneError:
			// Remove C0 and C1 control characters and invalid byte sequences
			continue
		default:
			b.WriteRune(r)
		}
	}

	// Collapse whitespace runs
	var collapsed strings.Builder
	inSpace := false
	for _, r := range b.String() {
		if r == ' ' {
			if !inSpace {
				collapsed.WriteRune(' ')
				inSpace = true
			}
		} else {
			collapsed.WriteRune(r)
			inSpace = false
		}
	}

	cleaned := strings.TrimSpace(collapsed.String())

	// Scrub-before-truncate law: scrub precedes truncation at every site.
	scrubbed := ScrubString(cleaned, needles)

	runes := []rune(scrubbed)
	if maxRunes > 0 && len(runes) > maxRunes {
		cut := maxRunes - 1
		if cut < 0 {
			cut = 0
		}
		return string(runes[:cut]) + "…"
	}
	return scrubbed
}

// utf16CodeUnits converts a string into UTF-16 code units for RFC 8785 sorting.
func utf16CodeUnits(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

// compareUTF16 compares two strings by UTF-16 code units per RFC 8785 §3.2.3.
func compareUTF16(a, b string) int {
	ua := utf16CodeUnits(a)
	ub := utf16CodeUnits(b)
	minLen := len(ua)
	if len(ub) < minLen {
		minLen = len(ub)
	}
	for i := range minLen {
		if ua[i] < ub[i] {
			return -1
		}
		if ua[i] > ub[i] {
			return 1
		}
	}
	return len(ua) - len(ub)
}

// CanonicalJSON marshals v into RFC 8785 (JCS) canonical JSON.
func CanonicalJSON(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeCanonicalJSON(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeCanonicalJSON(b *strings.Builder, v any) error {
	if v == nil {
		b.WriteString("null")
		return nil
	}

	switch val := v.(type) {
	case bool:
		if val {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil

	case string:
		writeCanonicalString(b, val)
		return nil

	case json.Number:
		b.WriteString(val.String())
		return nil

	case int:
		b.WriteString(strconv.FormatInt(int64(val), 10))
		return nil
	case int8:
		b.WriteString(strconv.FormatInt(int64(val), 10))
		return nil
	case int16:
		b.WriteString(strconv.FormatInt(int64(val), 10))
		return nil
	case int32:
		b.WriteString(strconv.FormatInt(int64(val), 10))
		return nil
	case int64:
		b.WriteString(strconv.FormatInt(val, 10))
		return nil

	case uint:
		b.WriteString(strconv.FormatUint(uint64(val), 10))
		return nil
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(val), 10))
		return nil
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(val), 10))
		return nil
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(val), 10))
		return nil
	case uint64:
		b.WriteString(strconv.FormatUint(val, 10))
		return nil

	case float32:
		return writeCanonicalFloat(b, float64(val))
	case float64:
		return writeCanonicalFloat(b, val)

	case []any:
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonicalJSON(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil

	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return compareUTF16(keys[i], keys[j]) < 0
		})
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonicalJSON(b, val[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil

	case json.RawMessage:
		var parsed any
		if len(val) == 0 {
			b.WriteString("null")
			return nil
		}
		if err := json.Unmarshal(val, &parsed); err != nil {
			return fmt.Errorf("canonical json unmarshal raw message: %w", err)
		}
		return writeCanonicalJSON(b, parsed)

	default:
		// Convert struct or typed map through generic JSON serialization.
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("canonical json marshal: %w", err)
		}
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			return fmt.Errorf("canonical json unmarshal: %w", err)
		}
		return writeCanonicalJSON(b, parsed)
	}
}

func writeCanonicalString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func writeCanonicalFloat(b *strings.Builder, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("canonical json: NaN and Infinity are forbidden")
	}
	// Check if integer
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		b.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// ToolFingerprint computes SHA-256 over RFC 8785 canonical JSON of
// {annotations, connector, description, inputSchema, name, title} per MCP-SPEC §4.3 (F5).
// Comparison input is collision-free across connectors and tools; display truncation stays display-only.
func ToolFingerprint(connector, name, title, description string, inputSchema any, annotations any) (string, error) {
	payload := map[string]any{
		"annotations": annotations,
		"connector":   connector,
		"description": description,
		"inputSchema": inputSchema,
		"name":        name,
		"title":       title,
	}
	data, err := CanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// SnapshotTool represents one tool in a CatalogSnapshot.
type SnapshotTool struct {
	RawName     string
	Canonical   string
	Title       string
	Description string
	InputSchema any
	Annotations *mcp.ToolAnnotations
	Fingerprint string
}

// CatalogSnapshot is an immutable tool catalog per MCP-SPEC §4.3.
type CatalogSnapshot struct {
	Connector    string
	Tools        []SnapshotTool
	ByName       map[string]*SnapshotTool
	ToolInfos    []ToolInfo
	Fingerprints map[string]string
	RawToCanon   map[string]string
	CanonToRaw   map[string]string
	DroppedTools []string
	CreatedAt    time.Time
	TTL          time.Duration
	ExpiresAt    time.Time
}

// HasTool reports whether canonical tool name exists in the snapshot.
func (s *CatalogSnapshot) HasTool(canonicalName string) bool {
	if s == nil || s.ByName == nil {
		return false
	}
	_, ok := s.ByName[canonicalName]
	return ok
}

// BuildCatalogSnapshot constructs an immutable CatalogSnapshot from raw
// MCP tools, applying allowlist/denylist, naming normalization with
// collision resolution, title/description sanitization, and RFC 8785
// fingerprints.
func BuildCatalogSnapshot(
	connector string,
	rawTools []*mcp.Tool,
	allowlist, denylist []string,
	needles []string,
	ttl time.Duration,
	now time.Time,
) (*CatalogSnapshot, error) {
	snap := &CatalogSnapshot{
		Connector:    connector,
		ByName:       make(map[string]*SnapshotTool),
		Fingerprints: make(map[string]string),
		RawToCanon:   make(map[string]string),
		CanonToRaw:   make(map[string]string),
		CreatedAt:    now,
		TTL:          ttl,
	}
	if ttl > 0 {
		snap.ExpiresAt = now.Add(ttl)
	}

	allowMap := make(map[string]bool, len(allowlist))
	for _, a := range allowlist {
		allowMap[a] = true
	}
	denyMap := make(map[string]bool, len(denylist))
	for _, d := range denylist {
		denyMap[d] = true
	}

	usedNames := make(map[string]bool)

	for _, rt := range rawTools {
		if rt == nil {
			continue
		}
		rawName := rt.Name

		canonBase, ok := NormalizeToolName(rawName)
		if !ok {
			// Dropped tool (empty after step 2)
			snap.DroppedTools = append(snap.DroppedTools, rawName)
			continue
		}

		// Allowlist / denylist check (deny wins per §3)
		if len(allowMap) > 0 && !allowMap[rawName] && !allowMap[canonBase] {
			continue
		}
		if denyMap[rawName] || denyMap[canonBase] {
			continue
		}

		// Collision resolution with -2, -3... suffixes truncated to 64 runes
		canon := canonBase
		if usedNames[canon] {
			for i := 2; ; i++ {
				suffix := fmt.Sprintf("-%d", i)
				baseLen := 64 - len(suffix)
				if baseLen > len(canonBase) {
					baseLen = len(canonBase)
				}
				candidate := canonBase[:baseLen] + suffix
				if !usedNames[candidate] {
					canon = candidate
					break
				}
			}
		}
		usedNames[canon] = true

		fullTitle := SanitizeText(rt.Title, needles, 0)
		fullDesc := SanitizeText(rt.Description, needles, 0)
		sanTitle := SanitizeText(rt.Title, needles, 120)
		sanDesc := SanitizeText(rt.Description, needles, 120)

		fp, err := ToolFingerprint(connector, rawName, fullTitle, fullDesc, rt.InputSchema, rt.Annotations)
		if err != nil {
			return nil, fmt.Errorf("fingerprint tool %q: %w", canon, err)
		}

		st := SnapshotTool{
			RawName:     rawName,
			Canonical:   canon,
			Title:       sanTitle,
			Description: sanDesc,
			InputSchema: rt.InputSchema,
			Annotations: rt.Annotations,
			Fingerprint: fp,
		}

		snap.Tools = append(snap.Tools, st)
		snap.ByName[canon] = &snap.Tools[len(snap.Tools)-1]
		snap.Fingerprints[canon] = fp
		snap.RawToCanon[rawName] = canon
		snap.CanonToRaw[canon] = rawName
		snap.ToolInfos = append(snap.ToolInfos, ToolInfo{
			Connector: connector,
			Name:      canon,
			Title:     sanTitle,
		})
	}

	return snap, nil
}

// CatalogDiff computes added, removed, and fingerprint-changed qualified
// tool names ("<connector>/<tool>") between oldSnap and newSnap per
// MCP-SPEC §4.3, A8.
func CatalogDiff(connector string, oldSnap, newSnap *CatalogSnapshot) []string {
	var diff []string

	oldFp := make(map[string]string)
	if oldSnap != nil {
		for k, v := range oldSnap.Fingerprints {
			oldFp[k] = v
		}
	}
	newFp := make(map[string]string)
	if newSnap != nil {
		for k, v := range newSnap.Fingerprints {
			newFp[k] = v
		}
	}

	// Added
	var added []string
	for k := range newFp {
		if _, exists := oldFp[k]; !exists {
			added = append(added, "+"+connector+"/"+k)
		}
	}
	sort.Strings(added)
	diff = append(diff, added...)

	// Removed
	var removed []string
	for k := range oldFp {
		if _, exists := newFp[k]; !exists {
			removed = append(removed, "-"+connector+"/"+k)
		}
	}
	sort.Strings(removed)
	diff = append(diff, removed...)

	// Fingerprint changed
	var changed []string
	for k, newHash := range newFp {
		if oldHash, exists := oldFp[k]; exists && oldHash != newHash {
			changed = append(changed, "~"+connector+"/"+k)
		}
	}
	sort.Strings(changed)
	diff = append(diff, changed...)

	return diff
}

// TruncateToolList reserves marker row space, tail-drops tools if serialized
// size exceeds budget, and appends the custos/truncated marker row per
// MCP-SPEC §4.5.
func TruncateToolList(tools []ToolInfo, budget int) []ToolInfo {
	if budget <= 0 {
		budget = 768 * 1024
	}

	// Conservative reservation for marker row serialized size
	const markerReserve = 256

	type listEnvelope struct {
		Tools []ToolInfo `json:"tools"`
	}

	raw, err := json.Marshal(listEnvelope{Tools: tools})
	if err == nil && len(raw) <= budget {
		return tools
	}

	target := budget - markerReserve
	cur := append([]ToolInfo(nil), tools...)

	for len(cur) > 0 {
		cur = cur[:len(cur)-1]
		b, err := json.Marshal(listEnvelope{Tools: cur})
		if err == nil && len(b) <= target {
			break
		}
	}

	droppedCount := len(tools) - len(cur)
	marker := ToolInfo{
		Connector: "custos",
		Name:      "truncated",
		Title:     fmt.Sprintf("[truncated by custos — %d more tools]", droppedCount),
	}
	return append(cur, marker)
}
