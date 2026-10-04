package nuntius

import (
	"context"
	"sync"
)

// Fake is an in-memory Transport for tests (spec §10: "Fake-TG" —
// scripted getUpdates + recorded outbound calls). Zero network.
type Fake struct {
	mu sync.Mutex

	// Queued update batches; each GetUpdates pops the next batch.
	Batches [][]Update
	// GetUpdatesErr makes the next GetUpdates fail (backoff tests).
	GetUpdatesErr error

	// Recorded calls.
	Sent     []FakeMsg
	Edits    []FakeEdit
	Toasts   []FakeToast
	Actions  []FakeAction
	Stripped []FakeTarget

	// SendErrs optionally fails SendMessage for the first N calls
	// (V8 undeliverable-card tests).
	SendErrs []error
	sendIdx  int

	// EditErrs optionally fails EditMessageText for the first N
	// calls (stream 3-failure rule tests).
	EditErrs []error
	editIdx  int

	// GetUpdatesCalls counts polls (V9: polling continues during a
	// send-bucket pause).
	GetUpdatesCalls int
}

// FakeMsg records a SendMessage.
type FakeMsg struct {
	ChatID   string
	Text     string
	Keyboard *Keyboard
	MsgID    int64
}

// FakeEdit records an EditMessageText.
type FakeEdit struct {
	ChatID    string
	MessageID int64
	Text      string
	Keyboard  *Keyboard
}

// FakeToast records an AnswerCallbackQuery.
type FakeToast struct {
	CallbackID string
	Text       string
}

// FakeAction records a SendChatAction.
type FakeAction struct {
	ChatID string
	Action string
}

// FakeTarget records a chat/message pair (keyboard strips).
type FakeTarget struct {
	ChatID    string
	MessageID int64
}

// Enqueue adds one update batch.
func (f *Fake) Enqueue(updates ...Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Batches = append(f.Batches, updates)
}

// GetUpdates pops the next scripted batch.
func (f *Fake) GetUpdates(_ context.Context, _ int64) ([]Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetUpdatesCalls++
	if f.GetUpdatesErr != nil {
		err := f.GetUpdatesErr
		f.GetUpdatesErr = nil
		return nil, err
	}
	if len(f.Batches) == 0 {
		return nil, nil
	}
	batch := f.Batches[0]
	f.Batches = f.Batches[1:]
	return batch, nil
}

// SendMessage records the send and mints a message id.
func (f *Fake) SendMessage(_ context.Context, chatID, text string, kb *Keyboard) (*Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SendErrs != nil && f.sendIdx < len(f.SendErrs) {
		err := f.SendErrs[f.sendIdx]
		f.sendIdx++
		if err != nil {
			return nil, err
		}
	} else if f.SendErrs != nil {
		f.sendIdx++
	}
	rec := FakeMsg{ChatID: chatID, Text: text, Keyboard: kb, MsgID: int64(len(f.Sent) + 1)}
	f.Sent = append(f.Sent, rec)
	return &Message{MessageID: rec.MsgID, Chat: Chat{ID: chatID, Type: "private"}, Text: text}, nil
}

// EditMessageText records the edit.
func (f *Fake) EditMessageText(_ context.Context, chatID string, messageID int64, text string, kb *Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EditErrs != nil && f.editIdx < len(f.EditErrs) {
		err := f.EditErrs[f.editIdx]
		f.editIdx++
		if err != nil {
			return err
		}
	} else if f.EditErrs != nil {
		f.editIdx++
	}
	f.Edits = append(f.Edits, FakeEdit{ChatID: chatID, MessageID: messageID, Text: text, Keyboard: kb})
	return nil
}

// SendChatAction records the typing ping.
func (f *Fake) SendChatAction(_ context.Context, chatID, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Actions = append(f.Actions, FakeAction{ChatID: chatID, Action: action})
	return nil
}

// AnswerCallback records the toast.
func (f *Fake) AnswerCallback(_ context.Context, callbackID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Toasts = append(f.Toasts, FakeToast{CallbackID: callbackID, Text: text})
	return nil
}

// DeleteMessageReplyMarkup records the strip.
func (f *Fake) DeleteMessageReplyMarkup(_ context.Context, chatID string, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stripped = append(f.Stripped, FakeTarget{ChatID: chatID, MessageID: messageID})
	return nil
}

// LastSent returns the most recent recorded send (zero value if none).
func (f *Fake) LastSent() FakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sent) == 0 {
		return FakeMsg{}
	}
	return f.Sent[len(f.Sent)-1]
}

// SentTexts returns every recorded send text in order.
func (f *Fake) SentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Sent))
	for i, m := range f.Sent {
		out[i] = m.Text
	}
	return out
}
