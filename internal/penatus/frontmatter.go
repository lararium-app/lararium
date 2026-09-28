package penatus

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNoFrontmatter is returned when a doc has no YAML frontmatter block.
var ErrNoFrontmatter = errors.New("no frontmatter")

// Doc holds parsed frontmatter metadata and the body below it.
type Doc struct {
	Meta map[string]string
	Body string

	// keyOrder preserves the original key order for round-trip marshaling.
	keyOrder []string
}

// ParseDoc parses a UTF-8 document with YAML frontmatter delimited by ---.
// The frontmatter block contains flat key: value lines (no nesting, no lists).
// Values with embedded colons are split on the FIRST colon only.
// A file with no frontmatter returns ErrNoFrontmatter.
// A frontmatter block not closed by --- returns an error.
func ParseDoc(b []byte) (Doc, error) {
	s := string(b)

	// Must start with ---\n
	if !strings.HasPrefix(s, "---\n") {
		return Doc{}, ErrNoFrontmatter
	}

	// Find the closing ---
	idx := strings.Index(s[4:], "\n---")
	if idx == -1 {
		return Doc{}, fmt.Errorf("frontmatter not closed")
	}

	// Extract the frontmatter block and body (closing --- may be at EOF
	// with or without a trailing newline).
	block := s[4 : 4+idx]
	rest := s[4+idx+4:] // after "\n---"
	var body string
	switch {
	case strings.HasPrefix(rest, "\r\n"):
		body = rest[2:]
	case strings.HasPrefix(rest, "\n"):
		body = rest[1:]
	case rest == "":
		body = ""
	default:
		return Doc{}, fmt.Errorf("frontmatter not closed")
	}

	meta := make(map[string]string)
	var keyOrder []string

	lines := strings.Split(block, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])

		if key != "" {
			meta[key] = value
			keyOrder = append(keyOrder, key)
		}
	}

	return Doc{
		Meta:     meta,
		Body:     body,
		keyOrder: keyOrder,
	}, nil
}

// Marshal returns the document as a byte slice with frontmatter and body.
// Key order matches the original parse order (or insertion order for new Docs).
func (d Doc) Marshal() []byte {
	order := d.keyOrder
	if len(order) == 0 && len(d.Meta) > 0 {
		// Fresh Doc (not parsed): deterministic sorted order.
		order = make([]string, 0, len(d.Meta))
		for k := range d.Meta {
			order = append(order, k)
		}
		sort.Strings(order)
	}
	var sb strings.Builder
	sb.WriteString("---\n")

	for _, key := range order {
		value := d.Meta[key]
		sb.WriteString(key)
		sb.WriteString(": ")
		sb.WriteString(value)
		sb.WriteString("\n")
	}

	sb.WriteString("---\n")
	sb.WriteString(d.Body)

	return []byte(sb.String())
}

// Required checks that the document has the required frontmatter keys for the
// given file kind. It verifies:
//   - "penatus" is present and equals "1"
//   - "updated" is present (unless kind is "memory-line")
func Required(d Doc, kind string) error {
	if v, ok := d.Meta["penatus"]; !ok {
		return fmt.Errorf("%s: missing required key 'penatus'", kind)
	} else if v != "1" {
		return fmt.Errorf("%s: penatus must be 1, got %q", kind, v)
	}

	if kind != "memory-line" {
		if _, ok := d.Meta["updated"]; !ok {
			return fmt.Errorf("%s: missing required key 'updated'", kind)
		}
	}

	return nil
}
