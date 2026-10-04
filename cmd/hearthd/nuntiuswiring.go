package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/surface"
)

// engineAdapter adapts surface.Hub to nuntius.Engine (NUNTIUS-SPEC §7.1).
// It runs turns headlessly without an SSE client, accumulating streamed
// delta increments into the full-text-so-far contract the bridge stream
// requires.
type engineAdapter struct {
	hub *surface.Hub
}

// newEngineAdapter constructs an engineAdapter backed by the live hub.
func newEngineAdapter(hub *surface.Hub) *engineAdapter {
	return &engineAdapter{hub: hub}
}

// RunTurn executes one Telegram-initiated turn on sessionID. Deltas
// from the loop engine are accumulated so req.OnDelta receives full
// text generated so far (NUNTIUS-SPEC §7.1).
func (e *engineAdapter) RunTurn(ctx context.Context, req nuntius.TurnReq) nuntius.TurnResult {
	var b strings.Builder
	onDeltaFull := func(delta string) {
		b.WriteString(delta)
		if req.OnDelta != nil {
			req.OnDelta(b.String())
		}
	}
	err := e.hub.RunHeadlessTurn(ctx, req.SessionID, req.Text, "telegram", req.UpdateID, onDeltaFull)
	return nuntius.TurnResult{
		Text: b.String(),
		Err:  err,
	}
}

// sessionsAdapter adapts surface.PenatusSource to nuntius.Sessions
// (NUNTIUS-SPEC §5) for Telegram /sessions and /new commands.
type sessionsAdapter struct {
	source *surface.PenatusSource
}

// newSessionsAdapter constructs a sessionsAdapter.
func newSessionsAdapter(source *surface.PenatusSource) *sessionsAdapter {
	return &sessionsAdapter{source: source}
}

// List maps Penatus sessions to nuntius SessionRefs (NUNTIUS-SPEC §5).
func (s *sessionsAdapter) List() ([]nuntius.SessionRef, error) {
	list, err := s.source.List()
	if err != nil {
		return nil, err
	}
	refs := make([]nuntius.SessionRef, len(list))
	for i, info := range list {
		title := ""
		if info.Title != nil {
			title = *info.Title
		}
		refs[i] = nuntius.SessionRef{
			ID:    info.ID,
			Title: title,
		}
	}
	return refs, nil
}

// Create makes a new side session (NUNTIUS-SPEC §5). The modelPin
// parameter is ignored for Telegram-initiated sessions since model
// pinning is reserved for web API callers.
func (s *sessionsAdapter) Create(title string, modelPin string) (nuntius.SessionRef, error) {
	info, err := s.source.Create(title, modelPin)
	if err != nil {
		return nuntius.SessionRef{}, err
	}
	outTitle := ""
	if info.Title != nil {
		outTitle = *info.Title
	}
	return nuntius.SessionRef{
		ID:    info.ID,
		Title: outTitle,
	}, nil
}

// fanoutAdapter adapts ApprovalFanout callbacks to bridge functions
// (NUNTIUS-SPEC §7.1).
type fanoutAdapter struct {
	pending  func(id, sessionID, name, argsSummary string)
	terminal func(id, sessionID, state, reason, source string)
}

func (f *fanoutAdapter) ApprovalPending(id, sessionID, name, argsSummary string) {
	if f.pending != nil {
		f.pending(id, sessionID, name, argsSummary)
	}
}

func (f *fanoutAdapter) ApprovalTerminal(id, sessionID, state, reason, source string) {
	if f.terminal != nil {
		f.terminal(id, sessionID, state, reason, source)
	}
}

