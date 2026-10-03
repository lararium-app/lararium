package penatus

import (
	"regexp"
	"strings"
)

// MemLine represents a single parsed line from MEMORY.md.
type MemLine struct {
	Priority string // "high", "med", or "low"
	Since    string // "YYYY-MM-DD" or empty
	Text     string // the fact text
	Raw      string // original raw line
}

var (
	rePriority = regexp.MustCompile(`\[p:(high|med|low)\]`)
	reSince    = regexp.MustCompile(`\[since:(\d{4}-\d{2}-\d{2})\]`)
)

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

		line.Text = strings.TrimSpace(content)

		lines = append(lines, line)
	}

	return lines
}

// SerializeMemoryBody converts MemLine entries back to canonical MEMORY.md body.
// Format: "- [p:X] [since:Y] text" (omits absent tags).
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

		sb.WriteString(l.Text)
		parts = append(parts, sb.String())
	}

	return strings.Join(parts, "\n")
}
