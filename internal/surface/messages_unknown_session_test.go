package surface

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modelCalls reports how many completions the fixture's stub provider has
// served, i.e. whether any turn reached the model.
func (f *audienceFixture) modelCalls() int {
	f.sp.mu.Lock()
	defer f.sp.mu.Unlock()
	return int(f.sp.n)
}

// A well-formed id that names no session must 404 before any turn starts,
// exactly as GET …/events does, and must leave nothing on disk.
func TestMessages_UnknownSessionIs404WithoutATurn(t *testing.T) {
	f := newAudienceFixture(t)
	const unknown = "s_0000000000000000000000000A"

	for _, body := range []string{`{"text":"hello"}`, `not json`, `{"text":""}`} {
		code, out := f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+unknown+"/messages", body)
		if code != http.StatusNotFound || out != `{"error":"not found"}` {
			t.Errorf("POST messages %q on unknown id = %d %s, want 404 {\"error\":\"not found\"}", body, code, out)
		}
	}

	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times for an unknown session, want 0", n)
	}
	if f.hub.InFlight(unknown) {
		t.Error("a turn is in flight for an unknown session")
	}
	if _, err := os.Stat(filepath.Join(f.hub.hearthHome, "sessions", unknown)); !os.IsNotExist(err) {
		t.Errorf("sessions/%s was created on disk (stat err = %v)", unknown, err)
	}
	if _, ok := f.listREST(t, f.tokA)[unknown]; ok {
		t.Error("unknown session appeared in the listing")
	}

	// Same reply as the events path gives the same id.
	ecode, ebody := f.do(t, f.tokA, http.MethodGet, "/v1/sessions/"+unknown+"/events", "")
	if ecode != http.StatusNotFound || ebody != `{"error":"not found"}` {
		t.Errorf("events on unknown id = %d %s, want the same 404", ecode, ebody)
	}
}

// A directory without a header is not a session (the events path treats it
// the same way); posting to it must not start a turn or touch the log.
func TestMessages_HeaderlessDirectoryIs404(t *testing.T) {
	f := newAudienceFixture(t)
	const orphan = "s_0000000000000000000000000B"
	dir := filepath.Join(f.hub.hearthHome, "sessions", orphan)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	code, out := f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+orphan+"/messages", `{"text":"hello"}`)
	if code != http.StatusNotFound || out != `{"error":"not found"}` {
		t.Fatalf("POST messages on headerless dir = %d %s, want 404", code, out)
	}
	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !os.IsNotExist(err) {
		t.Errorf("events.jsonl was created in the headerless dir (stat err = %v)", err)
	}
}

// The fix must not touch the working paths: a session created over the API
// still takes messages, and main takes them even before its directory
// exists (the first turn creates it).
func TestMessages_KnownSessionsStillRunTurns(t *testing.T) {
	f := newAudienceFixture(t)

	side := f.createREST(t, f.tokA, false)
	code, out := f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+side.ID+"/messages", `{"text":"hello"}`)
	if code != http.StatusOK || !strings.Contains(out, "turn_done") {
		t.Fatalf("POST messages on a created session = %d %s, want 200 with turn_done", code, out)
	}
	if n := f.modelCalls(); n != 1 {
		t.Errorf("model calls = %d after one turn on a known session, want 1", n)
	}

	if err := os.RemoveAll(filepath.Join(f.hub.hearthHome, "sessions", "main")); err != nil {
		t.Fatal(err)
	}
	code, out = f.do(t, f.tokA, http.MethodPost, "/v1/sessions/main/messages", `{"text":"hello"}`)
	if code != http.StatusOK || !strings.Contains(out, "turn_done") {
		t.Fatalf("POST messages on main without a directory = %d %s, want 200 with turn_done", code, out)
	}
}
