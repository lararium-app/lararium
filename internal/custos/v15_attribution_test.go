package custos

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lararium-app/lararium/internal/surface"
)

func b64u(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func startAttributionDoorHarness(t *testing.T) (*Vault, *surface.ApprovalHub, string, string) {
	t.Helper()
	SetTestAgeWorkFactor(10)
	dir, err := shortTestDir(t)
	if err != nil {
		t.Fatalf("short temp dir: %v", err)
	}

	passphrase := "test-passphrase-v15"
	v := NewVault(dir, 5*time.Second)
	if err := v.Init(passphrase); err != nil {
		t.Fatalf("Vault.Init: %v", err)
	}
	if err := v.Unlock(passphrase, false); err != nil {
		t.Fatalf("Vault.Unlock: %v", err)
	}
	t.Cleanup(func() { _ = v.Lock() })

	hub := surface.NewApprovalHub(10 * time.Second)
	srv, err := StartDoorServer(dir, hub, WithVault(v))
	if err != nil {
		t.Fatalf("StartDoorServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	sockPath, _ := DoorPaths(dir)
	token, err := ReadDoorToken(dir)
	if err != nil {
		t.Fatalf("ReadDoorToken: %v", err)
	}
	return v, hub, sockPath, token
}

func shortTestDir(t *testing.T) (string, error) {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lrd") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d, nil
}

func dialRawDoor(t *testing.T, sockPath, token, name string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	br := bufio.NewReader(conn)
	if _, err := conn.Write([]byte("HELLO " + token + " " + name + "\n")); err != nil {
		t.Fatalf("write HELLO: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read HELLO ack: %v", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		t.Fatalf("HELLO ack = %q, want OK ...", line)
	}
	return conn, br
}

func sendDoorCmd(t *testing.T, conn net.Conn, br *bufio.Reader, cmd string) string {
	t.Helper()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("write %q: %v", cmd, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read ack for %q: %v", cmd, err)
		}
		line = strings.TrimSpace(line)
		// Skip asynchronous CARD/GONE frames.
		if strings.HasPrefix(line, "CARD ") || strings.HasPrefix(line, "GONE ") {
			continue
		}
		return line
	}
}

func registerTestCard(hub *surface.ApprovalHub) string {
	id, _ := hub.RegisterCustos("cell-1", "mail", "api.example.com:send", "review text", 10*time.Second, nil)
	return id
}

func TestV15_DoorWireParser(t *testing.T) {
	v, hub, sockPath, token := startAttributionDoorHarness(t)
	conn, br := dialRawDoor(t, sockPath, token, "v15_parser")

	t.Run("TailLessCompat", func(t *testing.T) {
		c1 := registerTestCard(hub)
		if got := sendDoorCmd(t, conn, br, "APPROVE "+c1+" once via web"); got != "OK" {
			t.Fatalf("APPROVE tail-less = %q, want OK", got)
		}
		rec, err := v.Audit().ReadTailRecord()
		if err != nil || rec == nil {
			t.Fatalf("ReadTailRecord: %v", err)
		}
		if rec.Via != "web" || rec.DeviceID != "" || rec.DisplayName != "" || rec.UserAgent != "" {
			t.Fatalf("tail-less rec = %+v, want empty attribution", rec)
		}

		c2 := registerTestCard(hub)
		if got := sendDoorCmd(t, conn, br, "DENY "+c2+" via telegram"); got != "OK" {
			t.Fatalf("DENY tail-less = %q, want OK", got)
		}
		rec, _ = v.Audit().ReadTailRecord()
		if rec.Via != "telegram" || rec.Reason != "denied" || rec.DeviceID != "" || rec.DisplayName != "" || rec.UserAgent != "" {
			t.Fatalf("deny tail-less rec = %+v", rec)
		}
	})

	t.Run("FullTailHappy", func(t *testing.T) {
		c1 := registerTestCard(hub)
		cmd := "APPROVE " + c1 + " once via device dev phone-1 dn " + b64u("Pocket Phone") + " ua " + b64u("LarariumApp/1.0")
		if got := sendDoorCmd(t, conn, br, cmd); got != "OK" {
			t.Fatalf("full tail = %q, want OK", got)
		}
		rec, err := v.Audit().ReadTailRecord()
		if err != nil || rec == nil {
			t.Fatalf("ReadTailRecord: %v", err)
		}
		if rec.Via != "device" || rec.DeviceID != "phone-1" || rec.DisplayName != "Pocket Phone" || rec.UserAgent != "LarariumApp/1.0" {
			t.Fatalf("full tail rec = %+v", rec)
		}

		c2 := registerTestCard(hub)
		cmd2 := "DENY " + c2 + " via web ua " + b64u("Browser/1.0 (TestOS)")
		if got := sendDoorCmd(t, conn, br, cmd2); got != "OK" {
			t.Fatalf("web ua = %q, want OK", got)
		}
		rec2, _ := v.Audit().ReadTailRecord()
		if rec2.Via != "web" || rec2.DeviceID != "" || rec2.DisplayName != "" || rec2.UserAgent != "Browser/1.0 (TestOS)" {
			t.Fatalf("web ua rec = %+v", rec2)
		}
	})

	t.Run("LegacyDeviceNoTail", func(t *testing.T) {
		c := registerTestCard(hub)
		if got := sendDoorCmd(t, conn, br, "APPROVE "+c+" once via device"); got != "OK" {
			t.Fatalf("legacy device = %q, want OK", got)
		}
		rec, _ := v.Audit().ReadTailRecord()
		if rec.Via != "device" || rec.DeviceID != "" || rec.DisplayName != "" || rec.UserAgent != "" {
			t.Fatalf("legacy device rec = %+v, want empty identity fields", rec)
		}
	})

	t.Run("SlugsNamedLikeKeywords", func(t *testing.T) {
		for _, slug := range []string{"dn", "ua", "dev"} {
			c := registerTestCard(hub)
			cmd := "APPROVE " + c + " once via device dev " + slug + " dn " + b64u("Name "+slug) + " ua " + b64u("App/1.0")
			if got := sendDoorCmd(t, conn, br, cmd); got != "OK" {
				t.Fatalf("slug=%q got %q, want OK", slug, got)
			}
			rec, _ := v.Audit().ReadTailRecord()
			if rec.DeviceID != slug || rec.DisplayName != "Name "+slug || rec.UserAgent != "App/1.0" {
				t.Fatalf("slug=%q rec = %+v", slug, rec)
			}
		}
	})

	t.Run("GrammarRejections", func(t *testing.T) {
		validB64 := b64u("ok")
		stdPlus := "ab+c"
		stdSlash := "ab/c"
		padded := base64.URLEncoding.EncodeToString([]byte("hi")) // has '='
		oversized := strings.Repeat("A", 513)
		longSlug := strings.Repeat("a", 33)

		cases := []struct {
			name string
			cmd  string
			want string
		}{
			{"missing_value", "APPROVE c1 once via device dev", "ERR bad_request"},
			{"duplicate_dev", "APPROVE c1 once via device dev phone-1 dev phone-2", "ERR bad_request"},
			{"duplicate_ua", "APPROVE c1 once via web ua " + validB64 + " ua " + validB64, "ERR bad_request"},
			{"reordered_ua_before_dev", "APPROVE c1 once via device ua " + validB64 + " dev phone-1", "ERR bad_request"},
			{"reordered_dn_before_dev", "APPROVE c1 once via device dn " + validB64 + " dev phone-1", "ERR bad_request"},
			{"reordered_ua_before_dn", "APPROVE c1 once via device dev phone-1 ua " + validB64 + " dn " + validB64, "ERR bad_request"},
			{"unknown_key", "APPROVE c1 once via device foo bar", "ERR bad_request"},
			{"dn_without_dev", "APPROVE c1 once via device dn " + validB64, "ERR bad_request"},
			{"dev_on_telegram", "APPROVE c1 once via telegram dev phone-1", "ERR bad_request"},
			{"ua_on_telegram", "DENY c1 via telegram ua " + validB64, "ERR bad_request"},
			{"dev_on_web", "APPROVE c1 once via web dev phone-1", "ERR bad_request"},
			{"dn_on_web", "DENY c1 via web dn " + validB64, "ERR bad_request"},
			{"bad_slug_upper", "APPROVE c1 once via device dev Phone-1", "ERR bad_request"},
			{"bad_slug_leading_dash", "APPROVE c1 once via device dev -phone", "ERR bad_request"},
			{"bad_slug_too_long", "APPROVE c1 once via device dev " + longSlug, "ERR bad_request"},
			{"b64_plus", "APPROVE c1 once via web ua " + stdPlus, "ERR bad_request"},
			{"b64_slash", "APPROVE c1 once via web ua " + stdSlash, "ERR bad_request"},
			{"b64_padding", "APPROVE c1 once via web ua " + padded, "ERR bad_request"},
			{"b64_undecodable", "APPROVE c1 once via web ua !!!", "ERR bad_request"},
			{"b64_bad_length", "APPROVE c1 once via web ua a", "ERR bad_request"},
			{"oversized_value", "APPROVE c1 once via web ua " + oversized, "ERR bad_request"},
			{"bad_via_still_bad_source", "APPROVE c1 once via bogus dev phone-1", "ERR bad_source"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := sendDoorCmd(t, conn, br, tc.cmd)
				if got != tc.want {
					t.Fatalf("cmd %q -> %q, want %q", tc.cmd, got, tc.want)
				}
			})
		}

		// Verify connection survived the oversized value and all bad_request errors.
		c := registerTestCard(hub)
		if got := sendDoorCmd(t, conn, br, "APPROVE "+c+" once via web ua "+b64u("Survivor/1.0")); got != "OK" {
			t.Fatalf("post-error valid command = %q, want OK (connection must survive)", got)
		}
		rec, _ := v.Audit().ReadTailRecord()
		if rec.UserAgent != "Survivor/1.0" {
			t.Fatalf("post-error rec.UserAgent = %q, want Survivor/1.0", rec.UserAgent)
		}
	})
}

func TestV15_SanitizeAuditFreeText(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		capRunes  int
		wantSub   string
		forbidSub []string
	}{
		{
			name:      "email_redacted",
			in:        "Phone of user@example.com",
			capRunes:  64,
			wantSub:   "[redacted]",
			forbidSub: []string{"user@example.com"},
		},
		{
			name:      "ipv4_redacted",
			in:        "host 192.0.2.42 client",
			capRunes:  64,
			wantSub:   "[redacted]",
			forbidSub: []string{"192.0.2.42"},
		},
		{
			name:      "ipv6_redacted",
			in:        "host 2001:db8::1 and ::1 end",
			capRunes:  64,
			wantSub:   "[redacted]",
			forbidSub: []string{"2001:db8::1", "::1"},
		},
		{
			name:      "opaque_32_redacted",
			in:        "tok " + strings.Repeat("a", 32) + " ok",
			capRunes:  64,
			wantSub:   "tok [redacted] ok",
			forbidSub: []string{strings.Repeat("a", 32)},
		},
		{
			name:      "bearer_redacted",
			in:        "Auth Bearer secret12345 done",
			capRunes:  64,
			wantSub:   "Auth [redacted] done",
			forbidSub: []string{"secret12345", "Bearer"},
		},
		{
			name:      "controls_bidi_separators_stripped",
			in:        "A\x00B\r\nC\u202eD\u2028E\u2029F",
			capRunes:  64,
			wantSub:   "ABCDEF",
			forbidSub: []string{"\x00", "\r", "\n", "\u202e", "\u2028", "\u2029"},
		},
		{
			name:      "invalid_utf8_replaced",
			in:        "bad\xff\xfeutf8",
			capRunes:  64,
			wantSub:   "\uFFFD",
			forbidSub: []string{"\xff", "\xfe"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAuditFreeText(tc.in, tc.capRunes)
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Fatalf("got %q, want substring %q", got, tc.wantSub)
			}
			for _, bad := range tc.forbidSub {
				if strings.Contains(got, bad) {
					t.Fatalf("got %q, must not contain %q", got, bad)
				}
			}
		})
	}

	// Cap boundaries: 64 runes for dn, 200 runes for ua (including multi-byte runes,
	// space-separated so they are not redacted as >=32-char opaque tokens).
	longDN := strings.Repeat("é ", 40) // 80 runes
	gotDN := sanitizeAuditFreeText(longDN, auditDisplayNameCap)
	if utf8.RuneCountInString(gotDN) != 64 {
		t.Fatalf("dn rune count = %d, want 64", utf8.RuneCountInString(gotDN))
	}
	longUA := strings.Repeat("x ", 150) // 300 runes, no 32-char opaque run
	gotUA := sanitizeAuditFreeText(longUA, auditUserAgentCap)
	if utf8.RuneCountInString(gotUA) != 200 {
		t.Fatalf("ua rune count = %d, want 200", utf8.RuneCountInString(gotUA))
	}
}

