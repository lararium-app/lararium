package custos_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

// mockAuditSink records appended audit records in memory.
type mockAuditSink struct {
	mu      sync.Mutex
	records []custos.AuditRecord
}

func (m *mockAuditSink) Append(rec custos.AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	return nil
}

func (m *mockAuditSink) Records() []custos.AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]custos.AuditRecord, len(m.records))
	copy(copied, m.records)
	return copied
}

// V1 / V16l: Naming normalization table test per MCP-SPEC §4.2, §12 V1 / V16l.
func TestV1_NamingTable(t *testing.T) {
	tests := []struct {
		raw       string
		wantCanon string
		wantDrop  bool
	}{
		{raw: "readFile", wantCanon: "readfile", wantDrop: false},
		{raw: "_debug", wantCanon: "t-_debug", wantDrop: false},
		{raw: "notion.search", wantCanon: "notion_search", wantDrop: false},
		{raw: "", wantCanon: "", wantDrop: true},
		{
			raw:       strings.Repeat("a", 200),
			wantCanon: strings.Repeat("a", 64),
			wantDrop:  false,
		},
		{raw: "---foo", wantCanon: "t----foo", wantDrop: false},
		{raw: "123_tool", wantCanon: "123_tool", wantDrop: false},
		{raw: "CamelCaseAPI", wantCanon: "camelcaseapi", wantDrop: false},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			canon, ok := custos.NormalizeToolName(tt.raw)
			if tt.wantDrop {
				if ok {
					t.Fatalf("expected raw %q to drop, got canon %q", tt.raw, canon)
				}
				return
			}
			if !ok {
				t.Fatalf("expected raw %q to survive, but it dropped", tt.raw)
			}
			if canon != tt.wantCanon {
				t.Fatalf("NormalizeToolName(%q) = %q, want %q", tt.raw, canon, tt.wantCanon)
			}
			// V16l: every canonical name passes keyNameRe
			if !custos.KeyNameReForTest.MatchString(canon) {
				t.Fatalf("canonical name %q does not match keyNameRe", canon)
			}
		})
	}

	// Test collision resolution within snapshot: "readFile" and "readfile"
	// normalize to "readfile" -> second gets "readfile-2"
	tools := []*mcp.Tool{
		{Name: "readFile"},
		{Name: "readfile"},
		{Name: "read_file"},
		{Name: ""}, // dropped
	}

	snap, err := custos.BuildCatalogSnapshot("testconn", tools, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatalf("BuildCatalogSnapshot failed: %v", err)
	}

	if len(snap.Tools) != 3 {
		t.Fatalf("expected 3 tools in snapshot, got %d", len(snap.Tools))
	}
	if len(snap.DroppedTools) != 1 || snap.DroppedTools[0] != "" {
		t.Fatalf("expected 1 dropped tool (\" \"), got %v", snap.DroppedTools)
	}

	wantNames := []string{"readfile", "readfile-2", "read_file"}
	for i, want := range wantNames {
		if snap.Tools[i].Canonical != want {
			t.Errorf("tool[%d].Canonical = %q, want %q", i, snap.Tools[i].Canonical, want)
		}
		if !custos.KeyNameReForTest.MatchString(snap.Tools[i].Canonical) {
			t.Errorf("canonical name %q failed keyNameRe", snap.Tools[i].Canonical)
		}
	}
}

// V3 / V16j / V16n: SanitizeText test per MCP-SPEC §4.2, §12 V3, V16j, V16n.
func TestV3_SanitizeText(t *testing.T) {
	// 1. Control chars and runs of whitespace
	raw := "Hello\r\n\tWorld\x00\x1f\x7f\u0080\u009f   Extra   Space"
	san := custos.SanitizeText(raw, nil, 120)
	want := "Hello World Extra Space"
	if san != want {
		t.Fatalf("SanitizeText(%q) = %q, want %q", raw, san, want)
	}

	// 2. V16j: Title-injection fixture with CR/LF/TAB, control chars, sentinel
	needle := "secret-sentinel-value-12345"
	rawTitle := "Line1\r\nLine2\tTitle with " + needle + " and \x05control"
	sanTitle := custos.SanitizeText(rawTitle, []string{needle}, 120)
	if strings.Contains(sanTitle, needle) {
		t.Fatalf("sanitized title leaked needle: %q", sanTitle)
	}
	if !strings.Contains(sanTitle, "[redacted]") {
		t.Fatalf("sanitized title missing [redacted]: %q", sanTitle)
	}
	if strings.Contains(sanTitle, "\n") || strings.Contains(sanTitle, "\r") || strings.Contains(sanTitle, "\t") {
		t.Fatalf("sanitized title contains newlines/tabs: %q", sanTitle)
	}

	// 3. V16n: Scrub-before-truncate law: needle straddling the 120-rune cut
	// Prefix has length such that needle crosses rune 120
	prefix := strings.Repeat("A", 115)
	straddling := prefix + needle // 115 'A's + 27 chars of needle = 142 chars
	sanStraddle := custos.SanitizeText(straddling, []string{needle}, 120)

	// Result must be <= 120 runes including ellipsis
	runes := []rune(sanStraddle)
	if len(runes) > 120 {
		t.Fatalf("sanitized text rune count %d > 120", len(runes))
	}
	// Must not contain any needle fragment on either side of the ellipsis
	for i := 1; i <= len(needle); i++ {
		frag := needle[:i]
		if len(frag) >= 8 && strings.Contains(sanStraddle, frag) {
			t.Fatalf("sanitized straddle leaked needle fragment %q: %q", frag, sanStraddle)
		}
	}
	if !strings.HasSuffix(sanStraddle, "…") {
		t.Fatalf("expected truncated string to end with ellipsis: %q", sanStraddle)
	}
}

// RFC 8785 (JCS) Minimal Canonicalizer test.
func TestJCS_RFC8785(t *testing.T) {
	// Keys sorted by UTF-16 code units, no whitespace
	input := map[string]any{
		"z": 1,
		"a": "hello/world",
		"m": []any{true, false, nil, 42},
		"b": map[string]any{
			"y": "nested",
			"x": "<tag>&value",
		},
	}

	out, err := custos.CanonicalJSON(input)
	if err != nil {
		t.Fatalf("CanonicalJSON failed: %v", err)
	}

	// JCS must not escape '/' or '<' or '>'
	s := string(out)
	if strings.Contains(s, `\/`) {
		t.Fatalf("JCS incorrectly escaped '/': %s", s)
	}
	if strings.Contains(s, `\u003c`) {
		t.Fatalf("JCS incorrectly escaped '<': %s", s)
	}
	if strings.Contains(s, " ") {
		t.Fatalf("JCS contains whitespace: %s", s)
	}

	// Assert deterministic byte stability
	out2, err := custos.CanonicalJSON(input)
	if err != nil || string(out) != string(out2) {
		t.Fatalf("JCS is not deterministic: %s vs %s", string(out), string(out2))
	}
}

// V4: Snapshot byte-stability + swap atomicity under -race per §4.3, §12 V4.
func TestV4_SnapshotByteStabilityAndRace(t *testing.T) {
	toolsGen1 := []*mcp.Tool{
		{Name: "read_file", Title: "Read File", Description: "Read content"},
		{Name: "write_file", Title: "Write File", Description: "Write content"},
	}
	toolsGen2 := []*mcp.Tool{
		{Name: "read_file", Title: "Read File", Description: "Read content updated"},
		{Name: "search", Title: "Search Files", Description: "Search directory"},
	}

	snap1, err := custos.BuildCatalogSnapshot("fs", toolsGen1, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatalf("BuildCatalogSnapshot 1: %v", err)
	}
	snap2, err := custos.BuildCatalogSnapshot("fs", toolsGen2, nil, nil, nil, 0, time.Now())
	if err != nil {
		t.Fatalf("BuildCatalogSnapshot 2: %v", err)
	}

	// Verify byte-stability of ListTools output
	b1, err := json.Marshal(snap1.ToolInfos)
	if err != nil {
		t.Fatalf("marshal snap1: %v", err)
	}
	b2, err := json.Marshal(snap1.ToolInfos)
	if err != nil {
		t.Fatalf("marshal snap1 again: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("ListTools bytes not stable: %s vs %s", string(b1), string(b2))
	}

	// Swap atomicity under -race
	var holder atomic.Pointer[custos.CatalogSnapshot]
	holder.Store(snap1)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s := holder.Load()
					if s == nil {
						t.Errorf("loaded nil snapshot")
						return
					}
					// Verify snapshot internal consistency
					for _, ti := range s.ToolInfos {
						if !s.HasTool(ti.Name) {
							t.Errorf("inconsistent snapshot: tool %q missing", ti.Name)
						}
					}
				}
			}
		}()
	}

	// Writer swapping snapshots
	wg.Add(1)
	go func() {
		defer wg.Done()
		current := 1
		for range 100 {
			if current == 1 {
				holder.Store(snap2)
				current = 2
			} else {
				holder.Store(snap1)
				current = 1
			}
			time.Sleep(100 * time.Microsecond)
		}
		close(stop)
	}()

	wg.Wait()
}

