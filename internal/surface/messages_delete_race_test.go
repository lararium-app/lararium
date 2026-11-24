package surface

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gatedBody is a request body whose first Read blocks until released, so a
// test can hold a POST between the dispatcher's existence check and the
// turn's registration (the window the check/register race lives in).
type gatedBody struct {
	r       io.Reader
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedBody(s string) *gatedBody {
	return &gatedBody{r: strings.NewReader(s), reading: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.reading) })
	<-g.release
	return g.r.Read(p)
}

func (f *audienceFixture) sessionDir(id string) string {
	return filepath.Join(f.hub.hearthHome, "sessions", id)
}

func (f *audienceFixture) existsGate(id string) func() bool {
	return func() bool { return f.srv.sessionExists(id) }
}

// A delete that lands while the POST body is still being read must win: the
// turn that follows answers 404, runs no model call, and leaves no directory
// behind (previously it recreated a headerless zombie and ran a full turn).
func TestMessages_DeleteWhileBodyIsReadKeepsTheSessionGone(t *testing.T) {
	f := newAudienceFixture(t)
	id := f.createREST(t, f.tokA, false).ID

	body := newGatedBody(`{"text":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/messages", body)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.hub.HandleMessageGated(rec, req, id, f.existsGate(id))
	}()

	<-body.reading // the handler is past its caller's existence check, mid-read
	if code, out := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE during body read = %d %s, want 204 (no turn is registered yet)", code, out)
	}
	close(body.release)
	<-done

	if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"error":"not found"}` {
		t.Errorf("turn on a session deleted mid-request = %d %s, want 404 not found", rec.Code, rec.Body.String())
	}
	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times for a deleted session, want 0", n)
	}
	if f.hub.InFlight(id) {
		t.Error("a turn is still registered for the deleted session")
	}
	if _, err := os.Stat(f.sessionDir(id)); !os.IsNotExist(err) {
		t.Errorf("sessions/%s was recreated on disk (stat err = %v)", id, err)
	}
}

// A registered turn blocks deletion: 409 over REST, nothing removed.
func TestMessages_RegisteredTurnBlocksDelete(t *testing.T) {
	f := newAudienceFixture(t)
	id := f.createREST(t, f.tokA, false).ID

	if err := f.hub.acquire(id, &turn{cancel: func() {}}, f.existsGate(id)); err != nil {
		t.Fatalf("acquire on a stored session: %v", err)
	}
	if end, ok := f.hub.BeginDelete(id); ok {
		end()
		t.Fatal("BeginDelete succeeded while a turn is registered")
	}
	if code, out := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, ""); code != http.StatusConflict {
		t.Errorf("DELETE with a turn registered = %d %s, want 409", code, out)
	}
	if !f.srv.sessionExists(id) {
		t.Error("session was removed under a registered turn")
	}

	f.hub.release(id)
	if code, out := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, ""); code != http.StatusNoContent {
		t.Errorf("DELETE after the turn ended = %d %s, want 204", code, out)
	}
}

// While a deletion holds its claim a turn cannot register, even though the
// session's files may still be (partly) on disk; once the claim ends, turns
// on a surviving session work again.
func TestMessages_TurnRefusedWhileDeleteInProgress(t *testing.T) {
	f := newAudienceFixture(t)
	id := f.createREST(t, f.tokA, false).ID

	end1, ok := f.hub.BeginDelete(id)
	if !ok {
		t.Fatal("BeginDelete on an idle session failed")
	}
	end2, ok := f.hub.BeginDelete(id) // a concurrent delete may overlap; the later one finds nothing to remove
	if !ok {
		t.Fatal("second BeginDelete on an idle session failed")
	}

	code, out := f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+id+"/messages", `{"text":"hello"}`)
	if code != http.StatusNotFound || out != `{"error":"not found"}` {
		t.Errorf("POST while a delete is in progress = %d %s, want 404", code, out)
	}
	end1()
	if code, _ := f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+id+"/messages", `{"text":"hello"}`); code != http.StatusNotFound {
		t.Errorf("POST while one of two deletes is still in progress = %d, want 404", code)
	}
	end2()
	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times while deletes held the session, want 0", n)
	}

	code, out = f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+id+"/messages", `{"text":"hello"}`)
	if code != http.StatusOK || !strings.Contains(out, "turn_done") {
		t.Errorf("POST after the deletes ended = %d %s, want 200 with turn_done", code, out)
	}
}

