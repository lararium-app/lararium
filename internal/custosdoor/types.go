// Package custosdoor is the hearthd side of the custos fan-out door
// channel (CUSTOS-SPEC §6.4b, "The door client (hearthd side)"). It
// attaches to custosd's doors.sock, mirrors the pending custody cards
// into a hearthd-internal registry, and settles verdicts back over the
// same connection. It is deliberately NOT the ApprovalHub: the
// tool-approval state machine is untouched.
package custosdoor

import (
	"errors"
	"regexp"
)

// Card is one pending custody card (§6.4a wire shape).
type Card struct {
	ID         string `json:"id"`
	Cell       string `json:"cell"`
	Cred       string `json:"cred"`
	Tool       string `json:"tool"`
	Dest       string `json:"dest"`
	Review     string `json:"review"`
	AgeS       int    `json:"age_s"`
	ExpiresInS int    `json:"expires_in_s"`
}

// Gone is a terminal transition for a card (§6.4b GONE frame). The
// client also synthesizes {State: "card_dead", Reason: "door_down"}
// when the door connection drops under rendered cards.
type Gone struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// Frozen enums and synthesized GONE values.
const (
	StateCardDead  = "card_dead"
	ReasonDoorDown = "door_down"
	StateApproved  = "approved"
	StateDenied    = "denied"
	ViaWeb         = "web"
	ViaTelegram    = "telegram"
	VerdictOnce    = "once"
	VerdictAlways  = "always"
	VerdictDeny    = "deny"
)

const defaultDoorName = "hearthd"

// Event is a custody card event holding either a Card or Gone transition.
// Exactly one of Card or Gone is non-nil.
type Event struct {
	Card *Card
	Gone *Gone
}

// Registry mirrors the contract surface.CustosRegistry exports.
type Registry interface {
	Snapshot() []Card
	Subscribe(f func(card *Card, gone *Gone)) (cancel func())
	Resolve(id, verdict string) (state string, err error)
	Attach() (cards []Card, cancel func(), events <-chan Event)
}

// Sentinels mapped from the door's synchronous acks. Alias to the
// surface sentinels once slice 3 merges.
var (
	ErrCardAnswered = errors.New("custos card already answered")
	ErrStaleVerdict = errors.New("custos card verdict stale")
	ErrLocked       = errors.New("credential custody locked")
	ErrNoSuchCard   = errors.New("no such custos card")
	ErrIPAskOnly    = errors.New("custos card is allow-once only (ip destination)")

	// ErrDoorDown: no live door connection to carry the verb.
	ErrDoorDown = errors.New("custos door not connected")
	// ErrBadToken: custosd refused the door token.
	ErrBadToken = errors.New("custos door token refused")
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// idRe keeps card ids inert on the wire: no whitespace or newline,
	// so a verb can never be split into a second frame.
	idRe = regexp.MustCompile(`^[0-9A-Za-z_.:-]{1,128}$`)
)

// ValidName reports whether s satisfies the HELLO name grammar.
func ValidName(s string) bool { return nameRe.MatchString(s) }
