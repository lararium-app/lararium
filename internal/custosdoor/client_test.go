package custosdoor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeConn is one accepted door connection; lines the client sent are
// delivered on rx.
type fakeConn struct {
	c  net.Conn
	rx chan string
}

func (f *fakeConn) send(s string) { _, _ = io.WriteString(f.c, s+"\n") }

func (f *fakeConn) recv(t *testing.T) string {
	t.Helper()
	select {
	case l := <-f.rx:
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client line")
		return ""
	}
}

type fakeServer struct {
	sock  string
	ln    net.Listener
	conns chan *fakeConn
}

func newFakeServer(t *testing.T, sock string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{sock: sock, ln: ln, conns: make(chan *fakeConn, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc := &fakeConn{c: c, rx: make(chan string, 64)}
			go func() {
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					fc.rx <- sc.Text()
				}
				close(fc.rx)
			}()
			s.conns <- fc
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeServer) accept(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no client connection")
		return nil
	}
}

// hello performs the server side of a HELLO with the given snapshot.
func (s *fakeServer) hello(t *testing.T, snap string) *fakeConn {
	t.Helper()
	fc := s.accept(t)
	if got, want := fc.recv(t), "HELLO "+testToken+" hearthd"; got != want {
		t.Fatalf("HELLO = %q, want %q", got, want)
	}
	fc.send("OK " + snap)
	return fc
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cd") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if err != nil {
		d = t.TempDir()
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// hello then wait for the client to finish attaching.
func (h *harness) up(t *testing.T, snap string) *fakeConn {
	t.Helper()
	fc := h.srv.hello(t, snap)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.c.mu.Lock()
		ok := h.c.sess != nil
		h.c.mu.Unlock()
		if ok {
			return fc
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("client never attached")
	return nil
}

func tokenFile(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "door.token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type rec struct {
	cards chan Card
	gones chan Gone
}

func subscribe(c *Client) *rec {
	r := &rec{cards: make(chan Card, 32), gones: make(chan Gone, 32)}
	c.Subscribe(func(card *Card, gone *Gone) {
		if card != nil {
			r.cards <- *card
		}
		if gone != nil {
			r.gones <- *gone
		}
	})
	return r
}

func (r *rec) card(t *testing.T) Card {
	t.Helper()
	select {
	case c := <-r.cards:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no card event")
		return Card{}
	}
}

func (r *rec) gone(t *testing.T) Gone {
	t.Helper()
	select {
	case g := <-r.gones:
		return g
	case <-time.After(2 * time.Second):
		t.Fatal("no gone event")
		return Gone{}
	}
}

type harness struct {
	srv     *fakeServer
	c       *Client
	cancel  context.CancelFunc
	retries chan time.Duration
}

func start(t *testing.T, tokenContent string) *harness {
	t.Helper()
	dir := shortDir(t)
	sock := filepath.Join(dir, "doors.sock")
	srv := newFakeServer(t, sock)
	return startAt(t, dir, sock, srv, tokenContent)
}

func startAt(t *testing.T, dir, sock string, srv *fakeServer, tokenContent string) *harness {
	t.Helper()
	h := &harness{srv: srv, retries: make(chan time.Duration, 64)}
	c, err := New(Options{
		SockPath:  sock,
		TokenPath: tokenFile(t, dir, tokenContent),
		Log:       log.New(io.Discard, "", 0),
		BaseDelay: 5 * time.Millisecond,
		MaxDelay:  20 * time.Millisecond,
		OnRetry:   func(_ int, d time.Duration, _ error) { h.retries <- d },
	})
	if err != nil || c == nil {
		t.Fatalf("New: %v %v", c, err)
	}
	h.c = c
	return h
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.c.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.c.Done():
		case <-time.After(2 * time.Second):
			t.Error("Run did not stop on cancel")
		}
	})
}

const card1 = `{"id":"c1","cell":"main","cred":"gmail","dest":"mail.example.com:443","tool":"","review":"digest abc","age_s":3,"expires_in_s":60}`

func TestHelloSeedsSnapshotAndTrimsToken(t *testing.T) {
	h := start(t, "  "+testToken+"\n")
	r := subscribe(h.c)
	h.run(t)
	h.srv.hello(t, "["+card1+"]")
	got := r.card(t)
	if got.ID != "c1" || got.Cred != "gmail" || got.Dest != "mail.example.com:443" || got.Review != "digest abc" {
		t.Fatalf("card = %+v", got)
	}
	snap := h.c.Snapshot()
	if len(snap) != 1 || snap[0].ID != "c1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].AgeS < 3 || snap[0].ExpiresInS > 60 {
		t.Fatalf("ages not carried: %+v", snap[0])
	}
}

