package surface

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
)

func strPtr(s string) *string { return &s }

// ErrNoSession means the session does not exist on disk; handlers map it
// to 404 (spec: unknown session id → 404, never a phantom empty 200).
var ErrNoSession = errors.New("no such session")

// ErrRevisionConflict means expected_revision did not match the session's
// current header revision counter (RPC-SPEC §4a sessions.configure CAS).
var ErrRevisionConflict = errors.New("revision conflict")

// PenatusSource is the SessionSource over the Penatus file layer:
// sessions live at <root>/sessions/<id>/ exactly where repl.go and
// penatus put them (spec §5: the API never forks the storage format).
type PenatusSource struct {
	root string
	hub  TurnHub
	mu   sync.Mutex
}

var _ ModelPinSetter = (*PenatusSource)(nil)

// NewPenatusSource roots the source at the hearth home; hub supplies
// the in_flight flag for catch-up (may be nil in tests).
func NewPenatusSource(root string, hub TurnHub) *PenatusSource {
	return &PenatusSource{
		root: root,
		hub:  hub,
	}
}

// List returns every session (main first-class, spec §5), newest first.
func (p *PenatusSource) List() ([]SessionInfo, error) {
	sessionsDir := filepath.Join(p.root, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SessionInfo{}, nil
		}
		return nil, err
	}

	var sessions []SessionInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == "main" {
			// The REPL creates sessions/main/ without a session.json
			// header; synthesize the entry so main is always listed
			// (spec: main exists or the daemon is misconfigured).
			info, serr := os.Stat(filepath.Join(sessionsDir, "main"))
			if serr != nil || !info.IsDir() {
				continue
			}
			created := ""
			var title, modelPin *string
			var revision int64
			if data, rerr := os.ReadFile(filepath.Join(sessionsDir, "main", "session.json")); rerr == nil {
				var h penatus.SessionHeader
				if json.Unmarshal(data, &h) == nil {
					created = h.Created
					title = h.Title
					modelPin = h.ModelPin
					revision = h.Revision
				}
			}
			sessions = append(sessions, SessionInfo{
				ID:       "main",
				Created:  created,
				Kind:     "main",
				Title:    title,
				ModelPin: modelPin,
				Revision: revision,
			})
			continue
		}

		sessionPath := filepath.Join(sessionsDir, entry.Name(), "session.json")
		data, err := os.ReadFile(sessionPath)
		if err != nil {
			continue
		}

		var header penatus.SessionHeader
		if err := json.Unmarshal(data, &header); err != nil {
			continue
		}

		sessions = append(sessions, p.headerToInfo(header))
	}

	// newest first by created (string-comparable RFC3339Nano); entries
	// without a created stamp sort last.
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && sessions[j].Created > sessions[j-1].Created; j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
	}

	return sessions, nil
}

// ErrProtectedSession marks a session that outlives client deletion (the
// main session); handlers map it to 403, never silently succeed.
var ErrProtectedSession = errors.New("session is protected")

// Delete removes a side session directory from disk. main is the
// hearth's own lifetime session and always answers
// ErrProtectedSession; an unknown id is ErrNoSession. Callers must
// reject live turns before deleting (409 lives in the handler).
func (p *PenatusSource) Delete(id string) error {
	if !ValidSessionID(id) {
		return ErrNoSession
	}
	// Check the 403 protected session case FIRST before disk lookup
	// to prevent probe oracles on main session existence (L-4).
	if id == "main" {
		return ErrProtectedSession
	}
	// ValidSessionID whitelists ids (^s_ + 26 safe chars), so the joins
	// below cannot escape the sessions root.
	dir := filepath.Join(p.root, "sessions", id)                 // #nosec G703
	data, err := os.ReadFile(filepath.Join(dir, "session.json")) // #nosec G703
	if err != nil {
		return ErrNoSession
	}
	var header penatus.SessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return ErrNoSession
	}
	if header.Kind != "side" {
		return ErrProtectedSession
	}
	return os.RemoveAll(dir) // #nosec G703
}

