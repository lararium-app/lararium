package custosdoor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func b64u(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestResolveAttributedWireFormat(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		via     string
		attr    Attribution
		want    string
		state   string
	}{
		{
			name:    "zero_attr_matches_prev15",
			verdict: VerdictOnce,
			via:     ViaDevice,
			attr:    Attribution{},
			want:    "APPROVE c1 once via device",
			state:   StateApproved,
		},
		{
			name:    "device_full_tail",
			verdict: VerdictAlways,
			via:     ViaDevice,
			attr:    Attribution{DeviceID: "phone-1", DisplayName: "Pocket Phone", UserAgent: "LarariumApp/1.0"},
			want:    "APPROVE c1 always via device dev phone-1 dn " + b64u("Pocket Phone") + " ua " + b64u("LarariumApp/1.0"),
			state:   StateApproved,
		},
		{
			name:    "device_id_only_no_dn",
			verdict: VerdictDeny,
			via:     ViaDevice,
			attr:    Attribution{DeviceID: "phone-1"},
			want:    "DENY c1 via device dev phone-1",
			state:   StateDenied,
		},
		{
			name:    "web_ua_only",
			verdict: VerdictOnce,
			via:     ViaWeb,
			attr:    Attribution{DeviceID: "phone-1", DisplayName: "Ignored", UserAgent: "Browser/1.0"},
			want:    "APPROVE c1 once via web ua " + b64u("Browser/1.0"),
			state:   StateApproved,
		},
		{
			name:    "telegram_no_tail",
			verdict: VerdictDeny,
			via:     ViaTelegram,
			attr:    Attribution{UserAgent: "Ignored"},
			want:    "DENY c1 via telegram",
			state:   StateDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, testToken)
			h.run(t)
			fc := h.up(t, "["+card1+"]")

			done := make(chan struct{})
			var state string
			var err error
			go func() {
				defer close(done)
				state, err = h.c.ResolveAttributed("c1", tc.verdict, tc.via, tc.attr)
			}()

			if got := fc.recv(t); got != tc.want {
				t.Fatalf("wire verb = %q, want %q", got, tc.want)
			}
			fc.send("OK")
			<-done
			if err != nil || state != tc.state {
				t.Fatalf("state=%q err=%v, want %q nil", state, err, tc.state)
			}
		})
	}
}

func TestResolveAttributedRejectsBadDeviceIDBeforeWrite(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	h.up(t, "["+card1+"]")

	for _, badID := range []string{"BAD_UPPER", "-leading", "has space", "foo\nDENY c2 via web", strings.Repeat("a", 33)} {
		_, err := h.c.ResolveAttributed("c1", VerdictOnce, ViaDevice, Attribution{DeviceID: badID})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad DeviceID %q -> err=%v, want ErrBadRequest", badID, err)
		}
	}
}

func TestResolveAttributedClientSideSanitizeAndCap(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "["+card1+"]")

	longDN := "A\x00\r\n\u202e" + strings.Repeat("b", 100)
	longUA := "U\u2028\u2029" + strings.Repeat("c", 250)

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = h.c.ResolveAttributed("c1", VerdictOnce, ViaDevice, Attribution{
			DeviceID:    "phone-1",
			DisplayName: longDN,
			UserAgent:   longUA,
		})
	}()

	got := fc.recv(t)
	fc.send("OK")
	<-done
	if err != nil {
		t.Fatalf("ResolveAttributed: %v", err)
	}

	fields := strings.Fields(got)
	// APPROVE c1 once via device dev phone-1 dn <b64> ua <b64>
	if len(fields) != 11 || fields[7] != "dn" || fields[9] != "ua" {
		t.Fatalf("unexpected verb fields: %q", got)
	}
	dnRaw, err := base64.RawURLEncoding.DecodeString(fields[8])
	if err != nil {
		t.Fatalf("decode dn: %v", err)
	}
	uaRaw, err := base64.RawURLEncoding.DecodeString(fields[10])
	if err != nil {
		t.Fatalf("decode ua: %v", err)
	}
	if utf8.RuneCount(dnRaw) != clientDisplayNameCap {
		t.Fatalf("dn rune count = %d, want %d", utf8.RuneCount(dnRaw), clientDisplayNameCap)
	}
	if utf8.RuneCount(uaRaw) != clientUserAgentCap {
		t.Fatalf("ua rune count = %d, want %d", utf8.RuneCount(uaRaw), clientUserAgentCap)
	}
	if bytes.ContainsAny(dnRaw, "\x00\r\n\u202e") || bytes.ContainsAny(uaRaw, "\u2028\u2029") {
		t.Fatalf("control/bidi chars leaked into wire encoding")
	}
}

func TestResolveAttributedDowngradeRetryOnBadSource(t *testing.T) {
	h := start(t, testToken)
	h.run(t)
	fc := h.up(t, "["+card1+"]")

	done := make(chan struct{})
	var state string
	var err error
	go func() {
		defer close(done)
		state, err = h.c.ResolveAttributed("c1", VerdictOnce, ViaDevice, Attribution{
			DeviceID:    "phone-1",
			DisplayName: "Pocket Phone",
			UserAgent:   "LarariumApp/1.0",
		})
	}()

	first := fc.recv(t)
	if !strings.Contains(first, " dev phone-1 ") {
		t.Fatalf("first verb = %q, expected tailed verb", first)
	}
	// Simulate pre-v15 custosd rejecting the tail with ERR bad_source.
	fc.send("ERR bad_source")

	retry := fc.recv(t)
	wantRetry := "APPROVE c1 once via device"
	if retry != wantRetry {
		t.Fatalf("retry verb = %q, want %q", retry, wantRetry)
	}
	fc.send("OK")
	<-done
	if err != nil || state != StateApproved {
		t.Fatalf("downgraded resolve state=%q err=%v, want %q nil", state, err, StateApproved)
	}
}

func TestMapAckBadRequest(t *testing.T) {
	_, err := mapAck("ERR bad_request", StateApproved)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("mapAck(ERR bad_request) = %v, want ErrBadRequest", err)
	}
}
