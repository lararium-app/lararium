package surface

import (
	"testing"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

func staticTool(name string) loop.Tool {
	return loop.Tool{Spec: router.ToolSpec{Name: name}}
}

func TestHubToolSourceMerge(t *testing.T) {
	h := &Hub{tools: []loop.Tool{staticTool("remember"), staticTool("clock")}}

	if got := h.currentTools(); len(got) != 2 {
		t.Fatalf("no source: %d tools, want 2", len(got))
	}

	h.SetToolSource(func() []loop.Tool {
		return []loop.Tool{staticTool("mcp__demo__echo"), staticTool("remember")}
	})
	got := h.currentTools()
	if len(got) != 3 {
		t.Fatalf("merged: %d tools, want 3 (static wins on name clash)", len(got))
	}
	if got[0].Spec.Name != "remember" || got[2].Spec.Name != "mcp__demo__echo" {
		t.Fatalf("unexpected merge order: %v", got)
	}

	h.SetToolSource(nil)
	if got := h.currentTools(); len(got) != 2 {
		t.Fatalf("cleared: %d tools, want 2", len(got))
	}
}
