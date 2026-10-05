package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// safeBuffer captures bridge logs concurrently without race detector warnings.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// make429Error simulates the typed rate-limit error that BotAPI maps
// (botapi.go: apiError with status 429 and retryAfter seconds).
func make429Error(retryAfterSec int) error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"ok":false,"description":"too many requests","parameters":{"retry_after":%d}}`, retryAfterSec)
	}))
	defer srv.Close()

	api := nuntius.NewBotAPI("fake-token", nil)
	rf := reflect.ValueOf(api).Elem().FieldByName("base")
	reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem().SetString(srv.URL)

	_, err := api.GetUpdates(context.Background(), 0)
	return err
}

type fakeSentMsg struct {
	Time     time.Time
	ChatID   string
	Text     string
	Keyboard *nuntius.Keyboard
	MsgID    int64
}

type fakeEditMsg struct {
	Time      time.Time
	ChatID    string
	MessageID int64
	Text      string
	Keyboard  *nuntius.Keyboard
}

type fakeToastMsg struct {
	Time       time.Time
	CallbackID string
	Text       string
}

type fakeActionMsg struct {
	Time   time.Time
	ChatID string
	Action string
}

type fakeDeleteMsg struct {
	Time      time.Time
	ChatID    string
	MessageID int64
}

// fakeTransport implements nuntius.Transport for the in-process V-suite tests.
// Inbound updates are fed via inbound channel; outbound calls are recorded.
type fakeTransport struct {
	mu          sync.Mutex
	inbound     chan []nuntius.Update
	pollTimeout time.Duration

	sent    []fakeSentMsg
	edits   []fakeEditMsg
	toasts  []fakeToastMsg
	actions []fakeActionMsg
	deleted []fakeDeleteMsg

	pollCount int
	pollTimes []time.Time

	sendErrHook func(chatID, text string, kb *nuntius.Keyboard) error
	editErrHook func(chatID string, msgID int64, text string, kb *nuntius.Keyboard) error

	sendNotify  chan struct{}
	editNotify  chan struct{}
	toastNotify chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		inbound:     make(chan []nuntius.Update, 100),
		pollTimeout: 50 * time.Millisecond,
		sendNotify:  make(chan struct{}, 100),
		editNotify:  make(chan struct{}, 100),
		toastNotify: make(chan struct{}, 100),
	}
}

func (t *fakeTransport) SendMessage(ctx context.Context, chatID, text string, kb *nuntius.Keyboard) (*nuntius.Message, error) {
	t.mu.Lock()
	if t.sendErrHook != nil {
		if err := t.sendErrHook(chatID, text, kb); err != nil {
			t.mu.Unlock()
			return nil, err
		}
	}
	msgID := int64(len(t.sent) + 1)
	m := fakeSentMsg{
		Time:     time.Now(),
		ChatID:   chatID,
		Text:     text,
		Keyboard: kb,
		MsgID:    msgID,
	}
	t.sent = append(t.sent, m)
	select {
	case t.sendNotify <- struct{}{}:
	default:
	}
	t.mu.Unlock()
	return &nuntius.Message{
		MessageID: msgID,
		Chat:      nuntius.Chat{ID: chatID, Type: "private"},
		Text:      text,
	}, nil
}

func (t *fakeTransport) EditMessageText(ctx context.Context, chatID string, messageID int64, text string, kb *nuntius.Keyboard) error {
	t.mu.Lock()
	if t.editErrHook != nil {
		if err := t.editErrHook(chatID, messageID, text, kb); err != nil {
			t.mu.Unlock()
			return err
		}
	}
	e := fakeEditMsg{
		Time:      time.Now(),
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
		Keyboard:  kb,
	}
	t.edits = append(t.edits, e)
	select {
	case t.editNotify <- struct{}{}:
	default:
	}
	t.mu.Unlock()
	return nil
}

func (t *fakeTransport) AnswerCallback(ctx context.Context, callbackID, text string) error {
	t.mu.Lock()
	m := fakeToastMsg{
		Time:       time.Now(),
		CallbackID: callbackID,
		Text:       text,
	}
	t.toasts = append(t.toasts, m)
	select {
	case t.toastNotify <- struct{}{}:
	default:
	}
	t.mu.Unlock()
	return nil
}

func (t *fakeTransport) DeleteMessageReplyMarkup(ctx context.Context, chatID string, messageID int64) error {
	t.mu.Lock()
	t.deleted = append(t.deleted, fakeDeleteMsg{
		Time:      time.Now(),
		ChatID:    chatID,
		MessageID: messageID,
	})
	t.mu.Unlock()
	return nil
}

func (t *fakeTransport) SendChatAction(ctx context.Context, chatID, action string) error {
	t.mu.Lock()
	t.actions = append(t.actions, fakeActionMsg{
		Time:   time.Now(),
		ChatID: chatID,
		Action: action,
	})
	t.mu.Unlock()
	return nil
}

func (t *fakeTransport) GetUpdates(ctx context.Context, offset int64) ([]nuntius.Update, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case updates, ok := <-t.inbound:
		if !ok {
			return nil, ctx.Err()
		}
		t.mu.Lock()
		t.pollCount++
		t.pollTimes = append(t.pollTimes, time.Now())
		t.mu.Unlock()
		return updates, nil
	case <-time.After(t.pollTimeout):
		t.mu.Lock()
		t.pollCount++
		t.pollTimes = append(t.pollTimes, time.Now())
		t.mu.Unlock()
		return nil, nil
	}
}

func (t *fakeTransport) SendUpdate(u nuntius.Update) {
	t.inbound <- []nuntius.Update{u}
}

func (t *fakeTransport) WaitForSends(count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		n := len(t.sent)
		t.mu.Unlock()
		if n >= count {
			return nil
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return fmt.Errorf("timeout waiting for %d sends (got %d)", count, n)
		}
		select {
		case <-t.sendNotify:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (t *fakeTransport) WaitForEdits(count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		n := len(t.edits)
		t.mu.Unlock()
		if n >= count {
			return nil
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return fmt.Errorf("timeout waiting for %d edits (got %d)", count, n)
		}
		select {
		case <-t.editNotify:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (t *fakeTransport) WaitForToasts(count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		n := len(t.toasts)
		t.mu.Unlock()
		if n >= count {
			return nil
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return fmt.Errorf("timeout waiting for %d toasts (got %d)", count, n)
		}
		select {
		case <-t.toastNotify:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (t *fakeTransport) LastSent() fakeSentMsg {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sent) == 0 {
		return fakeSentMsg{}
	}
	return t.sent[len(t.sent)-1]
}

func (t *fakeTransport) LastEdit() fakeEditMsg {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.edits) == 0 {
		return fakeEditMsg{}
	}
	return t.edits[len(t.edits)-1]
}

func (t *fakeTransport) LastToast() fakeToastMsg {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.toasts) == 0 {
		return fakeToastMsg{}
	}
	return t.toasts[len(t.toasts)-1]
}

func (t *fakeTransport) SentCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sent)
}

func (t *fakeTransport) PollCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pollCount
}

func (t *fakeTransport) FinalMessageTexts(chatID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var order []int64
	byID := make(map[int64]string)
	for _, s := range t.sent {
		if s.ChatID == chatID {
			if _, exists := byID[s.MsgID]; !exists {
				order = append(order, s.MsgID)
			}
			byID[s.MsgID] = s.Text
		}
	}
	for _, e := range t.edits {
		if e.ChatID == chatID {
			byID[e.MessageID] = e.Text
		}
	}
	res := make([]string, len(order))
	for i, id := range order {
		res[i] = byID[id]
	}
	return res
}

func makeMsgUpdate(updateID int64, fromID, chatID, chatType, text string) nuntius.Update {
	return nuntius.Update{
		UpdateID: updateID,
		Message: &nuntius.Message{
			MessageID: updateID * 10,
			From:      &nuntius.User{ID: fromID, FirstName: "Tester"},
			Chat:      nuntius.Chat{ID: chatID, Type: chatType},
			Text:      text,
		},
	}
}

//nolint:unparam // signature matches Telegram update schema
func makeCallbackUpdate(updateID int64, fromID, chatID, chatType, callbackID, data string) nuntius.Update {
	return nuntius.Update{
		UpdateID: updateID,
		CallbackQuery: &nuntius.CallbackQuery{
			ID:   callbackID,
			From: nuntius.User{ID: fromID, FirstName: "Tester"},
			Message: nuntius.Message{
				MessageID: 100,
				Chat:      nuntius.Chat{ID: chatID, Type: chatType},
			},
			Data: data,
		},
	}
}

func seedPairedOwner(t *testing.T, home, ownerID, chatID string) {
	t.Helper()
	ndir := filepath.Join(home, "nuntius")
	ps, err := nuntius.NewPairStore(ndir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := ps.Mint(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Redeem(ownerID, code, time.Now()); err != nil {
		t.Fatal(err)
	}
	sf, err := nuntius.NewStateFile(ndir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Save(nuntius.State{ActiveSession: "main", OwnerChat: chatID}); err != nil {
		t.Fatal(err)
	}
}

func setBridgeEditInterval(b *nuntius.Bridge, d time.Duration) {
	rf := reflect.ValueOf(b).Elem().FieldByName("cfg")
	field := rf.FieldByName("EditInterval")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(d))
}

type vsuiteOpts struct {
	cfg      nuntius.Config
	provider router.Provider
	tools    []loop.Tool
	ownerID  string
	chatID   string
}

type vsuiteEnv struct {
	hub     *surface.Hub
	ap      *surface.ApprovalHub
	home    string
	bridge  *nuntius.Bridge
	tg      *fakeTransport
	logBuf  *safeBuffer
	cancel  context.CancelFunc
	engine  *recordingEngine
	wireDep nuntius.WireDeps
}

type recordingEngine struct {
	inner nuntius.Engine
	calls int
	mu    sync.Mutex
}

func (r *recordingEngine) RunTurn(ctx context.Context, req nuntius.TurnReq) nuntius.TurnResult {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.inner.RunTurn(ctx, req)
}

func (r *recordingEngine) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func setupHubWithTools(t *testing.T, sp router.Provider, tools []loop.Tool, approvalTimeout time.Duration) (*surface.Hub, *surface.ApprovalHub, string) {
	t.Helper()
	dir := t.TempDir()

	if _, err := penatus.CreateSession(dir, "main", "main"); err != nil {
		t.Fatal(err)
	}
	mainDir := filepath.Join(dir, "sessions", "main")
	logFile, err := penatus.OpenLog(mainDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = logFile.Append(penatus.Event{
		T:  "msg",
		TS: time.Now().UTC().Format(time.RFC3339),
		Fields: map[string]json.RawMessage{
			"role":  json.RawMessage(`"assistant"`),
			"text":  json.RawMessage(`"system prompt"`),
			"model": json.RawMessage(`"test"`),
			"usage": json.RawMessage(`{"in":10,"out":5}`),
		},
	})
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})

	if approvalTimeout <= 0 {
		approvalTimeout = 5 * time.Second
	}
	cfg := surface.ServeConfig{
		ApprovalTimeout: approvalTimeout,
		TurnTimeout:     10 * time.Second,
	}
	ap := surface.NewApprovalHub(cfg.ApprovalTimeout)
	hub := surface.NewHub(cfg, dir, 1000, 80, tools, rt, ap)
	return hub, ap, dir
}

func setupVSuite(t *testing.T, opts vsuiteOpts) *vsuiteEnv {
	t.Helper()
	if opts.provider == nil {
		opts.provider = &fakeProvider{name: "fake", streams: []string{"ok"}}
	}
	hub, ap, home := setupHubWithTools(t, opts.provider, opts.tools, opts.cfg.ApprovalTimeout)

	if opts.ownerID != "" {
		seedPairedOwner(t, home, opts.ownerID, opts.chatID)
	}

	source := surface.NewPenatusSource(home, hub)
	sess := newSessionsAdapter(source)
	wire := nuntiusWire(hub, ap, "prov/fake-model")
	recEngine := &recordingEngine{inner: newEngineAdapter(hub)}
	fakeTG := newFakeTransport()
	logBuf := &safeBuffer{}
	logger := log.New(logBuf, "", 0)

	cfg := opts.cfg
	cfg.Enabled = true
	cfg.BotTokenEnv = "FAKE_BOT_TOKEN_ENV"

	targetEditInterval := cfg.EditInterval

	deps := nuntius.Deps{
		Cfg:        cfg,
		Home:       home,
		TG:         fakeTG,
		Engine:     recEngine,
		Sess:       sess,
		SessionDir: hub.SessionDir,
		Wire:       wire,
		Log:        logger,
	}

	b, err := nuntius.New(deps)
	if err != nil {
		t.Fatalf("nuntius.New failed: %v", err)
	}
	if targetEditInterval > 0 && targetEditInterval < time.Second {
		setBridgeEditInterval(b, targetEditInterval)
	}

	b.Wire(wire)

	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx, "fake-bot-token-12345")

	t.Cleanup(func() {
		b.Stop()
		cancel()
	})

	return &vsuiteEnv{
		hub:     hub,
		ap:      ap,
		home:    home,
		bridge:  b,
		tg:      fakeTG,
		logBuf:  logBuf,
		cancel:  cancel,
		engine:  recEngine,
		wireDep: wire,
	}
}

// -----------------------------------------------------------------------
// V1. Pairing flow
// -----------------------------------------------------------------------

func TestV1PairingFlow(t *testing.T) {
	durPtr := func(d time.Duration) *time.Duration { return &d }

	env := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			PairReplyGap: durPtr(60 * time.Second),
		},
	})

	// 1. Unpaired bot ignores prose with pairing-required line once per reply gap
	env.tg.SendUpdate(makeMsgUpdate(1, "user1", "chat1", "private", "hello"))
	if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for pairing required send: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgPairingRequired {
		t.Fatalf("got %q, want %q", text, nuntius.MsgPairingRequired)
	}

	// 2. /help sent within reply gap is throttled (zero additional send)
	env.tg.SendUpdate(makeMsgUpdate(2, "user1", "chat1", "private", "/help"))
	time.Sleep(100 * time.Millisecond)
	if cnt := env.tg.SentCount(); cnt != 1 {
		t.Fatalf("expected 1 send, got %d (/help inside gap should be throttled)", cnt)
	}

	// 3. Mint code via nuntius.NewPairStore(home/nuntius).Mint
	ps, err := nuntius.NewPairStore(filepath.Join(env.home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	code, err := ps.Mint(time.Now(), 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(code, "pair1_") {
		t.Fatalf("minted code = %q, want pair1_ prefix", code)
	}

	// 4. /pair <code> binds owner -> "paired — this bot is now yours"
	env.tg.SendUpdate(makeMsgUpdate(3, "user1", "chat1", "private", "/pair "+code))
	if err := env.tg.WaitForSends(2, 2*time.Second); err != nil {
		t.Fatalf("waiting for paired confirmation: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgPaired {
		t.Fatalf("got %q, want %q", text, nuntius.MsgPaired)
	}

	// 5. Second /pair -> "already paired"
	env.tg.SendUpdate(makeMsgUpdate(4, "user1", "chat1", "private", "/pair "+code))
	if err := env.tg.WaitForSends(3, 2*time.Second); err != nil {
		t.Fatalf("waiting for already paired send: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgAlreadyPaired {
		t.Fatalf("got %q, want %q", text, nuntius.MsgAlreadyPaired)
	}

	// 6. Check owners.json holds hash not code, file mode 0600
	ownersPath := filepath.Join(env.home, "nuntius", "owners.json")
	fi, err := os.Stat(ownersPath)
	if err != nil {
		t.Fatalf("stat owners.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("owners.json mode = %04o, want 0600", perm)
	}
	content, err := os.ReadFile(ownersPath)
	if err != nil {
		t.Fatalf("read owners.json: %v", err)
	}
	if strings.Contains(string(content), code) {
		t.Fatalf("owners.json leaks plaintext code: %s", content)
	}
	var o nuntius.Owners
	if err := json.Unmarshal(content, &o); err != nil {
		t.Fatalf("unmarshal owners.json: %v", err)
	}
	if len(o.OwnerIDs) != 1 || o.OwnerIDs[0] != "user1" {
		t.Fatalf("unexpected OwnerIDs: %v", o.OwnerIDs)
	}
	if len(o.Codes) != 0 {
		t.Fatalf("used code should be removed from codes, got %v", o.Codes)
	}
}

// -----------------------------------------------------------------------
// V2. Code hygiene
// -----------------------------------------------------------------------

func TestV2CodeHygiene(t *testing.T) {
	durPtr := func(d time.Duration) *time.Duration { return &d }

	env := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			PairCodeTTL:    1 * time.Second,
			PairReplyGap:   durPtr(0 * time.Second),
			PairFailWindow: 60 * time.Second,
			PairMute:       30 * time.Second,
		},
	})

	ps, err := nuntius.NewPairStore(filepath.Join(env.home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}

	// 1. ttl 1s expired code -> invalid code
	code, err := ps.Mint(time.Now(), 1*time.Second)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // wait for 1s TTL expiration

	env.tg.SendUpdate(makeMsgUpdate(1, "user_hygiene", "chat_h", "private", "/pair "+code))
	if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for invalid code send: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgInvalidCode {
		t.Fatalf("got %q, want %q", text, nuntius.MsgInvalidCode)
	}

	// 2. 5 wrong codes in window -> replies muted (log continues)
	for i := 2; i <= 5; i++ {
		wrong := fmt.Sprintf("/pair pair1_wrong%d", i)
		env.tg.SendUpdate(makeMsgUpdate(int64(i), "user_hygiene", "chat_h", "private", wrong))
		if err := env.tg.WaitForSends(i, 2*time.Second); err != nil {
			t.Fatalf("waiting for send %d: %v", i, err)
		}
		if text := env.tg.LastSent().Text; text != nuntius.MsgInvalidCode {
			t.Fatalf("got %q, want %q", text, nuntius.MsgInvalidCode)
		}
	}

	// 6th attempt should be muted (no send)
	env.tg.SendUpdate(makeMsgUpdate(6, "user_hygiene", "chat_h", "private", "/pair pair1_wrong6"))
	time.Sleep(150 * time.Millisecond)
	if cnt := env.tg.SentCount(); cnt != 5 {
		t.Fatalf("expected 5 sends, got %d (attempt 6 should be muted)", cnt)
	}

	// Log continues: assert bridge log captured the failed attempt
	logs := env.logBuf.String()
	if !strings.Contains(logs, "user_hygiene") && !strings.Contains(logs, "invalid code") && !strings.Contains(logs, "refused") {
		t.Fatalf("spec requires log to continue during mute, but bridge log has no record: %q", logs)
	}

	// 3. no pair1_ plaintext anywhere under home/nuntius
	ndir := filepath.Join(env.home, "nuntius")
	err = filepath.WalkDir(ndir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		content, readErr := os.ReadFile(filepath.Clean(path)) //nolint:gosec // test directory walk
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), "pair1_") {
			return fmt.Errorf("file %s contains plaintext pair1_ material: %s", path, content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("code hygiene violation: %v", err)
	}
}

// -----------------------------------------------------------------------
// V3. Gate matrix
// -----------------------------------------------------------------------

func TestV3GateMatrix(t *testing.T) {
	env := setupVSuite(t, vsuiteOpts{
		ownerID: "owner_v3",
		chatID:  "chat_owner_v3",
	})

	// 1. Foreign user's text -> static refusal ("this bot is private"), zero Engine calls, zero queue
	env.tg.SendUpdate(makeMsgUpdate(1, "stranger_1", "chat_stranger_1", "private", "hello bot"))
	if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for private refusal: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgPrivate {
		t.Fatalf("got %q, want %q", text, nuntius.MsgPrivate)
	}
	if calls := env.engine.Calls(); calls != 0 {
		t.Fatalf("engine calls = %d, want 0", calls)
	}

	// 2. Foreign user's /new -> static refusal, zero Engine calls
	env.tg.SendUpdate(makeMsgUpdate(2, "stranger_2", "chat_stranger_2", "private", "/new side"))
	if err := env.tg.WaitForSends(2, 2*time.Second); err != nil {
		t.Fatalf("waiting for private refusal: %v", err)
	}
	if text := env.tg.LastSent().Text; text != nuntius.MsgPrivate {
		t.Fatalf("got %q, want %q", text, nuntius.MsgPrivate)
	}
	if calls := env.engine.Calls(); calls != 0 {
		t.Fatalf("engine calls = %d, want 0", calls)
	}

	// 3. Owner's message in a group chat (chat.type:"group") -> dropped at gate: zero replies, zero calls, one log line
	sentBefore := env.tg.SentCount()
	logsBefore := env.logBuf.String()
	env.tg.SendUpdate(makeMsgUpdate(3, "owner_v3", "group_chat_1", "group", "hello group"))
	time.Sleep(100 * time.Millisecond)

	if sentAfter := env.tg.SentCount(); sentAfter != sentBefore {
		t.Fatalf("group message should be dropped with zero replies, got %d sends", sentAfter-sentBefore)
	}
	if calls := env.engine.Calls(); calls != 0 {
		t.Fatalf("engine calls = %d, want 0", calls)
	}

	logsAfter := env.logBuf.String()
	newLogs := strings.TrimPrefix(logsAfter, logsBefore)
	if !strings.Contains(newLogs, "group") && !strings.Contains(newLogs, "dropped") {
		t.Fatalf("spec requires one log line for dropped group message, got logs: %q", newLogs)
	}

	// 4. Foreign callback_query -> answerCallbackQuery toast only, zero sendMessage calls
	env.tg.SendUpdate(makeCallbackUpdate(4, "stranger_3", "chat_owner_v3", "private", "cb_foreign", "ap:a_012345678901:a"))
	if err := env.tg.WaitForToasts(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for toast: %v", err)
	}
	if toast := env.tg.LastToast().Text; toast != nuntius.ToastPrivate {
		t.Fatalf("toast = %q, want %q", toast, nuntius.ToastPrivate)
	}
	if sentNow := env.tg.SentCount(); sentNow != sentBefore {
		t.Fatalf("foreign callback must send zero messages, sentCount=%d", sentNow)
	}
}

// -----------------------------------------------------------------------
// V4. Turn round-trip
// -----------------------------------------------------------------------

type intervalStreamingProvider struct {
	name   string
	chunks []string
	delay  time.Duration
}

func (p *intervalStreamingProvider) Name() string { return p.name }
func (p *intervalStreamingProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "fake"}, nil
}

func (p *intervalStreamingProvider) Complete(_ context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	full := strings.Join(p.chunks, "")
	return &router.Completion{Text: full, Model: "fake"}, nil
}

func (p *intervalStreamingProvider) StreamComplete(_ context.Context, _ []router.Message, _ router.Options, _ string, onDelta router.StreamHandler) (*router.Completion, error) {
	for _, c := range p.chunks {
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		if err := onDelta(c); err != nil {
			return nil, err
		}
	}
	full := strings.Join(p.chunks, "")
	return &router.Completion{Text: full, Model: "fake"}, nil
}

func TestV4TurnRoundTrip(t *testing.T) {
	chunks := []string{
		"Alpha sentence. ",
		"Beta sentence. ",
		"Gamma sentence.",
	}
	provider := &intervalStreamingProvider{
		name:   "streaming",
		chunks: chunks,
		delay:  80 * time.Millisecond,
	}

	env := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			EditInterval: 50 * time.Millisecond,
		},
		provider: provider,
		ownerID:  "owner_v4",
		chatID:   "chat_v4",
	})

	logPath := filepath.Join(env.home, "sessions", "main")
	logBefore, err := penatus.OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	eventsBeforeCount := len(logBefore.Live())

	// Send owner text turn
	env.tg.SendUpdate(makeMsgUpdate(100, "owner_v4", "chat_v4", "private", "Tell me the Greek letters"))

	// Wait for the final edit from TurnDone
	if err := env.tg.WaitForEdits(2, 4*time.Second); err != nil {
		t.Fatalf("waiting for edits: %v", err)
	}
	// Wait a moment for turn completion to persist to session log
	time.Sleep(150 * time.Millisecond)

	env.tg.mu.Lock()
	edits := make([]fakeEditMsg, len(env.tg.edits))
	copy(edits, env.tg.edits)
	initialSend := env.tg.sent[0]
	env.tg.mu.Unlock()

	if len(edits) < 2 {
		t.Fatalf("expected at least 2 edits, got %d", len(edits))
	}

	// 1. Every non-final edit ≥ edit_interval apart (turn_done final edit is exempt)
	// Check initial send to edit 0:
	if d := edits[0].Time.Sub(initialSend.Time); d < 45*time.Millisecond {
		t.Fatalf("edit 0 followed initial send after %v, want >= 50ms", d)
	}
	// Check all non-final edits:
	for i := 1; i < len(edits)-1; i++ {
		gap := edits[i].Time.Sub(edits[i-1].Time)
		if gap < 45*time.Millisecond {
			t.Fatalf("non-final edit %d followed edit %d after %v, want >= 50ms", i, i-1, gap)
		}
	}

	// 2. Final edit text == assistant penatus event text exactly
	finalEditText := edits[len(edits)-1].Text

	logAfter, err := penatus.OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	liveAfter := logAfter.Live()

	// 3. Exactly user+assistant events gained
	if len(liveAfter) != eventsBeforeCount+2 {
		t.Fatalf("session log gained %d events, want 2 (user+assistant)", len(liveAfter)-eventsBeforeCount)
	}

	userEvent := liveAfter[eventsBeforeCount]
	assistantEvent := liveAfter[eventsBeforeCount+1]

	var roleUser string
	if err := json.Unmarshal(userEvent.Fields["role"], &roleUser); err != nil || roleUser != "user" {
		t.Fatalf("user event role = %q, want user", roleUser)
	}
	var roleAssistant, assistantText string
	if err := json.Unmarshal(assistantEvent.Fields["role"], &roleAssistant); err != nil || roleAssistant != "assistant" {
		t.Fatalf("assistant event role = %q, want assistant", roleAssistant)
	}
	if err := json.Unmarshal(assistantEvent.Fields["text"], &assistantText); err != nil {
		t.Fatalf("unmarshal assistant text: %v", err)
	}

	if finalEditText != assistantText {
		t.Fatalf("final edit text %q != assistant log text %q", finalEditText, assistantText)
	}

	// Check update_id present on both events (A3)
	var userUpdateID, assistantUpdateID int64
	if err := json.Unmarshal(userEvent.Fields["update_id"], &userUpdateID); err != nil || userUpdateID != 100 {
		t.Fatalf("user event update_id = %d, want 100", userUpdateID)
	}
	if err := json.Unmarshal(assistantEvent.Fields["update_id"], &assistantUpdateID); err != nil || assistantUpdateID != 100 {
		t.Fatalf("assistant event update_id = %d, want 100", assistantUpdateID)
	}
}

// -----------------------------------------------------------------------
// V5. Long reply
// -----------------------------------------------------------------------

func TestV5LongReply(t *testing.T) {
	// 1. Provider returns 10,000 chars with words and spaces
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	var b strings.Builder
	for b.Len() < 10000 {
		w := words[rand.IntN(len(words))] //nolint:gosec // fake text generation for test
		if b.Len()+len(w)+1 <= 10000 {
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(w)
		} else {
			b.WriteString(strings.Repeat("x", 10000-b.Len()))
		}
	}
	source10k := b.String()

	p1 := &intervalStreamingProvider{
		name:   "long-reply",
		chunks: []string{source10k},
	}

	env1 := setupVSuite(t, vsuiteOpts{
		provider: p1,
		ownerID:  "owner_v5_1",
		chatID:   "chat_v5_1",
	})

	env1.tg.SendUpdate(makeMsgUpdate(1, "owner_v5_1", "chat_v5_1", "private", "give me 10000 chars"))
	if err := env1.tg.WaitForSends(3, 4*time.Second); err != nil {
		t.Fatalf("waiting for >= 3 messages: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	texts1 := env1.tg.FinalMessageTexts("chat_v5_1")
	if len(texts1) < 3 {
		t.Fatalf("got %d messages, want >= 3", len(texts1))
	}
	for i, m := range texts1 {
		if len(m) > 3900 {
			t.Fatalf("message %d length = %d > 3900", i, len(m))
		}
	}

	// Concatenation minus the ▼done suffix equals the source verbatim
	var concat1 strings.Builder
	for i, m := range texts1 {
		if i == len(texts1)-1 {
			// Suffix is joinWithSpace(remaining, "▼done") which appends " ▼done"
			trimmed := strings.TrimSuffix(m, " ▼done")
			trimmed = strings.TrimSuffix(trimmed, "▼done")
			concat1.WriteString(trimmed)
		} else {
			concat1.WriteString(m)
		}
	}
	if concat1.String() != source10k {
		t.Fatalf("concatenation len=%d != source len=%d", concat1.Len(), len(source10k))
	}

	// 2. 5,000-char whitespace-free token -> hard split at 3900
	token5k := strings.Repeat("z", 5000)
	p2 := &intervalStreamingProvider{
		name:   "token-split",
		chunks: []string{token5k},
	}

	env2 := setupVSuite(t, vsuiteOpts{
		provider: p2,
		ownerID:  "owner_v5_2",
		chatID:   "chat_v5_2",
	})

	env2.tg.SendUpdate(makeMsgUpdate(2, "owner_v5_2", "chat_v5_2", "private", "give me 5000 continuous chars"))
	if err := env2.tg.WaitForSends(2, 4*time.Second); err != nil {
		t.Fatalf("waiting for 2 messages: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	texts2 := env2.tg.FinalMessageTexts("chat_v5_2")
	if len(texts2) != 2 {
		t.Fatalf("got %d messages, want 2", len(texts2))
	}
	if len(texts2[0]) != 3900 {
		t.Fatalf("first message len = %d, want hard split at 3900", len(texts2[0]))
	}
	if texts2[0] != strings.Repeat("z", 3900) {
		t.Fatalf("first message content mismatch")
	}
	trimmedLast := strings.TrimSuffix(texts2[1], " ▼done")
	trimmedLast = strings.TrimSuffix(trimmedLast, "▼done")
	if trimmedLast != strings.Repeat("z", 1100) {
		t.Fatalf("second message content mismatch: len %d, want 1100", len(trimmedLast))
	}
}

// -----------------------------------------------------------------------
// V6. Approval card race
// -----------------------------------------------------------------------

type scriptedStepProvider struct {
	name  string
	mu    sync.Mutex
	steps []func(step int) (*router.Completion, error)
	step  int
}

func (p *scriptedStepProvider) Name() string { return p.name }
func (p *scriptedStepProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "fake"}, nil
}

func (p *scriptedStepProvider) Complete(_ context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	p.mu.Lock()
	p.step++
	s := p.step
	p.mu.Unlock()
	if s <= len(p.steps) {
		return p.steps[s-1](s)
	}
	return &router.Completion{Text: "done"}, nil
}

func (p *scriptedStepProvider) StreamComplete(ctx context.Context, _ []router.Message, _ router.Options, _ string, onDelta router.StreamHandler) (*router.Completion, error) {
	comp, err := p.Complete(ctx, nil, router.Options{})
	if err != nil {
		return nil, err
	}
	if comp.Text != "" {
		_ = onDelta(comp.Text)
	}
	return comp, nil
}

func TestV6ApprovalCardRace(t *testing.T) {
	var toolRunCount int
	var toolMu sync.Mutex

	untrustedTool := loop.Tool{
		Spec: router.ToolSpec{
			Name:             "deploy_cluster",
			Description:      "Deploy production cluster",
			ParamsJSONSchema: `{"type":"object","properties":{"cluster":{"type":"string"}}}`,
		},
		Trusted: false,
		Run: func(ctx context.Context, args string) (string, error) {
			toolMu.Lock()
			toolRunCount++
			toolMu.Unlock()
			return "deployed successfully", nil
		},
	}

	argsOver700 := `{"cluster":"` + strings.Repeat("node-", 160) + `"}` // > 700 chars
	provider := &scriptedStepProvider{
		name: "scripted-tools",
		steps: []func(step int) (*router.Completion, error){
			func(step int) (*router.Completion, error) {
				return &router.Completion{
					ToolCalls: []router.ToolCall{
						{ID: "call_deploy_1", Name: "deploy_cluster", ArgsJSON: argsOver700},
					},
				}, nil
			},
			func(step int) (*router.Completion, error) {
				return &router.Completion{Text: "cluster deployed."}, nil
			},
		},
	}

	env := setupVSuite(t, vsuiteOpts{
		provider: provider,
		tools:    []loop.Tool{untrustedTool},
		ownerID:  "owner_v6",
		chatID:   "chat_v6",
	})

	// 1. Trigger untrusted tool turn
	env.tg.SendUpdate(makeMsgUpdate(1, "owner_v6", "chat_v6", "private", "deploy cluster"))

	// 2. Wait for approval card send
	if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for approval card: %v", err)
	}
	card := env.tg.LastSent()

	// Assert card body contains tool name, session id8, args ≤700 chars
	if !strings.Contains(card.Text, "deploy_cluster") {
		t.Fatalf("card body %q missing tool name deploy_cluster", card.Text)
	}
	if !strings.Contains(card.Text, "main") {
		t.Fatalf("card body %q missing session id8 main", card.Text)
	}
	parts := strings.Split(card.Text, "\n")
	if len(parts) < 2 {
		t.Fatalf("expected card body to have arguments summary line: %q", card.Text)
	}
	argsLine := parts[1]
	if len(argsLine) > 700 {
		t.Fatalf("args line len = %d > 700 chars", len(argsLine))
	}

	// Extract approval ID from keyboard callback data
	if card.Keyboard == nil || len(card.Keyboard.Buttons) == 0 || len(card.Keyboard.Buttons[0]) == 0 {
		t.Fatalf("card missing keyboard: %+v", card.Keyboard)
	}
	cbData := card.Keyboard.Buttons[0][0].CallbackData
	cbParts := strings.Split(cbData, ":")
	if len(cbParts) != 3 || cbParts[0] != "ap" {
		t.Fatalf("unexpected callback_data: %q", cbData)
	}
	approvalID := cbParts[1]

	// 3. Fire the approve callback twice concurrently through bridge-injected callback_updates
	cb1 := makeCallbackUpdate(2, "owner_v6", "chat_v6", "private", "toast_1", "ap:"+approvalID+":a")
	cb2 := makeCallbackUpdate(3, "owner_v6", "chat_v6", "private", "toast_2", "ap:"+approvalID+":a")

	// Inject callback updates concurrently in batch
	env.tg.inbound <- []nuntius.Update{cb1, cb2}

	// Wait for both toasts to be recorded
	if err := env.tg.WaitForToasts(2, 5*time.Second); err != nil {
		env.tg.mu.Lock()
		toasts := env.tg.toasts
		env.tg.mu.Unlock()
		t.Fatalf("waiting for 2 toasts: %v, got toasts: %+v, bridge logs: %s", err, toasts, env.logBuf.String())
	}

	env.tg.mu.Lock()
	t1 := env.tg.toasts[0].Text
	t2 := env.tg.toasts[1].Text
	env.tg.mu.Unlock()

	hasAllowed := t1 == nuntius.ToastAllowed || t2 == nuntius.ToastAllowed
	hasAlready := t1 == nuntius.ToastAlready || t2 == nuntius.ToastAlready
	if !hasAllowed || !hasAlready {
		t.Fatalf("toasts = (%q, %q), want one %q and one %q", t1, t2, nuntius.ToastAllowed, nuntius.ToastAlready)
	}

	// Wait for turn completion
	time.Sleep(200 * time.Millisecond)

	// Exactly one hub transition (one tool run)
	toolMu.Lock()
	runs := toolRunCount
	toolMu.Unlock()
	if runs != 1 {
		t.Fatalf("tool run count = %d, want exactly 1", runs)
	}

	// 4. Tool_result audit event carries source "telegram"
	logFile, err := penatus.OpenLog(filepath.Join(env.home, "sessions", "main"))
	if err != nil {
		t.Fatal(err)
	}
	var foundAudit bool
	for _, ev := range logFile.Live() {
		if ev.T == "tool_result" {
			var app struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
				Source   string `json:"source"`
			}
			if err := json.Unmarshal(ev.Fields["approval"], &app); err == nil {
				foundAudit = true
				if app.Source != "telegram" {
					t.Fatalf("audit source = %q, want telegram", app.Source)
				}
				if app.Reason != "ok" {
					t.Fatalf("audit reason = %q, want ok", app.Reason)
				}
			}
		}
	}
	if !foundAudit {
		t.Fatal("tool_result audit event not found in session log")
	}
}

// -----------------------------------------------------------------------
// V7. Terminal cleanup matrix
// -----------------------------------------------------------------------

//nolint:maintidx // comprehensive matrix covers 5 resolution paths and audit contracts
func TestV7TerminalCleanupMatrix(t *testing.T) {
	makeTool := func(toolRan *int, mu *sync.Mutex) loop.Tool {
		return loop.Tool{
			Spec: router.ToolSpec{
				Name:             "action_tool",
				Description:      "action tool",
				ParamsJSONSchema: `{"type":"object","properties":{"v":{"type":"string"}}}`,
			},
			Trusted: false,
			Run: func(ctx context.Context, args string) (string, error) {
				mu.Lock()
				*toolRan++
				mu.Unlock()
				return "done", nil
			},
		}
	}

	makeProvider := func() router.Provider {
		return &scriptedStepProvider{
			name: "v7-prov",
			steps: []func(step int) (*router.Completion, error){
				func(step int) (*router.Completion, error) {
					return &router.Completion{
						ToolCalls: []router.ToolCall{{ID: "c1", Name: "action_tool", ArgsJSON: `{"v":"1"}`}},
					}, nil
				},
				func(step int) (*router.Completion, error) {
					return &router.Completion{Text: "all finished."}, nil
				},
			},
		}
	}

	// 1. click: telegram tap -> Allowed ✓ (telegram)
	t.Run("click", func(t *testing.T) {
		var toolRan int
		var mu sync.Mutex
		env := setupVSuite(t, vsuiteOpts{
			provider: makeProvider(),
			tools:    []loop.Tool{makeTool(&toolRan, &mu)},
			ownerID:  "owner_v7_click",
			chatID:   "chat_v7_click",
		})

		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v7_click", "chat_v7_click", "private", "run action"))
		if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		card := env.tg.LastSent()
		approvalID := strings.Split(card.Keyboard.Buttons[0][0].CallbackData, ":")[1]

		env.tg.SendUpdate(makeCallbackUpdate(2, "owner_v7_click", "chat_v7_click", "private", "cb1", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForEdits(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		edit := env.tg.LastEdit()
		if edit.Text != "Allowed ✓ (telegram)" {
			t.Fatalf("edit text = %q, want %q", edit.Text, "Allowed ✓ (telegram)")
		}
		if edit.Keyboard != nil {
			t.Fatalf("keyboard must be stripped on terminal edit: %+v", edit.Keyboard)
		}

		time.Sleep(100 * time.Millisecond)
		// Late click -> already resolved toast, zero additional tool runs
		env.tg.SendUpdate(makeCallbackUpdate(3, "owner_v7_click", "chat_v7_click", "private", "cb2", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForToasts(2, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if toast := env.tg.LastToast().Text; toast != nuntius.ToastAlready {
			t.Fatalf("late click toast = %q, want %q", toast, nuntius.ToastAlready)
		}
		mu.Lock()
		runs := toolRan
		mu.Unlock()
		if runs != 1 {
			t.Fatalf("tool ran %d times, want 1", runs)
		}
	})

	// 2. web: web ResolveFrom -> Allowed ✓ (web)
	t.Run("web", func(t *testing.T) {
		var toolRan int
		var mu sync.Mutex
		env := setupVSuite(t, vsuiteOpts{
			provider: makeProvider(),
			tools:    []loop.Tool{makeTool(&toolRan, &mu)},
			ownerID:  "owner_v7_web",
			chatID:   "chat_v7_web",
		})

		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v7_web", "chat_v7_web", "private", "run action"))
		if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		card := env.tg.LastSent()
		approvalID := strings.Split(card.Keyboard.Buttons[0][0].CallbackData, ":")[1]

		// Resolve via web
		if code := env.ap.ResolveFrom("main", approvalID, true, "web"); code != 200 {
			t.Fatalf("web resolve code = %d", code)
		}
		if err := env.tg.WaitForEdits(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		edit := env.tg.LastEdit()
		if edit.Text != "Allowed ✓ (web)" {
			t.Fatalf("edit text = %q, want %q", edit.Text, "Allowed ✓ (web)")
		}
		if edit.Keyboard != nil {
			t.Fatalf("keyboard must be stripped on terminal edit: %+v", edit.Keyboard)
		}

		time.Sleep(100 * time.Millisecond)
		// Late click -> already resolved toast, zero additional tool runs
		env.tg.SendUpdate(makeCallbackUpdate(2, "owner_v7_web", "chat_v7_web", "private", "cb_late", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForToasts(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if toast := env.tg.LastToast().Text; toast != nuntius.ToastAlready {
			t.Fatalf("late click toast = %q, want %q", toast, nuntius.ToastAlready)
		}
		mu.Lock()
		runs := toolRan
		mu.Unlock()
		if runs != 1 {
			t.Fatalf("tool ran %d times, want 1", runs)
		}
	})

	// 3. timeout: bridge timeout fires -> Denied — timed out
	t.Run("timeout", func(t *testing.T) {
		var toolRan int
		var mu sync.Mutex
		env := setupVSuite(t, vsuiteOpts{
			cfg: nuntius.Config{
				ApprovalTimeout: 1500 * time.Millisecond,
			},
			provider: makeProvider(),
			tools:    []loop.Tool{makeTool(&toolRan, &mu)},
			ownerID:  "owner_v7_timeout",
			chatID:   "chat_v7_timeout",
		})

		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v7_timeout", "chat_v7_timeout", "private", "run action"))
		if err := env.tg.WaitForSends(1, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		card := env.tg.LastSent()
		approvalID := strings.Split(card.Keyboard.Buttons[0][0].CallbackData, ":")[1]

		// Wait for timeout edit (timer is 1500ms + 1s per-chat refill)
		if err := env.tg.WaitForEdits(1, 4*time.Second); err != nil {
			t.Fatalf("timeout waiting for 1 edits (got %d), logs:\n%s\nsent: %+v\npending: %d", len(env.tg.edits), env.logBuf.String(), env.tg.sent, env.ap.PendingCount("main"))
		}
		edit := env.tg.LastEdit()
		if edit.Text != "Denied — timed out" {
			t.Fatalf("edit text = %q, want %q", edit.Text, "Denied — timed out")
		}
		if edit.Keyboard != nil {
			t.Fatalf("keyboard must be stripped on terminal edit: %+v", edit.Keyboard)
		}

		time.Sleep(100 * time.Millisecond)
		// Late click -> already resolved toast, zero tool runs
		env.tg.SendUpdate(makeCallbackUpdate(2, "owner_v7_timeout", "chat_v7_timeout", "private", "cb_late", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForToasts(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if toast := env.tg.LastToast().Text; toast != nuntius.ToastAlready {
			t.Fatalf("late click toast = %q, want %q", toast, nuntius.ToastAlready)
		}
		mu.Lock()
		runs := toolRan
		mu.Unlock()
		if runs != 0 {
			t.Fatalf("tool ran %d times, want 0", runs)
		}
	})

	// 4. cancel: /cancel mid-turn -> Denied — cancelled, no tool_result committed
	t.Run("cancel", func(t *testing.T) {
		var toolRan int
		var mu sync.Mutex
		env := setupVSuite(t, vsuiteOpts{
			provider: makeProvider(),
			tools:    []loop.Tool{makeTool(&toolRan, &mu)},
			ownerID:  "owner_v7_cancel",
			chatID:   "chat_v7_cancel",
		})

		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v7_cancel", "chat_v7_cancel", "private", "run action"))
		if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		card := env.tg.LastSent()
		approvalID := strings.Split(card.Keyboard.Buttons[0][0].CallbackData, ":")[1]

		// Cancel turn via bridge /cancel
		env.tg.SendUpdate(makeMsgUpdate(2, "owner_v7_cancel", "chat_v7_cancel", "private", "/cancel"))
		if err := env.tg.WaitForEdits(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		edit := env.tg.LastEdit()
		if edit.Text != "Denied — cancelled" {
			t.Fatalf("edit text = %q, want %q", edit.Text, "Denied — cancelled")
		}
		if edit.Keyboard != nil {
			t.Fatalf("keyboard must be stripped on terminal edit: %+v", edit.Keyboard)
		}

		time.Sleep(100 * time.Millisecond)
		// Cancel path asserts: turn aborted, NO tool_result committed in session log
		logFile, err := penatus.OpenLog(filepath.Join(env.home, "sessions", "main"))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range logFile.Live() {
			if ev.T == "tool_result" {
				t.Fatalf("cancel path must not commit tool_result event: %+v", ev)
			}
		}

		// Late click -> already resolved toast, zero tool runs
		env.tg.SendUpdate(makeCallbackUpdate(3, "owner_v7_cancel", "chat_v7_cancel", "private", "cb_late", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForToasts(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if toast := env.tg.LastToast().Text; toast != nuntius.ToastAlready {
			t.Fatalf("late click toast = %q, want %q", toast, nuntius.ToastAlready)
		}
		mu.Lock()
		runs := toolRan
		mu.Unlock()
		if runs != 0 {
			t.Fatalf("tool ran %d times, want 0", runs)
		}
	})

	// 5. shutdown: ap.Shutdown() -> Denied — shutdown
	t.Run("shutdown", func(t *testing.T) {
		var toolRan int
		var mu sync.Mutex
		env := setupVSuite(t, vsuiteOpts{
			provider: makeProvider(),
			tools:    []loop.Tool{makeTool(&toolRan, &mu)},
			ownerID:  "owner_v7_shutdown",
			chatID:   "chat_v7_shutdown",
		})

		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v7_shutdown", "chat_v7_shutdown", "private", "run action"))
		if err := env.tg.WaitForSends(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		card := env.tg.LastSent()
		approvalID := strings.Split(card.Keyboard.Buttons[0][0].CallbackData, ":")[1]

		// Shutdown approval hub via Hub.Shutdown()
		env.hub.Shutdown()
		if err := env.tg.WaitForEdits(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		edit := env.tg.LastEdit()
		if edit.Text != "Denied — shutdown" {
			t.Fatalf("edit text = %q, want %q", edit.Text, "Denied — shutdown")
		}
		if edit.Keyboard != nil {
			t.Fatalf("keyboard must be stripped on terminal edit: %+v", edit.Keyboard)
		}

		// Denial record carries source: "shutdown", reason: "shutdown"
		if r := env.ap.Reason("main", approvalID); r != "shutdown" {
			t.Fatalf("denial reason = %q, want shutdown", r)
		}
		if s := env.ap.Source("main", approvalID); s != "shutdown" {
			t.Fatalf("denial source = %q, want shutdown", s)
		}

		// Late click -> already resolved toast
		env.tg.SendUpdate(makeCallbackUpdate(2, "owner_v7_shutdown", "chat_v7_shutdown", "private", "cb_late", "ap:"+approvalID+":a"))
		if err := env.tg.WaitForToasts(1, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if toast := env.tg.LastToast().Text; toast != nuntius.ToastAlready {
			t.Fatalf("late click toast = %q, want %q", toast, nuntius.ToastAlready)
		}
		mu.Lock()
		runs := toolRan
		mu.Unlock()
		if runs != 0 {
			t.Fatalf("tool ran %d times, want 0", runs)
		}
	})
}

// -----------------------------------------------------------------------
// V8. Undeliverable card
// -----------------------------------------------------------------------

func setBridgeFastSleep(b *nuntius.Bridge, sleepFn func(context.Context, time.Duration) error) {
	rf := reflect.ValueOf(b).Elem().FieldByName("deps")
	field := rf.FieldByName("sleep")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(sleepFn))
}

func TestV8UndeliverableCard(t *testing.T) {
	// (a) Telegram sole channel: immediate denied:undeliverable, turn proceeds
	t.Run("sole_channel", func(t *testing.T) {
		untrustedTool := loop.Tool{
			Spec: router.ToolSpec{
				Name:             "unreachable_tool",
				Description:      "tool whose card fails delivery",
				ParamsJSONSchema: `{"type":"object","properties":{"x":{"type":"string"}}}`,
			},
			Trusted: false,
			Run: func(ctx context.Context, args string) (string, error) {
				return "should not run", nil
			},
		}

		provider := &scriptedStepProvider{
			name: "v8-prov",
			steps: []func(step int) (*router.Completion, error){
				func(step int) (*router.Completion, error) {
					return &router.Completion{
						ToolCalls: []router.ToolCall{{ID: "c1", Name: "unreachable_tool", ArgsJSON: `{"x":"1"}`}},
					}, nil
				},
				func(step int) (*router.Completion, error) {
					return &router.Completion{Text: "turn completed after denial."}, nil
				},
			},
		}

		env := setupVSuite(t, vsuiteOpts{
			provider: provider,
			tools:    []loop.Tool{untrustedTool},
			ownerID:  "owner_v8_a",
			chatID:   "chat_v8_a",
		})

		// Make card retries fast (cardBackoff in cards.go is 3s; 10ms in test)
		setBridgeFastSleep(env.bridge, func(ctx context.Context, d time.Duration) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		})

		// Fake TG 500s on card send (cards have ApprovalKeyboard)
		var attempts int
		env.tg.sendErrHook = func(chatID, text string, kb *nuntius.Keyboard) error {
			if kb != nil {
				attempts++
				return errors.New("500 internal server error")
			}
			return nil
		}

		// Start turn
		env.tg.SendUpdate(makeMsgUpdate(1, "owner_v8_a", "chat_v8_a", "private", "run unreachable"))

		// Wait for turn to complete and send final message
		if err := env.tg.WaitForSends(1, 6*time.Second); err != nil {
			t.Fatalf("waiting for turn completion message: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("card send attempts = %d, want 3", attempts)
		}

		// Assert tool_result event has source "hub", reason "undeliverable"
		logFile, err := penatus.OpenLog(filepath.Join(env.home, "sessions", "main"))
		if err != nil {
			t.Fatal(err)
		}
		var foundUndeliverable bool
		for _, ev := range logFile.Live() {
			if ev.T == "tool_result" {
				var app struct {
					Decision string `json:"decision"`
					Reason   string `json:"reason"`
					Source   string `json:"source"`
				}
				if err := json.Unmarshal(ev.Fields["approval"], &app); err == nil {
					if app.Reason == "undeliverable" && app.Source == "hub" {
						foundUndeliverable = true
					}
				}
			}
		}
		if !foundUndeliverable {
			t.Fatal("expected tool_result event with reason:undeliverable and source:hub")
		}

		// Session accepts next message within seconds
		time.Sleep(50 * time.Millisecond)
		env.tg.SendUpdate(makeMsgUpdate(2, "owner_v8_a", "chat_v8_a", "private", "next message"))
		if err := env.tg.WaitForSends(2, 2*time.Second); err != nil {
			t.Fatalf("session did not accept next message: %v", err)
		}
	})

	// (b) SSE listener also live: NOT denied, card_dead set, rides timer, web resolution wins
	t.Run("dual_channel_rides_timer", func(t *testing.T) {
		env := setupVSuite(t, vsuiteOpts{
			ownerID: "owner_v8_b",
			chatID:  "chat_v8_b",
		})
		setBridgeFastSleep(env.bridge, func(ctx context.Context, d time.Duration) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		})

		env.tg.sendErrHook = func(chatID, text string, kb *nuntius.Keyboard) error {
			if kb != nil {
				return errors.New("500 internal server error")
			}
			return nil
		}

		// Register approval with webLive: true (simulating web SSE listener)
		id, _ := env.ap.RegisterOn("main", "tool_b", `{"v":"b"}`, true, func(string) {})

		// Wait for 3 card send attempts to fail and call MarkUndeliverable
		time.Sleep(150 * time.Millisecond)

		// Approval must NOT be denied: card_dead set, rides timer
		if pending := env.ap.PendingCount("main"); pending != 1 {
			t.Fatalf("pending approvals = %d, want 1 (dual-channel undeliverable must stay pending)", pending)
		}

		// Web resolution still wins
		if code := env.ap.ResolveFrom("main", id, true, "web"); code != 200 {
			t.Fatalf("web resolve code = %d, want 200", code)
		}
		if env.ap.Reason("main", id) != "ok" || env.ap.Source("main", id) != "web" {
			t.Fatalf("approval reason/source = %q/%q, want ok/web", env.ap.Reason("main", id), env.ap.Source("main", id))
		}
	})

	// (c) As (b), then SSE listener drops -> denied:disconnected within ≤2 s disconnect budget
	t.Run("dual_channel_disconnect_after_card_dead", func(t *testing.T) {
		env := setupVSuite(t, vsuiteOpts{
			ownerID: "owner_v8_c",
			chatID:  "chat_v8_c",
		})
		setBridgeFastSleep(env.bridge, func(ctx context.Context, d time.Duration) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		})

		env.tg.sendErrHook = func(chatID, text string, kb *nuntius.Keyboard) error {
			if kb != nil {
				return errors.New("500 internal server error")
			}
			return nil
		}

		// Register approval with webLive: true
		id, decision := env.ap.RegisterOn("main", "tool_c", `{"v":"c"}`, true, func(string) {})

		// Wait for card send failures and MarkUndeliverable
		time.Sleep(150 * time.Millisecond)

		if env.ap.PendingCount("main") != 1 {
			t.Fatal("must stay pending while web is live")
		}

		// Drop the web SSE listener
		disconnectStart := time.Now()
		env.ap.DisconnectFor("main")

		select {
		case got := <-decision:
			if got {
				t.Fatal("decision must be false (denied)")
			}
			if elapsed := time.Since(disconnectStart); elapsed > 2*time.Second {
				t.Fatalf("disconnect took %v, want <= 2s", elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("approval did not resolve denied:disconnected within 2s")
		}

		if r := env.ap.Reason("main", id); r != "disconnected" {
			t.Fatalf("reason = %q, want disconnected", r)
		}
	})
}

// -----------------------------------------------------------------------
// V9. Flood
// -----------------------------------------------------------------------

func TestV9Flood(t *testing.T) {
	// Part 1: Fake-TG returns 429 retry_after:3 on an edit -> send bucket pauses ≥ 3 s,
	// polling continues (getUpdates calls observed during the pause), turn completes, final text intact.
	chunks := []string{
		"Start of streaming response. ",
		"End of streaming response.",
	}
	provider := &intervalStreamingProvider{
		name:   "flood-prov",
		chunks: chunks,
		delay:  60 * time.Millisecond,
	}

	env := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			EditInterval: 50 * time.Millisecond,
		},
		provider: provider,
		ownerID:  "owner_v9",
		chatID:   "chat_v9",
	})

	var returned429 bool
	var pauseStart time.Time
	var pollsDuringPause int
	var mu sync.Mutex

	env.tg.editErrHook = func(chatID string, msgID int64, text string, kb *nuntius.Keyboard) error {
		mu.Lock()
		defer mu.Unlock()
		if !returned429 {
			returned429 = true
			pauseStart = time.Now()
			return make429Error(3) // 429 retry_after: 3s
		}
		return nil
	}

	pollsBefore := env.tg.PollCount()

	// Send owner message to start streaming turn
	env.tg.SendUpdate(makeMsgUpdate(1, "owner_v9", "chat_v9", "private", "stream flood test"))

	// Wait for turn to complete (it must pause >= 3s on the 429, then complete)
	turnStart := time.Now()
	if err := env.tg.WaitForEdits(1, 6*time.Second); err != nil {
		t.Fatalf("waiting for edits to complete: %v", err)
	}
	turnElapsed := time.Since(turnStart)

	mu.Lock()
	pStart := pauseStart
	mu.Unlock()

	if !returned429 {
		t.Fatal("429 error hook was never called")
	}

	// Sends paused ≥ 3s
	if pauseDuration := time.Since(pStart); pauseDuration < 2900*time.Millisecond {
		t.Fatalf("send bucket pause duration = %v, want >= 3s", pauseDuration)
	}
	if turnElapsed < 2900*time.Millisecond {
		t.Fatalf("turn elapsed = %v, want >= 3s due to 429 pause", turnElapsed)
	}

	// Polling continued during pause: assert GetUpdates was called multiple times
	pollsAfter := env.tg.PollCount()
	pollsDuringPause = pollsAfter - pollsBefore
	if pollsDuringPause < 10 {
		t.Fatalf("polling calls during pause = %d, want >= 10 (polling must continue during send pause)", pollsDuringPause)
	}

	// Final text intact
	finalEdit := env.tg.LastEdit()
	expectedText := "Start of streaming response. End of streaming response."
	if finalEdit.Text != expectedText {
		t.Fatalf("final edit text = %q, want %q", finalEdit.Text, expectedText)
	}

	// Part 2: Per-chat bucket caps sends to ≤ 1/s under a 10-message burst (N9)
	var clkMu sync.Mutex
	var sleptTotal time.Duration
	fakeNow := time.Now()
	fc := nuntius.NewFloodControl(
		func() time.Time {
			clkMu.Lock()
			defer clkMu.Unlock()
			return fakeNow
		},
		func(ctx context.Context, d time.Duration) error {
			clkMu.Lock()
			sleptTotal += d
			fakeNow = fakeNow.Add(d)
			clkMu.Unlock()
			return nil
		},
	)

	// Send 10 messages burst to one chat
	for i := range 10 {
		if err := fc.Send(context.Background(), "chat_burst"); err != nil {
			t.Fatalf("fc.Send %d: %v", i, err)
		}
	}

	clkMu.Lock()
	totalSlept := sleptTotal
	clkMu.Unlock()

	// 10 messages: first is immediate (burst 1), remaining 9 each wait 1s -> total >= 9s
	if totalSlept < 8900*time.Millisecond {
		t.Fatalf("per-chat 10-message burst slept %v, want >= 9s (rate <= 1 msg/s)", totalSlept)
	}
}

// -----------------------------------------------------------------------
// V10. Backpressure
// -----------------------------------------------------------------------

type backpressureProvider struct {
	name       string
	turn1Start chan struct{}
	turn1Wait  chan struct{}
	turn2Start chan struct{}
	mu         sync.Mutex
	calls      int
}

func (p *backpressureProvider) Name() string { return p.name }
func (p *backpressureProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "fake"}, nil
}

func (p *backpressureProvider) Complete(_ context.Context, msgs []router.Message, _ router.Options) (*router.Completion, error) {
	p.mu.Lock()
	p.calls++
	c := p.calls
	p.mu.Unlock()

	if c == 1 {
		select {
		case p.turn1Start <- struct{}{}:
		default:
		}
		<-p.turn1Wait
		return &router.Completion{Text: "turn1 reply"}, nil
	}
	select {
	case p.turn2Start <- struct{}{}:
	default:
	}
	return &router.Completion{Text: "turn2 reply"}, nil
}

func (p *backpressureProvider) StreamComplete(ctx context.Context, msgs []router.Message, opts router.Options, _ string, onDelta router.StreamHandler) (*router.Completion, error) {
	comp, err := p.Complete(ctx, msgs, opts)
	if err != nil {
		return nil, err
	}
	if comp.Text != "" {
		_ = onDelta(comp.Text)
	}
	return comp, nil
}

func TestV10Backpressure(t *testing.T) {
	intPtr := func(i int) *int { return &i }

	// Part 1: queue_depth 1 (default):
	// Turn in flight (slow fake-LLM) → msg2 queued and runs after;
	// msg3 → "still working" reply then dropped;
	// /new mid-turn refused with /cancel hint.
	bp1 := &backpressureProvider{
		name:       "bp1",
		turn1Start: make(chan struct{}, 1),
		turn1Wait:  make(chan struct{}),
		turn2Start: make(chan struct{}, 1),
	}

	env1 := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			QueueDepth: intPtr(1),
		},
		provider: bp1,
		ownerID:  "owner_v10_1",
		chatID:   "chat_v10_1",
	})

	// 1. Send msg1 -> starts turn 1 in flight
	env1.tg.SendUpdate(makeMsgUpdate(101, "owner_v10_1", "chat_v10_1", "private", "msg1"))
	select {
	case <-bp1.turn1Start:
	case <-time.After(2 * time.Second):
		t.Fatal("turn 1 did not start")
	}

	// 2. msg2 queued
	env1.tg.SendUpdate(makeMsgUpdate(102, "owner_v10_1", "chat_v10_1", "private", "msg2"))
	time.Sleep(50 * time.Millisecond)

	// 3. msg3 -> "still working — /status" reply then dropped
	env1.tg.SendUpdate(makeMsgUpdate(103, "owner_v10_1", "chat_v10_1", "private", "msg3"))
	if err := env1.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for still working reply: %v", err)
	}
	if text := env1.tg.LastSent().Text; text != nuntius.MsgStillWorking {
		t.Fatalf("msg3 reply = %q, want %q", text, nuntius.MsgStillWorking)
	}

	// 4. /new mid-turn refused with /cancel hint ("turn running — /cancel to stop it")
	env1.tg.SendUpdate(makeMsgUpdate(104, "owner_v10_1", "chat_v10_1", "private", "/new side"))
	if err := env1.tg.WaitForSends(2, 2*time.Second); err != nil {
		t.Fatalf("waiting for /new mid-turn refusal: %v", err)
	}
	if text := env1.tg.LastSent().Text; text != nuntius.MsgTurnRunning {
		t.Fatalf("/new reply = %q, want %q", text, nuntius.MsgTurnRunning)
	}

	// 5. Release turn 1 -> msg2 runs after
	close(bp1.turn1Wait)
	select {
	case <-bp1.turn2Start:
	case <-time.After(4 * time.Second):
		t.Fatal("msg2 did not run after turn 1 completed")
	}

	// Part 2: queue_depth: 0 → msg2 itself rejected
	bp2 := &backpressureProvider{
		name:       "bp2",
		turn1Start: make(chan struct{}, 1),
		turn1Wait:  make(chan struct{}),
		turn2Start: make(chan struct{}, 1),
	}

	env2 := setupVSuite(t, vsuiteOpts{
		cfg: nuntius.Config{
			QueueDepth: intPtr(0),
		},
		provider: bp2,
		ownerID:  "owner_v10_2",
		chatID:   "chat_v10_2",
	})

	// Send msg1 -> starts turn 1 in flight
	env2.tg.SendUpdate(makeMsgUpdate(201, "owner_v10_2", "chat_v10_2", "private", "msg1"))
	select {
	case <-bp2.turn1Start:
	case <-time.After(2 * time.Second):
		t.Fatal("turn 1 did not start")
	}

	// msg2 itself rejected immediately with "still working — /status"
	env2.tg.SendUpdate(makeMsgUpdate(202, "owner_v10_2", "chat_v10_2", "private", "msg2"))
	if err := env2.tg.WaitForSends(1, 2*time.Second); err != nil {
		t.Fatalf("waiting for queue_depth:0 rejection: %v", err)
	}
	if text := env2.tg.LastSent().Text; text != nuntius.MsgStillWorking {
		t.Fatalf("msg2 reply = %q, want %q", text, nuntius.MsgStillWorking)
	}

	close(bp2.turn1Wait)
}
