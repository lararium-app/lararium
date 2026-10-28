package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/loop"
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
	// UnlockKeyfile is the optional path to an unlock keyfile per CUSTOS-SPEC §4.1, §C3, §12 Q1.
	UnlockKeyfile string `yaml:"unlock_keyfile"`
	// Connectors holds external connector configurations; ignored here, validated by custosd.
	Connectors custos.ConnectorsConfig `yaml:"connectors"`
}

// startCustosDoor starts the door client as a supervised goroutine and
// feeds the Nuntius sibling feed and web surface (bridge and srv may be nil).
// When hub is non-nil, its rail tools join the web-chat registry as a
// dynamic tool source with policy gating (issue #83, #129). It returns nil — having
// opened no socket, started no goroutine and logged nothing — unless both paths are
// set. The returned stop closes the door connection and waits for the
// goroutine; it is idempotent.
func startCustosDoor(ctx context.Context, cfg CustosConfig, bridge *nuntius.Bridge, srv *surface.Server, hub *surface.Hub, deciders ...PolicyDecider) (stop func()) {
	var (
		doorMu        sync.Mutex
		doorOnline    bool
		doorInitDone  bool
		lastToolCount = -1
	)
	// logToolCount logs the rail tool census only when it changes — the
	// tool source is consulted on every turn, so an unconditional line
	// would be one log entry per chat message.
	logToolCount := func(n int) {
		doorMu.Lock()
		defer doorMu.Unlock()
		if n == lastToolCount {
			return
		}
		lastToolCount = n
		log.Printf("custos door: %d rail tools available", n)
	}
	markConnected := func() {
		doorMu.Lock()
		defer doorMu.Unlock()
		if !doorOnline {
			doorOnline = true
			log.Print("custos door: connected")
		}
	}
	markDisconnected := func() {
		doorMu.Lock()
		defer doorMu.Unlock()
		if doorOnline {
			doorOnline = false
			log.Print("custos door: disconnected")
		}
	}

	opts := custosdoor.Options{
		SockPath:  cfg.DoorsSock,
		TokenPath: cfg.DoorToken,
		Name:      cfg.DoorName,
		OnRetry: func(attempt int, delay time.Duration, err error) {
			markDisconnected()
		},
	}
	client, err := custosdoor.New(opts)
	if err != nil {
		// A half-set or invalid pair is local misconfiguration: say so
		// once and keep serving without custody cards.
		log.Printf("WARN custos door disabled: %v", err)
		return nil
	}
	if client == nil {
		return nil
	}

	client.SubscribeMCP(func(statuses []custosdoor.ConnectorStatus, tools []custosdoor.ToolInfo) {
		doorMu.Lock()
		if !doorInitDone {
			doorInitDone = true
			doorMu.Unlock()
			return
		}
		doorMu.Unlock()
		if statuses != nil || tools != nil {
			markConnected()
			logToolCount(len(tools))
		}
	})

	if bridge != nil {
		//nolint:contextcheck // custodyFeed edits run on the feed goroutine with their own detached 15 s ctx (same posture as nuntius background card edits)
		bridge.WireCustody(custodyWire(client))
	}
	if srv != nil {
		srv.SetCustosRegistry(NewCustosWebAdapter(client))
		// Issue #132: an "always" verdict on a turn-approval card mints
		// the canonical worker-lane rule (connector/tool -> auto,
		// always:true) so the next turn's Decide runs the call silently.
		srv.SetAlwaysRuleHook(turnAlwaysRuleMinter(srv, client))
		// Issue #142: an "Always: allow connector" action on a worker ask card
		// mints the connector-wide auto rule (<connector>/* -> auto).
		srv.SetConnectorTrustHook(turnConnectorTrustMinter(srv, client))
	}
	if hub != nil {
		var decider PolicyDecider
		if len(deciders) > 0 && deciders[0] != nil {
			decider = deciders[0]
		} else if srv != nil {
			decider = &serverPolicyDecider{srv: srv}
		}
		baseToolSrc := mcpToolSource(client, decider)
		toolSrc := func() []loop.Tool {
			tools := baseToolSrc()
			if len(tools) > 0 {
				// A non-empty census proves the door is live; a zero
				// census also happens while the door is down, so it
				// must not claim connectivity or log noise.
				markConnected()
				logToolCount(len(tools))
			}
			return tools
		}
		hub.SetToolSource(toolSrc)
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

// serverPolicyDecider adapts surface.Server's PolicySource to PolicyDecider dynamically (issue #129).
type serverPolicyDecider struct {
	srv *surface.Server
}

// alwaysRuleMinter is the mint seam for card-minted always-rules
// (issue #132); *policySourceAdapter implements it over the custody vault.
type alwaysRuleMinter interface {
	AddAlwaysRule(pattern string) error
}

// turnAlwaysRuleMinter builds the surface hook that mints the
// "<connector>/<tool>" worker rule for a turn-approval card settled with
// "always" (issue #132). The minter is resolved dynamically from the
// server's PolicySource so a vault attached after boot still mints; the
// rail list is consulted for the authoritative connector/tool pair
// because the model-facing name is a lossy sanitization.
func turnAlwaysRuleMinter(srv *surface.Server, lister MCPToolLister) func(toolName string) error {
	return func(toolName string) error {
		src := srv.PolicySource()
		minter, ok := src.(alwaysRuleMinter)
		if src == nil || !ok {
			return errors.New("policy backend unavailable: cannot mint always-rule")
		}
		pattern, ok := mcpRulePattern(lister, toolName)
		if !ok {
			return fmt.Errorf("no rail tool matches %q (stale card or non-rail tool)", toolName)
		}
		return minter.AddAlwaysRule(pattern)
	}
}

// turnConnectorTrustMinter builds the surface hook that mints the
// "<connector>/*" worker auto rule when an approval card is resolved with
// scope "connector" (issue #142). Reuses the exact same AddRule mutation path
// on PolicySource that runMCPTrust uses.
func turnConnectorTrustMinter(srv *surface.Server, lister MCPToolLister) func(toolName string) error {
	return func(toolName string) error {
		if srv == nil {
			return errors.New("policy backend unavailable: cannot mint connector trust rule")
		}
		src := srv.PolicySource()
		if src == nil {
			return errors.New("policy backend unavailable: cannot mint connector trust rule")
		}
		connector, ok := mcpConnectorName(lister, toolName)
		if !ok {
			return fmt.Errorf("no rail connector matches tool %q (stale card or non-rail tool)", toolName)
		}
		pat := connector + "/*"
		if _, err := custos.ValidateCredentialPattern(pat, "auto", false); err != nil {
			return fmt.Errorf("invalid connector pattern %q: %w", pat, err)
		}
		_, err := src.AddRule("credential", pat, "auto")
		return err
	}
}

func (d *serverPolicyDecider) Decide(connector, tool string) string {
	if d == nil || d.srv == nil {
		return custos.VerdictAsk
	}
	ps := d.srv.PolicySource()
	if pd, ok := ps.(PolicyDecider); ok && pd != nil {
		return pd.Decide(connector, tool)
	}
	return custos.VerdictAsk
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
