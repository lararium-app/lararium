package memindex

import (
	"path/filepath"
	"strings"

	"github.com/lararium-app/lararium/internal/penatus"
)

// parseFile reads a markdown file and extracts title and body.
// For notes without a title in frontmatter, falls back to the filename stem.
func parseFile(path string, data []byte) (title, body string, err error) {
	doc, err := penatus.ParseDoc(data)
	if err != nil {
		return "", "", err
	}

	title = doc.Meta["title"]
	if title == "" {
		// Fall back to filename stem.
		stem := filepath.Base(path)
		stem = strings.TrimSuffix(stem, ".md")
		title = stem
	}

	return title, doc.Body, nil
}