func TestCardAndGoneFrames(t *testing.T) {
	h := start(t, testToken)
	r := subscribe(h.c)
	h.run(t)
	fc := h.up(t, "[]")
	fc.send("CARD " + card1)
	if r.card(t).ID != "c1" {
		t.Fatal("card id")
	}
	fc.send(`GONE {"id":"c1","state":"cancelled","reason":"flow_gone"}`)
	g := r.gone(t)
	if g.ID != "c1" || g.State != "cancelled" || g.Reason != "flow_gone" {
		t.Fatalf("gone = %+v", g)
	}
	if len(h.c.Snapshot()) != 0 {
		t.Fatal("card not removed")
	}
}

func TestResolveVerbsExact(t *testing.T) {
	cases := []struct {
		verdict, via, want, state string
	}{
		{"once", "web", "APPROVE c1 once via web", "approved"},
		{"always", "web", "APPROVE c1 always via web", "approved"},
		{"deny", "web", "DENY c1 via web", "denied"},
		{"once", "telegram", "APPROVE c1 once via telegram", "approved"},
		{"always", "telegram", "APPROVE c1 always via telegram", "approved"},
		{"deny", "telegram", "DENY c1 via telegram", "denied"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			h := start(t, testToken)
			h.run(t)
			fc := h.up(t, "["+card1+"]")
			var state string
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				if tc.via == "web" {
					state, err = h.c.Resolve("c1", tc.verdict)
				} else {
					state, err = h.c.ResolveVia("c1", tc.verdict, tc.via)
				}
			}()
			if got := fc.recv(t); got != tc.want {
				t.Fatalf("verb = %q, want %q", got, tc.want)
			}
			fc.send("OK")
			<-done
			if err != nil || state != tc.state {
				t.Fatalf("state=%q err=%v", state, err)
			}
		})
	}
}

func TestResolveAckErrors(t *testing.T) {
	cases := map[string]error{
		"already_answered": ErrCardAnswered,
		"stale_verdict":    ErrStaleVerdict,
		"locked":           ErrLocked,
		"no_such_card":     ErrNoSuchCard,
		"ip_ask_only":      ErrIPAskOnly,
	}
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "[]")
	for code, want := range cases {
		go func() {
			fc.recv(t)
			fc.send("ERR " + code)
		}()
		_, err := h.c.Resolve("c1", "once")
		if !errors.Is(err, want) {
			t.Fatalf("ERR %s -> %v, want %v", code, err, want)
		}
	}
	go func() { fc.recv(t); fc.send("ERR bad_source") }()
	_, err := h.c.Resolve("c1", "once")
	if err == nil || strings.Contains(err.Error(), "already") {
		t.Fatalf("bad_source err = %v", err)
	}
}

func TestResolveRejectsBadInput(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	h.up(t, "[]")
	if _, err := h.c.Resolve("c1\nDENY x via web", "once"); !errors.Is(err, ErrNoSuchCard) {
		t.Fatalf("injection id: %v", err)
	}
	if _, err := h.c.Resolve("c1", "maybe"); err == nil {
		t.Fatal("bad verdict accepted")
	}
	if _, err := h.c.ResolveVia("c1", "once", "cli"); err == nil {
		t.Fatal("bad via accepted")
	}
}

func TestResolveWhileDown(t *testing.T) {
	h := start(t, testToken)
	if _, err := h.c.Resolve("c1", "once"); !errors.Is(err, ErrDoorDown) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSerialized(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "[]")
	type res struct {
		id    string
		state string
		err   error
	}
	out := make(chan res, 2)
	for _, v := range []struct{ id, verdict string }{{"a1", "once"}, {"b2", "deny"}} {
		go func() {
			s, err := h.c.Resolve(v.id, v.verdict)
			out <- res{v.id, s, err}
		}()
	}
	// First verb arrives alone; the second must wait for its ack.
	first := fc.recv(t)
	select {
	case l := <-fc.rx:
		t.Fatalf("second verb %q sent before first ack", l)
	case <-time.After(50 * time.Millisecond):
	}
	ackFor := func(l string) string {
		if strings.HasPrefix(l, "APPROVE a1 once via web") {
			return "OK"
		}
		return "ERR already_answered"
	}
	fc.send(ackFor(first))
	second := fc.recv(t)
	if second == first {
		t.Fatal("same verb twice")
	}
	fc.send(ackFor(second))
	for range 2 {
		r := <-out
		switch r.id {
		case "a1":
			if r.err != nil || r.state != "approved" {
				t.Fatalf("a1: %+v", r)
			}
		case "b2":
			if !errors.Is(r.err, ErrCardAnswered) {
				t.Fatalf("b2: %+v", r)
			}
		}
	}
}

