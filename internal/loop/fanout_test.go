package loop

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

type fanoutProvider struct {
	n     int32
	calls int
}

func (p *fanoutProvider) Name() string { return "fan" }
func (p *fanoutProvider) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{}, nil
}

func (p *fanoutProvider) Complete(context.Context, []router.Message, router.Options) (*router.Completion, error) {
	if atomic.AddInt32(&p.n, 1) > 1 {
		return &router.Completion{Text: "done", Model: "fan"}, nil
	}
	c := &router.Completion{Model: "fan"}
	for i := range p.calls {
		c.ToolCalls = append(c.ToolCalls, router.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "echo", ArgsJSON: `{}`})
	}
	return c, nil
}

func TestFanoutCapDropsExcessToolCalls(t *testing.T) {
	const limit, total = 5, 12
	sp := &fanoutProvider{calls: total}
	log, err := penatus.OpenLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	var warns []string
	s := &Session{
		Log: log, Sys: "SYS", Router: rt, MaxToolCalls: limit,
		Warnf: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) },
	}
	ran := 0
	tools := []Tool{{
		Spec:    router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run:     func(context.Context, string) (string, error) { ran++; return "ok", nil },
	}}
	reply, err := s.RunTurnTools(context.Background(), "go", nil, tools, nil)
	if err != nil || reply != "done" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if ran != limit {
		t.Fatalf("executed %d, want %d", ran, limit)
	}
	notice := fmt.Sprintf("not executed: per-turn tool-call fan-out limit (%d) reached", limit)
	dropped, okc := 0, 0
	for _, m := range s.messages() {
		if m.Role != router.RoleTool {
			continue
		}
		if strings.Contains(m.Content, notice) {
			dropped++
		} else {
			okc++
		}
	}
	if okc != limit || dropped != total-limit {
		t.Fatalf("results ok=%d dropped=%d", okc, dropped)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "attempted=12") || !strings.Contains(warns[0], "allowed=5") {
		t.Fatalf("warns=%v", warns)
	}
}
