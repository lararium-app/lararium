// MCP result processing, scrubbing, converge loop, and review line construction
// per MCP-SPEC §4.4, §4.5, §5, §6, §8.4, and CUSTOS-SPEC §5.1a / §8.1.

package custos

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Constants for result cap and markers per MCP-SPEC §4.5.
const (
	MaxWorkerResultBytes = 768 * 1024 // 768 KiB budget
	ContentOmittedMarker = "[content omitted by custos]"
	TruncatedSuffix      = " [truncated by custos]"
	ContentCollapsedText = "[content collapsed by custos]"
)

// Detail constants per MCP-SPEC A2 / §8.3 and CUSTOS-SPEC §5.1a.
const (
	DetailProtocolViolation = "protocol violation"
)

// ScrubString replaces all occurrences of needles (length >= 8) with
// [redacted], longest needles first per CUSTOS-SPEC §5.1a.
func ScrubString(s string, needles []string) string {
	if len(needles) == 0 || s == "" {
		return s
	}
	var vals []string
	seen := make(map[string]bool)
	for _, n := range needles {
		if len(n) >= scrubMinLength && !seen[n] {
			vals = append(vals, n)
			seen[n] = true
		}
	}
	if len(vals) == 0 {
		return s
	}

	sort.Slice(vals, func(i, j int) bool {
		return len(vals[i]) > len(vals[j])
	})

	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, redactedPlaceholder)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// base64NeedleHit checks if data matches needles at any of 3 base64
// alignments or in strict decoded bytes per MCP-SPEC §4.5. Returns true
// if hit or if strict decoding fails (fail closed).
func base64NeedleHit(data string, needles []string) bool {
	clean := strings.TrimSpace(data)
	if clean == "" {
		return false
	}

	// 1. Strict decode: failure fails closed per §4.5
	decoded, err := base64.StdEncoding.Strict().DecodeString(clean)
	if err != nil {
		return true
	}

	// 2. Substring match on decoded payload
	for _, needle := range needles {
		if len(needle) >= scrubMinLength {
			if bytes.Contains(decoded, []byte(needle)) {
				return true
			}
		}
	}

	// 3. Alignment checks at 3 offsets on the base64 string
	for _, needle := range needles {
		if len(needle) < scrubMinLength {
			continue
		}
		bNeedle := []byte(needle)

		// Offset 0
		enc0 := base64.StdEncoding.EncodeToString(bNeedle)
		enc0 = strings.TrimRight(enc0, "=")
		if len(enc0) >= 4 && strings.Contains(clean, enc0) {
			return true
		}

		// Offset 1: prefix with 1 byte (4 bits consumed by prefix, 2 bits spill)
		enc1 := base64.StdEncoding.EncodeToString(append([]byte("X"), bNeedle...))
		enc1 = strings.TrimRight(enc1, "=")
		if len(enc1) > 2 {
			chunk1 := enc1[2:]
			if len(chunk1) >= 4 && strings.Contains(clean, chunk1) {
				return true
			}
		}

		// Offset 2: prefix with 2 bytes
		enc2 := base64.StdEncoding.EncodeToString(append([]byte("XX"), bNeedle...))
		enc2 = strings.TrimRight(enc2, "=")
		if len(enc2) > 3 {
			chunk2 := enc2[3:]
			if len(chunk2) >= 4 && strings.Contains(clean, chunk2) {
				return true
			}
		}
	}

	return false
}

// scrubStructuredContent recursively scrubs strings and map keys in
// structuredContent per MCP-SPEC §4.5.
func scrubStructuredContent(v any, needles []string) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return ScrubString(val, needles)
	case map[string]any:
		res := make(map[string]any, len(val))
		for k, child := range val {
			scrubbedKey := ScrubString(k, needles)
			res[scrubbedKey] = scrubStructuredContent(child, needles)
		}
		return res
	case []any:
		res := make([]any, len(val))
		for i, child := range val {
			res[i] = scrubStructuredContent(child, needles)
		}
		return res
	default:
		return v
	}
}