func TestStreamFramesDuringVerbDoNotBreakAck(t *testing.T) {
	h := start(t, testToken)
	r := subscribe(h.c)
	h.run(t)
	fc := h.up(t, "[]")
	done := make(chan error, 1)
	go func() { _, err := h.c.Resolve("c1", "once"); done <- err }()
	fc.recv(t)
	fc.send("CARD " + card1)
	fc.send("OK")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.card(t).ID != "c1" {
		t.Fatal("card lost while verb in flight")
	}
}

func TestDropKillsCardsThenReconnectRefreshes(t *testing.T) {
	h := start(t, testToken)
	r := subscribe(h.c)
	h.run(t)
	fc := h.srv.hello(t, "["+card1+"]")
	r.card(t)
	fc.c.Close()

	g := r.gone(t)
	if g.ID != "c1" || g.State != "card_dead" || g.Reason != "door_down" {
		t.Fatalf("gone = %+v", g)
	}
	if len(h.c.Snapshot()) != 0 {
		t.Fatal("pending not cleared on drop")
	}
	if _, err := h.c.Resolve("c1", "once"); !errors.Is(err, ErrDoorDown) {
		t.Fatalf("resolve while down: %v", err)
	}

	card2 := strings.Replace(card1, `"c1"`, `"c2"`, 1)
	h.srv.hello(t, "["+card2+"]")
	if got := r.card(t); got.ID != "c2" {
		t.Fatalf("refresh card = %+v", got)
	}
	snap := h.c.Snapshot()
	if len(snap) != 1 || snap[0].ID != "c2" {
		t.Fatalf("snapshot after refresh = %+v", snap)
	}
}

