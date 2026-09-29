package memindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyQuery is returned when Search is called with an empty query.
var ErrEmptyQuery = errors.New("memindex: empty query")

// Hit is a single search result.
type Hit struct {
	Path  string
	Title string
	Score float64
}

// Search executes an FTS5 query and returns ranked hits.
// Score is the raw bm25 value (negative; lower is better).
func (ix *Index) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, ErrEmptyQuery
	}

	escaped := escapeQuery(query)
	rows, err := ix.db.QueryContext(ctx,
		"SELECT path, title, bm25(docs) AS score FROM docs WHERE docs MATCH ? ORDER BY score ASC LIMIT ?",
		escaped, limit)
	if err != nil {
		return nil, fmt.Errorf("memindex: search: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Path, &h.Title, &h.Score); err != nil {
			return nil, fmt.Errorf("memindex: scan: %w", err)
		}
		hits = append(hits, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memindex: rows: %w", err)
	}

	if hits == nil {
		return []Hit{}, nil
	}

	return hits, nil
}

// escapeQuery splits the user query on whitespace, wraps each token in
// double quotes (doubling internal double-quotes), and joins with spaces.
// This ensures FTS5 AND semantics and prevents operator injection.
func escapeQuery(query string) string {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return ""
	}
	escaped := make([]string, 0, len(tokens))
	for _, t := range tokens {
		s := strings.ReplaceAll(t, `"`, `""`)
		escaped = append(escaped, `"`+s+`"`)
	}
	return strings.Join(escaped, " ")
}
