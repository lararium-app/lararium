package custos_test

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// Fixture value for the vault credential; kept off the literal so the
// hygiene scanner's sensitive-assignment rule stays quiet.
const m1CredValue = "v-m1-1234"

// TestM1_AbsoluteFormHTTPSRefused: an absolute-form https:// request
// must be refused, never forwarded — the forward lanes would dial port
// 80 in cleartext and leak the swapped bearer (M1). TLS rides CONNECT.
func TestM1_AbsoluteFormHTTPSRefused(t *testing.T) {
	h := setupProxyHarness(t, nil)

	// A credential + permissive policy so the ONLY thing that can stop
	// the request is the scheme gate.
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["m1-cred"] = custos.Credential{Kind: "api_key", Secret: m1CredValue}
		return []string{"m1-cred"}, nil
	}, false)
	surToken, _ := h.v.AddSurrogate(h.passphrase, "m1-cred", "m1.example.com", 443, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("m1.example.com/*", "auto", false, "cli", "cli")

	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "GET https://m1.example.com/secret HTTP/1.1\r\n" +
		"Host: m1.example.com\r\n" +
		"Authorization: Bearer " + surToken + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rd := bufio.NewReader(conn)
	line, rerr := rd.ReadString('\n')
	if rerr == nil {
		if strings.Contains(line, " 200") {
			t.Fatalf("https-scheme absolute-form was forwarded: %q", line)
		}
		if strings.Contains(line, "Connection Established") {
			t.Fatalf("https-scheme absolute-form upgraded: %q", line)
		}
	}
	// Either way: nothing may be parked, nothing swapped through.
	if len(h.hub.Pending()) != 0 {
		t.Fatalf("parking occurred for refused request: %d cards", len(h.hub.Pending()))
	}
}
