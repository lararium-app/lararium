package nuntius

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// FlexID is a Telegram numeric id carried as a decimal string. The Bot
// API sends user.id and chat.id as JSON numbers, but this bridge keys
// pairing and replies on the string form (ids exceed int32; parsing as
// JSON numbers would risk float mangling). UnmarshalJSON accepts both
// spellings: a JSON number is rendered decimal, never via float64.
type FlexID string

// UnmarshalJSON accepts a quoted string or a JSON integer id, keeping
// integers as exact decimal text (never via float64).
func (f *FlexID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("nuntius: id is neither string nor number: %s", b)
	}
	if _, err := strconv.ParseInt(n.String(), 10, 64); err != nil {
		return fmt.Errorf("nuntius: id not a valid int64: %s", n.String())
	}
	*f = FlexID(n.String())
	return nil
}

// User is a Telegram user (id decimal string on both wire spellings —
// Telegram sends integers and we key everything as strings).
type User struct {
	ID       string `json:"id"`
	Username string `json:"username,omitempty"`
	// FirstName is logged for unknown senders (§3) and never echoed
	// back into any reply.
	FirstName string `json:"first_name,omitempty"`
}

// UnmarshalJSON accepts the Bot API's numeric ids (live) as well as the
// decimal-string spelling used in fixtures.
func (u *User) UnmarshalJSON(b []byte) error {
	type alias User
	var a struct {
		alias

		ID FlexID `json:"id"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*u = User(a.alias)
	u.ID = string(a.ID)
	return nil
}

// Chat is the conversation an update arrived in.
type Chat struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "private" | "group" | "supergroup" | "channel"
}

// UnmarshalJSON accepts numeric chat ids (live Bot API) and strings.
func (c *Chat) UnmarshalJSON(b []byte) error {
	type alias Chat
	var a struct {
		alias

		ID FlexID `json:"id"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*c = Chat(a.alias)
	c.ID = string(a.ID)
	return nil
}

// Message is an inbound (or sent) message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

// CallbackQuery is an inline-keyboard tap (§7.2).
type CallbackQuery struct {
	ID      string  `json:"id"`
	From    User    `json:"from"`
	Message Message `json:"message"`
	Data    string  `json:"data"`
}

// Update is one getUpdates entry.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Envelope extracts the gate's minimal view of an update (§3): update
// id, from.id, chat.type — nothing semantic is parsed before the gate.
func (u Update) Envelope() Envelope {
	if u.CallbackQuery != nil {
		return Envelope{
			UpdateID:   u.UpdateID,
			IsCallback: true,
			FromID:     u.CallbackQuery.From.ID,
			ChatType:   u.CallbackQuery.Message.Chat.Type,
			Text:       u.CallbackQuery.Data,
			ChatID:     u.CallbackQuery.Message.Chat.ID,
		}
	}
	if u.Message != nil {
		from := ""
		firstName := ""
		if u.Message.From != nil {
			from = u.Message.From.ID
			firstName = u.Message.From.FirstName
		}
		return Envelope{
			UpdateID:  u.UpdateID,
			FromID:    from,
			FirstName: firstName,
			ChatType:  u.Message.Chat.Type,
			Text:      u.Message.Text,
			ChatID:    u.Message.Chat.ID,
		}
	}
	// Anything the spec has not decided that arrives on the wire:
	// refuse and log (P4). An empty envelope fails the private-chat
	// check and drops.
	return Envelope{UpdateID: u.UpdateID, ChatType: "unknown", ChatID: ""}
}

// Transport is the Bot API surface this bridge needs — six calls, all
// outbound (P5). The real implementation (T11b) adds the flood-control
// token buckets (N9) behind this same interface.
type Transport interface {
	// GetUpdates long-polls with timeout=30 and
	// allowed_updates=["message","callback_query"] (spec §2).
	GetUpdates(ctx context.Context, offset int64) ([]Update, error)
	// SendMessage posts a message; returns it (message_id needed for
	// edits).
	SendMessage(ctx context.Context, chatID, text string, keyboard *Keyboard) (*Message, error)
	// EditMessageText replaces a previously sent message's text.
	EditMessageText(ctx context.Context, chatID string, messageID int64, text string, keyboard *Keyboard) error
	// SendChatAction drives the "typing…" indicator (§6).
	SendChatAction(ctx context.Context, chatID, action string) error
	// AnswerCallbackQuery toasts a callback tap (§7.2: callbacks are
	// always toasted, never answered with new messages).
	AnswerCallback(ctx context.Context, callbackID, text string) error
	// DeleteMessageReplyMarkup strips a resolved card's keyboard
	// (§7.5: no stale buttons).
	DeleteMessageReplyMarkup(ctx context.Context, chatID string, messageID int64) error
}

// Keyboard is the minimal inline-keyboard shape (§7.2: two buttons,
// Allow / Deny, callback_data `ap:<id>:a` / `ap:<id>:d`).
type Keyboard struct {
	Buttons [][]Button `json:"inline_keyboard"`
}

// Button is one inline-keyboard button.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// ApprovalKeyboard builds the frozen two-button approval card keyboard
// (N5: callbacks carry only ap:<id>:<a|d>; session scoping is
// server-side in the hub).
func ApprovalKeyboard(approvalID string) *Keyboard {
	return &Keyboard{Buttons: [][]Button{
		{
			{Text: "Allow", CallbackData: "ap:" + approvalID + ":a"},
			{Text: "Deny", CallbackData: "ap:" + approvalID + ":d"},
		},
	}}
}
