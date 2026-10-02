// Package loop wires penatus (memory) + router (models) into the session
// loop: prompt assembly with spec budgets, compaction trigger, turn runner.
package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lararium-app/lararium/internal/penatus"
)

// Persona file budgets in BYTES (spec §1: SOUL 4KB, IDENTITY 2KB, USER 4KB;
// MEMORY budget is token-based, handled separately via its 2k-line/len check).
var personaBudget = map[string]int{
	"SOUL.md":     4096,
	"IDENTITY.md": 2048,
	"USER.md":     4096,
}

// personaOrder is the injection order into the system prompt.
var personaOrder = []string{"SOUL.md", "IDENTITY.md", "USER.md"}

// SystemPrompt reads the persona files from home and assembles the system
// prompt. Per spec §1.1: over-budget persona files cause a REFUSAL, never
// silent truncation. Over-budget MEMORY.md is a warning with a rotation
// hint (§1.4 — different rule by design). HEARTBEAT.md is NOT injected into
// chat turns (the heartbeat loop reads it separately).
func SystemPrompt(home string) (string, []string, error) {
	var warnings []string
	var sb strings.Builder
	for _, name := range personaOrder {
		path := filepath.Join(home, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil, fmt.Errorf("persona: missing required file %s", name)
			}
			return "", nil, fmt.Errorf("persona: read %s: %w", name, err)
		}
		// Budget applies to the *injected* content: the body, not the
		// frontmatter (frontmatter is metadata, not prompt).
		doc, err := penatus.ParseDoc(data)
		if err != nil {
			return "", nil, fmt.Errorf("persona: parse %s: %w", name, err)
		}
		body := strings.TrimSpace(doc.Body)
		if lim, ok := personaBudget[name]; ok && len(body) > lim {
			return "", nil, fmt.Errorf("persona: %s body is %d bytes, over budget %d — refusing to start (spec §1.1); edit the file or split content into memory/", name, len(body), lim)
		}
		if body == "" {
			continue
		}
		sb.WriteString("## " + strings.TrimSuffix(name, ".md") + "\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	// MEMORY.md always rides along (spec §1.4: "injected in full" under its
	// own budget, enforced at 2,000 tokens ≈ generous 8 KB byte proxy here;
	// exact tokenizer accounting arrives with per-provider caps).
	memPath := filepath.Join(home, "MEMORY.md")
	if data, err := os.ReadFile(memPath); err == nil {
		doc, err := penatus.ParseDoc(data)
		if err != nil {
			return "", nil, fmt.Errorf("persona: parse MEMORY.md: %w", err)
		}
		body := strings.TrimSpace(doc.Body)
		if len(body) > 8192 {
			// Spec §1.4: over budget => prompt to rotate oldest low lines
			// into memory/notes/ — a warning, NOT a refusal (contrast §1.1).
			warnings = append(warnings, fmt.Sprintf("MEMORY.md body %d bytes over ~2k-token budget: rotate oldest [p:low] lines into memory/notes/ (spec §1.4)", len(body)))
		}
		if body != "" {
			sb.WriteString("## MEMORY\n")
			sb.WriteString(body)
			sb.WriteString("\n")
		}
	} else if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("persona: read MEMORY.md: %w", err)
	}
	return sb.String(), warnings, nil
}