func TestBackoffWhileServerDownThenAttach(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "doors.sock")
	h := startAt(t, dir, sock, &fakeServer{}, testToken)
	h.run(t)

	var delays []time.Duration
	for range 4 {
		select {
		case d := <-h.retries:
			delays = append(delays, d)
		case <-time.After(2 * time.Second):
			t.Fatal("no retry observed")
		}
	}
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	for i, d := range delays {
		if d != want[i] {
			t.Fatalf("delays = %v, want 5,10,20,20ms (doubling, capped)", delays)
		}
	}

	srv := newFakeServer(t, sock)
	srv.hello(t, "[]")
	// A fresh failure series restarts at the base delay.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-h.retries:
			continue
		case <-deadline:
			t.Fatal("never attached")
		default:
		}
		h.c.mu.Lock()
		up := h.c.sess != nil
		h.c.mu.Unlock()
		if up {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBadTokenBacksOffWithoutSpin(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	for i := range 3 {
		fc := h.srv.accept(t)
		fc.recv(t)
		fc.send("ERR bad_token")
		fc.c.Close()
		select {
		case <-h.retries:
		case <-time.After(2 * time.Second):
			t.Fatalf("attempt %d: no retry", i)
		}
	}
	if len(h.c.Snapshot()) != 0 {
		t.Fatal("snapshot not empty")
	}
}

func TestUnsetConfigIsDisabled(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "doors.sock")
	c, err := New(Options{})
	if c != nil || err != nil {
		t.Fatalf("New(unset) = %v, %v", c, err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket path touched: %v", err)
	}
	if _, err := New(Options{SockPath: sock}); err == nil {
		t.Fatal("half-set config accepted")
	}
	if _, err := New(Options{SockPath: sock, TokenPath: "x", Name: "Bad Name"}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestCustomNameInHello(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "doors.sock")
	srv := newFakeServer(t, sock)
	c, err := New(Options{SockPath: sock, TokenPath: tokenFile(t, dir, testToken), Name: "web-2", Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	fc := srv.accept(t)
	if got := fc.recv(t); got != "HELLO "+testToken+" web-2" {
		t.Fatalf("HELLO = %q", got)
	}
}

func TestAttachAtomicity(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.srv.hello(t, "["+card1+"]")

	deadline := time.Now().Add(2 * time.Second)
	for len(h.c.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("snapshot not populated")
		}
		time.Sleep(time.Millisecond)
	}

	cards, cancel, events := h.c.Attach()
	defer cancel()

	if len(cards) != 1 || cards[0].ID != "c1" {
		t.Fatalf("snapshot cards = %+v, want 1 card with c1", cards)
	}

	card2 := strings.Replace(card1, `"c1"`, `"c2"`, 1)
	card3 := strings.Replace(card1, `"c1"`, `"c3"`, 1)

	fc.send("CARD " + card2)
	fc.send(`GONE {"id":"c1","state":"approved","reason":"web"}`)
	fc.send("CARD " + card3)

	var gotEvents []Event
	for len(gotEvents) < 3 {
		select {
		case ev := <-events:
			gotEvents = append(gotEvents, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for events, got %d", len(gotEvents))
		}
	}

	if gotEvents[0].Card == nil || gotEvents[0].Card.ID != "c2" {
		t.Fatalf("event 0 want card c2, got %+v", gotEvents[0])
	}
	if gotEvents[1].Gone == nil || gotEvents[1].Gone.ID != "c1" || gotEvents[1].Gone.State != "approved" {
		t.Fatalf("event 1 want gone c1 approved, got %+v", gotEvents[1])
	}
	if gotEvents[2].Card == nil || gotEvents[2].Card.ID != "c3" {
		t.Fatalf("event 2 want card c3, got %+v", gotEvents[2])
	}

	snap := h.c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (c2 and c3)", len(snap))
	}
}

func TestAttachOverflowDropToDead(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "[]")

	_, fastCancel, fastEvents := h.c.Attach()
	defer fastCancel()

	_, slowCancel, slowEvents := h.c.Attach()
	defer slowCancel()

	fastReceived := make(chan Event, 100)
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		for ev := range fastEvents {
			fastReceived <- ev
			if len(fastReceived) == 70 {
				return
			}
		}
	}()

	for i := range 70 {
		cid := fmt.Sprintf("ov_%d", i)
		fc.send(fmt.Sprintf(`CARD {"id":%q,"cell":"main","cred":"gmail","dest":"x","tool":"","review":"r","age_s":0,"expires_in_s":60}`, cid))
	}

	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("fast consumer timed out, got %d/70 events", len(fastReceived))
	}

	for i := range 64 {
		select {
		case ev, ok := <-slowEvents:
			if !ok {
				t.Fatalf("slowEvents closed prematurely at index %d", i)
			}
			if ev.Card == nil {
				t.Fatalf("expected card event at %d", i)
			}
		case <-time.After(5 * time.Second): // generous: shared runners stall goroutines
			t.Fatalf("slow consumer failed reading buffered event %d", i)
		}
	}

	select {
	case ev, ok := <-slowEvents:
		if ok {
			t.Fatalf("slowEvents still open after 64 events: got %+v", ev)
		}
	case <-time.After(5 * time.Second): // generous: shared runners stall goroutines
		t.Fatal("slowEvents not closed after overflow")
	}

	fc.send(`CARD {"id":"post_overflow","cell":"main","cred":"gmail","dest":"x","tool":"","review":"r","age_s":0,"expires_in_s":60}`)
	select {
	case ev := <-fastEvents:
		if ev.Card == nil || ev.Card.ID != "post_overflow" {
			t.Fatalf("fast consumer got %+v, want post_overflow", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("fast consumer did not receive post-overflow card")
	}
}

func TestAttachIdempotentCancel(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "[]")

	_, cancel, events := h.c.Attach()

	h.c.mu.Lock()
	subCount := len(h.c.subs)
	h.c.mu.Unlock()
	if subCount == 0 {
		t.Fatal("subscriber not registered")
	}

	cancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("events channel not closed on cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for events channel close")
	}

	h.c.mu.Lock()
	afterCount := len(h.c.subs)
	h.c.mu.Unlock()
	if afterCount != 0 {
		t.Fatalf("subscribers remaining = %d, want 0", afterCount)
	}

	cancel()
	cancel()

	fc.c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := h.c.Resolve("c1", "once"); errors.Is(err, ErrDoorDown) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client did not notice door drop")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, cancel2, events2 := h.c.Attach()
	cancel2()
	cancel2()
	select {
	case _, ok := <-events2:
		if ok {
			t.Fatal("events2 not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("events2 not closed on cancel after door drop")
	}
}
