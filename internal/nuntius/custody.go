package nuntius

import (
	"context"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Custody sibling feed (CUSTOS-SPEC §6.4b). Custody cards ride the
// same transport as approval cards but have their own layout, button
// set (Allow once / Always / Deny) and callback namespace (cu:), and
// they never touch the ApprovalHub: settlement goes through the
// injected CustodyWire, which the wiring binds to the door client with
// the verb source "telegram".

// CustodyCard is the card content the feed renders (CUSTOS-SPEC §6.4).
type CustodyCard struct {
	ID, Cell, Cred, Tool, Dest, Review string
	ExpiresInS                         int
}

// CustodyGone is a terminal transition (§6.4b GONE, plus the
// synthesized card_dead / door_down).
type CustodyGone struct{ ID, State, Reason string }

// Closed set of resolve outcomes (door ack codes, §6.4b).
const (
	CustodyOK              = ""
	CustodyAlreadyAnswered = "already_answered"
	CustodyStale           = "stale_verdict"
	CustodyLocked          = "locked"
	CustodyNoSuchCard      = "no_such_card"
	CustodyIPAskOnly       = "ip_ask_only"
	CustodyUnavailable     = "unavailable"
)

// CustodyWire binds the feed to the custody card registry. The zero
// value leaves the feed off.
type CustodyWire struct {
	// Snapshot lists the cards already pending at attach time.
	Snapshot func() []CustodyCard
	// Subscribe registers the event sink; the returned func detaches.
	Subscribe func(f func(card *CustodyCard, gone *CustodyGone)) (cancel func())
	// Resolve settles verdict "once"|"always"|"deny" with source
	// telegram. outcome is CustodyOK or one of the codes above;
	// state is "approved"|"denied" on success.
	Resolve func(id, verdict string) (state, outcome string)
}

// Custody toasts (callback answers).
const (
	ToastCustodyLocked      = "custody locked"
	ToastCustodyExpired     = "too late — expired"
	ToastCustodyIPAskOnly   = "Allow once only (IP target)"
	ToastCustodyUnavailable = "custody unavailable"
)

var custodyIDRe = regexp.MustCompile(`^[0-9A-Za-z_.:-]{1,48}$`)

type custodyCard struct {
	chatID string
	msgID  int64
	body   string
}

type custodyFeed struct {
	b *Bridge

	mu     sync.Mutex
	cards  map[string]*custodyCard
	queue  []func()
	sig    chan struct{}
	cancel func()
}

// CustodyKeyboard builds the frozen Allow once / Always / Deny set
// (§6.5); callbacks carry only cu:<id>:<o|a|d>. An IP-literal
// destination degrades to Allow once / Deny (no Always, §6.5).
func CustodyKeyboard(id, dest string) *Keyboard {
	row := []Button{{Text: "Allow once", CallbackData: "cu:" + id + ":o"}}
	if !isIPLiteralDest(dest) {
		row = append(row, Button{Text: "Always", CallbackData: "cu:" + id + ":a"})
	}
	row = append(row, Button{Text: "Deny", CallbackData: "cu:" + id + ":d"})
	return &Keyboard{Buttons: [][]Button{row}}
}

func isIPLiteralDest(dest string) bool {
	h := strings.TrimSpace(dest)
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.IsValid()
	}
	h = strings.Trim(h, "[]")
	_, err := netip.ParseAddr(h)
	return err == nil
}

// custodyBody is the card layout: cred / dest / tool / review text.
func custodyBody(c CustodyCard) string {
	var sb strings.Builder
	sb.WriteString("Credential request: " + c.Cred)
	if c.Cell != "" {
		sb.WriteString(" (cell " + c.Cell + ")")
	}
	if c.Dest != "" {
		sb.WriteString("\nDestination: " + c.Dest)
	}
	if c.Tool != "" {
		sb.WriteString("\nTool: " + c.Tool)
	}
	if c.Review != "" {
		sb.WriteString("\n" + truncateArgs(c.Review, 700))
	}
	if c.ExpiresInS > 0 {
		sb.WriteString("\nExpires in " + strconv.Itoa(c.ExpiresInS) + "s")
	}
	return sb.String()
}

