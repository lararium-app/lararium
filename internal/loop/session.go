package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lararium-app/lararium/internal/keystore"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// OnToolFunc is the callback invoked as each tool call is dispatched
// ("start") and finishes ("done"), if set. The REPL uses it for a live
// status line.
type OnToolFunc func(phase, name, approval string, ok bool)

// Session is one live chat bound to its event log.
type Session struct {
	// MaxTokens caps each generation (0 = provider default). Slow local
	// hardware needs a bound: an unbounded completion can idle for minutes.
	MaxTokens int
	// OnTool is optional tool-activity feedback (nil-safe).
	OnTool OnToolFunc
	Log    *penatus.Log
	Sys    string // assembled system prompt
	Router *router.Router
	// CompactProfile is the router profile used for summarization.
	CompactProfile string
	// TriggerPct: compact when assembled prompt exceeds this % of the
	// probed context window (checked AFTER each turn, never mid-stream).
	TriggerPct int
	// Keys is the live key registry (nil = no key plumbing). At turn
	// start its snapshot is pinned to the turn context so a mid-turn
	// key edit cannot split one turn across two keys (KEYS-SPEC K6).
	Keys *keystore.Registry

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
		if ev.T != "compact" {
			continue
		}
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
	msgs = append(msgs, summaries...)
	for _, ev := range live {
		switch ev.T {
		case "msg":
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
		case "tool_call":
			// Rebuild the assistant turn that carried this call (OpenAI
			// wire requires the tool_calls field on the assistant msg).
			var f struct {
				CallID string          `json:"call_id"`
				Name   string          `json:"name"`
				Args   json.RawMessage `json:"args"`
			}
			decodeInto(ev, &f)
			if f.CallID == "" {
				continue
			}
			msgs = append(msgs, router.Message{
				Role:      router.RoleAssistant,
				ToolCalls: []router.ToolCall{{ID: f.CallID, Name: f.Name, ArgsJSON: string(f.Args)}},
			})
		case "tool_result":
			var f struct {
				CallID string `json:"call_id"`
				OK     bool   `json:"ok"`
				Ref    string `json:"result_ref"`
			}
			decodeInto(ev, &f)
			if f.CallID == "" {
				continue
			}
			text := f.Ref // inline results store their text under "text"
			var tf struct {
				Text string `json:"text"`
			}
			decodeInto(ev, &tf)
			if tf.Text != "" {
				text = tf.Text
			} else if f.Ref != "" {
				// Blob-spilled result: read it back for the model.
				if b, err := os.ReadFile(s.resolveRef(f.Ref)); err == nil {
					text = string(b)
				} else {
					text = "[result blob missing: " + f.Ref + "]"
				}
			}
			if !f.OK && text == "" {
				text = "[tool failed]"
			}
			msgs = append(msgs, router.Message{Role: router.RoleTool, Content: text, ToolCallID: f.CallID})
		}
	}
	return msgs
}

// resolveRef maps a result_ref (relative to the session dir) to a path.
func (s *Session) resolveRef(ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(s.Log.Dir(), ref)
}

// decodeInto unmarshals an event's flattened fields into dst.
func decodeInto(ev penatus.Event, dst any) {
	b, err := ev.MarshalJSON()
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, dst)
}

// RunTurn appends the user message and drives the agent loop: completion ->
// tool dispatch (each dispatch logged BEFORE execution) -> follow-up
// completions until the model answers without calling tools (or maxSteps).
// Returns the final assistant text.
func (s *Session) RunTurn(ctx context.Context, userText string, onDelta func(string)) (string, error) {
	return s.runTurn(ctx, userText, onDelta, nil)
}

// RunTurnTools is RunTurn with a tool registry + approval hook.
func (s *Session) RunTurnTools(ctx context.Context, userText string, onDelta func(string), tools []Tool, ap Approver) (string, error) {
	return s.runTurn(ctx, userText, onDelta, newToolEngine(tools, s.Log.Dir(), ap))
}

// maxSteps bounds one user turn's model round-trips (no infinite tool loops).
const maxSteps = 12

