package surface

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// actionDispatchFrame is the frame carried by event: action_dispatch over GET /v1/devices/<id>/stream.
type actionDispatchFrame struct {
	ActionID string          `json:"action_id"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
}

// actionReceiptResult holds the result of POST /v1/devices/<id>/actions/<action_id>/receipt.
type actionReceiptResult struct {
	OK     bool
	Result json.RawMessage
	Error  string
}

// deviceStreamSession represents one active SSE connection to GET /v1/devices/<id>/stream.
type deviceStreamSession struct {
	id         string
	deviceID   string
	dispatchCh chan actionDispatchFrame
	doneCh     chan struct{}
	closeOnce  sync.Once
}

func newDeviceStreamSession(deviceID string) *deviceStreamSession {
	return &deviceStreamSession{
		id:         randomHex(8),
		deviceID:   deviceID,
		dispatchCh: make(chan actionDispatchFrame, 16),
		doneCh:     make(chan struct{}),
	}
}

func (s *deviceStreamSession) close() {
	s.closeOnce.Do(func() {
		close(s.doneCh)
	})
}

// deviceBroker implements ActionBroker per DEVICE-RAILS-SPEC D5/D6.
type deviceBroker struct {
	mu           sync.RWMutex
	s            *Server
	streams      map[string]*deviceStreamSession
	actions      map[string]chan actionReceiptResult
	actionDevice map[string]string
}

func newDeviceBroker(s *Server) *deviceBroker {
	return &deviceBroker{
		s:            s,
		streams:      make(map[string]*deviceStreamSession),
		actions:      make(map[string]chan actionReceiptResult),
		actionDevice: make(map[string]string),
	}
}

// RegisterStream attaches a live device stream, replacing any older concurrent stream per spec Deliverable 1.
func (b *deviceBroker) RegisterStream(deviceID string) (*deviceStreamSession, func()) {
	b.mu.Lock()
	if old, exists := b.streams[deviceID]; exists && old != nil {
		old.close()
	}
	sess := newDeviceStreamSession(deviceID)
	b.streams[deviceID] = sess
	b.mu.Unlock()

	door := b.s.getDeviceDoor()
	if door != nil {
		door.SetOnline(deviceID, true)
		b.s.pokeDeviceRail()
	}

	cleanup := func() {
		b.mu.Lock()
		cur := b.streams[deviceID]
		isCur := (cur == sess)
		if isCur {
			delete(b.streams, deviceID)
		}
		b.mu.Unlock()

		if isCur {
			d := b.s.getDeviceDoor()
			if d != nil {
				d.SetOnline(deviceID, false)
				grace := d.GetGracePeriod()
				if grace <= 0 {
					grace = 30 * time.Second
				}
				time.AfterFunc(grace, func() {
					b.mu.RLock()
					stillNoStream := b.streams[deviceID] == nil
					b.mu.RUnlock()
					doorNow := b.s.getDeviceDoor()
					if stillNoStream && doorNow != nil && !doorNow.IsOnline(deviceID) {
						b.s.pokeDeviceRail()
					}
				})
			}
		}
	}

	return sess, cleanup
}

// HasStream reports whether a live stream is open for deviceID.
func (b *deviceBroker) HasStream(deviceID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.streams[deviceID] != nil
}

// CloseStream closes any open stream for deviceID.
func (b *deviceBroker) CloseStream(deviceID string) {
	b.mu.Lock()
	if sess, ok := b.streams[deviceID]; ok && sess != nil {
		delete(b.streams, deviceID)
		sess.close()
	}
	b.mu.Unlock()
}

// Dispatch enqueues an action frame on the device's open stream and awaits execution receipt.
func (b *deviceBroker) Dispatch(ctx context.Context, deviceID, tool string, args json.RawMessage) (json.RawMessage, error) {
	b.mu.Lock()
	sess, ok := b.streams[deviceID]
	if !ok || sess == nil {
		b.mu.Unlock()
		return nil, ErrDeviceOffline
	}

	actionID := "act_" + randomHex(8)
	receiptCh := make(chan actionReceiptResult, 1)
	b.actions[actionID] = receiptCh
	b.actionDevice[actionID] = deviceID

	frame := actionDispatchFrame{
		ActionID: actionID,
		Tool:     tool,
		Args:     args,
	}

	select {
	case sess.dispatchCh <- frame:
	default:
		delete(b.actions, actionID)
		delete(b.actionDevice, actionID)
		b.mu.Unlock()
		return nil, ErrDeviceOffline
	}
	b.mu.Unlock()

	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()

	select {
	case res := <-receiptCh:
		if !res.OK {
			errMsg := res.Error
			if errMsg == "" {
				errMsg = "action failed"
			}
			return nil, errors.New(errMsg)
		}
		return res.Result, nil
	case <-timer.C:
		b.mu.Lock()
		delete(b.actions, actionID)
		delete(b.actionDevice, actionID)
		b.mu.Unlock()
		return nil, errors.New("action_timeout")
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.actions, actionID)
		delete(b.actionDevice, actionID)
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// ResolveAction correlates a receipt submission with its parked dispatch. Returns false on unknown/stale actionID.
func (b *deviceBroker) ResolveAction(deviceID, actionID string, ok bool, result json.RawMessage, errStr string) bool {
	b.mu.Lock()
	dev, hasDev := b.actionDevice[actionID]
	ch, hasAction := b.actions[actionID]
	if !hasDev || !hasAction || dev != deviceID {
		b.mu.Unlock()
		return false
	}
	delete(b.actions, actionID)
	delete(b.actionDevice, actionID)
	b.mu.Unlock()

	ch <- actionReceiptResult{OK: ok, Result: result, Error: errStr}
	return true
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
