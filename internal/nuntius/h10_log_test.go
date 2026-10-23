package nuntius

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// TestH10_RefusedLogCarriesNoBody: the unpaired-refusal log line must
// not echo the message (an unpaired sender can plant a fresh pairing
// code there) and must not let sender text forge audit lines (CRLF).
func TestH10_RefusedLogCarriesNoBody(t *testing.T) {
	b := newReplayBridge(t)
	pairOwner(t, b.pair, "owner-1")

	var buf bytes.Buffer
	b.log = log.New(&buf, "", 0)
	b.pairRL = NewLimiter(time.Hour, time.Now)
	b.pairRL.Allow("stranger-9") // consume the one reply so process() stays offline

	secret := "pair1_NOTAREALSECRET_butplanted\nnuntius: FORGED LINE admitted update 999"
	if _, err := b.inbox.Append(Record{
		UpdateID: 42, Kind: KindTurn, Payload: secret,
		SenderID: "stranger-9", ChatID: "c-9", ChatType: "private",
	}); err != nil {
		t.Fatal(err)
	}
	u := Update{
		UpdateID: 42,
		Message: &Message{
			From: &User{ID: "stranger-9", FirstName: "Eve"},
			Chat: Chat{ID: "c-9", Type: "private"},
			Text: secret,
		},
	}
	b.process(context.Background(), u, Record{UpdateID: 42, SenderID: "stranger-9", ChatID: "c-9"}, unpairedOwners())

	out := buf.String()
	if !strings.Contains(out, "refused unpaired update 42") {
		t.Fatalf("expected refusal line, got %q", out)
	}
	if strings.Contains(out, "NOTAREALSECRET") || strings.Contains(out, "FORGED") {
		t.Fatalf("message body leaked into the log: %q", out)
	}
	if strings.Count(strings.TrimSuffix(out, "\n"), "\n") != 0 {
		t.Fatalf("expected exactly one log line, got %q", out)
	}
}
