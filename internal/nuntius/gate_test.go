package nuntius

import (
	"strings"
	"testing"
)

func pairedOwners() Owners   { return Owners{OwnerIDs: []string{"42"}} }
func unpairedOwners() Owners { return Owners{} }

// V3/N2: the gate matrix — group drops (even the owner's), unpaired
// admits only /pair, non-owners refuse, owner private admits.
func TestGateMatrix(t *testing.T) {
	cases := []struct {
		name   string
		env    Envelope
		owners Owners
		want   Decision
	}{
		{"owner group drop", Envelope{FromID: "42", ChatType: "group"}, pairedOwners(), Drop},
		{"stranger group drop", Envelope{FromID: "7", ChatType: "supergroup"}, pairedOwners(), Drop},
		{"owner private admit", Envelope{FromID: "42", ChatType: "private", Text: "hi"}, pairedOwners(), Admit},
		{"stranger private refuse", Envelope{FromID: "7", ChatType: "private", Text: "hi"}, pairedOwners(), ReplyPrivate},
		{"stranger callback refuse", Envelope{FromID: "7", ChatType: "private", IsCallback: true, Text: "ap:x:a"}, pairedOwners(), ReplyPrivate},
		{"unpaired pair attempt", Envelope{FromID: "7", ChatType: "private", Text: "/pair pair1_abc"}, unpairedOwners(), PairAttempt},
		{"unpaired prose required", Envelope{FromID: "7", ChatType: "private", Text: "hello"}, unpairedOwners(), ReplyPairingRequired},
		{"unpaired help required", Envelope{FromID: "7", ChatType: "private", Text: "/help"}, unpairedOwners(), ReplyPairingRequired},
		{"unpaired callback toast", Envelope{FromID: "7", ChatType: "private", IsCallback: true}, unpairedOwners(), ReplyPairingRequired},
		{"unknown chat type drop", Envelope{FromID: "42", ChatType: "channel"}, pairedOwners(), Drop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.env, tc.owners); got != tc.want {
				t.Fatalf("Decide = %v, want %v", got, tc.want)
			}
		})
	}
}

// An update with neither message nor callback_query fails the gate
// (P4: anything undecided on the wire → refuse and log).
func TestEnvelopeUnknownKindDrops(t *testing.T) {
	env := Update{UpdateID: 1}.Envelope()
	if got := Decide(env, pairedOwners()); got != Drop {
		t.Fatalf("empty update: %v, want Drop", got)
	}
}

func TestPairCommandParsing(t *testing.T) {
	if !IsPairCommand("/pair pair1_abc123") {
		t.Fatal("bare /pair not recognized")
	}
	if !IsPairCommand("/pair@MyBot pair1_abc123") {
		t.Fatal("@BotName /pair not recognized")
	}
	if IsPairCommand("/pair") {
		t.Fatal("/pair without code is not an attempt")
	}
	if IsPairCommand("pair pair1_x") {
		t.Fatal("non-command accepted")
	}
	if got := PairCode("/pair@Bot pair1_abc123 "); got != "pair1_abc123" {
		t.Fatalf("PairCode = %q", got)
	}
}

func TestCommandName(t *testing.T) {
	cases := map[string]string{
		"/start":            "start",
		"/Cancel":           "cancel",
		"/sessions@MyBot x": "sessions",
		"hello":             "",
		"/":                 "",
		"/new first title":  "new",
	}
	for in, want := range cases {
		if got := CommandName(in); got != want {
			t.Fatalf("CommandName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Frozen strings are byte-exact (spec §3/§5).
func TestFrozenStrings(t *testing.T) {
	checks := map[string]string{
		MsgPairingRequired: "pairing required",
		MsgInvalidCode:     "invalid code",
		MsgPaired:          "paired — this bot is now yours",
		MsgAlreadyPaired:   "already paired",
		MsgPrivate:         "this bot is private",
		MsgUnknownCommand:  "unknown command",
	}
	for got, want := range checks {
		if got != want {
			t.Fatalf("frozen string mismatch: %q != %q", got, want)
		}
	}
	if !strings.HasPrefix(LogPairedOwner, "nuntius: paired owner ") {
		t.Fatalf("log line: %q", LogPairedOwner)
	}
}

// N5: approval callback data shape.
func TestApprovalKeyboard(t *testing.T) {
	kb := ApprovalKeyboard("01HXX")
	if len(kb.Buttons) != 1 || len(kb.Buttons[0]) != 2 {
		t.Fatalf("keyboard shape: %+v", kb)
	}
	allow := kb.Buttons[0][0].CallbackData
	deny := kb.Buttons[0][1].CallbackData
	if allow != "ap:01HXX:a" || deny != "ap:01HXX:d" {
		t.Fatalf("callback data: %q %q", allow, deny)
	}
}
