// Package router normalizes OpenAI-compatible backends behind one
// Provider interface with capability probing.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ProbeOpenAI queries the provider's /models endpoint to discover
// capabilities. Returns error if the GET fails; callers should fall
// back to config Caps with Source="config".
//
// Parses llama.cpp extension fields (trained_tokens, n_ctx, projector)
// when present. Anthropic-style /v1/models id-match is best-effort.
// Absent fields result in 0/false — never invent numbers.
func ProbeOpenAI(ctx context.Context, baseURL, apiKey, wanted string) (Caps, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return Caps{}, fmt.Errorf("create request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Caps{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Caps{}, fmt.Errorf("HTTP %d from /models", resp.StatusCode)
	}

	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Caps{}, fmt.Errorf("decode response: %w", err)
	}

	if len(body.Data) == 0 {
		return Caps{}, fmt.Errorf("no models returned")
	}

	// Prefer the entry whose id matches the requested model; else first with signal.
	var fallback *Caps
	for _, raw := range body.Data {
		entry, err := unmarshalModelEntry(raw)
		if err != nil {
			continue
		}
		caps := parseModelEntry(entry)
		if wanted != "" && entry.ID == wanted {
			if caps.ContextLength == 0 {
				// llama.cpp publishes the serving window on /props, not
				// /models — probe it before giving up (detection, not
				// assumption).
				if n := probeLlamaProps(ctx, baseURL); n > 0 {
					caps.ContextLength = n
				}
			}
			return caps, nil
		}
		if fallback == nil && (caps.ContextLength > 0 || caps.SupportsVision) {
			c := caps
			fallback = &c
		}
	}
	if fallback != nil {
		return *fallback, nil
	}

	// Return best-effort from first entry.
	first, err := unmarshalModelEntry(body.Data[0])
	if err != nil {
		return Caps{}, fmt.Errorf("parse first model: %w", err)
	}
	return parseModelEntry(first), nil
}

type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Owned  string `json:"owned_by"`
	// llama.cpp extension fields
	TrainedTokens *int    `json:"trained_tokens"`
	Projector     *string `json:"projector"`
	// Generic field for n_ctx (llama.cpp may use different key)
	Raw map[string]interface{} `json:"-"`
}

// unmarshalModelEntry handles the flexible field parsing needed for
// llama.cpp vs Anthropic-style responses.
func unmarshalModelEntry(raw []byte) (modelEntry, error) {
	var entry modelEntry
	// First pass: standard fields
	type standard struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Owned  string `json:"owned_by"`
	}
	var std standard
	if err := json.Unmarshal(raw, &std); err != nil {
		return entry, err
	}
	entry.ID = std.ID
	entry.Object = std.Object
	entry.Owned = std.Owned

	// Second pass: catch-all for extension fields
	var all map[string]interface{}
	if err := json.Unmarshal(raw, &all); err != nil {
		return entry, err
	}
	entry.Raw = all

	if v, ok := all["trained_tokens"]; ok {
		if f, ok := v.(float64); ok {
			i := int(f)
			entry.TrainedTokens = &i
		}
	}
	if v, ok := all["projector"]; ok {
		if s, ok := v.(string); ok && s != "" {
			entry.Projector = &s
		}
	}

	return entry, nil
}

func parseModelEntry(entry modelEntry) Caps {
	caps := Caps{
		ContextLength:  0,
		SupportsVision: false,
		SupportsTools:  false,
		Source:         "probed",
	}

	// llama.cpp: trained_tokens or n_ctx
	if entry.TrainedTokens != nil {
		caps.ContextLength = *entry.TrainedTokens
	}
	if v, ok := entry.Raw["n_ctx"]; ok {
		if f, ok := v.(float64); ok {
			caps.ContextLength = int(f)
		}
	}
	// OpenAI-compatible hosts (OpenRouter, etc.): top-level context_length,
	// preferring the nested top_provider's serving window when present.
	if v, ok := entry.Raw["context_length"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			caps.ContextLength = int(f)
		}
	}
	if tp, ok := entry.Raw["top_provider"].(map[string]interface{}); ok {
		if v, ok := tp["context_length"]; ok {
			if f, ok := v.(float64); ok && f > 0 {
				caps.ContextLength = int(f)
			}
		}
	}

	// llama.cpp: projector presence indicates vision
	if entry.Projector != nil && *entry.Projector != "" {
		caps.SupportsVision = true
	}

	// Anthropic-style: tools support inferred from model family
	if strings.Contains(entry.ID, "claude-3") || strings.Contains(entry.ID, "claude-3-opus") ||
		strings.Contains(entry.ID, "claude-3-sonnet") || strings.Contains(entry.ID, "claude-3-haiku") ||
		strings.Contains(entry.ID, "claude-3.5") || strings.Contains(entry.ID, "claude-3.7") {
		caps.SupportsTools = true
	}

	// OpenAI-style: gpt-4 and later support tools
	if strings.Contains(entry.ID, "gpt-4") || strings.Contains(entry.ID, "gpt-3.5") ||
		strings.Contains(entry.ID, "o1") || strings.Contains(entry.ID, "o3") {
		caps.SupportsTools = true
	}

	return caps
}

// probeLlamaProps fetches llama.cpp's /props endpoint (server root, i.e.
// baseURL with any /v1 suffix trimmed) and returns the serving window,
// or 0 on any failure. This is the authoritative *serving* window (what
// -ctx actually gave us), not the training length. Builds differ on
// placement (top level vs. default_generation_settings.params), so we
// search recursively for the first "n_ctx" numeric.
func probeLlamaProps(ctx context.Context, baseURL string) int {
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var props map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return 0
	}
	return findNCTX(props)
}

// findNCTX returns the first positive "n_ctx" integer found in the JSON
// tree (breadth-first so top-level wins over nested params).
func findNCTX(m map[string]any) int {
	if v, ok := m["n_ctx"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			return int(f)
		}
	}
	// Deterministic order for nested descent.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if sub, ok := m[k].(map[string]any); ok {
			if n := findNCTX(sub); n > 0 {
				return n
			}
		}
	}
	return 0
}