func (s *Session) runTurn(ctx context.Context, userText string, onDelta func(string), eng *toolEngine) (string, error) {
	// Turn-pinned key snapshot (KEYS-SPEC K6): one registry read at
	// turn start; every provider call in this turn resolves through
	// the pinned map, so a mid-turn reload cannot split the turn.
	if s.Keys != nil {
		ctx = keystore.WithSnapshot(ctx, s.Keys.Snapshot())
	}
	//nolint:errchkjson // all-string map always marshals
	src, _ := json.Marshal(map[string]string{"channel": "api", "device": ""})
	if err := s.appendEvent("msg", map[string]json.RawMessage{
		"role": raw("user"), "text": raw(userText), "src": src,
	}); err != nil {
		return "", fmt.Errorf("log user msg: %w", err)
	}

	var final string
	for range maxSteps {
		msgs := s.messages()
		opts := router.Options{MaxTokens: s.MaxTokens}
		if eng != nil {
			opts.Tools = eng.specs()
		}
		var streamed string
		comp, err := s.Router.CompleteStream(ctx, "chat", msgs, opts, func(d string) error {
			// Stream everything: we can't know mid-stream whether this
			// round ends in a tool call; visible preambles are fine.
			if onDelta != nil {
				onDelta(d)
			}
			streamed += d
			return nil
		})
		if err != nil {
			return final, fmt.Errorf("complete: %w", err)
		}
		s.lastIn, s.lastOut = comp.InTokens, comp.OutTokens

		text := comp.Text
		if text == "" {
			text = streamed
		}

		// A completion with no text and no tool calls is a provider
		// hiccup, not an answer (spec §3: turns are facts, not noise).
		// Refuse to log it: the user sees the error and can retry.
		if text == "" && len(comp.ToolCalls) == 0 {
			return "", fmt.Errorf("empty completion from %s (nothing logged — retry)", comp.Model)
		}

		if len(comp.ToolCalls) == 0 {
			// Final answer for this user turn.
			if err := s.appendEvent("msg", map[string]json.RawMessage{
				"role": raw("assistant"), "text": raw(text), "model": raw(comp.Model),
				"usage": mustJSON(map[string]int{"in": comp.InTokens, "out": comp.OutTokens}),
			}); err != nil {
				return text, fmt.Errorf("log assistant msg: %w", err)
			}
			return text, nil
		}

		// Tool round: log the assistant's call turn, then dispatch each.
		// The messages() projection rebuilds wire shape from these events,
		// so the log stays the single source of truth.
		for _, tc := range comp.ToolCalls {
			args := json.RawMessage(tc.ArgsJSON)
			if !json.Valid(args) {
				args = raw(tc.ArgsJSON)
			}
			approval, tool, denyText := eng.Gate(tc)
			if err := s.appendEvent("tool_call", map[string]json.RawMessage{
				"call_id": raw(tc.ID), "name": raw(tc.Name), "args": args,
				"approval": raw(approval),
			}); err != nil {
				return final, fmt.Errorf("log tool_call: %w", err)
			}
			if s.OnTool != nil {
				s.OnTool("start", tc.Name, approval, false)
			}

			var out ToolOutcome
			if tool != nil {
				out = eng.Execute(ctx, tc, tool, approval)
			} else {
				out = ToolOutcome{Approval: approval, OK: false, Text: denyText, Digest: digest(tc.ArgsJSON)}
			}
			if s.OnTool != nil {
				s.OnTool("done", tc.Name, approval, out.OK)
			}
			resFields := map[string]json.RawMessage{
				"call_id": raw(tc.ID), "ok": mustJSON(out.OK),
				"result_digest": raw(out.Digest),
			}
			if out.ResultRef != "" {
				resFields["result_ref"] = raw(out.ResultRef)
			} else {
				resFields["text"] = raw(out.Text)
			}
			if err := s.appendEvent("tool_result", resFields); err != nil {
				return final, fmt.Errorf("log tool_result: %w", err)
			}
		}
		// Loop: messages() now includes the tool results; model continues.
		final = text
	}
	return final, fmt.Errorf("tool loop exceeded %d steps", maxSteps)
}

func (s *Session) appendEvent(t string, fields map[string]json.RawMessage) error {
	return s.Log.Append(penatus.Event{T: t, TS: time.Now().UTC().Format(time.RFC3339), Fields: fields})
}

// MaybeCompact checks the post-turn trigger and, if over, summarizes the
// older half of the transcript into a compact event. Returns true if a
// compaction was written.
func (s *Session) MaybeCompact(ctx context.Context) (bool, error) {
	// Trigger per spec: prompt tokens > triggerPct% of probed window,
	// using the real completion usage when the provider reports it.
	caps, err := s.Router.Probe(ctx, "chat")
	if err != nil || caps.ContextLength <= 0 {
		//nolint:nilerr // can't trigger what we can't measure; probe failure is not the caller's error
		return false, nil
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
