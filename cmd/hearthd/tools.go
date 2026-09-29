package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// buildTools returns the v1 tool registry. home is the persona root.
func buildTools(home string) []loop.Tool {
	return []loop.Tool{
		{
			Spec: router.ToolSpec{
				Name:        "remember",
				Description: "Persist a durable fact to long-term memory. Use for anything worth recalling after a restart: preferences, decisions, identifiers, people, projects.",
				ParamsJSONSchema: `{"type":"object","properties":{
					"text":{"type":"string","description":"the fact, one concise sentence"},
					"priority":{"type":"string","enum":["high","med","low"],"description":"high=identity-grade, med=standing, low=transient"}
				},"required":["text"]}`,
			},
			Trusted: true,
			Run: func(_ context.Context, argsJSON string) (string, error) {
				var a struct {
					Text     string `json:"text"`
					Priority string `json:"priority"`
				}
				if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
					return "", fmt.Errorf("bad args: %w", err)
				}
				if strings.TrimSpace(a.Text) == "" {
					return "", fmt.Errorf("text is required")
				}
				if a.Priority == "" {
					a.Priority = "med"
				}
				if err := appendMemoryLine(home, a.Text, a.Priority); err != nil {
					return "", err
				}
				return "stored", nil
			},
		},
		{
			Spec: router.ToolSpec{
				Name:        "forget",
				Description: "Remove a remembered fact from MEMORY.md by exact (or distinctive) text match.",
				ParamsJSONSchema: `{"type":"object","properties":{
					"text":{"type":"string","description":"text of the fact to remove"}
				},"required":["text"]}`,
			},
			Trusted: false, // deletion always asks (spec: destructive → approval)
			Run: func(_ context.Context, argsJSON string) (string, error) {
				var a struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
					return "", fmt.Errorf("bad args: %w", err)
				}
				n, err := removeMemoryLines(home, a.Text)
				if err != nil {
					return "", err
				}
				if n == 0 {
					return "no matching line", nil
				}
				return fmt.Sprintf("removed %d line(s)", n), nil
			},
		},
		{
			Spec: router.ToolSpec{
				Name:             "clock",
				Description:      "Current date and time (host clock).",
				ParamsJSONSchema: `{"type":"object","properties":{}}`,
			},
			Trusted: true,
			Run: func(_ context.Context, _ string) (string, error) {
				return time.Now().Format("Monday, 2006-01-02 15:04:05 MST"), nil
			},
		},
	}
}

// memoryPath is MEMORY.md under home (same file the system prompt injects).
func memoryPath(home string) string { return filepath.Join(home, "MEMORY.md") }

// loadMemory reads MEMORY.md, preserving its frontmatter for rewrite.
func loadMemory(home string) (penatus.Doc, error) {
	b, err := os.ReadFile(memoryPath(home))
	if os.IsNotExist(err) {
		return penatus.Doc{Meta: map[string]string{"name": "MEMORY", "description": "long-term memory", "metadata": "{ \"node_type\": \"memory\" }"}}, nil
	}
	if err != nil {
		return penatus.Doc{}, err
	}
	return penatus.ParseDoc(b)
}

func saveMemory(home string, doc penatus.Doc) error {
	return os.WriteFile(memoryPath(home), doc.Marshal(), 0o600)
}

func appendMemoryLine(home, text, priority string) error {
	doc, err := loadMemory(home)
	if err != nil {
		return err
	}
	lines := penatus.ParseMemoryBody(doc.Body)
	lines = append(lines, penatus.MemLine{Priority: priority, Since: time.Now().UTC().Format("2006-01-02"), Text: text})
	doc.Body = penatus.SerializeMemoryBody(lines)
	return saveMemory(home, doc)
}

func removeMemoryLines(home, find string) (int, error) {
	doc, err := loadMemory(home)
	if err != nil {
		return 0, err
	}
	lines := penatus.ParseMemoryBody(doc.Body)
	var kept []penatus.MemLine
	n := 0
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l.Text), strings.ToLower(find)) {
			n++
			continue
		}
		kept = append(kept, l)
	}
	if n > 0 {
		doc.Body = penatus.SerializeMemoryBody(kept)
		if err := saveMemory(home, doc); err != nil {
			return 0, err
		}
	}
	return n, nil
}
