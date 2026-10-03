package surface

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/lararium-app/lararium/internal/penatus"
)

func strPtr(s string) *string { return &s }

// ErrNoSession means the session does not exist on disk; handlers map it
// to 404 (spec: unknown session id → 404, never a phantom empty 200).
var ErrNoSession = errors.New("no such session")

// PenatusSource is the SessionSource over the Penatus file layer:
// sessions live at <root>/sessions/<id>/ exactly where repl.go and
// penatus put them (spec §5: the API never forks the storage format).
type PenatusSource struct {
	root string
	hub  TurnHub
}

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
			if data, rerr := os.ReadFile(filepath.Join(sessionsDir, "main", "session.json")); rerr == nil {
				var h penatus.SessionHeader
				if json.Unmarshal(data, &h) == nil {
					created = h.Created
				}
			}
			sessions = append(sessions, SessionInfo{
				ID:      "main",
				Created: created,
				Kind:    "main",
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

		sessions = append(sessions, SessionInfo{
			ID:       header.ID,
			Created:  header.Created,
			Kind:     header.Kind,
			Title:    header.Title,
			Parent:   header.Parent,
			ModelPin: header.ModelPin,
		})
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

func (p *PenatusSource) headerToInfo(h penatus.SessionHeader) SessionInfo {
	return SessionInfo{
		ID:       h.ID,
		Created:  h.Created,
		Kind:     h.Kind,
		Title:    h.Title,
		Parent:   h.Parent,
		ModelPin: h.ModelPin,
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