// scrubContentBlock scrubs all string fields of recognized content blocks and
// applies the decode-or-replace rule to base64 payload blocks.
func scrubContentBlock(block map[string]any, needles []string) map[string]any {
	bType, _ := block["type"].(string)
	switch bType {
	case "text":
		txt, _ := block["text"].(string)
		block["text"] = ScrubString(txt, needles)

	case "image", "audio":
		if data, ok := block["data"].(string); ok {
			if base64NeedleHit(data, needles) {
				return map[string]any{"type": "text", "text": redactedPlaceholder}
			}
		}
		if mime, ok := block["mimeType"].(string); ok {
			block["mimeType"] = ScrubString(mime, needles)
		}

	case "resource_link":
		if uri, ok := block["uri"].(string); ok {
			block["uri"] = ScrubString(uri, needles)
		}

	case "resource":
		if res, ok := block["resource"].(map[string]any); ok {
			if txt, ok := res["text"].(string); ok {
				res["text"] = ScrubString(txt, needles)
			}
			if blob, ok := res["blob"].(string); ok {
				if base64NeedleHit(blob, needles) {
					return map[string]any{"type": "text", "text": redactedPlaceholder}
				}
			}
			if uri, ok := res["uri"].(string); ok {
				res["uri"] = ScrubString(uri, needles)
			}
			if mime, ok := res["mimeType"].(string); ok {
				res["mimeType"] = ScrubString(mime, needles)
			}
			block["resource"] = res
		}
	}

	return block
}

// rawClosedResult parses incoming MCP result into the four declared members
// only per §4.5.
type rawClosedResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent any               `json:"structuredContent"`
	IsError           bool              `json:"isError"`
	ResultType        string            `json:"resultType"`
}

// workerResultPayload serializes the final worker response payload per §4.5.
type workerResultPayload struct {
	Content           []any `json:"content"`
	StructuredContent any   `json:"structuredContent"`
	IsError           bool  `json:"isError,omitempty"`
}

// ParseClosedResult unmarshals raw JSON into closed members, discarding _meta
// and unknown block types per MCP-SPEC §4.5.
func ParseClosedResult(data []byte) (rawClosedResult, []map[string]any, error) {
	var parsed rawClosedResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		return parsed, nil, err
	}

	var survivingBlocks []map[string]any
	for _, raw := range parsed.Content {
		var block map[string]any
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text", "image", "audio", "resource_link", "resource":
			survivingBlocks = append(survivingBlocks, block)
		default:
			// Discard bogus block shapes
		}
	}

	return parsed, survivingBlocks, nil
}

