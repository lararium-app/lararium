package penatus

import (
	"regexp"
	"strings"
)

// MemLine represents a single parsed line from MEMORY.md.
type MemLine struct {
	Priority string // "high", "med", or "low"
	Since    string // "YYYY-MM-DD" or empty
	Source   string // "" (operator-owned) or MemSourceAssistant (F4)
	Text     string // the fact text
	Raw      string // original raw line
}

// MemSourceAssistant marks MEMORY.md lines persisted by the assistant
// through the remember tool. Such lines render under a data-not-
// instructions framing (F4): content that reached the model from tools
// or pages must not return as trusted instruction after a restart.
const MemSourceAssistant = "assistant"

var (
	rePriority = regexp.MustCompile(`\[p:(high|med|low)\]`)
	reSince    = regexp.MustCompile(`\[since:(\d{4}-\d{2}-\d{2})\]`)
	reSrc      = regexp.MustCompile(`\[src:([a-z]+)\]`)
)

// reTagLike matches the opening of any memory tag. The remember tool
// strips these from fact text so stored text can never forge provenance
// or priority (F4); the brackets are what ParseMemoryBody keys on.
var reTagLike = regexp.MustCompile(`(?i)\[(src|p|since):`)

// SanitizeFactText normalizes assistant-supplied fact text for storage:
// line breaks collapse (one fact = one line), tag-like openings lose
// their bracket so they parse as plain words, and the text is capped at
// the spec §1.4 promotion threshold (500 chars).
func SanitizeFactText(s string) string {
	r := strings.NewReplacer("\r", " ", "\n", " ", "	", " ")
	s = reTagLike.ReplaceAllString(r.Replace(s), "$1:")
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > 500 {
		s = string(runes[:500])
	}
	return s
}

// ParseMemoryBody parses the body of MEMORY.md into MemLine entries.
// Per spec §6.2, parsing is lenient: lines starting with "- " are entries.
// [p:high|med|low] and [since:YYYY-MM-DD] tags are extracted in any order.
// Malformed lines degrade to Priority "med", Since "", Text = full text.
// Non-entry lines (not starting with "- ") are skipped. Never errors.
func ParseMemoryBody(body string) []MemLine {
	var lines []MemLine

	for _, rawLine := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}

		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}

		// Strip the leading "- "
		content := strings.TrimSpace(trimmed[2:])

		line := MemLine{
			Priority: "med",
			Since:    "",
			Text:     content,
			Raw:      rawLine,
		}

		// Extract priority tag
		if m := rePriority.FindStringSubmatch(content); m != nil {
			line.Priority = m[1]
			content = rePriority.ReplaceAllString(content, "")
		}

		// Extract since tag
		if m := reSince.FindStringSubmatch(content); m != nil {
			line.Since = m[1]
			content = reSince.ReplaceAllString(content, "")
		}

		// Extract provenance tag (F4). Untagged stays operator-owned.
		if m := reSrc.FindStringSubmatch(content); m != nil {
			line.Source = m[1]
			content = reSrc.ReplaceAllString(content, "")
		}

		line.Text = strings.TrimSpace(content)

		lines = append(lines, line)
	}

	return lines
}

// SerializeMemoryBody converts MemLine entries back to canonical MEMORY.md body.
// Format: "- [p:X] [since:Y] [src:Z] text" (omits absent tags).
// Must be idempotent with ParseMemoryBody.
func SerializeMemoryBody(lines []MemLine) string {
	var parts []string

	for _, l := range lines {
		var sb strings.Builder
		sb.WriteString("- ")

		if l.Priority != "" && l.Priority != "med" {
			sb.WriteString("[p:")
			sb.WriteString(l.Priority)
			sb.WriteString("] ")
		} else if l.Priority == "med" {
			sb.WriteString("[p:med] ")
		}

		if l.Since != "" {
			sb.WriteString("[since:")
			sb.WriteString(l.Since)
			sb.WriteString("] ")
		}

		if l.Source != "" {
			sb.WriteString("[src:")
			sb.WriteString(l.Source)
			sb.WriteString("] ")
		}

		sb.WriteString(l.Text)
		parts = append(parts, sb.String())
	}

	return strings.Join(parts, "\n")
}
