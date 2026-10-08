package main

import (
	"errors"
	"sync"

	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/surface"
)

// CustosWebAdapter adapts *custosdoor.Client to surface.CustosRegistry.
type CustosWebAdapter struct {
	client *custosdoor.Client
}

// NewCustosWebAdapter wraps client to satisfy surface.CustosRegistry.
func NewCustosWebAdapter(client *custosdoor.Client) surface.CustosRegistry {
	return &CustosWebAdapter{client: client}
}

var _ surface.CustosRegistry = (*CustosWebAdapter)(nil)

// Snapshot returns the pending custody cards mapped to surface.CustosCard.
func (a *CustosWebAdapter) Snapshot() []surface.CustosCard {
	if a == nil || a.client == nil {
		return nil
	}
	snap := a.client.Snapshot()
	out := make([]surface.CustosCard, len(snap))
	for i, c := range snap {
		out[i] = toSurfaceCard(c)
	}
	return out
}

// Subscribe registers f for card/gone updates mapped to surface types.
func (a *CustosWebAdapter) Subscribe(f func(card *surface.CustosCard, gone *surface.CustosGone)) func() {
	if a == nil || a.client == nil {
		return func() {}
	}
	return a.client.Subscribe(func(c *custosdoor.Card, g *custosdoor.Gone) {
		var sc *surface.CustosCard
		if c != nil {
			card := toSurfaceCard(*c)
			sc = &card
		}
		var sg *surface.CustosGone
		if g != nil {
			gone := toSurfaceGone(*g)
			sg = &gone
		}
		f(sc, sg)
	})
}

// Resolve settles a custody card via web attribution and maps sentinel errors.
func (a *CustosWebAdapter) Resolve(id, verdict string) (string, error) {
	if a == nil || a.client == nil {
		return "", custosdoor.ErrDoorDown
	}
	state, err := a.client.Resolve(id, verdict)
	return state, mapDoorError(err)
}

// Attach delegates to the door client Attach and maps events to surface types.
func (a *CustosWebAdapter) Attach() ([]surface.CustosCard, func(), <-chan surface.CustosEvent) {
	if a == nil || a.client == nil {
		ch := make(chan surface.CustosEvent)
		close(ch)
		return nil, func() {}, ch
	}
	doorCards, cancel, doorEvents := a.client.Attach()
	cards := make([]surface.CustosCard, len(doorCards))
	for i, c := range doorCards {
		cards[i] = toSurfaceCard(c)
	}

	events := make(chan surface.CustosEvent, 64)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			cancel()
		})
	}

	go func() {
		defer close(events)
		for {
			select {
			case <-done:
				return
			case ev, ok := <-doorEvents:
				if !ok {
					return
				}
				sev := toSurfaceEvent(ev)
				select {
				case <-done:
					return
				case events <- sev:
				default:
					stop()
					return
				}
			}
		}
	}()

	return cards, stop, events
}

func toSurfaceCard(c custosdoor.Card) surface.CustosCard {
	return surface.CustosCard{
		ID:         c.ID,
		Cell:       c.Cell,
		Cred:       c.Cred,
		Tool:       c.Tool,
		Dest:       c.Dest,
		Review:     c.Review,
		AgeS:       c.AgeS,
		ExpiresInS: c.ExpiresInS,
	}
}

func toSurfaceGone(g custosdoor.Gone) surface.CustosGone {
	return surface.CustosGone{
		ID:     g.ID,
		State:  g.State,
		Reason: g.Reason,
	}
}

func toSurfaceEvent(ev custosdoor.Event) surface.CustosEvent {
	var sev surface.CustosEvent
	if ev.Card != nil {
		c := toSurfaceCard(*ev.Card)
		sev.Card = &c
	}
	if ev.Gone != nil {
		g := toSurfaceGone(*ev.Gone)
		sev.Gone = &g
	}
	return sev
}

// mapDoorError maps door sentinel errors to surface sentinels.
// ErrBadSource is surface-only (never synthesized from door).
// ErrDoorDown and unknown errors pass through unchanged.
func mapDoorError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, custosdoor.ErrNoSuchCard):
		return surface.ErrNoSuchCard
	case errors.Is(err, custosdoor.ErrCardAnswered):
		return surface.ErrCardAnswered
	case errors.Is(err, custosdoor.ErrStaleVerdict):
		return surface.ErrStaleVerdict
	case errors.Is(err, custosdoor.ErrLocked):
		return surface.ErrLocked
	case errors.Is(err, custosdoor.ErrIPAskOnly):
		return surface.ErrIPAskOnly
	default:
		return err
	}
}