// ConvergeResult runs the 768 KiB converge loop with strict progress rule
// per §4.5. Step 0 (scrub) must be performed prior to budget measurement.
func ConvergeResult(
	blocks []map[string]any,
	structuredContent any,
	isError bool,
) ([]byte, []string, error) {
	var shrunkTokens []string
	shrunkSet := make(map[string]bool)

	// Convert blocks to []any for serialization
	workingBlocks := make([]any, len(blocks))
	for i, b := range blocks {
		workingBlocks[i] = b
	}

	for {
		// 1. Serialize and measure final post-scrub bytes
		payload := workerResultPayload{
			Content:           workingBlocks,
			StructuredContent: structuredContent,
			IsError:           isError,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("serialize result payload: %w", err)
		}

		if len(data) <= MaxWorkerResultBytes {
			// Done
			if shrunkSet[TruncationTokenBlocksCollapsed] {
				shrunkTokens = append(shrunkTokens, TruncationTokenBlocksCollapsed)
			}
			if shrunkSet[TruncationTokenTextTruncated] {
				shrunkTokens = append(shrunkTokens, TruncationTokenTextTruncated)
			}
			if shrunkSet[TruncationTokenStructuredContentDrop] {
				shrunkTokens = append(shrunkTokens, TruncationTokenStructuredContentDrop)
			}
			sort.Strings(shrunkTokens)
			return data, shrunkTokens, nil
		}

		currentLen := len(data)

		// 2. Replace largest non-text content block if it strictly shrinks the whole
		largestNonTextIdx := -1
		largestNonTextSize := -1
		for i, blk := range workingBlocks {
			m, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "text" {
				bData, err := json.Marshal(m)
				if err == nil && len(bData) > largestNonTextSize {
					largestNonTextSize = len(bData)
					largestNonTextIdx = i
				}
			}
		}

		if largestNonTextIdx >= 0 {
			candidateBlocks := make([]any, len(workingBlocks))
			copy(candidateBlocks, workingBlocks)
			candidateBlocks[largestNonTextIdx] = map[string]any{
				"type": "text",
				"text": ContentOmittedMarker,
			}
			candPayload := workerResultPayload{
				Content:           candidateBlocks,
				StructuredContent: structuredContent,
				IsError:           isError,
			}
			candData, err := json.Marshal(candPayload)
			if err == nil && len(candData) < currentLen {
				workingBlocks = candidateBlocks
				shrunkSet[TruncationTokenBlocksCollapsed] = true
				continue
			}
		}

		// 3. Truncate largest text block body if strictly shrinkable
		largestTextIdx := -1
		largestTextLen := -1
		for i, blk := range workingBlocks {
			m, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" {
				if txt, ok := m["text"].(string); ok {
					if len(txt) > largestTextLen {
						largestTextLen = len(txt)
						largestTextIdx = i
					}
				}
			}
		}

		if largestTextIdx >= 0 && largestTextLen > len(TruncatedSuffix) {
			m, okM := workingBlocks[largestTextIdx].(map[string]any)
			origText, okT := m["text"].(string)
			if okM && okT {
				overflow := currentLen - MaxWorkerResultBytes
				cutLen := len(origText) - overflow - len(TruncatedSuffix)
				if cutLen < 0 {
					cutLen = 0
				}

				newText := origText[:cutLen] + TruncatedSuffix
				if len(newText) < len(origText) {
					candidateBlocks := make([]any, len(workingBlocks))
					copy(candidateBlocks, workingBlocks)
					candidateMap := make(map[string]any, len(m))
					for k, v := range m {
						candidateMap[k] = v
					}
					candidateMap["text"] = newText
					candidateBlocks[largestTextIdx] = candidateMap

					candPayload := workerResultPayload{
						Content:           candidateBlocks,
						StructuredContent: structuredContent,
						IsError:           isError,
					}
					candData, err := json.Marshal(candPayload)
					if err == nil && len(candData) < currentLen {
						workingBlocks = candidateBlocks
						shrunkSet[TruncationTokenTextTruncated] = true
						continue
					}
				}
			}
		}

		// 4. No-progress floor: drop structuredContent to null and collapse content
		if structuredContent != nil {
			structuredContent = nil
			shrunkSet[TruncationTokenStructuredContentDrop] = true
		}
		workingBlocks = []any{
			map[string]any{
				"type": "text",
				"text": ContentCollapsedText,
			},
		}
		shrunkSet[TruncationTokenBlocksCollapsed] = true
		// Loop restarts at step 5 / 1 and terminates immediately.
	}
}

// ValidateArgs checks shallow structure of args against schema per
// MCP-SPEC §4.4.
func ValidateArgs(schema any, args json.RawMessage) error {
	var obj map[string]any
	if err := json.Unmarshal(args, &obj); err != nil {
		return wErr(ErrCodeBadArgs, detailBadArgs)
	}

	if schema == nil {
		return nil
	}

	var schemaMap map[string]any
	switch s := schema.(type) {
	case map[string]any:
		schemaMap = s
	case json.RawMessage:
		if err := json.Unmarshal(s, &schemaMap); err != nil {
			return nil //nolint:nilerr // malformed schema accepts any object per §4.4
		}
	case []byte:
		if err := json.Unmarshal(s, &schemaMap); err != nil {
			return nil //nolint:nilerr // malformed schema accepts any object per §4.4
		}
	default:
		data, err := json.Marshal(schema)
		if err != nil {
			return nil //nolint:nilerr // malformed schema accepts any object per §4.4
		}
		_ = json.Unmarshal(data, &schemaMap)
	}

	if schemaMap == nil {
		return nil
	}

	// 1. Required fields
	if req, ok := schemaMap["required"].([]any); ok {
		for _, item := range req {
			if reqName, ok := item.(string); ok {
				if _, exists := obj[reqName]; !exists {
					return wErr(ErrCodeBadArgs, detailBadArgs)
				}
			}
		}
	} else if req, ok := schemaMap["required"].([]string); ok {
		for _, reqName := range req {
			if _, exists := obj[reqName]; !exists {
				return wErr(ErrCodeBadArgs, detailBadArgs)
			}
		}
	}

	// 2. Top-level property types and enum membership
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		return nil
	}

	for k, val := range obj {
		propDef, ok := props[k].(map[string]any)
		if !ok {
			continue
		}

		if tVal, ok := propDef["type"]; ok {
			if !checkKindMatch(tVal, val) {
				return wErr(ErrCodeBadArgs, detailBadArgs)
			}
		}

		if enumVals, ok := propDef["enum"].([]any); ok {
			if !checkEnumMatch(enumVals, val) {
				return wErr(ErrCodeBadArgs, detailBadArgs)
			}
		}
	}

	return nil
}

