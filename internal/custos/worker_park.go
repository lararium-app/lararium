package custos

import (
	"encoding/json"
	"time"
)

// askCard parks the worker call behind an approval card carrying the tool
// schema's review fields (§7 card contents; hold/shed/timeout mechanics
// identical to bearer parking via the shared park manager).
func (ws *WorkerServer) askCard(cw *cellWorker, conn Connector, tool string, args json.RawMessage) (bool, WorkerResponse) {
	if ws.hub == nil {
		ws.auditDenied(cw, conn.Credential(), conn.Host(), tool, "no_approval_door")
		return false, errResponse(ErrCodeDenied, detailDeniedPolicy)
	}

	pm := ws.parkMgr
	key := pendingKey{cellID: cw.cellID, cred: conn.Credential(), tool: tool}
	pm.mu.Lock()
	if pc, ok := pm.pending[key]; ok {
		pm.mu.Unlock()
		return ws.awaitSharedCard(cw, conn, tool, pc)
	}
	if len(pm.byCell[cw.cellID]) >= pm.maxPerCell {
		pm.mu.Unlock()
		ws.auditDenied(cw, conn.Credential(), conn.Host(), tool, "too_many_asks")
		return false, errResponse(ErrCodeDenied, detailDeniedApproval)
	}
	// Mirror the bearer-lane order (registerParkFlow): register the card
	// and bind its id to the flow INSIDE the pm.mu critical section, so a
	// concurrent global-shed can never signal 'too_many_asks' against a
	// parked flow whose id is still empty (an orphaned card nobody can
	// cancel). The shed signal is delivered after this lock releases.
	var shedFlow *parkedFlow
	if pm.totalParked >= pm.maxGlobal {
		shedFlow = pm.findBiggestCellParkLocked()
		if shedFlow != nil {
			pm.removeParkLocked(shedFlow)
		}
	}
	review := conn.ReviewFields(tool, args)
	dest := conn.Host() + ":" + tool
	id, decisionCh := ws.hub.RegisterCustos(cw.cellID, conn.Credential(), dest, review, ws.cfg.AskHoldTimeout, nil)
	flow := &parkedFlow{
		id:         id,
		cellID:     cw.cellID,
		cred:       conn.Credential(),
		host:       conn.Host(),
		path:       tool,
		review:     review,
		decisionCh: decisionCh,
		outcomeCh:  make(chan string, 1),
		createdAt:  time.Now(),
	}
	pc := &pendingCard{key: key, cardID: id, done: make(chan struct{})}
	pm.addParkLocked(flow)
	pm.pending[key] = pc
	pm.mu.Unlock()

	if shedFlow != nil {
		select {
		case shedFlow.outcomeCh <- "too_many_asks":
		default:
		}
	}

	timer := time.NewTimer(ws.cfg.AskHoldTimeout)
	defer timer.Stop()

	settle := func() {
		pm.mu.Lock()
		pm.removeParkLocked(flow)
		pm.mu.Unlock()
	}

	// finish publishes the single outcome to every flow parked on this
	// card (the leader included) and audits the leader's own denial.
	finish := func(allowed bool, reason string, resp WorkerResponse) (bool, WorkerResponse) {
		pc.allowed, pc.reason, pc.resp = allowed, reason, resp
		close(pc.done)
		if !allowed {
			ws.auditDenied(cw, conn.Credential(), conn.Host(), tool, reason)
		}
		return allowed, resp
	}

	select {
	case <-flow.outcomeCh:
		// Evicted by global shed.
		if flow.cancelled.CompareAndSwap(false, true) {
			ws.hub.CancelCard(flow.id, "too_many_asks", "custos")
		}
		settle()
		return finish(false, "too_many_asks", errResponse(ErrCodeDenied, detailDeniedApproval))

	case <-timer.C:
		if flow.cancelled.CompareAndSwap(false, true) {
			settle()
			ws.hub.CancelCard(flow.id, "timeout", "timer")
		}
		return finish(false, "ask_timeout", errResponse(ErrCodeDenied, detailDeniedApproval))

	case <-ws.done:
		if flow.cancelled.CompareAndSwap(false, true) {
			settle()
			ws.hub.CancelCard(flow.id, "timeout", "server_closed")
		}
		return finish(false, "server_closed", errResponse(ErrCodeLocked, detailLocked))

	case approved := <-decisionCh:
		settle()
		if !approved {
			reason := ws.hub.Reason(cw.cellID, id)
			switch reason {
			case "custos_locked":
				return finish(false, "custos_locked", errResponse(ErrCodeLocked, detailLocked))
			case "credential_revoked":
				return finish(false, "credential_revoked", errResponse(ErrCodeDenied, detailDeniedRevoked))
			default:
				return finish(false, "denied_by_user", errResponse(ErrCodeDenied, detailDeniedApproval))
			}
		}
		return finish(true, "", WorkerResponse{})
	}
}

// awaitSharedCard parks a duplicate call on the already-pending card for the
// same connector+tool and resumes with that card's outcome. The leader owns
// the card's hold timer and shutdown handling, so no waiter is orphaned.
func (ws *WorkerServer) awaitSharedCard(cw *cellWorker, conn Connector, tool string, pc *pendingCard) (bool, WorkerResponse) {
	select {
	case <-pc.done:
	case <-ws.done:
		ws.auditDenied(cw, conn.Credential(), conn.Host(), tool, "server_closed")
		return false, errResponse(ErrCodeLocked, detailLocked)
	}
	if !pc.allowed {
		ws.auditDenied(cw, conn.Credential(), conn.Host(), tool, pc.reason)
	}
	return pc.allowed, pc.resp
}

// RegisterParkForTest registers a parked flow in the worker park manager for testing.
func (ws *WorkerServer) RegisterParkForTest(id, cellID, cred, tool, review string) {
	ws.parkMgr.mu.Lock()
	defer ws.parkMgr.mu.Unlock()
	ws.parkMgr.addParkLocked(&parkedFlow{
		id:        id,
		cellID:    cellID,
		cred:      cred,
		host:      "gmail.googleapis.com",
		path:      tool,
		review:    review,
		createdAt: time.Now(),
	})
}
