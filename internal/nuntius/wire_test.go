package nuntius

import (
	"encoding/json"
	"strings"
	"testing"
)

// The live Bot API sends user.id / chat.id as JSON numbers; fixtures
// historically use decimal strings. getUpdates used to fail decoding
// every real update ("cannot unmarshal number into Go struct field
// User.message.from.id of type string"), which killed the Telegram card
// leg entirely (dogfood 2026-10-08: card went undeliverable).
func TestWireIDsNumericAndString(t *testing.T) {
	numeric := `{"update_id":104017699,
	  "message":{"message_id":42,
	    "from":{"id":8910000001,"first_name":"Test User"},
	    "chat":{"id":8910000001,"type":"private"},
	    "text":"/start"}}`
	callback := `{"update_id":104017700,
	  "callback_query":{"id":"cb1","from":{"id":8910000001},
	    "message":{"message_id":43,"chat":{"id":-1001234567890,"type":"supergroup"}},
	    "data":"approve:abc"}}`
	stringIDs := `{"update_id":2,"message":{"message_id":1,
	  "from":{"id":"12345678901234567890"},
	  "chat":{"id":"777","type":"private"},"text":"hi"}}`

	for _, tc := range []struct {
		name  string
		raw   string
		from  string
		chat  string
		updID int64
	}{
		{"numeric message", numeric, "8910000001", "8910000001", 104017699},
		{"numeric callback", callback, "8910000001", "-1001234567890", 104017700},
		{"string ids", stringIDs, "12345678901234567890", "777", 2},
	} {
		var u Update
		if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		env := u.Envelope()
		if env.UpdateID != tc.updID {
			t.Fatalf("%s: update id %d want %d", tc.name, env.UpdateID, tc.updID)
		}
		if env.FromID != tc.from {
			t.Fatalf("%s: from id %q want %q", tc.name, env.FromID, tc.from)
		}
		if env.ChatID != tc.chat {
			t.Fatalf("%s: chat id %q want %q", tc.name, env.ChatID, tc.chat)
		}
	}

	// Malformed ids must be errors, not silent empty strings.
	for _, bad := range []string{
		`{"update_id":1,"message":{"message_id":1,"chat":{"id":null,"type":"private"}}}`,
		`{"update_id":1,"message":{"message_id":1,"from":{"id":1.5},"chat":{"id":"7","type":"private"}}}`,
	} {
		var u Update
		if err := json.Unmarshal([]byte(bad), &u); err == nil {
			t.Fatalf("want decode error for %s", bad)
		}
	}

	// Round-trip: string id marshals back quoted (outbound shapes stay
	// the bridge's own).
	b, err := json.Marshal(Chat{ID: "777", Type: "private"})
	if err != nil || !strings.Contains(string(b), `"id":"777"`) {
		t.Fatalf("chat marshal = %s, err %v", b, err)
	}
}
