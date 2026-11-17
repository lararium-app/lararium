package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// fanoutStub answers the first completion with n tool calls, then a plain
// final answer. Reproduces the SOAK-INCIDENT-2026-11-15 shape (model
// emitted 452 identical calls in one round).
type fanoutStub struct {
	n         int
	execCount *int
}

func (f *fanoutStub) Name() string { return "fanout" }
func (f *fanoutStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{ContextLength: 200_000}, nil
}

func (f *fanoutStub) Complete(_ context.Context, msgs []router.Message, _ router.Options) (*router.Completion, error) {
	seenToolResults := 0
	for _, m := range msgs {
		if m.Role == router.RoleTool {
			seenToolResults++
		}
	}
	if seenToolResults == 0 {
		calls := make([]router.ToolCall, f.n)
		for i := range calls {
			calls[i] = router.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "clock", ArgsJSON: `{}`}
		}
		return &router.Completion{ToolCalls: calls, Model: "fanout"}, nil
	}
	return &router.Completion{Text: "final", Model: "fanout"}, nil
}

func fanoutSession(t *testing.T, sp router.Provider) *Session {
	t.Helper()
	dir := t.TempDir()
	log, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})
	return &Session{Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80}
}

func echoTool(execCount *int) Tool {
	return Tool{
		Spec:    router.ToolSpec{Name: "clock", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run: func(_ context.Context, _ string) (string, error) {
			*execCount++
			return "tick", nil
		},
	}
}

// countEvents returns log event counts by type.
func countEvents(t *testing.T, log *penatus.Log) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, ev := range log.Live() {
		out[ev.T]++
	}
	return out
}

// approvalOf extracts the "approval" field of tool_call events (in order).
func approvalOf(t *testing.T, log *penatus.Log) []string {
	t.Helper()
	var out []string
	for _, ev := range log.Live() {
		if ev.T != "tool_call" {
			continue
		}
		var f struct {
			Approval string `json:"approval"`
		}
		decodeInto(ev, &f)
		out = append(out, f.Approval)
	}
	return out
}

// Defect A: per-round fanout cap.
func TestFanoutRoundCap(t *testing.T) {
	exec := 0
	s := fanoutSession(t, &fanoutStub{n: 20})
	tool := echoTool(&exec)

	if _, err := s.RunTurnTools(context.Background(), "go", nil, []Tool{tool}, nil); err != nil {
		t.Fatal(err)
	}

	if exec != maxToolCallsPerRound {
		t.Errorf("executed %d calls, want exactly %d", exec, maxToolCallsPerRound)
	}
	counts := countEvents(t, s.Log)
	if counts["tool_call"] != 20 {
		t.Errorf("audit trail: %d tool_call events, want all 20 logged", counts["tool_call"])
	}
	if counts["tool_result"] != 20 {
		t.Errorf("%d tool_result events, want 20 (capped calls get guidance results)", counts["tool_result"])
	}
	approvals := approvalOf(t, s.Log)
	capped := 0
	for _, a := range approvals {
		if a == "capped" {
			capped++
		}
	}
	if capped != 20-maxToolCallsPerRound {
		t.Errorf("%d calls marked capped, want %d", capped, 20-maxToolCallsPerRound)
	}
}

// Defect A: per-turn executed-call budget across rounds.
// multiRoundStub emits perRound calls every round until maxRounds rounds
// have passed, then answers.
type multiRoundStub struct {
	perRound  int
	maxRounds int
}

func (m *multiRoundStub) Name() string { return "multi" }
func (m *multiRoundStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{ContextLength: 200_000}, nil
}

func (m *multiRoundStub) Complete(_ context.Context, msgs []router.Message, _ router.Options) (*router.Completion, error) {
	roundsDone := 0
	for _, msg := range msgs {
		if msg.Role == router.RoleAssistant && len(msg.ToolCalls) > 0 {
			roundsDone++
		}
	}
	if roundsDone >= m.maxRounds {
		return &router.Completion{Text: "final", Model: "multi"}, nil
	}
	calls := make([]router.ToolCall, m.perRound)
	for i := range calls {
		calls[i] = router.ToolCall{ID: fmt.Sprintf("r%d_%d", roundsDone, i), Name: "clock", ArgsJSON: `{}`}
	}
	return &router.Completion{ToolCalls: calls, Model: "multi"}, nil
}

func TestFanoutTurnBudget(t *testing.T) {
	exec := 0
	// 6 calls/round x 12 rounds would be 72 executed without a budget.
	s := fanoutSession(t, &multiRoundStub{perRound: 6, maxRounds: 12})
	tool := echoTool(&exec)

	if _, err := s.runTurnToolsBounded(context.Background(), "go", nil, []Tool{tool}, nil, 10); err != nil {
		t.Fatal(err)
	}
	if exec != 10 {
		t.Errorf("executed %d calls, want exactly the 10-call budget", exec)
	}
}

// probeFailRouter reports a failing probe; completion passes through to
// the embedded provider semantics we need (a plain reply).
type probeFailStub struct{}

func (probeFailStub) Name() string { return "pf" }
func (probeFailStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{}, fmt.Errorf("probe down")
}

func (probeFailStub) Complete(_ context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	return &router.Completion{Text: "SUMMARY", Model: "pf"}, nil
}

// Defect C: a probe failure must NOT disable compaction (fallback floor).
func TestCompactProbeFailFallback(t *testing.T) {
	s := fanoutSession(t, probeFailStub{})
	// Seed transcripts well over the fallback floor (16384*0.8 ≈ 13107
	// tokens ≈ 52k chars of content per estTokens len/4 rule).
	for i := 0; i < 24; i++ {
		if _, err := s.RunTurn(context.Background(), strings.Repeat("word ", 4000), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.MaybeCompact(context.Background()); err != nil {
		t.Fatalf("MaybeCompact with failing probe: %v", err)
	}
	if got := countEvents(t, s.Log)["compact"]; got != 1 {
		t.Fatalf("compact events = %d, want 1 (fallback floor must trigger)", got)
	}
	// The compact must still cover a sane seq range.
	for _, ev := range s.Log.Live() {
		if ev.T != "compact" {
			continue
		}
		var f struct {
			Covers      []int64 `json:"covers"`
			SummaryText string  `json:"summary_text"`
		}
		decodeInto(ev, &f)
		if len(f.Covers) != 2 || f.Covers[0] > f.Covers[1] {
			t.Errorf("bad covers: %v", f.Covers)
		}
		if f.SummaryText == "" {
			t.Error("empty summary_text")
		}
	}
}