func TestV15_DoorScrubsWrittenAuditJSON(t *testing.T) {
	v, hub, sockPath, token := startAttributionDoorHarness(t)
	conn, br := dialRawDoor(t, sockPath, token, "v15_scrub")

	dirtyDN := "Dev\x00 user@example.com \u202e 192.0.2.10"
	dirtyUA := "App/1.0 Bearer tok_abc 2001:db8::1 " + strings.Repeat("z", 36) + "\u2028\xff"

	c := registerTestCard(hub)
	cmd := "APPROVE " + c + " once via device dev phone-1 dn " + b64u(dirtyDN) + " ua " + b64u(dirtyUA)
	if got := sendDoorCmd(t, conn, br, cmd); got != "OK" {
		t.Fatalf("cmd = %q, want OK", got)
	}

	raw, _, _, err := v.Audit().ReadTailLine()
	if err != nil {
		t.Fatalf("ReadTailLine: %v", err)
	}
	line := string(raw)
	for _, forbidden := range []string{"user@example.com", "192.0.2.10", "tok_abc", "2001:db8::1", strings.Repeat("z", 36), "\u202e", "\u2028"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("audit JSON leaked %q in %s", forbidden, line)
		}
	}
	var rec AuditRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.DeviceID != "phone-1" || !strings.Contains(rec.DisplayName, "[redacted]") || !strings.Contains(rec.UserAgent, "[redacted]") {
		t.Fatalf("unexpected scrubbed record: %+v", rec)
	}
}