// custodyOutcome maps a GONE to its settled line.
func custodyOutcome(g CustodyGone) string {
	switch g.State {
	case "approved":
		return "Allowed ✓ (" + orDash(g.Reason) + ")"
	case "denied":
		return "Denied ✗ (" + orDash(g.Reason) + ")"
	case "timed_out":
		return "Denied — timed out"
	case "cancelled":
		return "Cancelled — " + orDash(g.Reason)
	case "card_dead":
		return "Unavailable — custody link lost"
	}
	return "Closed — " + orDash(g.State)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// WireCustody attaches the custody feed: existing cards are rendered,
// then live events flow through one ordered worker so a slow Telegram
// call never blocks the registry. No-op when w has no Subscribe. Safe
// to call once; Stop detaches it.
func (b *Bridge) WireCustody(w CustodyWire) {
	if w.Subscribe == nil || w.Resolve == nil {
		return
	}
	f := &custodyFeed{b: b, cards: map[string]*custodyCard{}, sig: make(chan struct{}, 1)}
	b.custodyMu.Lock()
	if b.custody != nil {
		b.custodyMu.Unlock()
		return
	}
	b.custody = f
	b.custodyW = w
	b.custodyMu.Unlock()

	go f.run()
	f.cancel = w.Subscribe(func(card *CustodyCard, gone *CustodyGone) {
		switch {
		case card != nil:
			c := *card
			f.enqueue(func() { f.pending(c) })
		case gone != nil:
			g := *gone
			f.enqueue(func() { f.terminal(g) })
		}
	})
	if w.Snapshot != nil {
		for _, c := range w.Snapshot() {
			f.enqueue(func() { f.pending(c) })
		}
	}
}

func (f *custodyFeed) enqueue(fn func()) {
	f.mu.Lock()
	f.queue = append(f.queue, fn)
	f.mu.Unlock()
	select {
	case f.sig <- struct{}{}:
	default:
	}
}

func (f *custodyFeed) run() {
	for {
		select {
		case <-f.b.stop:
			return
		case <-f.sig:
		}
		for {
			f.mu.Lock()
			if len(f.queue) == 0 {
				f.mu.Unlock()
				break
			}
			fn := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			fn()
		}
	}
}

func (b *Bridge) stopCustody() {
	b.custodyMu.Lock()
	f := b.custody
	b.custodyMu.Unlock()
	if f != nil && f.cancel != nil {
		f.cancel()
	}
}

func (f *custodyFeed) pending(c CustodyCard) {
	b := f.b
	if !custodyIDRe.MatchString(c.ID) {
		b.log.Printf("nuntius: custody card with unusable id skipped")
		return
	}
	f.mu.Lock()
	_, dup := f.cards[c.ID]
	f.mu.Unlock()
	if dup {
		return
	}
	st, err := b.state.Load()
	if err != nil || st.OwnerChat == "" {
		b.log.Printf("nuntius: custody card %s undeliverable: no owner chat", c.ID)
		return
	}
	body := custodyBody(c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for attempt := range cardAttempts {
		if attempt > 0 {
			if err := b.deps.sleep(ctx, cardBackoff); err != nil {
				lastErr = err
				break
			}
		}
		msg, err := b.tg.SendMessage(ctx, st.OwnerChat, body, CustodyKeyboard(c.ID, c.Dest))
		if err == nil {
			f.mu.Lock()
			f.cards[c.ID] = &custodyCard{chatID: st.OwnerChat, msgID: msg.MessageID, body: body}
			f.mu.Unlock()
			return
		}
		lastErr = err
	}
	b.log.Printf("nuntius: custody card send failed for %s: %v", c.ID, lastErr)
}

// terminal edits a delivered card to its settled state and strips the
// buttons (one edit, one retry, then log — same posture as §7.5).
func (f *custodyFeed) terminal(g CustodyGone) {
	f.mu.Lock()
	c, ok := f.cards[g.ID]
	delete(f.cards, g.ID)
	f.mu.Unlock()
	if !ok {
		return
	}
	f.edit(g.ID, c, c.body+"\n\n"+custodyOutcome(g))
}

func (f *custodyFeed) edit(id string, c *custodyCard, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.b.tg.EditMessageText(ctx, c.chatID, c.msgID, text, nil); err != nil {
		if err2 := f.b.tg.EditMessageText(ctx, c.chatID, c.msgID, text, nil); err2 != nil {
			f.b.log.Printf(LogCardStale, id)
		}
	}
}

// parseCustodyData decodes cu:<id>:<o|a|d>.
func parseCustodyData(data string) (id, verdict string, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != "cu" {
		return "", "", false
	}
	id = strings.Join(parts[1:len(parts)-1], ":")
	if !custodyIDRe.MatchString(id) {
		return "", "", false
	}
	switch parts[len(parts)-1] {
	case "o":
		return id, "once", true
	case "a":
		return id, "always", true
	case "d":
		return id, "deny", true
	}
	return "", "", false
}

// handleCustodyCallback settles a custody-card tap and ALWAYS toasts
// (N10). Settled-card edits come from the GONE frame; error outcomes
// that mean the card is no longer actionable also strip the buttons.
func (b *Bridge) handleCustodyCallback(ctx context.Context, u Update, rec Record) {
	cbID := callbackIDOf(u)
	defer b.markDone(rec, nil)

	id, verdict, ok := parseCustodyData(rec.Payload)
	b.custodyMu.Lock()
	f, w := b.custody, b.custodyW
	b.custodyMu.Unlock()
	if !ok || f == nil || w.Resolve == nil {
		b.toast(ctx, cbID, ToastNotPending)
		b.log.Printf(LogForeignCallback, rec.Payload, rec.UpdateID)
		return
	}
	state, outcome := w.Resolve(id, verdict)
	switch outcome {
	case CustodyOK:
		if state == "denied" {
			b.toast(ctx, cbID, ToastDenied)
		} else {
			b.toast(ctx, cbID, ToastAllowed)
		}
	case CustodyAlreadyAnswered:
		b.toast(ctx, cbID, ToastAlready)
		//nolint:contextcheck // card edits detach on purpose: the callback ctx ends before the edit settles; edit() carries its own 15 s ctx
		f.stripButtons(id, "Already answered")
	case CustodyStale:
		b.toast(ctx, cbID, ToastCustodyExpired)
		//nolint:contextcheck // card edits detach on purpose: the callback ctx ends before the edit settles; edit() carries its own 15 s ctx
		f.stripButtons(id, "Expired")
	case CustodyLocked:
		b.toast(ctx, cbID, ToastCustodyLocked)
	case CustodyIPAskOnly:
		b.toast(ctx, cbID, ToastCustodyIPAskOnly)
	case CustodyNoSuchCard:
		b.toast(ctx, cbID, ToastNotPending)
		//nolint:contextcheck // card edits detach on purpose: the callback ctx ends before the edit settles; edit() carries its own 15 s ctx
		f.drop(id, "No longer pending")
	default:
		b.toast(ctx, cbID, ToastCustodyUnavailable)
	}
}

// stripButtons removes the keyboard but keeps the card tracked so the
// authoritative GONE (which carries the settling door) still lands.
func (f *custodyFeed) stripButtons(id, note string) {
	f.mu.Lock()
	c, ok := f.cards[id]
	f.mu.Unlock()
	if ok {
		f.edit(id, c, c.body+"\n\n"+note)
	}
}

func (f *custodyFeed) drop(id, note string) {
	f.mu.Lock()
	c, ok := f.cards[id]
	delete(f.cards, id)
	f.mu.Unlock()
	if ok {
		f.edit(id, c, c.body+"\n\n"+note)
	}
}
