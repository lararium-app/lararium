package nuntius

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type custodyHarness struct {
	b    *Bridge
	fake *Fake

	mu    sync.Mutex
	sink  func(*CustodyCard, *CustodyGone)
	asked []string
	reply func(id, verdict string) (string, string)
}

func newCustodyHarness(t *testing.T) *custodyHarness {
	t.Helper()
	home := t.TempDir()
	sf, err := NewStateFile(home + "/nuntius")
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Save(State{ActiveSession: "main", OwnerChat: "42"}); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{}
	b, err := New(Deps{Cfg: Config{Enabled: true}, Home: home, TG: fake, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	h := &custodyHarness{b: b, fake: fake}
	h.reply = func(string, string) (string, string) { return "approved", CustodyOK }
	b.WireCustody(CustodyWire{
		Subscribe: func(f func(*CustodyCard, *CustodyGone)) func() {
			h.sink = f
			return func() {}
		},
		Resolve: func(id, verdict string) (string, string) {
			h.mu.Lock()
			h.asked = append(h.asked, id+" "+verdict)
			r := h.reply
			h.mu.Unlock()
			return r(id, verdict)
		},
	})
	t.Cleanup(b.Stop)
	return h
}

func (h *custodyHarness) waitSent(t *testing.T, n int) FakeMsg {
	t.Helper()
	for range 400 {
		h.fake.mu.Lock()
		if len(h.fake.Sent) >= n {
			m := h.fake.Sent[n-1]
			h.fake.mu.Unlock()
			return m
		}
		h.fake.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("send %d never happened", n)
	return FakeMsg{}
}

func (h *custodyHarness) waitEdit(t *testing.T, n int) FakeEdit {
	t.Helper()
	for range 400 {
		h.fake.mu.Lock()
		if len(h.fake.Edits) >= n {
			e := h.fake.Edits[n-1]
			h.fake.mu.Unlock()
			return e
		}
		h.fake.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("edit %d never happened", n)
	return FakeEdit{}
}

func (h *custodyHarness) tap(t *testing.T, data string) string {
	t.Helper()
	u := Update{UpdateID: 9, CallbackQuery: &CallbackQuery{ID: "cb1", Data: data}}
	h.b.handleCallback(context.Background(), u, Record{UpdateID: 9, Kind: KindCallback, Payload: data})
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.Toasts[len(h.fake.Toasts)-1].Text
}

func buttons(kb *Keyboard) []string {
	var out []string
	for _, b := range kb.Buttons[0] {
		out = append(out, b.Text+"="+b.CallbackData)
	}
	return out
}

func TestCustodyCardRendersAndSettles(t *testing.T) {
	h := newCustodyHarness(t)
	h.sink(&CustodyCard{ID: "c1", Cell: "main", Cred: "gmail", Dest: "mail.example.com:443", Tool: "send", Review: "digest abc", ExpiresInS: 60}, nil)
	m := h.waitSent(t, 1)
	for _, want := range []string{"gmail", "mail.example.com:443", "send", "digest abc"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("card text %q lacks %q", m.Text, want)
		}
	}
	got := strings.Join(buttons(m.Keyboard), ",")
	if got != "Allow once=cu:c1:o,Always=cu:c1:a,Deny=cu:c1:d" {
		t.Fatalf("buttons = %s", got)
	}

	if toast := h.tap(t, "cu:c1:a"); toast != ToastAllowed {
		t.Fatalf("toast = %q", toast)
	}
	if h.asked[0] != "c1 always" {
		t.Fatalf("resolve = %v", h.asked)
	}

	h.sink(nil, &CustodyGone{ID: "c1", State: "approved", Reason: "telegram"})
	e := h.waitEdit(t, 1)
	if e.Keyboard != nil || !strings.HasSuffix(e.Text, "Allowed ✓ (telegram)") {
		t.Fatalf("edit = %+v", e)
	}
}

func TestCustodyIPDestHasNoAlways(t *testing.T) {
	h := newCustodyHarness(t)
	h.sink(&CustodyCard{ID: "c2", Cred: "k", Dest: "203.0.113.7:443"}, nil)
	m := h.waitSent(t, 1)
	if got := strings.Join(buttons(m.Keyboard), ","); got != "Allow once=cu:c2:o,Deny=cu:c2:d" {
		t.Fatalf("buttons = %s", got)
	}
}

func TestCustodyAckErrorsToastAndNeverCrash(t *testing.T) {
	h := newCustodyHarness(t)
	h.sink(&CustodyCard{ID: "c3", Cred: "k"}, nil)
	h.waitSent(t, 1)
	cases := map[string]string{
		CustodyAlreadyAnswered: ToastAlready,
		CustodyStale:           ToastCustodyExpired,
		CustodyLocked:          ToastCustodyLocked,
		CustodyIPAskOnly:       ToastCustodyIPAskOnly,
		CustodyUnavailable:     ToastCustodyUnavailable,
	}
	for code, want := range cases {
		h.mu.Lock()
		h.reply = func(string, string) (string, string) { return "", code }
		h.mu.Unlock()
		if got := h.tap(t, "cu:c3:o"); got != want {
			t.Fatalf("%s: toast %q, want %q", code, got, want)
		}
	}
	// already_answered stripped the buttons once; no_such_card drops the card.
	h.mu.Lock()
	h.reply = func(string, string) (string, string) { return "", CustodyNoSuchCard }
	h.mu.Unlock()
	if got := h.tap(t, "cu:c3:d"); got != ToastNotPending {
		t.Fatalf("toast %q", got)
	}
	for _, e := range h.fake.Edits {
		if e.Keyboard != nil {
			t.Fatalf("edit kept keyboard: %+v", e)
		}
	}
}

func TestCustodyCancelAndDeadEdits(t *testing.T) {
	h := newCustodyHarness(t)
	h.sink(&CustodyCard{ID: "c4", Cred: "k"}, nil)
	h.sink(&CustodyCard{ID: "c5", Cred: "k"}, nil)
	h.waitSent(t, 2)
	h.sink(nil, &CustodyGone{ID: "c4", State: "cancelled", Reason: "credential_revoked"})
	h.sink(nil, &CustodyGone{ID: "c5", State: "card_dead", Reason: "door_down"})
	e1, e2 := h.waitEdit(t, 1), h.waitEdit(t, 2)
	if !strings.HasSuffix(e1.Text, "Cancelled — credential_revoked") || e1.Keyboard != nil {
		t.Fatalf("cancel edit = %+v", e1)
	}
	if !strings.HasSuffix(e2.Text, "Unavailable — custody link lost") || e2.Keyboard != nil {
		t.Fatalf("dead edit = %+v", e2)
	}
}

func TestCustodyForeignCallbackRejected(t *testing.T) {
	h := newCustodyHarness(t)
	for _, d := range []string{"cu:", "cu:x y:o", "cu:c1:z", "cu::o"} {
		if got := h.tap(t, d); got != ToastNotPending {
			t.Fatalf("%q toast %q", d, got)
		}
	}
	if len(h.asked) != 0 {
		t.Fatalf("resolve called for junk: %v", h.asked)
	}
}

func TestRecordForAcceptsCustodyCallbacks(t *testing.T) {
	u := Update{UpdateID: 1, CallbackQuery: &CallbackQuery{Data: "cu:c1:o"}}
	if _, ok := recordFor(u); !ok {
		t.Fatal("cu: callback dropped before the inbox")
	}
}
