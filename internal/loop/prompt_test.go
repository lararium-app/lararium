package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func validFiles() map[string]string {
	return map[string]string{
		"SOUL.md":     "---\npenatus: 1\nupdated: 2026-09-28\n---\nBe direct. Never fabricate.\n",
		"IDENTITY.md": "---\npenatus: 1\n---\nName: Lararium\nEmoji: 🔥\n",
		"USER.md":     "---\npenatus: 1\n---\nTest User, solo dev.\n",
		"MEMORY.md":   "---\npenatus: 1\nupdated: 2026-09-28\n---\n- [p:high] [since:2026-09-28] Gate G1 approved.\n",
	}
}

func TestSystemPromptGolden(t *testing.T) {
	home := mkHome(t, validFiles())
	got, warnings, err := SystemPrompt(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := "## SOUL\nBe direct. Never fabricate.\n\n" +
		"## IDENTITY\nName: Lararium\nEmoji: 🔥\n\n" +
		"## USER\nTest User, solo dev.\n\n" +
		"## MEMORY\n- [p:high] [since:2026-09-28] Gate G1 approved.\n"
	if got != want {
		t.Errorf("golden mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestSystemPromptMissingFileRefuses(t *testing.T) {
	f := validFiles()
	delete(f, "SOUL.md")
	_, _, err := SystemPrompt(mkHome(t, f))
	if err == nil || !strings.Contains(err.Error(), "missing required file SOUL.md") {
		t.Fatalf("want missing-file refusal, got %v", err)
	}
}

func TestSystemPromptOverBudgetRefuses(t *testing.T) {
	f := validFiles()
	f["SOUL.md"] = "---\npenatus: 1\n---\n" + strings.Repeat("x", 5000) + "\n"
	_, _, err := SystemPrompt(mkHome(t, f))
	if err == nil || !strings.Contains(err.Error(), "over budget") {
		t.Fatalf("want budget refusal, got %v", err)
	}
}

func TestSystemPromptMemoryOverBudgetWarnsNotRefuses(t *testing.T) {
	f := validFiles()
	f["MEMORY.md"] = "---\npenatus: 1\n---\n" + strings.Repeat("y", 9000) + "\n"
	got, warnings, err := SystemPrompt(mkHome(t, f))
	if err != nil {
		t.Fatalf("MEMORY over budget must warn, not refuse: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "rotate") {
		t.Fatalf("want rotation warning, got %v", warnings)
	}
	if !strings.Contains(got, "## MEMORY") {
		t.Fatal("over-budget MEMORY.md must still be injected (warning, not truncation)")
	}
}

func TestSystemPromptSkipsHeartbeat(t *testing.T) {
	f := validFiles()
	f["HEARTBEAT.md"] = "---\npenatus: 1\n---\nSECRET STANDING ORDER\n"
	got, _, err := SystemPrompt(mkHome(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "SECRET STANDING ORDER") {
		t.Fatal("HEARTBEAT.md must not be injected into chat turns")
	}
}