// Hammer delete against turns on the same session. Whatever order they land
// in, the session is either fully there or fully gone: never a directory
// without its header.
func TestMessages_DeleteRacingTurnsNeverLeavesAZombie(t *testing.T) {
	f := newAudienceFixture(t)
	for i := range 40 {
		id := f.createREST(t, f.tokA, false).ID
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+id+"/messages", `{"text":"hello"}`)
		}()
		go func() {
			defer wg.Done()
			f.do(t, f.tokA, http.MethodPost, "/v1/sessions/"+id+"/messages", `{"text":"again"}`)
		}()
		go func() {
			defer wg.Done()
			f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, "")
		}()
		wg.Wait()

		if _, err := os.Stat(f.sessionDir(id)); err == nil && !f.srv.sessionExists(id) {
			t.Fatalf("iteration %d: sessions/%s exists without a header (zombie)", i, id)
		}
		if f.hub.InFlight(id) {
			t.Fatalf("iteration %d: a turn is still registered for %s", i, id)
		}
	}
}

// sessions.send is the same door over WebSocket: a connection that subscribed
// before the session was deleted must not revive it.
func TestRPCSessionsSend_AfterDeleteDoesNotReviveTheSession(t *testing.T) {
	f := newAudienceFixture(t)
	id := f.createREST(t, f.tokA, false).ID
	client := dialAuthenticatedWS(t, f.ts.URL, f.tokA)

	if resp := client.callRPC(t, 1, "sessions.subscribe", map[string]any{"id": id}); resp.Error != nil {
		t.Fatalf("subscribe: %+v", resp.Error)
	}
	if code, out := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", code, out)
	}

	resp := client.callRPC(t, 2, "sessions.send", map[string]any{"id": id, "text": "hello"})
	assertRPCErrorReason(t, resp, "not_found")
	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times for a deleted session, want 0", n)
	}
	if _, err := os.Stat(f.sessionDir(id)); !os.IsNotExist(err) {
		t.Errorf("sessions/%s was recreated on disk (stat err = %v)", id, err)
	}
}

// RPC delete takes the same claim as REST: a registered turn answers busy.
func TestRPCSessionsDelete_BusyWhileTurnRegistered(t *testing.T) {
	f := newAudienceFixture(t)
	id := f.createREST(t, f.tokA, false).ID
	client := dialAuthenticatedWS(t, f.ts.URL, f.tokA)

	if err := f.hub.acquire(id, &turn{cancel: func() {}}, nil); err != nil {
		t.Fatal(err)
	}
	assertRPCErrorReason(t, client.callRPC(t, 1, "sessions.delete", map[string]any{"id": id}), "session_busy")
	if !f.srv.sessionExists(id) {
		t.Error("session was removed under a registered turn")
	}
	f.hub.release(id)
	if resp := client.callRPC(t, 2, "sessions.delete", map[string]any{"id": id}); resp.Error != nil {
		t.Errorf("delete after the turn ended: %+v", resp.Error)
	}
}

// SessionExists names only what the rest of the surface can read: a real
// directory with a regular-file header. main keeps its looser rule.
func TestSessionExists_RequiresRealDirectoryAndRegularHeader(t *testing.T) {
	f := newAudienceFixture(t)
	src, ok := f.srv.Sessions.(*PenatusSource)
	if !ok {
		t.Fatalf("fixture sessions source is %T, want *PenatusSource", f.srv.Sessions)
	}
	sessions := filepath.Join(f.hub.hearthHome, "sessions")

	good := f.createREST(t, f.tokA, false).ID
	if !src.SessionExists(good) {
		t.Fatalf("a created session must exist")
	}

	// Directory named session.json instead of a header file.
	const dirHeader = "s_0000000000000000000000000C"
	if err := os.MkdirAll(filepath.Join(sessions, dirHeader, "session.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if src.SessionExists(dirHeader) {
		t.Error("a directory named session.json passed for a header")
	}

	// Symlinked header.
	const linkHeader = "s_0000000000000000000000000D"
	if err := os.MkdirAll(filepath.Join(sessions, linkHeader), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sessions, good, "session.json"), filepath.Join(sessions, linkHeader, "session.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if src.SessionExists(linkHeader) {
		t.Error("a symlinked session.json passed for a header")
	}

	// Symlinked session directory.
	const linkDir = "s_0000000000000000000000000E"
	if err := os.Symlink(filepath.Join(sessions, good), filepath.Join(sessions, linkDir)); err != nil {
		t.Fatal(err)
	}
	if src.SessionExists(linkDir) {
		t.Error("a symlinked session directory passed for a session")
	}

	// main: existence is the directory alone, symlinks as before.
	if err := os.RemoveAll(filepath.Join(sessions, "main")); err != nil {
		t.Fatal(err)
	}
	if src.SessionExists("main") {
		t.Error("main without a directory must not exist")
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(sessions, "main")); err != nil {
		t.Fatal(err)
	}
	if !src.SessionExists("main") {
		t.Error("a symlinked main directory stopped existing")
	}
}
