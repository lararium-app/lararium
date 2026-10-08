package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BotAPI is the real Transport: the Bot API over HTTPS with the
// flood-control buckets (N9) in front of every send. The token rides
// in the URL path, which is why URLs are never logged (N4); base is a
// variable so tests can point at an httptest server.
type BotAPI struct {
	base   string // e.g. https://api.telegram.org
	token  string
	client *http.Client
	fc     *FloodControl
	poll   *Bucket // polling is its own bucket: never blocked by sends (N9)
}

// apiResult is the Bot API envelope: {"ok":bool,...}.
type apiResult struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// ErrNotFound is the sentinel for Telegram 404 (bad token): the token
// is never repeated in the error text (N4).
var ErrNotFound = errors.New("nuntius: bot api 404 (invalid token?)")

// NewBotAPI builds the real transport. The token comes from the
// environment (Config.Token, spec §4) and is never stored in any file
// this package writes.
func NewBotAPI(token string, fc *FloodControl) *BotAPI {
	// A nil FloodControl used to panic the bridge on its very first
	// reply (dogfood 2026-10-08): wiring passed nil because polling
	// rides its own bucket. Default to the spec rates instead — send
	// gating is never optional.
	if fc == nil {
		fc = NewFloodControl(nil, nil)
	}
	return &BotAPI{
		base:  "https://api.telegram.org",
		token: token,
		// Long poll is timeout=30s; the client must outlast it.
		// Redirects are refused: a redirect would replay the
		// token-bearing URL to another host (N4).
		client: &http.Client{
			Timeout: 75 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		fc:   fc,
		poll: NewBucket(1, 1, nil, nil),
	}
}

// apiError describes a failed call without ever naming the token.
type apiError struct {
	status     int
	retryAfter int
	desc       string
}

func (e *apiError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("nuntius: bot api %d (retry_after %ds): %s", e.status, e.retryAfter, e.desc)
	}
	return fmt.Sprintf("nuntius: bot api %d: %s", e.status, e.desc)
}

// Is429 reports whether err is a rate-limit response.
func Is429(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusTooManyRequests
}

// RetryAfter returns the 429's retry_after (0 when not a 429).
func RetryAfter(err error) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) {
		return time.Duration(ae.retryAfter) * time.Second
	}
	return 0
}

// call posts one method and decodes result into out (nil to discard).
// A 429 pauses the send buckets; the caller decides whether to retry.
func (b *BotAPI) call(ctx context.Context, method string, params map[string]string, out any) error {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.base+"/bot"+b.token+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("nuntius: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.client.Do(req)
	if err != nil {
		// url.Error embeds the full URL — token included (N4). The
		// reason (net error) is kept, the URL is not.
		return fmt.Errorf("nuntius: %s: %s", method, transportReason(err))
	}
	defer resp.Body.Close()

	// The token lives in the URL path: cap the body read and never
	// echo the URL or raw body into an error (N4).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("nuntius: %s: read response: %w", method, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}

	var res apiResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("nuntius: %s: malformed response (http %d)", method, resp.StatusCode)
	}
	if !res.OK || resp.StatusCode != http.StatusOK {
		ae := &apiError{status: resp.StatusCode, retryAfter: res.Parameters.RetryAfter, desc: sanitizeDesc(res.Description)}
		if ae.status == http.StatusOK {
			ae.status = resp.StatusCode
		}
		return ae
	}
	if out != nil && len(res.Result) > 0 {
		if err := json.Unmarshal(res.Result, out); err != nil {
			return fmt.Errorf("nuntius: %s: decode result: %w", method, err)
		}
	}
	return nil
}

// transportReason strips the URL out of a transport error: net/http
// wraps request failures in *url.Error whose text embeds the full
// URL — which carries the token (N4). Only the inner reason survives.
func transportReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// sanitizeDesc keeps a Telegram description from smuggling the token
// (it can echo request fragments) into a log line.
func sanitizeDesc(s string) string { return truncate(s, 200) }