// nuntiusWire builds the WireDeps hooks connecting nuntius to the live hub
// and approval coordinator (NUNTIUS-SPEC §7.1, §7.2, §7.3).
func nuntiusWire(hub *surface.Hub, ap *surface.ApprovalHub, defaultModel string) nuntius.WireDeps {
	return nuntius.WireDeps{
		SetFanout: func(pending func(id, sessionID, name, argsSummary string), terminal func(id, sessionID, state, reason, source string)) {
			// nil callbacks detach (ApprovalFanout contract): a
			// non-nil adapter wrapping two nils would leave a
			// silently-dropping subscriber installed.
			if pending == nil && terminal == nil {
				ap.SetFanout(nil)
				return
			}
			ap.SetFanout(&fanoutAdapter{pending: pending, terminal: terminal})
		},
		SetBridgeLive:     ap.SetBridgeLive,
		SetBridgeTimeout:  ap.SetBridgeTimeout,
		MarkUndeliverable: ap.MarkUndeliverable,
		ResolveApproval: func(sessionID, approvalID string, allow bool) int {
			return ap.ResolveFrom(sessionID, approvalID, allow, "telegram")
		},
		SessionOf:        ap.SessionOf,
		PendingApprovals: ap.PendingCount,
		CancelTurn:       hub.Cancel,
		InFlight:         hub.InFlight,
		ModelRef: func() string {
			return defaultModel
		},
	}
}

// scrubWriter intercepts log writes to replace any occurrence of the
// Telegram bot token with "«token»" (NUNTIUS-SPEC N4).
type scrubWriter struct {
	w        io.Writer
	replacer *strings.Replacer
}

// newScrubWriter builds a scrubWriter redacting token.
func newScrubWriter(w io.Writer, token string) io.Writer {
	if token == "" {
		return w
	}
	return &scrubWriter{
		w:        w,
		replacer: strings.NewReplacer(token, "«token»"),
	}
}

func (s *scrubWriter) Write(p []byte) (n int, err error) {
	scrubbed := s.replacer.Replace(string(p))
	_, err = s.w.Write([]byte(scrubbed))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// startNuntius starts the Telegram bridge if enabled in config (NUNTIUS-SPEC §2, §4).
// If enabled but the bot token is missing in the environment, it logs the spec remedy
// line (nuntius.LogDormant) and returns nil so serve continues with web-only access
// (N8 posture: config issues are loud and local).
func startNuntius(
	ctx context.Context,
	cfg *Config,
	hearthHome string,
	hub *surface.Hub,
	sessions *surface.PenatusSource,
	ap *surface.ApprovalHub,
) (*nuntius.Bridge, func(), error) {
	if !cfg.Nuntius.Enabled {
		// §2: the startup line is loud either way — a reader of
		// the log must never wonder whether the bridge "should"
		// be up.
		log.Println(nuntius.LogDisabled)
		return nil, nil, nil
	}
	warns, err := cfg.Nuntius.Normalize()
	if err != nil {
		return nil, nil, err
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠ "+w)
	}

	token, _ := cfg.Nuntius.Token(nil)
	if token == "" {
		log.Println(nuntius.LogDormant)
		return nil, nil, nil
	}

	transport := nuntius.NewBotAPI(token, nil)
	engine := newEngineAdapter(hub)
	sess := newSessionsAdapter(sessions)

	defaultModel := ""
	if len(cfg.Models.Default) > 0 {
		defaultModel = cfg.Models.Default[0]
	}

	scrubLog := log.New(newScrubWriter(os.Stderr, token), "", log.LstdFlags)

	wire := nuntiusWire(hub, ap, defaultModel)

	deps := nuntius.Deps{
		Cfg:        cfg.Nuntius,
		Home:       hearthHome,
		TG:         transport,
		Engine:     engine,
		Sess:       sess,
		SessionDir: hub.SessionDir,
		Wire:       wire,
		Log:        scrubLog,
	}

	b, err := nuntius.New(deps)
	if err != nil {
		// V15/N8: corrupt bridge state is a LOCAL failure — log the
		// remedy line and stay dormant, web surface keeps serving.
		// Anything else (unwritable home, bad deps) is a real startup
		// failure.
		if errors.Is(err, nuntius.ErrCorruptState) {
			log.Println(err)
			return nil, nil, nil
		}
		return nil, nil, err
	}
	b.Wire(deps.Wire)
	b.Start(ctx, token)

	return b, b.Stop, nil
}
