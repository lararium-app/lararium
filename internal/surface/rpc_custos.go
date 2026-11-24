package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// custosRPCBufferCap matches the v1 per-connection custody buffer cap (64).
const custosRPCBufferCap = 64

// custosRPCSub tracks one connection's live custody card subscription.
type custosRPCSub struct {
	events       chan CustosEvent
	overflowCh   chan struct{}
	overflowOnce sync.Once
	stopCh       chan struct{}
	stopOnce     sync.Once
	done         chan struct{}

	cancelMu  sync.Mutex
	cancelReg func()
	cancelHub func()
	cancelled bool
}

func newCustosRPCSub() *custosRPCSub {
	return &custosRPCSub{
		events:     make(chan CustosEvent, custosRPCBufferCap),
		overflowCh: make(chan struct{}),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
	}
}

func (sub *custosRPCSub) push(card *CustosCard, gone *CustosGone) {
	if card == nil && gone == nil {
		return
	}
	ev := CustosEvent{Card: card, Gone: gone}
	select {
	case sub.events <- ev:
	default:
		sub.overflowOnce.Do(func() {
			close(sub.overflowCh)
		})
	}
}

func (sub *custosRPCSub) setCancelReg(fn func()) {
	sub.cancelMu.Lock()
	if sub.cancelled {
		sub.cancelMu.Unlock()
		if fn != nil {
			fn()
		}
		return
	}
	sub.cancelReg = fn
	sub.cancelMu.Unlock()
}

func (sub *custosRPCSub) setCancelHub(fn func()) {
	sub.cancelMu.Lock()
	if sub.cancelled {
		sub.cancelMu.Unlock()
		if fn != nil {
			fn()
		}
		return
	}
	sub.cancelHub = fn
	sub.cancelMu.Unlock()
}

func (sub *custosRPCSub) cancelUpstreams() {
	sub.cancelMu.Lock()
	if sub.cancelled {
		sub.cancelMu.Unlock()
		return
	}
	sub.cancelled = true
	cReg := sub.cancelReg
	cHub := sub.cancelHub
	sub.cancelReg = nil
	sub.cancelHub = nil
	sub.cancelMu.Unlock()

	if cReg != nil {
		cReg()
	}
	if cHub != nil {
		cHub()
	}
}

func (sub *custosRPCSub) stop() {
	sub.stopOnce.Do(func() {
		close(sub.stopCh)
		sub.cancelUpstreams()
	})
}

// DeviceApproverLive reports whether at least one authenticated RPC
// connection holds a live custos.cards.subscribe subscription
// (NUNTIUS-SPEC §7.3, A9). Wired as ApprovalHub's deviceLive probe via
// Server.SetApprovalHub; connections without an active custody
// subscription do not count.
func (s *Server) DeviceApproverLive() bool {
	s.rpcCustosMu.Lock()
	defer s.rpcCustosMu.Unlock()
	return len(s.rpcCustosSubs) > 0
}

func (s *Server) removeCustosRPCSub(conn *RPCConn, sub *custosRPCSub) {
	s.rpcCustosMu.Lock()
	lost := false
	if s.rpcCustosSubs[conn] == sub {
		delete(s.rpcCustosSubs, conn)
		lost = len(s.rpcCustosSubs) == 0
	}
	s.rpcCustosMu.Unlock()
	// NUNTIUS-SPEC §7.4 A9: notify the hub OUTSIDE rpcCustosMu (hub settle
	// fans out to observers) when the last custody subscriber detaches.
	if lost {
		if hub := s.approvalsViaRegistry(); hub != nil {
			hub.DeviceChannelLost()
		}
	}
}

func (s *Server) unsubscribeCustosRPCConn(conn *RPCConn) {
	s.rpcCustosMu.Lock()
	sub := s.rpcCustosSubs[conn]
	lost := false
	if sub != nil {
		delete(s.rpcCustosSubs, conn)
		lost = len(s.rpcCustosSubs) == 0
	}
	s.rpcCustosMu.Unlock()
	if sub != nil {
		sub.stop()
	}
	// NUNTIUS-SPEC §7.4 A9: notify the hub OUTSIDE rpcCustosMu when the
	// last custody subscriber detaches.
	if lost {
		if hub := s.approvalsViaRegistry(); hub != nil {
			hub.DeviceChannelLost()
		}
	}
}