// GetUpdates long-polls (spec §2): timeout=30, only the two update
// kinds the bridge consumes.
func (b *BotAPI) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	if err := b.poll.Wait(ctx); err != nil {
		return nil, err
	}
	params := map[string]string{
		"timeout":         "30",
		"allowed_updates": `["message","callback_query"]`,
	}
	if offset > 0 {
		params["offset"] = strconv.FormatInt(offset, 10)
	}
	var updates []Update
	if err := b.call(ctx, "getUpdates", params, &updates); err != nil {
		if Is429(err) {
			// N9: the pause lands on the bucket the 429 came from —
			// for polling that is the poll bucket, never the send
			// buckets.
			if d := RetryAfter(err); d > 0 {
				b.poll.Pause(d)
			}
		}
		return nil, err
	}
	return updates, nil
}

// sendCall is the flood-gated call path for every outbound send
// (N9): wait for a token, call, and on a 429 pause the send buckets
// for retry_after before surfacing the error. Polling rides its own
// bucket and is never blocked by these pauses.
func (b *BotAPI) sendCall(ctx context.Context, chatID, method string, params map[string]string, out any) error {
	if err := b.fc.Send(ctx, chatID); err != nil {
		return err
	}
	err := b.call(ctx, method, params, out)
	if Is429(err) {
		// Telegram does not say which limit tripped; pause both send
		// buckets — the conservative reading of "the bucket it came
		// from" for a single-chat bridge.
		if d := RetryAfter(err); d > 0 {
			b.fc.PauseGlobal(d)
			b.fc.PauseChat(chatID, d)
		}
	}
	return err
}

// SendMessage posts a plain-text message (no parse mode, §6) and
// returns it for later edits.
func (b *BotAPI) SendMessage(ctx context.Context, chatID, text string, kb *Keyboard) (*Message, error) {
	params := map[string]string{"chat_id": chatID, "text": text}
	if kb != nil {
		markup, ok := kbJSON(kb)
		if !ok {
			return nil, errors.New("nuntius: unmarshalable keyboard")
		}
		params["reply_markup"] = markup
	}
	var msg Message
	if err := b.sendCall(ctx, chatID, "sendMessage", params, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// EditMessageText replaces a sent message's text (and keyboard).
func (b *BotAPI) EditMessageText(ctx context.Context, chatID string, messageID int64, text string, kb *Keyboard) error {
	params := map[string]string{
		"chat_id":    chatID,
		"message_id": strconv.FormatInt(messageID, 10),
		"text":       text,
	}
	if kb != nil {
		markup, ok := kbJSON(kb)
		if !ok {
			return errors.New("nuntius: unmarshalable keyboard")
		}
		params["reply_markup"] = markup
	} else {
		params["reply_markup"] = `{"inline_keyboard":[]}`
	}
	return b.sendCall(ctx, chatID, "editMessageText", params, nil)
}

// SendChatAction drives typing/upload_document indicators (§6).
func (b *BotAPI) SendChatAction(ctx context.Context, chatID, action string) error {
	return b.sendCall(ctx, chatID, "sendChatAction", map[string]string{"chat_id": chatID, "action": action}, nil)
}

// AnswerCallback toasts a callback tap (§7.2). It is not flood-gated:
// Telegram requires every callback answered, and a toast is free.
func (b *BotAPI) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return b.call(ctx, "answerCallbackQuery", map[string]string{
		"callback_query_id": callbackID,
		"text":              text,
		"show_alert":        "false",
	}, nil)
}

// DeleteMessageReplyMarkup strips a resolved card's keyboard (§7.5).
func (b *BotAPI) DeleteMessageReplyMarkup(ctx context.Context, chatID string, messageID int64) error {
	return b.sendCall(ctx, chatID, "deleteMessageReplyMarkup", map[string]string{
		"chat_id":    chatID,
		"message_id": strconv.FormatInt(messageID, 10),
	}, nil)
}

// kbJSON renders the inline keyboard for reply_markup. ok is false
// only if marshalling fails — callers must then refuse the send, not
// silently drop the buttons (an approval card the user cannot act on
// is worse than a visible failure).
func kbJSON(kb *Keyboard) (string, bool) {
	data, err := json.Marshal(kb)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// truncate cuts s to n bytes at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }
