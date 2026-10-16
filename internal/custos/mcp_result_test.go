package custos_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// V5: ValidateArgs shallow structural matrix per MCP-SPEC §4.4, §12 V5.
func TestV5_ValidateArgsMatrix(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"path", "mode"},
		"properties": map[string]any{
			"path": map[string]any{
				"type": "string",
			},
			"mode": map[string]any{
				"type": "string",
				"enum": []any{"read", "write"},
			},
			"count": map[string]any{
				"type": "integer",
			},
			"flags": map[string]any{
				"type": "array",
			},
			"options": map[string]any{
				"type": "object",
			},
			"enabled": map[string]any{
				"type": "boolean",
			},
		},
	}

	tests := []struct {
		name    string
		schema  any
		args    string
		wantErr bool
	}{
		{
			name:    "valid full object",
			schema:  schema,
			args:    `{"path": "/etc/hosts", "mode": "read", "count": 42, "flags": ["a"], "options": {}, "enabled": true}`,
			wantErr: false,
		},
		{
			name:    "valid minimal required",
			schema:  schema,
			args:    `{"path": "/etc/hosts", "mode": "write"}`,
			wantErr: false,
		},
		{
			name:    "missing required path",
			schema:  schema,
			args:    `{"mode": "read"}`,
			wantErr: true,
		},
		{
			name:    "missing required mode",
			schema:  schema,
			args:    `{"path": "/etc/hosts"}`,
			wantErr: true,
		},
		{
			name:    "wrong type for path (number instead of string)",
			schema:  schema,
			args:    `{"path": 123, "mode": "read"}`,
			wantErr: true,
		},
		{
			name:    "wrong type for count (float instead of integer)",
			schema:  schema,
			args:    `{"path": "/etc", "mode": "read", "count": 42.5}`,
			wantErr: true,
		},
		{
			name:    "wrong type for flags (object instead of array)",
			schema:  schema,
			args:    `{"path": "/etc", "mode": "read", "flags": {}}`,
			wantErr: true,
		},
		{
			name:    "wrong type for enabled (string instead of boolean)",
			schema:  schema,
			args:    `{"path": "/etc", "mode": "read", "enabled": "true"}`,
			wantErr: true,
		},
		{
			name:    "enum violation for mode",
			schema:  schema,
			args:    `{"path": "/etc", "mode": "append"}`,
			wantErr: true,
		},
		{
			name:    "non-object json: array",
			schema:  schema,
			args:    `["path", "/etc"]`,
			wantErr: true,
		},
		{
			name:    "non-object json: string",
			schema:  schema,
			args:    `"not an object"`,
			wantErr: true,
		},
		{
			name:    "non-object json: null",
			schema:  schema,
			args:    `null`,
			wantErr: true,
		},
		{
			name:    "absent schema accepts any object",
			schema:  nil,
			args:    `{"arbitrary": [1, 2, 3]}`,
			wantErr: false,
		},
		{
			name:    "malformed schema accepts any object",
			schema:  json.RawMessage(`{"malformed_json":`),
			args:    `{"arbitrary": true}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := custos.ValidateArgs(tt.schema, json.RawMessage(tt.args))
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// V8: Closed result parsing per MCP-SPEC §4.5, §12 V8.
func TestV8_ClosedParsing(t *testing.T) {
	// Payload with unknown top-level member, _meta, and bogus block type
	rawJSON := `{
		"_meta": {"large_extension": "data-to-discard"},
		"unknown_top_level": 12345,
		"isError": false,
		"structuredContent": {"output": "ok"},
		"content": [
			{"type": "text", "text": "valid text block"},
			{"type": "bogus_unsupported_type", "payload": "must be dropped"}
		]
	}`

	parsed, blocks, err := custos.ParseClosedResult([]byte(rawJSON))
	if err != nil {
		t.Fatalf("ParseClosedResult failed: %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("expected 1 surviving block, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "valid text block" {
		t.Fatalf("unexpected block content: %+v", blocks[0])
	}

	// Converge and serialize
	delivered, _, err := custos.ConvergeResult(blocks, parsed.StructuredContent, parsed.IsError)
	if err != nil {
		t.Fatalf("ConvergeResult failed: %v", err)
	}

	// Verify _meta and unknown_top_level are NOT present in final serialized bytes
	var outMap map[string]any
	if err := json.Unmarshal(delivered, &outMap); err != nil {
		t.Fatalf("unmarshal delivered result: %v", err)
	}

	if _, exists := outMap["_meta"]; exists {
		t.Fatalf("_meta leaked into delivered result: %s", string(delivered))
	}
	if _, exists := outMap["unknown_top_level"]; exists {
		t.Fatalf("unknown_top_level leaked into delivered result: %s", string(delivered))
	}

	// isError: true passes through as data per §4.5
	errorPayload := `{
		"isError": true,
		"content": [{"type": "text", "text": "tool returned error"}]
	}`
	parsedErr, errBlocks, err := custos.ParseClosedResult([]byte(errorPayload))
	if err != nil {
		t.Fatalf("ParseClosedResult errorPayload: %v", err)
	}
	deliveredErr, _, err := custos.ConvergeResult(errBlocks, parsedErr.StructuredContent, parsedErr.IsError)
	if err != nil {
		t.Fatalf("ConvergeResult errorPayload: %v", err)
	}
	var outErrMap map[string]any
	_ = json.Unmarshal(deliveredErr, &outErrMap)
	if outErrMap["isError"] != true {
		t.Fatalf("isError: true was not preserved in data: %s", string(deliveredErr))
	}
}

// V16b: Sentinel echo probe across all surfaces per MCP-SPEC §4.5, §12 V16b.
func TestV16b_SentinelEchoProbe(t *testing.T) {
	sentinel := "sentinel-secret-val-987654"
	needles := []string{sentinel}

	// 1. Text block body
	textBlock := map[string]any{"type": "text", "text": "echo: " + sentinel}
	scrubbedText := custos.ScrubContentBlockForTest(textBlock, needles)
	txt, ok := scrubbedText["text"].(string)
	if !ok || strings.Contains(txt, sentinel) {
		t.Fatalf("sentinel leaked in text block: %v", scrubbedText)
	}
	if !strings.Contains(txt, "[redacted]") {
		t.Fatalf("text block missing [redacted]: %v", scrubbedText)
	}

	// 2. StructuredContent leaf and object KEY
	sc := map[string]any{
		"normal_key": "val with " + sentinel,
		sentinel:     "key was the sentinel",
	}
	scrubbedSC, ok := custos.ScrubStructuredContentForTest(sc, needles).(map[string]any)
	if !ok {
		t.Fatalf("scrubbedSC is not map[string]any: %T", scrubbedSC)
	}

	// Leaf must be scrubbed
	normVal, ok := scrubbedSC["normal_key"].(string)
	if !ok || strings.Contains(normVal, sentinel) {
		t.Fatalf("sentinel leaked in structuredContent leaf: %v", scrubbedSC)
	}
	// Key must be scrubbed
	if _, exists := scrubbedSC[sentinel]; exists {
		t.Fatalf("sentinel key leaked into structuredContent: %v", scrubbedSC)
	}
	if _, exists := scrubbedSC["[redacted]"]; !exists {
		t.Fatalf("structuredContent missing [redacted] key: %v", scrubbedSC)
	}

	// 3. Base64 image block alignment 0
	b64Align0 := base64.StdEncoding.EncodeToString([]byte(sentinel))
	img0 := map[string]any{"type": "image", "data": b64Align0, "mimeType": "image/png"}
	res0 := custos.ScrubContentBlockForTest(img0, needles)
	if res0["type"] != "text" || res0["text"] != "[redacted]" {
		t.Fatalf("align 0 not replaced with text [redacted]: %v", res0)
	}

	// 4. Base64 image block alignment 1
	b64Align1 := base64.StdEncoding.EncodeToString(append([]byte("A"), []byte(sentinel)...))
	img1 := map[string]any{"type": "image", "data": b64Align1, "mimeType": "image/png"}
	res1 := custos.ScrubContentBlockForTest(img1, needles)
	if res1["type"] != "text" || res1["text"] != "[redacted]" {
		t.Fatalf("align 1 not replaced with text [redacted]: %v", res1)
	}

	// 5. Base64 image block alignment 2
	b64Align2 := base64.StdEncoding.EncodeToString(append([]byte("AB"), []byte(sentinel)...))
	img2 := map[string]any{"type": "image", "data": b64Align2, "mimeType": "image/png"}
	res2 := custos.ScrubContentBlockForTest(img2, needles)
	if res2["type"] != "text" || res2["text"] != "[redacted]" {
		t.Fatalf("align 2 not replaced with text [redacted]: %v", res2)
	}

	// 6. Non-canonical base64 re-encode (strict decode fails closed)
	invalidB64 := b64Align0 + "!!!"
	imgInv := map[string]any{"type": "image", "data": invalidB64, "mimeType": "image/png"}
	resInv := custos.ScrubContentBlockForTest(imgInv, needles)
	if resInv["type"] != "text" || resInv["text"] != "[redacted]" {
		t.Fatalf("non-canonical base64 did not fail closed to [redacted]: %v", resInv)
	}
}

// V6 / V16c / V16m: Budget probes per MCP-SPEC §4.5, §12 V6, V16c, V16m.
func TestV6_V16c_V16m_BudgetProbes(t *testing.T) {
	needle := "needle01" // 8 bytes -> [redacted] is 10 bytes (+2 expansion)

	// V16m: ~760 KiB of repeating 8-byte sentinels expands past 768 KiB
	// and is delivered <= 768 KiB with mcp_result_truncated audit token
	repeatCount := 95000 // 95,000 * 8 = 760,000 bytes (~742 KiB)
	var sb strings.Builder
	for range repeatCount {
		sb.WriteString(needle)
	}
	expandedText := sb.String()

	// Step 0: scrub expands 760,000 bytes into 950,000 bytes (~928 KiB > 768 KiB)
	scrubbedText := custos.ScrubString(expandedText, []string{needle})
	blocks := []map[string]any{
		{"type": "text", "text": scrubbedText},
	}

	delivered, tokens, err := custos.ConvergeResult(blocks, nil, false)
	if err != nil {
		t.Fatalf("ConvergeResult V16m failed: %v", err)
	}
	if len(delivered) > custos.MaxWorkerResultBytes {
		t.Fatalf("delivered bytes %d > 768 KiB cap (%d)", len(delivered), custos.MaxWorkerResultBytes)
	}
	if len(tokens) == 0 {
		t.Fatalf("expected truncation audit tokens for V16m expansion, got none")
	}

	// Framing bomb: 35,000 empty text blocks {"type": "text", "text": ""}
	// Must hit step 4 floor in bounded time
	framingBlocks := make([]map[string]any, 35000)
	for i := range framingBlocks {
		framingBlocks[i] = map[string]any{"type": "text", "text": ""}
	}
	delivFraming, fTokens, err := custos.ConvergeResult(framingBlocks, nil, false)
	if err != nil {
		t.Fatalf("ConvergeResult framing bomb failed: %v", err)
	}
	if len(delivFraming) > custos.MaxWorkerResultBytes {
		t.Fatalf("framing bomb delivered bytes %d > cap", len(delivFraming))
	}
	if !strings.Contains(string(delivFraming), custos.ContentCollapsedText) {
		t.Fatalf("expected floor collapsed text block, got: %s", string(delivFraming))
	}
	foundCollapse := false
	for _, tok := range fTokens {
		if tok == custos.TruncationTokenBlocksCollapsed {
			foundCollapse = true
		}
	}
	if !foundCollapse {
		t.Fatalf("expected blocks_collapsed token, got: %v", fTokens)
	}

	// StructuredContent-only breach: 2 MiB structuredContent with small content
	sc2MB := map[string]any{
		"huge": strings.Repeat("X", 2*1024*1024),
	}
	smallBlocks := []map[string]any{
		{"type": "text", "text": "small"},
	}
	delivSC, scTokens, err := custos.ConvergeResult(smallBlocks, sc2MB, false)
	if err != nil {
		t.Fatalf("ConvergeResult 2MB structuredContent failed: %v", err)
	}
	if len(delivSC) > custos.MaxWorkerResultBytes {
		t.Fatalf("delivered SC bytes %d > cap", len(delivSC))
	}
	foundSCDrop := false
	for _, tok := range scTokens {
		if tok == custos.TruncationTokenStructuredContentDrop {
			foundSCDrop = true
		}
	}
	if !foundSCDrop {
		t.Fatalf("expected structuredContent_dropped token, got: %v", scTokens)
	}

	// Escape-heavy text: 768 KiB of newlines and quotes
	escapeText := strings.Repeat("\n\"\t\\", 200000) // ~800 KiB
	escBlocks := []map[string]any{
		{"type": "text", "text": escapeText},
	}
	delivEsc, _, err := custos.ConvergeResult(escBlocks, nil, false)
	if err != nil {
		t.Fatalf("ConvergeResult escape text failed: %v", err)
	}
	if len(delivEsc) > custos.MaxWorkerResultBytes {
		t.Fatalf("delivered escape bytes %d > cap", len(delivEsc))
	}
}

// V13: Review line shapes and A5 destructive prefix per MCP-SPEC §5, §12 V13.
func TestV13_ReviewLineShapes(t *testing.T) {
	// 1. Scalar args: key=value, sorted, up to 3
	args1 := json.RawMessage(`{"z": 100, "a": "hello", "m": true, "d": "ignored_fourth"}`)
	rev1 := custos.BuildReviewLine(args1, false, nil)
	want1 := "a=hello, d=ignored_fourth, m=true"
	if rev1 != want1 {
		t.Fatalf("review line = %q, want %q", rev1, want1)
	}

	// 2. Structured args named with key=…
	args2 := json.RawMessage(`{"items": [1, 2, 3], "name": "bob"}`)
	rev2 := custos.BuildReviewLine(args2, false, nil)
	want2 := "items=…, name=bob"
	if rev2 != want2 {
		t.Fatalf("review line = %q, want %q", rev2, want2)
	}

	// 3. Unparseable args -> "?"
	argsBad := json.RawMessage(`not a json object`)
	revBad := custos.BuildReviewLine(argsBad, false, nil)
	if revBad != "?" {
		t.Fatalf("unparseable review line = %q, want '?'", revBad)
	}

	// 4. A5 Destructive prefix
	revDest := custos.BuildReviewLine(args1, true, nil)
	if !strings.HasPrefix(revDest, "⚠ destructive ") {
		t.Fatalf("expected destructive prefix, got: %q", revDest)
	}
	if !strings.Contains(revDest, want1) {
		t.Fatalf("destructive review line missing content: %q", revDest)
	}

	// 5. Destructive unparseable -> "⚠ destructive ?"
	revDestBad := custos.BuildReviewLine(argsBad, true, nil)
	if revDestBad != "⚠ destructive ?" {
		t.Fatalf("destructive bad = %q, want '⚠ destructive ?'", revDestBad)
	}

	// 6. V16n: Scrub-before-truncate on review line value (straddling 96-rune cut)
	needle := "secret-sentinel-value-12345" // 27 runes
	prefix := strings.Repeat("B", 90)       // 90 'B's + 27 runes = 117 runes > 96
	straddling := prefix + needle
	argsStraddle, err := json.Marshal(map[string]any{"key": straddling})
	if err != nil {
		t.Fatalf("marshal straddling: %v", err)
	}
	revStraddle := custos.BuildReviewLine(argsStraddle, false, []string{needle})

	// Must not contain any needle fragment on either side of the ellipsis
	for i := 1; i <= len(needle); i++ {
		frag := needle[:i]
		if len(frag) >= 8 && strings.Contains(revStraddle, frag) {
			t.Fatalf("review line straddle leaked needle fragment %q: %q", frag, revStraddle)
		}
	}
	if !strings.HasSuffix(revStraddle, "…") {
		t.Fatalf("expected truncated review value to end with ellipsis: %q", revStraddle)
	}
}