// V16k: Catalog overflow budget truncation test per MCP-SPEC §4.5, §12 V16k.
func TestTruncateToolList_V16k(t *testing.T) {
	// Construct tools with long titles that exceed budget
	numTools := 3000
	tools := make([]custos.ToolInfo, numTools)
	for i := range numTools {
		tools[i] = custos.ToolInfo{
			Connector: "largeconn",
			Name:      fmt.Sprintf("tool_%04d", i),
			Title:     strings.Repeat("A long description and title ", 20),
		}
	}

	// Budget 768 KiB
	budget := 768 * 1024
	truncated := custos.TruncateToolList(tools, budget)

	// Final serialized bytes must be <= budget
	data, err := json.Marshal(map[string]any{"tools": truncated})
	if err != nil {
		t.Fatalf("marshal truncated list: %v", err)
	}
	if len(data) > budget {
		t.Fatalf("truncated list serialized size %d > budget %d", len(data), budget)
	}

	// Last row must be marker row with connector: "custos", name: "truncated"
	if len(truncated) == 0 {
		t.Fatalf("truncated list is empty")
	}
	last := truncated[len(truncated)-1]
	if last.Connector != "custos" || last.Name != "truncated" {
		t.Fatalf("last row is not marker row: %+v", last)
	}
	if !strings.Contains(last.Title, "[truncated by custos —") {
		t.Fatalf("marker title invalid: %q", last.Title)
	}

	// Response must unmarshal cleanly into []ToolInfo
	type resp struct {
		Tools []custos.ToolInfo `json:"tools"`
	}
	var unmarshaled resp
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal into []ToolInfo failed: %v", err)
	}
	if len(unmarshaled.Tools) != len(truncated) {
		t.Fatalf("unmarshaled count %d != truncated count %d", len(unmarshaled.Tools), len(truncated))
	}
}

// CatalogDiff audit computation test per MCP-SPEC §4.3, A8.
func TestCatalogDiff(t *testing.T) {
	t1 := []*mcp.Tool{
		{Name: "tool1", Title: "Tool 1", Description: "Desc 1"},
		{Name: "tool2", Title: "Tool 2", Description: "Desc 2"},
		{Name: "tool3", Title: "Tool 3", Description: "Desc 3"},
	}
	t2 := []*mcp.Tool{
		{Name: "tool1", Title: "Tool 1", Description: "Desc 1 Changed"}, // fingerprint changed
		{Name: "tool3", Title: "Tool 3", Description: "Desc 3"},         // unchanged
		{Name: "tool4", Title: "Tool 4", Description: "Desc 4"},         // added
		// tool2 removed
	}

	now := time.Now()
	s1, err := custos.BuildCatalogSnapshot("conn", t1, nil, nil, nil, 0, now)
	if err != nil {
		t.Fatalf("build s1: %v", err)
	}
	s2, err := custos.BuildCatalogSnapshot("conn", t2, nil, nil, nil, 0, now)
	if err != nil {
		t.Fatalf("build s2: %v", err)
	}

	diff := custos.CatalogDiff("conn", s1, s2)
	wantDiff := []string{
		"+conn/tool4",
		"-conn/tool2",
		"~conn/tool1",
	}

	if len(diff) != len(wantDiff) {
		t.Fatalf("diff length %d != want %d; got: %v", len(diff), len(wantDiff), diff)
	}
	for i, w := range wantDiff {
		if diff[i] != w {
			t.Errorf("diff[%d] = %q, want %q", i, diff[i], w)
		}
	}
}