func TestV15_AuditOmitemptyAndChainVerify(t *testing.T) {
	SetTestAgeWorkFactor(10)
	dir := t.TempDir()
	passphrase := "test-passphrase-chain"
	v := NewVault(dir, 5*time.Second)
	if err := v.Init(passphrase); err != nil {
		t.Fatalf("Vault.Init: %v", err)
	}
	if err := v.Unlock(passphrase, false); err != nil {
		t.Fatalf("Vault.Unlock: %v", err)
	}
	defer func() { _ = v.Lock() }()

	// Byte-identity check: non-attributed record marshals identically to AppendApprovalAnsweredVia shape.
	ts := "2026-11-25T12:00:00Z"
	pre := AuditRecord{T: ts, Kind: AuditKindApprovalAnswered, Cred: "mail", Actor: "surface", Via: "web", Reason: "ok", PrevHash: "abc"}
	post := AuditRecord{T: ts, Kind: AuditKindApprovalAnswered, Cred: "mail", Actor: "surface", Via: "web", Reason: "ok", PrevHash: "abc", DeviceID: "", DisplayName: "", UserAgent: ""}
	b1, err := json.Marshal(pre) //nolint:gosec // G117: Cred carries the credential NAME, a fake fixture in tests, never a secret value
	if err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(post) //nolint:gosec // G117: Cred carries the credential NAME, a fake fixture in tests, never a secret value
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("omitempty mismatch:\npre:  %s\npost: %s", b1, b2)
	}
	for _, key := range []string{"device_id", "display_name", "user_agent"} {
		if strings.Contains(string(b2), key) {
			t.Fatalf("empty field %q present in JSON: %s", key, b2)
		}
	}

	// Chain verification across pre-v15 and post-v15 lines.
	if err := v.Audit().AppendApprovalAnsweredVia("mail", "ok", "surface", "web"); err != nil {
		t.Fatalf("AppendApprovalAnsweredVia: %v", err)
	}
	if err := v.Audit().AppendApprovalAnsweredAttributed("mail", "ok", "surface", "device", "phone-1", "Pocket Phone", "LarariumApp/1.0"); err != nil {
		t.Fatalf("AppendApprovalAnsweredAttributed: %v", err)
	}
	if err := v.Audit().AppendApprovalAnsweredAttributed("mail", "denied", "surface", "web", "", "", "Browser/1.0"); err != nil {
		t.Fatalf("AppendApprovalAnsweredAttributed web: %v", err)
	}

	res, err := v.Audit().Verify(v.instanceKey)
	if err != nil || res.IsBroken || res.IsDangling {
		t.Fatalf("Audit.Verify failed across pre-v15 and post-v15 lines: res=%+v err=%v", res, err)
	}
}

