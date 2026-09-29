package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// Session is one live chat bound to its event log.
type Session struct {
	Log    *penatus.Log
	Sys    string // assembled system prompt
	Router *router.Router
	// CompactProfile is the router profile used for summarization.
	CompactProfile string
	// TriggerPct: compact when assembled prompt exceeds this % of the
	// probed context window (checked AFTER each turn, never mid-stream).
	TriggerPct int

	// lastUsage carries In/Out tokens from the most recent completion so
	// the trigger check uses real counts when available.
	lastIn  int
	lastOut int
}

// messages rebuilds the router message slice from the log per the spec §3
// assembly rule: everything after the latest compact, plus its summary in
// place of covers, minus tombstoned (Log.Live() already excludes those).
func (s *Session) messages() []router.Message {
	live := s.Log.Live()
	msgs := []router.Message{{Role: router.RoleSystem, Content: s.Sys}}
	// Spec §3: the summary stands IN PLACE OF the covered range. Covered
	// seqs are always earlier than the compact event itself, so summaries
	// are emitted first (in compact order), then surviving messages.
	var summaries []router.Message
	for _, ev := range live {
		switch ev.T {
		case "compact":
			var f struct {
				SummaryText string `json:"summary_text"`
			}
			decodeInto(ev, &f)
			if f.SummaryText != "" {
				summaries = append(summaries, router.Message{
					Role:    router.RoleAssistant,
					Content: "[earlier conversation summary]\n" + f.SummaryText,
				})
			}
		}
	}
	msgs = append(msgs, summaries...)
	for _, ev := range live {
		if ev.T == "msg" {
			var f struct {
				Role string `json:"role"`
				Text string `json:"text"`
			}
			decodeInto(ev, &f)
			if f.Text == "" {
				continue
			}
			var role router.Role
			switch f.Role {
			case "user":
				role = router.RoleUser
			case "assistant":
				role = router.RoleAssistant
			default:
				continue
			}
			msgs = append(msgs, router.Message{Role: role, Content: f.Text})
		}
	}
	return msgs
}

// decodeInto unmarshals an event's flattened fields into dst.
func decodeInto(ev penatus.Event, dst any) {
	b, err := ev.MarshalJSON()
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, dst)
}

// RunTurn appends the user message, streams the assistant completion, and
// appends it. Returns the full assistant text.
func (s *Session) RunTurn(ctx context.Context, userText string, onDelta func(string)) (string, error) {
	src, _ := json.Marshal(map[string]any{"channel": "api", "device": nil})
	if err := s.Log.Append(penatus.Event{
		T:  "msg",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"role": raw("user"),
			"text": raw(userText),
			"src":  src,
		},
	}); err != nil {
		return "", fmt.Errorf("log user msg: %w", err)
	}

	msgs := s.messages()
	var full string
	comp, err := s.Router.CompleteStream(ctx, "chat", msgs, router.Options{},
		func(delta string) error {
			if onDelta != nil {
				onDelta(delta)
			}
			full += delta
			return nil
		})
	if err != nil {
		return full, fmt.Errorf("complete: %w", err)
	}
	text := comp.Text
	if text == "" {
		text = full
	}

	if err := s.Log.Append(penatus.Event{
		T:  "msg",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"role":  raw("assistant"),
			"text":  raw(text),
			"model": raw(comp.Model),
			"usage": mustJSON(map[string]int{"in": comp.InTokens, "out": comp.OutTokens}),
		},
	}); err != nil {
		return text, fmt.Errorf("log assistant msg: %w", err)
	}
	s.lastIn, s.lastOut = comp.InTokens, comp.OutTokens
	return text, nil
}

// MaybeCompact checks the post-turn trigger and, if over, summarizes the
// older half of the transcript into a compact event. Returns true if a
// compaction was written.
func (s *Session) MaybeCompact(ctx context.Context) (bool, error) {
	// Trigger per spec: prompt tokens > triggerPct% of probed window,
	// using the real completion usage when the provider reports it.
	caps, err := s.Router.Probe(ctx, "chat")
	if err != nil || caps.ContextLength <= 0 {
		return false, nil // can't trigger what we can't measure
	}
	approx := s.lastIn
	if approx <= 0 {
		approx = estTokens(s.messages())
	}
	if approx*100 <= caps.ContextLength*s.TriggerPct {
		return false, nil
	}
	return true, s.Compact(ctx)
}

// Compact summarizes everything after the last compact (minus the most
// recent turns, kept verbatim) and appends the compact event.
func (s *Session) Compact(ctx context.Context) error {
	live := s.Log.Live()
	// Find last compact to bound the new one.
	startSeq := int64(1)
	for _, ev := range live {
		if ev.T == "compact" {
			startSeq = ev.Seq + 1
		}
	}
	// Collect msg events in range; keep the last 4 verbatim.
	var inRange []penatus.Event
	for _, ev := range live {
		if ev.T == "msg" && ev.Seq >= startSeq {
			inRange = append(inRange, ev)
		}
	}
	keep := 4
	if len(inRange) <= keep+2 {
		return nil // nothing worth compacting yet
	}
	toSummarize := inRange[:len(inRange)-keep]

	var transcript string
	for _, ev := range toSummarize {
		var f struct {
			Role string `json:"role"`
			Text string `json:"text"`
		}
		decodeInto(ev, &f)
		transcript += fmt.Sprintf("[%s] %s\n", f.Role, f.Text)
	}

	req := []router.Message{
		{Role: router.RoleSystem, Content: compactInstruction},
		{Role: router.RoleUser, Content: transcript},
	}
	comp, err := s.Router.Complete(ctx, s.CompactProfile, req, router.Options{})
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	kept := make([]map[string]any, 0, keep)
	for _, ev := range inRange[len(inRange)-keep:] {
		kept = append(kept, map[string]any{"t": "msg", "seq": ev.Seq})
	}
	if err := s.Log.Append(penatus.Event{
		T:  "compact",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"covers":        mustJSON([]int64{toSummarize[0].Seq, toSummarize[len(toSummarize)-1].Seq}),
			"summary_text":  raw(comp.Text),
			"kept":          mustJSON(kept),
			"tokenizer":     raw("est:words"),
			"tokens_before": mustJSON(approxTokensTranscript(transcript)),
			"tokens_after":  mustJSON(len(comp.Text) / 4),
		},
	}); err != nil {
		return fmt.Errorf("log compact: %w", err)
	}
	return nil
}

const compactInstruction = `Summarize the conversation transcript for continuation as context.
Preserve VERBATIM: every decision made, every task opened, every named entity
(people, projects, files, hosts), every commitment and deadline.
Drop: pleasantries, restatements, dead-end exploration.
Output dense notes, not prose.`

func estTokens(msgs []router.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) / 4
	}
	return n
}

func approxTokensTranscript(s string) int { return len(s) / 4 }

func raw(s string) json.RawMessage { return mustJSON(s) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("loop: marshal: " + err.Error())
	}
	return b
}