func checkKindMatch(typeSpec any, val any) bool {
	switch t := typeSpec.(type) {
	case string:
		return matchesJSONKind(t, val)
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && matchesJSONKind(s, val) {
				return true
			}
		}
		return false
	case []string:
		for _, s := range t {
			if matchesJSONKind(s, val) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func matchesJSONKind(kind string, val any) bool {
	if val == nil {
		return kind == "null"
	}
	switch kind {
	case "string":
		_, ok := val.(string)
		return ok
	case "number":
		switch val.(type) {
		case float64, float32, int, int64, json.Number:
			return true
		default:
			return false
		}
	case "integer":
		switch v := val.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return v == math.Trunc(v) && !math.IsNaN(v) && !math.IsInf(v, 0)
		case json.Number:
			_, err := v.Int64()
			return err == nil
		default:
			return false
		}
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "null":
		return val == nil
	default:
		return true
	}
}

func checkEnumMatch(enumVals []any, val any) bool {
	for _, e := range enumVals {
		if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", val) {
			return true
		}
	}
	return false
}

// BuildReviewLine constructs card review fields per MCP-SPEC §5, A5:
// up to 3 scalar args key=value scrubbed then truncated at 96 runes,
// structured args key=…, unparseable ?, runs <= 512 runes total,
// and '⚠ destructive ' prefix when destructiveHint is true.
func BuildReviewLine(args json.RawMessage, destructive bool, needles []string) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		if destructive {
			return "⚠ destructive ?"
		}
		return "?"
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	limit := 3
	if len(keys) < limit {
		limit = len(keys)
	}

	for i := range limit {
		k := keys[i]
		v := m[k]

		var valStr string
		switch val := v.(type) {
		case string:
			scrubbed := ScrubString(val, needles)
			runes := []rune(scrubbed)
			if len(runes) > 96 {
				valStr = string(runes[:95]) + "…"
			} else {
				valStr = scrubbed
			}
			parts = append(parts, fmt.Sprintf("%s=%s", k, valStr))

		case bool:
			parts = append(parts, fmt.Sprintf("%s=%t", k, val))

		case float64:
			if val == math.Trunc(val) && math.Abs(val) < 1e15 {
				parts = append(parts, fmt.Sprintf("%s=%d", k, int64(val)))
			} else {
				parts = append(parts, fmt.Sprintf("%s=%g", k, val))
			}

		case int, int64, json.Number:
			parts = append(parts, fmt.Sprintf("%s=%v", k, val))

		case map[string]any, []any:
			parts = append(parts, fmt.Sprintf("%s=…", k))

		case nil:
			parts = append(parts, fmt.Sprintf("%s=null", k))

		default:
			parts = append(parts, fmt.Sprintf("%s=…", k))
		}
	}

	res := strings.Join(parts, ", ")
	resRunes := []rune(res)
	if len(resRunes) > 512 {
		res = string(resRunes[:511]) + "…"
	}

	if destructive {
		return "⚠ destructive " + res
	}
	return res
}