func TestV15_Acceptance339_TwoDevicesAndNonDevice(t *testing.T) {
	v, hub, sockPath, token := startAttributionDoorHarness(t)
	conn, br := dialRawDoor(t, sockPath, token, "v15_accept")

	c1 := registerTestCard(hub)
	if got := sendDoorCmd(t, conn, br, "APPROVE "+c1+" once via device dev phone-alpha dn "+b64u("Alpha Phone")+" ua "+b64u("App/1.0")); got != "OK" {
		t.Fatalf("c1 = %q", got)
	}
	c2 := registerTestCard(hub)
	if got := sendDoorCmd(t, conn, br, "DENY "+c2+" via device dev phone-beta dn "+b64u("Beta Phone")+" ua "+b64u("App/2.0")); got != "OK" {
		t.Fatalf("c2 = %q", got)
	}
	c3 := registerTestCard(hub)
	if got := sendDoorCmd(t, conn, br, "APPROVE "+c3+" once via web ua "+b64u("WebBrowser/1.0")); got != "OK" {
		t.Fatalf("c3 = %q", got)
	}
	c4 := registerTestCard(hub)
	if got := sendDoorCmd(t, conn, br, "DENY "+c4+" via telegram"); got != "OK" {
		t.Fatalf("c4 = %q", got)
	}

	recs, err := v.Audit().ReadTailRecords(20)
	if err != nil {
		t.Fatalf("ReadTailRecords: %v", err)
	}
	var answered []AuditRecord
	for _, r := range recs {
		if r.Kind == AuditKindApprovalAnswered {
			answered = append(answered, r)
		}
	}
	if len(answered) != 4 {
		t.Fatalf("answered count = %d, want 4", len(answered))
	}
	if answered[0].DeviceID != "phone-alpha" || answered[0].DisplayName != "Alpha Phone" || answered[0].Via != "device" {
		t.Fatalf("rec[0] = %+v", answered[0])
	}
	if answered[1].DeviceID != "phone-beta" || answered[1].DisplayName != "Beta Phone" || answered[1].Via != "device" {
		t.Fatalf("rec[1] = %+v", answered[1])
	}
	if answered[2].Via != "web" || answered[2].DeviceID != "" || answered[2].DisplayName != "" || answered[2].UserAgent != "WebBrowser/1.0" {
		t.Fatalf("rec[2] = %+v", answered[2])
	}
	if answered[3].Via != "telegram" || answered[3].DeviceID != "" || answered[3].DisplayName != "" || answered[3].UserAgent != "" {
		t.Fatalf("rec[3] = %+v", answered[3])
	}
}