// Create makes a side chat branched from main: header with parent,
// optional title/model_pin, and the branch event (PENATUS §2).
func (p *PenatusSource) Create(title string, modelPin string) (SessionInfo, error) {
	id := penatus.NewID()
	log, err := penatus.CreateSession(p.root, id, "side")
	if err != nil {
		return SessionInfo{}, err
	}

	// Persist parent + optional title/pin in the header, and append the
	// branch event (PENATUS §2: side chats carry provenance). The API
	// never invents a format the file spec doesn't define.
	sessionPath := filepath.Join(log.Dir(), "session.json")
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return SessionInfo{}, err
	}
	var header penatus.SessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return SessionInfo{}, err
	}
	parent := "main"
	header.Parent = &parent
	if title != "" {
		header.Title = &title
	}
	if modelPin != "" {
		header.ModelPin = &modelPin
	}
	newData, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return SessionInfo{}, err
	}
	tmpPath := sessionPath + ".tmp"
	if err := os.WriteFile(tmpPath, newData, 0o600); err != nil {
		return SessionInfo{}, err
	}
	if err := os.Rename(tmpPath, sessionPath); err != nil {
		return SessionInfo{}, err
	}

	fromSeq := int64(0)
	if mainLog, err := penatus.OpenLog(filepath.Join(p.root, "sessions", "main")); err == nil {
		if evs := mainLog.Live(); len(evs) > 0 {
			fromSeq = evs[len(evs)-1].Seq
		}
	}
	sb, err := json.Marshal(fromSeq)
	if err != nil {
		return SessionInfo{}, err
	}
	fb, err := json.Marshal("main")
	if err != nil {
		return SessionInfo{}, err
	}
	if err := log.Append(penatus.Event{T: "branch", Fields: map[string]json.RawMessage{
		"from_session": fb,
		"from_seq":     sb,
	}}); err != nil {
		return SessionInfo{}, err
	}

	return p.headerToInfo(header), nil
}

// Configure updates session configuration fields (currently model_pin) with
// optional compare-and-swap via expectedRevision.
//   - modelPin == nil: field absent (leave header.ModelPin unchanged).
//   - modelPin != nil && *modelPin == "": clear header.ModelPin.
//   - modelPin != nil && *modelPin != "": set header.ModelPin to *modelPin.
//
// For the main session, if sessions/main/ exists without a session.json header,
// a minimal header is synthesized and written. Any applied change increments
// header.Revision by 1 before writing atomically.
func (p *PenatusSource) Configure(id string, modelPin *string, expectedRevision *int64) (SessionInfo, error) {
	if !ValidSessionID(id) {
		return SessionInfo{}, ErrNoSession
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Validated above: id matches ^(s_[0-9A-Z]{26}|main)$ — no traversal
	// is possible.
	sessionDir := filepath.Join(p.root, "sessions", id) // #nosec G703
	dirInfo, err := os.Stat(sessionDir)
	if err != nil || !dirInfo.IsDir() {
		return SessionInfo{}, ErrNoSession
	}

	sessionPath := filepath.Join(sessionDir, "session.json") // #nosec G304
	var header penatus.SessionHeader
	synthesized := false
	data, err := os.ReadFile(sessionPath) // #nosec G703 -- id is regex-validated above
	if err != nil {
		if !os.IsNotExist(err) {
			return SessionInfo{}, err
		}
		if id != "main" {
			return SessionInfo{}, ErrNoSession
		}
		header = penatus.SessionHeader{
			Penatus: 1,
			ID:      "main",
			Kind:    "main",
			Created: dirInfo.ModTime().UTC().Format(time.RFC3339),
		}
		synthesized = true
	} else if err := json.Unmarshal(data, &header); err != nil {
		return SessionInfo{}, err
	}

	if expectedRevision != nil && *expectedRevision != header.Revision {
		return SessionInfo{}, ErrRevisionConflict
	}

	changed := false
	if modelPin != nil {
		if *modelPin == "" {
			header.ModelPin = nil
		} else {
			v := *modelPin
			header.ModelPin = &v
		}
		changed = true
	}

	if changed {
		header.Revision++
	}

	if changed || synthesized {
		newData, err := json.MarshalIndent(header, "", "  ")
		if err != nil {
			return SessionInfo{}, err
		}
		tmpPath := sessionPath + ".tmp"
		if err := os.WriteFile(tmpPath, newData, 0o600); err != nil {
			return SessionInfo{}, err
		}
		if err := os.Rename(tmpPath, sessionPath); err != nil {
			return SessionInfo{}, err
		}
	}

	return p.headerToInfo(header), nil
}

// SetModelPin updates model_pin in the session's session.json header.
// A nil (or empty) pin clears header.ModelPin. Unknown or invalid ids
// return ErrNoSession. Bumps header.Revision so paired devices share one
// monotonic revision counter across REST and RPC mutations.
func (p *PenatusSource) SetModelPin(id string, pin *string) error {
	if !ValidSessionID(id) {
		return ErrNoSession
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Validated above: id matches ^(s_[0-9A-Z]{26}|main)$ — no traversal
	// is possible.
	sessionDir := filepath.Join(p.root, "sessions", id)                // #nosec G703
	if info, err := os.Stat(sessionDir); err != nil || !info.IsDir() { //nolint:gosec // regex-validated id
		return ErrNoSession
	}

	sessionPath := filepath.Join(sessionDir, "session.json") // #nosec G304
	var header penatus.SessionHeader
	data, err := os.ReadFile(sessionPath) // #nosec G703 -- id is regex-validated above
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoSession
		}
		return err
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}

	if pin == nil || *pin == "" {
		header.ModelPin = nil
	} else {
		v := *pin
		header.ModelPin = &v
	}
	header.Revision++

	newData, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := sessionPath + ".tmp"
	if err := os.WriteFile(tmpPath, newData, 0o600); err != nil { //nolint:gosec // regex-validated id
		return err
	}
	if err := os.Rename(tmpPath, sessionPath); err != nil { //nolint:gosec // regex-validated id
		return err
	}
	return nil
}