// rpcCustosCardsSubscribe implements custos.cards.subscribe (RPC-SPEC §4).
// Subscribes to both the door registry and the approval hub before taking the
// snapshot, sends the snapshot result, and only then starts the untracked pump
// goroutine so no custody notification can precede the subscribe result and
// wsHandler teardown never blocks on reqWg.Wait.
func (s *Server) rpcCustosCardsSubscribe(ctx context.Context, conn *RPCConn, req *RPCRequest) {
	sub := newCustosRPCSub()

	// Replace any existing custody subscription on this connection first so
	// re-subscribing never causes double delivery.
	s.rpcCustosMu.Lock()
	if s.rpcCustosSubs == nil {
		s.rpcCustosSubs = make(map[*RPCConn]*custosRPCSub)
	}
	old := s.rpcCustosSubs[conn]
	s.rpcCustosSubs[conn] = sub
	s.rpcCustosMu.Unlock()

	if old != nil {
		old.stop()
		<-old.done
	}

	select {
	case <-ctx.Done():
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
		return
	case <-sub.stopCh:
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
		return
	default:
	}

	// Step 1: subscribe to registry and hub BEFORE taking the snapshot.
	if reg := s.getCustosRegistry(); reg != nil {
		sub.setCancelReg(reg.Subscribe(sub.push))
	}
	if hub := s.approvalsViaRegistry(); hub != nil {
		sub.setCancelHub(hub.SubscribeCustos(sub.push))
	}

	// Step 2: build the merged snapshot via the shared helper.
	cards := s.custosCardsSnapshot()

	// Step 3: send the subscribe result BEFORE starting the delivery pump.
	if err := conn.SendResult(*req.ID, map[string]any{"cards": cards}); err != nil {
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
		return
	}

	select {
	case <-ctx.Done():
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
		return
	case <-sub.stopCh:
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
		return
	default:
	}

	// Step 4: start the delivery pump outside reqWg so this handler returns
	// immediately and wsHandler's defer order never deadlocks on teardown.
	go s.runCustosRPCPump(ctx, conn, sub)
}

func (s *Server) runCustosRPCPump(ctx context.Context, conn *RPCConn, sub *custosRPCSub) {
	defer func() {
		sub.stop()
		s.removeCustosRPCSub(conn, sub)
		close(sub.done)
	}()

	handleOverflow := func() {
		select {
		case <-ctx.Done():
			return
		case <-sub.stopCh:
			return
		default:
		}
		sub.cancelUpstreams()
		s.removeCustosRPCSub(conn, sub)
		_ = conn.Notify("custos.resync", nil)
	}

	for {
		// Prioritize cancellation/replacement and overflow ahead of buffered events.
		select {
		case <-ctx.Done():
			return
		case <-sub.stopCh:
			return
		default:
		}
		select {
		case <-sub.overflowCh:
			handleOverflow()
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-sub.stopCh:
			return
		case <-sub.overflowCh:
			handleOverflow()
			return
		case ev := <-sub.events:
			if ev.Card != nil {
				if err := conn.Notify("custos.card", ev.Card); err != nil {
					return
				}
			} else if ev.Gone != nil {
				if err := conn.Notify("custos.gone", ev.Gone); err != nil {
					return
				}
			}
		}
	}
}

// rpcCustosCardsResolve implements custos.cards.resolve (RPC-SPEC §4) by
// delegating to the shared resolveCustosCard core in custos_cards.go.
func (s *Server) rpcCustosCardsResolve(_ context.Context, conn *RPCConn, req *RPCRequest) {
	trimmed := bytes.TrimSpace(req.Params)
	var params struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || json.Unmarshal(trimmed, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	out := s.resolveCustosCard(params.ID, params.Verdict, viaForKind(conn.callerKind))
	if out.status != http.StatusOK {
		reason := strings.ReplaceAll(out.reason, " ", "_")
		_ = conn.SendError(*req.ID, out.reason, reason, nil)
		return
	}

	_ = conn.SendResult(*req.ID, map[string]string{"state": out.state})
}