func TestV15_RaceTwoDevicesOneCard(t *testing.T) {
	v, hub, sockPath, token := startAttributionDoorHarness(t)
	conn1, br1 := dialRawDoor(t, sockPath, token, "race_dev1")
	conn2, br2 := dialRawDoor(t, sockPath, token, "race_dev2")

	cardID := registerTestCard(hub)
	cmd1 := "APPROVE " + cardID + " once via device dev phone-alpha dn " + b64u("Alpha Phone")
	cmd2 := "APPROVE " + cardID + " once via device dev phone-beta dn " + b64u("Beta Phone")

	startCh := make(chan struct{})
	results := make([]string, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		results[0] = sendDoorCmd(t, conn1, br1, cmd1)
	}()
	go func() {
		defer wg.Done()
		<-startCh
		results[1] = sendDoorCmd(t, conn2, br2, cmd2)
	}()
	close(startCh)
	wg.Wait()

	var winnerDev, winnerDN string
	switch {
	case results[0] == "OK" && results[1] == "ERR already_answered":
		winnerDev, winnerDN = "phone-alpha", "Alpha Phone"
	case results[1] == "OK" && results[0] == "ERR already_answered":
		winnerDev, winnerDN = "phone-beta", "Beta Phone"
	default:
		t.Fatalf("unexpected race outcomes: dev1=%q dev2=%q", results[0], results[1])
	}

	recs, err := v.Audit().ReadTailRecords(20)
	if err != nil {
		t.Fatalf("ReadTailRecords: %v", err)
	}
	var answered []AuditRecord
	for _, r := range recs {
		if r.Kind == AuditKindApprovalAnswered {
			answered = append(answered, r)
		}
	}
	if len(answered) != 1 {
		t.Fatalf("expected exactly 1 approval_answered line, got %d: %+v", len(answered), answered)
	}
	if answered[0].DeviceID != winnerDev || answered[0].DisplayName != winnerDN {
		t.Fatalf("audit line = %+v, want winner (%q, %q)", answered[0], winnerDev, winnerDN)
	}
}