func (p *PenatusSource) headerToInfo(h penatus.SessionHeader) SessionInfo {
	return SessionInfo{
		ID:       h.ID,
		Created:  h.Created,
		Kind:     h.Kind,
		Title:    h.Title,
		Parent:   h.Parent,
		ModelPin: h.ModelPin,
		Revision: h.Revision,
	}
}

// Events replays the session log after afterSeq (clamped by limit)
// plus the in_flight flag; unknown sessions are ErrNoSession -> 404.
func (p *PenatusSource) Events(id string, afterSeq int64, limit int) ([]json.RawMessage, bool, error) {
	if !ValidSessionID(id) {
		return nil, false, ErrNoSession
	}

	// Validated above: id matches ^(s_[0-9A-Z]{26}|main)$ — no traversal
	// is possible, which is what gosec's taint analysis cannot see.
	sessionDir := filepath.Join(p.root, "sessions", id)
	// Existence gate: OpenLog happily opens a missing log, which would
	// answer 200-empty for sessions that never existed. Require the
	// session dir (and, for side chats, the header) before serving.
	if info, err := os.Stat(sessionDir); err != nil || !info.IsDir() { //nolint:gosec // regex-validated id
		return nil, false, ErrNoSession
	}
	if id != "main" {
		if _, err := os.Stat(filepath.Join(sessionDir, "session.json")); err != nil { //nolint:gosec // regex-validated id
			return nil, false, ErrNoSession
		}
	}

	log, err := penatus.OpenLog(sessionDir)
	if err != nil {
		return nil, false, err
	}

	liveEvents := log.Live()

	result := []json.RawMessage{}
	for _, evt := range liveEvents {
		if evt.Seq <= afterSeq {
			continue
		}
		if len(result) >= limit {
			break
		}
		data, err := json.Marshal(evt)
		if err != nil {
			continue
		}
		result = append(result, json.RawMessage(data))
	}

	inFlight := false
	if p.hub != nil {
		inFlight = p.hub.InFlight(id)
	}

	return result, inFlight, nil
}
