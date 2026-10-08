package custosdoor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Reconnect and timing defaults (§6.4b: capped exponential backoff,
// 1 s → 30 s).
const (
	DefaultBaseDelay  = time.Second
	DefaultMaxDelay   = 30 * time.Second
	defaultAckTimeout = 10 * time.Second
	helloTimeout      = 10 * time.Second
	writeTimeout      = 5 * time.Second
	maxLine           = 1 << 20
	// logEvery: failures are logged on the first attempt and then once
	// per this many consecutive attempts, so a permanently absent or
	// misconfigured custosd never floods the log.
	logEvery = 100
)

// Options configures a Client. SockPath and TokenPath are the frozen
// config pair custos.doors_sock / custos.door_token.
type Options struct {
	SockPath  string
	TokenPath string
	// Name is the HELLO door name ([a-z0-9_-]{1,32}); empty = "hearthd".
	// Non-frozen extension key; the frozen pair is the two paths.
	Name string
	Log  *log.Logger

	// Test seams; zero values select the spec defaults.
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	AckTimeout time.Duration
	// OnRetry observes each backoff wait (consecutive failure count,
	// delay, cause).
	OnRetry func(attempt int, delay time.Duration, err error)
}

type pendingCard struct {
	card Card
	seq  uint64
	at   time.Time
}

type event struct {
	card *Card
	gone *Gone
}

// session is one attached door connection.
type session struct {
	conn net.Conn
	ack  chan string   // OK / ERR lines, routed to the one waiter
	dead chan struct{} // closed when the reader exits
}

// Client is the door-client goroutine's state and the custos card
// registry. It implements Registry.
type Client struct {
	opts Options
	name string
	log  *log.Logger

	mu      sync.Mutex
	pending map[string]*pendingCard
	seq     uint64
	subs    map[int]func(*Card, *Gone)
	nextSub int
	sess    *session
	evq     []event
	evSig   chan struct{}

	cmdMu sync.Mutex // one outstanding verb at a time

	done chan struct{}
}

var _ Registry = (*Client)(nil)

// New builds a Client. Either path empty means the feature is off: it
// returns (nil, nil) and touches nothing (no socket, no goroutine, no
// log line). Exactly one path set is a configuration error.
func New(o Options) (*Client, error) {
	if o.SockPath == "" && o.TokenPath == "" {
		return nil, nil
	}
	if o.SockPath == "" || o.TokenPath == "" {
		return nil, errors.New("custos: doors_sock and door_token must both be set")
	}
	name := o.Name
	if name == "" {
		name = defaultDoorName
	}
	if !ValidName(name) {
		return nil, fmt.Errorf("custos: door name %q must match [a-z0-9_-]{1,32}", name)
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = DefaultBaseDelay
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = DefaultMaxDelay
	}
	if o.AckTimeout <= 0 {
		o.AckTimeout = defaultAckTimeout
	}
	lg := o.Log
	if lg == nil {
		lg = log.Default()
	}
	return &Client{
		opts:    o,
		name:    name,
		log:     lg,
		pending: map[string]*pendingCard{},
		subs:    map[int]func(*Card, *Gone){},
		evSig:   make(chan struct{}, 1),
		done:    make(chan struct{}),
	}, nil
}

// Done is closed once Run has returned and the connection is closed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Run attaches, serves and reconnects until ctx is cancelled, then
// closes the door connection. It never returns an error and never
// panics on an absent custosd: it backs off and retries.
func (c *Client) Run(ctx context.Context) {
	defer close(c.done)
	go c.dispatch(ctx)

	delay := c.opts.BaseDelay
	fails := 0
	for ctx.Err() == nil {
		attached, err := c.session1(ctx)
		if ctx.Err() != nil {
			return
		}
		if attached {
			fails = 0
			delay = c.opts.BaseDelay
		}
		fails++
		if fails == 1 || fails%logEvery == 0 {
			c.logFailure(fails, err)
		}
		if c.opts.OnRetry != nil {
			c.opts.OnRetry(fails, delay, err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, c.opts.MaxDelay)
	}
}

func (c *Client) logFailure(n int, err error) {
	if errors.Is(err, ErrBadToken) {
		c.log.Printf("WARN custos door: bad token (check custos.door_token); backing off (attempt %d)", n)
		return
	}
	c.log.Printf("WARN custos door: unavailable: %v; retrying with backoff (attempt %d)", err, n)
}

// session1 runs one connection lifetime. attached reports whether the
// HELLO succeeded (resets the backoff).
func (c *Client) session1(ctx context.Context) (attached bool, err error) {
	raw, err := os.ReadFile(c.opts.TokenPath)
	if err != nil {
		return false, fmt.Errorf("read door token: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n\x00") {
		return false, errors.New("door token file is empty or malformed")
	}

	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, helloTimeout)
	conn, err := d.DialContext(dctx, "unix", c.opts.SockPath)
	cancel()
	if err != nil {
		return false, err
	}
	defer conn.Close()

	// Cancel closes the conn so a blocked read returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(helloTimeout))
	if _, err := conn.Write([]byte("HELLO " + tok + " " + c.name + "\n")); err != nil {
		return false, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	line, err := readLine(br)
	if err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})

	switch {
	case line == "ERR bad_token":
		return false, ErrBadToken
	case strings.HasPrefix(line, "OK "):
	default:
		return false, fmt.Errorf("unexpected HELLO reply %q", truncate(line))
	}
	var snap []Card
	if err := json.Unmarshal([]byte(line[3:]), &snap); err != nil {
		return false, fmt.Errorf("bad HELLO snapshot: %w", err)
	}

	s := &session{conn: conn, ack: make(chan string, 1), dead: make(chan struct{})}
	c.attach(s, snap)
	defer func() {
		close(s.dead)
		c.detach(s)
	}()

	for {
		line, err := readLine(br)
		if err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, fmt.Errorf("door link dropped: %w", err)
		}
		c.handleLine(s, line)
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		frag, err := br.ReadSlice('\n')
		sb.Write(frag)
		if sb.Len() > maxLine {
			return "", errors.New("door frame too long")
		}
		if err == nil {
			return strings.TrimRight(sb.String(), "\r\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

func (c *Client) handleLine(s *session, line string) {
	switch {
	case strings.HasPrefix(line, "CARD "):
		var card Card
		if err := json.Unmarshal([]byte(line[5:]), &card); err != nil || card.ID == "" {
			c.log.Printf("WARN custos door: dropped malformed CARD frame")
			return
		}
		c.upsert(card)
	case strings.HasPrefix(line, "GONE "):
		var g Gone
		if err := json.Unmarshal([]byte(line[5:]), &g); err != nil || g.ID == "" {
			c.log.Printf("WARN custos door: dropped malformed GONE frame")
			return
		}
		c.remove(g)
	case line == "OK" || strings.HasPrefix(line, "OK ") || strings.HasPrefix(line, "ERR "):
		select {
		case s.ack <- line:
		default: // no waiter: unsolicited ack, drop
		}
	}
}

// attach installs the session and replaces the pending set with the
// HELLO snapshot (the refresh path: no GET verb exists on doors.sock).
func (c *Client) attach(s *session, snap []Card) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess = s
	for id := range c.pending {
		delete(c.pending, id)
	}
	for i := range snap {
		if snap[i].ID == "" {
			continue
		}
		c.addLocked(snap[i])
	}
}

// addLocked inserts card and queues a render event; a card already
// held (replay/race) is refreshed silently so surfaces never double-render.
func (c *Client) addLocked(card Card) {
	if p, ok := c.pending[card.ID]; ok {
		p.card, p.at = card, time.Now()
		return
	}
	c.seq++
	c.pending[card.ID] = &pendingCard{card: card, seq: c.seq, at: time.Now()}
	cp := card
	c.queueLocked(event{card: &cp})
}

func (c *Client) upsert(card Card) {
	c.mu.Lock()
	c.addLocked(card)
	c.mu.Unlock()
}

func (c *Client) remove(g Gone) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[g.ID]; !ok {
		return
	}
	delete(c.pending, g.ID)
	gg := g
	c.queueLocked(event{gone: &gg})
}

// detach drops the session and kills every rendered card.
func (c *Client) detach(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == s {
		c.sess = nil
	}
	for _, p := range c.sorted() {
		delete(c.pending, p.card.ID)
		c.queueLocked(event{gone: &Gone{ID: p.card.ID, State: StateCardDead, Reason: ReasonDoorDown}})
	}
}

func (c *Client) sorted() []*pendingCard {
	out := make([]*pendingCard, 0, len(c.pending))
	for _, p := range c.pending {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].seq < out[j-1].seq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Snapshot returns the pending cards in arrival order, with ages
// advanced to now.
func (c *Client) Snapshot() []Card {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make([]Card, 0, len(c.pending))
	for _, p := range c.sorted() {
		card := p.card
		el := int(now.Sub(p.at).Seconds())
		card.AgeS += el
		card.ExpiresInS = max(card.ExpiresInS-el, 0)
		out = append(out, card)
	}
	return out
}

// Subscribe registers f for card/gone events, delivered in order from
// a dedicated dispatcher goroutine (so f may block or call Resolve
// without stalling the door reader). Exactly one of card/gone is
// non-nil per call. The returned func unsubscribes.
func (c *Client) Subscribe(f func(card *Card, gone *Gone)) (cancel func()) {
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = f
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

func (c *Client) queueLocked(e event) {
	c.evq = append(c.evq, e)
	select {
	case c.evSig <- struct{}{}:
	default:
	}
}

func (c *Client) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.evSig:
		}
		for {
			c.mu.Lock()
			if len(c.evq) == 0 {
				c.mu.Unlock()
				break
			}
			e := c.evq[0]
			c.evq = c.evq[1:]
			subs := make([]func(*Card, *Gone), 0, len(c.subs))
			for _, f := range c.subs {
				subs = append(subs, f)
			}
			c.mu.Unlock()
			for _, f := range subs {
				f(e.card, e.gone)
			}
		}
	}
}

// Resolve settles a card from the web surface (via web).
func (c *Client) Resolve(id, verdict string) (string, error) {
	return c.ResolveVia(id, verdict, ViaWeb)
}

// ResolveVia settles a card, attributing via ("web" or "telegram").
// It returns the synchronous door ack mapped to a state or sentinel.
func (c *Client) ResolveVia(id, verdict, via string) (string, error) {
	if via != ViaWeb && via != ViaTelegram {
		return "", fmt.Errorf("custos: unknown via %q", via)
	}
	if !idRe.MatchString(id) {
		return "", ErrNoSuchCard
	}
	var verb, state string
	switch verdict {
	case VerdictOnce, VerdictAlways:
		verb, state = "APPROVE "+id+" "+verdict+" via "+via+"\n", StateApproved
	case VerdictDeny:
		verb, state = "DENY "+id+" via "+via+"\n", StateDenied
	default:
		return "", fmt.Errorf("custos: unknown verdict %q", verdict)
	}

	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return "", ErrDoorDown
	}
	select { // discard any stray ack from an earlier exchange
	case <-s.ack:
	default:
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write([]byte(verb)); err != nil {
		s.conn.Close()
		return "", fmt.Errorf("%w: %v", ErrDoorDown, err)
	}
	t := time.NewTimer(c.opts.AckTimeout)
	defer t.Stop()
	select {
	case line := <-s.ack:
		return mapAck(line, state)
	case <-s.dead:
		return "", ErrDoorDown
	case <-t.C:
		// A late ack would poison the next exchange: drop the link
		// and let the reconnect path rebuild a clean one.
		s.conn.Close()
		return "", errors.New("custos: door ack timed out")
	}
}

func mapAck(line, okState string) (string, error) {
	if line == "OK" || strings.HasPrefix(line, "OK ") {
		return okState, nil
	}
	switch strings.TrimPrefix(line, "ERR ") {
	case "already_answered":
		return "", ErrCardAnswered
	case "stale_verdict":
		return "", ErrStaleVerdict
	case "locked":
		return "", ErrLocked
	case "no_such_card":
		return "", ErrNoSuchCard
	case "ip_ask_only":
		return "", ErrIPAskOnly
	case "bad_source":
		return "", errors.New("custos: door refused verb source (bad_source)")
	}
	return "", fmt.Errorf("custos: door error %q", truncate(line))
}
