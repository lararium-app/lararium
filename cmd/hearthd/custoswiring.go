package main

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/surface"
)

// CustosConfig is the custody door-client section of lararium.yaml
// (CUSTOS-SPEC §6.4b). DoorsSock and DoorToken are the frozen pair
// (custos.doors_sock / custos.door_token, paths); both unset keeps
// hearthd byte-for-byte at v0.5.0 behavior.
type CustosConfig struct {
	DoorsSock string `yaml:"doors_sock"`
	DoorToken string `yaml:"door_token"`
	// DoorName is a NON-FROZEN extension: the HELLO door name
	// ([a-z0-9_-]{1,32}); empty = "hearthd".
	DoorName string `yaml:"door_name"`
}

// startCustosDoor starts the door client as a supervised goroutine and
// feeds the Nuntius sibling feed and web surface (bridge and srv may be nil).
// It returns nil — having opened no socket, started no goroutine and logged
// nothing — unless both paths are set. The returned stop closes the door
// connection and waits for the goroutine; it is idempotent.
func startCustosDoor(ctx context.Context, cfg CustosConfig, bridge *nuntius.Bridge, srv *surface.Server) (stop func()) {
	client, err := custosdoor.New(custosdoor.Options{
		SockPath:  cfg.DoorsSock,
		TokenPath: cfg.DoorToken,
		Name:      cfg.DoorName,
	})
	if err != nil {
		// A half-set or invalid pair is local misconfiguration: say so
		// once and keep serving without custody cards.
		log.Printf("WARN custos door disabled: %v", err)
		return nil
	}
	if client == nil {
		return nil
	}
	if bridge != nil {
		//nolint:contextcheck // custodyFeed edits run on the feed goroutine with their own detached 15 s ctx (same posture as nuntius background card edits)
		bridge.WireCustody(custodyWire(client))
	}
	if srv != nil {
		srv.SetCustosRegistry(NewCustosWebAdapter(client))
	}

	runCtx, cancel := context.WithCancel(ctx)
	go client.Run(runCtx)
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-client.Done()
		})
	}
}

// custodyWire adapts the door client to the Nuntius sibling feed.
// Settles carry the frozen source "telegram".
func custodyWire(c *custosdoor.Client) nuntius.CustodyWire {
	return nuntius.CustodyWire{
		Snapshot: func() []nuntius.CustodyCard {
			snap := c.Snapshot()
			out := make([]nuntius.CustodyCard, len(snap))
			for i, s := range snap {
				out[i] = toCustodyCard(s)
			}
			return out
		},
		Subscribe: func(f func(*nuntius.CustodyCard, *nuntius.CustodyGone)) func() {
			return c.Subscribe(func(card *custosdoor.Card, gone *custosdoor.Gone) {
				if card != nil {
					nc := toCustodyCard(*card)
					f(&nc, nil)
				}
				if gone != nil {
					f(nil, &nuntius.CustodyGone{ID: gone.ID, State: gone.State, Reason: gone.Reason})
				}
			})
		},
		Resolve: func(id, verdict string) (string, string) {
			state, err := c.ResolveVia(id, verdict, custosdoor.ViaTelegram)
			return state, custodyOutcome(err)
		},
	}
}

func toCustodyCard(c custosdoor.Card) nuntius.CustodyCard {
	return nuntius.CustodyCard{
		ID: c.ID, Cell: c.Cell, Cred: c.Cred, Tool: c.Tool,
		Dest: c.Dest, Review: c.Review, ExpiresInS: c.ExpiresInS,
	}
}

func custodyOutcome(err error) string {
	switch {
	case err == nil:
		return nuntius.CustodyOK
	case errors.Is(err, custosdoor.ErrCardAnswered):
		return nuntius.CustodyAlreadyAnswered
	case errors.Is(err, custosdoor.ErrStaleVerdict):
		return nuntius.CustodyStale
	case errors.Is(err, custosdoor.ErrLocked):
		return nuntius.CustodyLocked
	case errors.Is(err, custosdoor.ErrNoSuchCard):
		return nuntius.CustodyNoSuchCard
	case errors.Is(err, custosdoor.ErrIPAskOnly):
		return nuntius.CustodyIPAskOnly
	}
	return nuntius.CustodyUnavailable
}
