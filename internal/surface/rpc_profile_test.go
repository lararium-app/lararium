package surface

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
)

func TestRPCProfileDescribeAndConfigure(t *testing.T) {
	home := t.TempDir()
	soulContent := "---\npenatus: 1\nvoice: direct\ncustom_key: keep-me\n---\nOriginal soul body.\n"
	identityContent := "---\npenatus: 1\nname: Hearth\n---\nOriginal identity.\n"
	userContent := "---\npenatus: 1\ntimezone: UTC\n---\nOriginal user.\n"
	if err := os.WriteFile(filepath.Join(home, "SOUL.md"), []byte(soulContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "IDENTITY.md"), []byte(identityContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "USER.md"), []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// MEMORY.md intentionally absent initially.

	var auditActor string
	var auditChanges []ProfileAuditChange

	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	srv.PersonaHome = home
	srv.ProfileAudit = func(actor string, changes []ProfileAuditChange) {
		auditActor = actor
		auditChanges = append([]ProfileAuditChange(nil), changes...)
	}

	tok, err := store.Create("profile-test")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	// 1. profile.describe returns stripped bodies, exact budgets (no memory_budget),
	// omits missing memory + memory_bytes, and returns mtime ms revisions.
	descResp := client.callRPC(t, 1, "profile.describe", map[string]any{})
	if descResp.Error != nil {
		t.Fatalf("profile.describe error: %+v", descResp.Error)
	}
	var descRaw map[string]json.RawMessage
	if err := json.Unmarshal(descResp.Result, &descRaw); err != nil {
		t.Fatalf("unmarshal describe raw: %v", err)
	}
	var desc struct {
		Soul           string           `json:"soul"`
		SoulBytes      int              `json:"soul_bytes"`
		SoulBudget     int              `json:"soul_budget"`
		IdentityBudget int              `json:"identity_budget"`
		UserBudget     int              `json:"user_budget"`
		Revisions      map[string]int64 `json:"revisions"`
	}
	if err := json.Unmarshal(descResp.Result, &desc); err != nil {
		t.Fatalf("unmarshal describe: %v", err)
	}
	if desc.Soul != "Original soul body.\n" {
		t.Errorf("soul = %q, want stripped body", desc.Soul)
	}
	if desc.SoulBytes != len(soulContent) {
		t.Errorf("soul_bytes = %d, want %d", desc.SoulBytes, len(soulContent))
	}
	if desc.SoulBudget != 4096 || desc.IdentityBudget != 2048 || desc.UserBudget != 4096 {
		t.Errorf("unexpected budgets: %+v", desc)
	}
	if _, hasMemBudget := descRaw["memory_budget"]; hasMemBudget {
		t.Errorf("profile.describe must NOT include memory_budget")
	}
	if _, hasMem := descRaw["memory"]; hasMem {
		t.Errorf("missing MEMORY.md should omit memory field")
	}
	if _, hasMemBytes := descRaw["memory_bytes"]; hasMemBytes {
		t.Errorf("missing MEMORY.md should omit memory_bytes field")
	}

	soulRev := desc.Revisions["soul"]
	identityRev := desc.Revisions["identity"]
	if soulRev <= 0 || identityRev <= 0 {
		t.Fatalf("expected positive mtime ms revisions, got %+v", desc.Revisions)
	}
	if desc.Revisions["memory"] != 0 {
		t.Fatalf("missing memory revision = %d, want 0", desc.Revisions["memory"])
	}

	// 2. Partial-apply independence + CAS + budgets + frontmatter preservation + MEMORY create + audit.
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	secretPhrase := "TOP_SECRET_PERSONAL_SENTENCE_DO_NOT_LOG"
	cfgResp := client.callRPC(t, 2, "profile.configure", map[string]any{
		"soul":     "Updated soul with " + secretPhrase + "\n",
		"identity": strings.Repeat("x", 2049), // over 2048 budget
		"user":     "   \t\n  ",               // empty/whitespace -> invalid
		"memory":   "- [p:high] Created memory entry.\n",
		"expected_revisions": map[string]int64{
			"soul":     soulRev,
			"identity": identityRev,
			"memory":   0,
		},
	})
	if cfgResp.Error != nil {
		t.Fatalf("profile.configure error: %+v", cfgResp.Error)
	}
	var cfgOut struct {
		OK      bool              `json:"ok"`
		Applied map[string]string `json:"applied"`
	}
	if err := json.Unmarshal(cfgResp.Result, &cfgOut); err != nil {
		t.Fatalf("unmarshal configure: %v", err)
	}
	if !cfgOut.OK {
		t.Fatalf("ok = false, want true")
	}
	if cfgOut.Applied["soul"] != "ok" {
		t.Errorf("applied.soul = %q, want ok", cfgOut.Applied["soul"])
	}
	if cfgOut.Applied["identity"] != "over_budget" {
		t.Errorf("applied.identity = %q, want over_budget", cfgOut.Applied["identity"])
	}
	if cfgOut.Applied["user"] != "invalid" {
		t.Errorf("applied.user = %q, want invalid", cfgOut.Applied["user"])
	}
	if cfgOut.Applied["memory"] != "ok" {
		t.Errorf("applied.memory = %q, want ok", cfgOut.Applied["memory"])
	}

	// Verify SOUL.md preserved frontmatter keys verbatim.
	newSoulRaw, err := os.ReadFile(filepath.Join(home, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	parsedSoul, err := penatus.ParseDoc(newSoulRaw)
	if err != nil {
		t.Fatalf("ParseDoc updated SOUL.md: %v", err)
	}
	if parsedSoul.Meta["voice"] != "direct" || parsedSoul.Meta["custom_key"] != "keep-me" {
		t.Errorf("frontmatter not preserved: %+v", parsedSoul.Meta)
	}
	if !strings.Contains(parsedSoul.Body, secretPhrase) {
		t.Errorf("updated soul body missing new content: %q", parsedSoul.Body)
	}

	// Verify MEMORY.md was created with template frontmatter.
	newMemRaw, err := os.ReadFile(filepath.Join(home, "MEMORY.md"))
	if err != nil {
		t.Fatalf("MEMORY.md not created: %v", err)
	}
	parsedMem, err := penatus.ParseDoc(newMemRaw)
	if err != nil {
		t.Fatalf("ParseDoc created MEMORY.md: %v", err)
	}
	if parsedMem.Meta["penatus"] != "1" {
		t.Errorf("MEMORY.md frontmatter penatus = %q, want 1", parsedMem.Meta["penatus"])
	}

	// Verify audit line + callback carry field names and byte deltas, NEVER personal content.
	if auditActor != "web" || len(auditChanges) != 2 {
		t.Errorf("audit hook actor=%q changes=%+v, want actor=web len=2", auditActor, auditChanges)
	}
	logLine := logBuf.String()
	if !strings.Contains(logLine, "actor=web") || !strings.Contains(logLine, "soul:") || !strings.Contains(logLine, "memory:") {
		t.Errorf("audit log missing expected fields: %q", logLine)
	}
	if strings.Contains(logLine, secretPhrase) {
		t.Errorf("audit log leaked personal content: %q", logLine)
	}

	// 3. CAS conflict per field (stale revision -> revision_conflict, no write).
	conflictResp := client.callRPC(t, 3, "profile.configure", map[string]any{
		"soul": "Should not overwrite.\n",
		"expected_revisions": map[string]int64{
			"soul": 1, // stale
		},
	})
	if conflictResp.Error != nil {
		t.Fatalf("unexpected error: %+v", conflictResp.Error)
	}
	var conflictOut struct {
		Applied map[string]string `json:"applied"`
	}
	if err := json.Unmarshal(conflictResp.Result, &conflictOut); err != nil {
		t.Fatal(err)
	}
	if conflictOut.Applied["soul"] != "revision_conflict" {
		t.Errorf("applied.soul = %q, want revision_conflict", conflictOut.Applied["soul"])
	}

	// 4. Unparseable frontmatter: describe returns raw bytes, configure refuses with invalid.
	brokenRaw := "no frontmatter at all\n"
	if err := os.WriteFile(filepath.Join(home, "USER.md"), []byte(brokenRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	descBroken := client.callRPC(t, 4, "profile.describe", map[string]any{})
	var descBrokenMap map[string]any
	if err := json.Unmarshal(descBroken.Result, &descBrokenMap); err != nil {
		t.Fatal(err)
	}
	if descBrokenMap["user"] != brokenRaw {
		t.Errorf("broken USER.md describe = %q, want raw bytes %q", descBrokenMap["user"], brokenRaw)
	}

	refuseResp := client.callRPC(t, 5, "profile.configure", map[string]any{
		"user": "Try to overwrite broken file.\n",
	})
	var refuseOut struct {
		Applied map[string]string `json:"applied"`
	}
	if err := json.Unmarshal(refuseResp.Result, &refuseOut); err != nil {
		t.Fatal(err)
	}
	if refuseOut.Applied["user"] != "invalid" {
		t.Errorf("broken USER.md configure = %q, want invalid", refuseOut.Applied["user"])
	}
	afterBroken, _ := os.ReadFile(filepath.Join(home, "USER.md"))
	if string(afterBroken) != brokenRaw {
		t.Errorf("broken USER.md was clobbered!")
	}
}
