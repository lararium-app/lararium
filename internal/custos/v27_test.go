package custos_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestV27_StandaloneApprovalDoor verifies the standalone approval door per CUSTOS-SPEC §6.4a.
func TestV27_StandaloneApprovalDoor(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)

	// Ensure gmail/send asks per policy
	if _, err := h.v.Policy().AddCredentialRule("gmail/send", "ask", false, "cli", "cli"); err != nil {
		t.Fatalf("policy add ask: %v", err)
	}

	ctlServer, err := custos.StartCtlServer(h.v.StateDir(), h.v, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlServer.Close()
	ctlServer.SetWorkers(h.ws)
	ctlServer.SetHub(h.hub)

	client, err := custos.NewCtlClient(h.v.StateDir(), 5*time.Second)
	if err != nil {
		t.Fatalf("new ctl client: %v", err)
	}

	var cardID string

	// Step 1 & 2: ask policy + parked worker call -> CARDS uniform shape + APPROVE
	t.Run("AskPark_CARDS_Approve", func(t *testing.T) {
		cards, cerr := client.Cards()
		if cerr != nil || len(cards) != 0 {
			t.Fatalf("initial cards not empty: %v, %+v", cerr, cards)
		}

		respCh := make(chan custos.WorkerResponse, 1)
		go func() {
			respCh <- h.send(t, sendReq(t, v13CellToken,
				map[string]interface{}{"to": []string{"bob@example.com"}, "subject": "hi", "body": "test"}))
		}()

		var card custos.ApprovalCardWire
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cards, cerr = client.Cards()
			if cerr == nil && len(cards) == 1 {
				card = cards[0]
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if card.ID == "" {
			t.Fatalf("card never appeared in CARDS")
		}
		cardID = card.ID

		assertCardShape(t, card)

		approveResp, aerr := client.Approve(card.ID)
		if aerr != nil || approveResp != "approved" {
			t.Fatalf("approve: resp=%q, err=%v", approveResp, aerr)
		}

		select {
		case callResp := <-respCh:
			if callResp.ErrorCode != "" {
				t.Fatalf("worker call failed after approve: %+v", callResp)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("worker call never returned after approve")
		}

		assertAuditAfterApprove(t, h.v)
	})

	// Step 3: second APPROVE -> ERR already answered + stale_verdict
	t.Run("SecondApprove_AlreadyAnswered", func(t *testing.T) {
		_, aerr := client.Approve(cardID)
		if aerr == nil || !strings.Contains(aerr.Error(), "already answered") {
			t.Fatalf("second approve want already answered, got %v", aerr)
		}

		records, rerr := h.v.Audit().ReadTailRecords(0)
		if rerr != nil {
			t.Fatalf("read audit: %v", rerr)
		}
		foundStale := false
		for _, r := range records {
			if r.Kind == custos.AuditKindStaleVerdict && r.Cred == "gmail" {
				foundStale = true
			}
		}
		if !foundStale {
			t.Fatalf("audit log missing stale_verdict after second approve")
		}
	})

	// Step 4: DENY on second park -> worker_call_denied(denied_by_user) + approval_answered(denied)
	t.Run("Deny_DeniedByUser", func(t *testing.T) {
		respCh2 := make(chan custos.WorkerResponse, 1)
		go func() {
			respCh2 <- h.send(t, sendReq(t, v13CellToken,
				map[string]interface{}{"to": []string{"alice@example.com"}, "subject": "bye", "body": "test2"}))
		}()

		var card2 custos.ApprovalCardWire
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cards, cerr := client.Cards()
			if cerr == nil && len(cards) == 1 {
				card2 = cards[0]
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if card2.ID == "" {
			t.Fatalf("second card never appeared in CARDS")
		}

		denyResp, derr := client.Deny(card2.ID)
		if derr != nil || denyResp != "denied" {
			t.Fatalf("deny: resp=%q, err=%v", denyResp, derr)
		}

		select {
		case callResp := <-respCh2:
			if callResp.ErrorCode != "denied" {
				t.Fatalf("worker call err code = %q, want denied", callResp.ErrorCode)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("worker call never returned after deny")
		}

		assertAuditAfterDeny(t, h.v)
	})

	// Step 5: locked vault -> CARDS/APPROVE/DENY answer ERR locked, no state change, no audit append
	t.Run("LockedVault_Refusal", func(t *testing.T) {
		recordsBefore, err := h.v.Audit().ReadTailRecords(0)
		if err != nil {
			t.Fatalf("read audit before locked: %v", err)
		}

		if err := h.v.Lock(); err != nil {
			t.Fatalf("lock vault: %v", err)
		}
		defer func() {
			_ = h.v.Unlock("test-secret-passphrase-1234", false)
		}()

		rawCards, err := client.RoundTrip("CARDS")
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("CARDS on locked vault want ERR locked, got resp=%q, err=%v", rawCards, err)
		}

		rawApprove, err := client.RoundTripWithArg("APPROVE", "a_nonexistent")
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("APPROVE on locked vault want ERR locked, got resp=%q, err=%v", rawApprove, err)
		}

		rawDeny, err := client.RoundTripWithArg("DENY", "a_nonexistent")
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("DENY on locked vault want ERR locked, got resp=%q, err=%v", rawDeny, err)
		}

		recordsAfter, err := h.v.Audit().ReadTailRecords(0)
		if err != nil {
			t.Fatalf("read audit after locked: %v", err)
		}
		if len(recordsAfter) != len(recordsBefore) {
			t.Fatalf("expected no audit lines appended while locked, before=%d, after=%d", len(recordsBefore), len(recordsAfter))
		}
	})

	// Step 6: restart mid-park -> parked calls dropped per §3, CARDS returns OK []
	t.Run("Restart_DroppedCalls", func(t *testing.T) {
		respCh3 := make(chan custos.WorkerResponse, 1)
		go func() {
			respCh3 <- h.send(t, sendReq(t, v13CellToken,
				map[string]interface{}{"to": []string{"carol@example.com"}, "subject": "mid", "body": "test3"}))
		}()

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cards, cerr := client.Cards()
			if cerr == nil && len(cards) == 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		_ = ctlServer.Close()
		_ = h.ws.Close()
		select {
		case <-respCh3:
		case <-time.After(2 * time.Second):
		}

		cfgRestart := &custos.Config{AskHoldTimeout: 5 * time.Second}
		dRestart := custos.NewDaemon(h.v.StateDir(), cfgRestart)
		if rerr := dRestart.Start(); rerr != nil {
			t.Fatalf("daemon restart: %v", rerr)
		}
		defer dRestart.Stop()

		// Unlock daemon after restart so it can serve CARDS
		if err := dRestart.Vault().Unlock("test-secret-passphrase-1234", false); err != nil {
			t.Fatalf("unlock restart daemon: %v", err)
		}

		clientRestart, cerr := custos.NewCtlClient(h.v.StateDir(), 5*time.Second)
		if cerr != nil {
			t.Fatalf("new ctl client restart: %v", cerr)
		}

		cardsRestart, cerr := clientRestart.Cards()
		if cerr != nil || len(cardsRestart) != 0 {
			t.Fatalf("cards restart want 0: %v, %+v", cerr, cardsRestart)
		}

		rawResp, rerr := clientRestart.RoundTrip("CARDS")
		if rerr != nil || rawResp != "[]" {
			t.Fatalf("raw CARDS = %q, want [] (err: %v)", rawResp, rerr)
		}
	})

	// Step 7: nil hub -> immediate denial with reason no_approval_door
	t.Run("NilHub_NoApprovalDoor", func(t *testing.T) {
		assertNilHubRejection(t, h.v)
	})
}

func assertCardShape(t *testing.T, card custos.ApprovalCardWire) {
	t.Helper()
	if !strings.HasPrefix(card.ID, "a_") {
		t.Fatalf("card ID %q does not start with a_", card.ID)
	}
	if card.Cell != "cell-1" || card.Cred != "gmail" || card.Tool != "send" || card.Dest != "" {
		t.Fatalf("unexpected card shape: %+v", card)
	}
	if card.Review == "" {
		t.Fatalf("expected non-empty review field, got %q", card.Review)
	}
	if card.AgeS < 0 {
		t.Fatalf("expected age_s >= 0, got %d", card.AgeS)
	}
	if card.ExpiresInS <= 0 {
		t.Fatalf("expected expires_in_s > 0, got %d", card.ExpiresInS)
	}

	b, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw card: %v", err)
	}
	expectedKeys := map[string]bool{
		"id": true, "cell": true, "cred": true, "dest": true,
		"tool": true, "review": true, "age_s": true, "expires_in_s": true,
	}
	if len(raw) != len(expectedKeys) {
		t.Fatalf("expected exactly %d keys in JSON, got %d: %+v", len(expectedKeys), len(raw), raw)
	}
	for k := range raw {
		if !expectedKeys[k] {
			t.Fatalf("unexpected wire key %q in card JSON: %+v", k, raw)
		}
	}
}

func assertAuditAfterApprove(t *testing.T, v *custos.Vault) {
	t.Helper()
	records, err := v.Audit().ReadTailRecords(0)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var foundAnsweredOk, foundAllowed, foundDenied bool
	for _, r := range records {
		if r.Kind == custos.AuditKindApprovalAnswered && r.Cred == "gmail" && r.Actor == "cli" && r.Reason == "ok" {
			foundAnsweredOk = true
		}
		if r.Kind == custos.AuditKindWorkerCallAllowed && r.Cred == "gmail" && r.Tool == "send" {
			foundAllowed = true
		}
		if r.Kind == custos.AuditKindWorkerCallDenied {
			foundDenied = true
		}
	}
	if !foundAnsweredOk || !foundAllowed || foundDenied {
		t.Fatalf("audit verify approve failed: answered=%v, allowed=%v, denied=%v", foundAnsweredOk, foundAllowed, foundDenied)
	}
}

func assertAuditAfterDeny(t *testing.T, v *custos.Vault) {
	t.Helper()
	records, err := v.Audit().ReadTailRecords(0)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var foundDeniedByUser, foundAnsweredDenied bool
	for _, r := range records {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Cred == "gmail" && r.Reason == "denied_by_user" {
			foundDeniedByUser = true
		}
		if r.Kind == custos.AuditKindApprovalAnswered && r.Cred == "gmail" && r.Actor == "cli" && r.Reason == "denied" {
			foundAnsweredDenied = true
		}
	}
	if !foundDeniedByUser || !foundAnsweredDenied {
		t.Fatalf("audit verify deny failed: deniedByUser=%v, answeredDenied=%v", foundDeniedByUser, foundAnsweredDenied)
	}
}

func assertNilHubRejection(t *testing.T, v *custos.Vault) {
	t.Helper()
	cfgNil := &custos.Config{AskHoldTimeout: 5 * time.Second}
	cfgNil.Normalize()
	wsNil := custos.NewWorkerServer(v, nil, cfgNil)
	defer func() { _ = wsNil.Close() }()

	dirNil, merr := os.MkdirTemp("/tmp", "cwnil") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if merr != nil {
		t.Fatalf("mkdtemp nil: %v", merr)
	}
	defer func() { _ = os.RemoveAll(dirNil) }()
	sockNil := filepath.Join(dirNil, "wnil.sock")
	if err := wsNil.BindCell("cell-nil", sockNil, v13CellToken); err != nil {
		t.Fatalf("bind cell nil: %v", err)
	}

	connNil, err := net.Dial("unix", sockNil)
	if err != nil {
		t.Fatalf("dial nil worker: %v", err)
	}
	defer connNil.Close()

	reqNil := sendReq(t, v13CellToken, map[string]interface{}{"to": []string{"z@example.com"}, "subject": "nil", "body": "test"})
	reqNilData, err := json.Marshal(reqNil)
	if err != nil {
		t.Fatalf("marshal nil req: %v", err)
	}
	reqNilData = append(reqNilData, '\n')
	if _, err := connNil.Write(reqNilData); err != nil {
		t.Fatalf("write nil req: %v", err)
	}

	var respNil custos.WorkerResponse
	if err := json.NewDecoder(connNil).Decode(&respNil); err != nil {
		t.Fatalf("decode nil resp: %v", err)
	}
	if respNil.ErrorCode != "denied" {
		t.Fatalf("nil hub err code = %q, want denied", respNil.ErrorCode)
	}

	records, err := v.Audit().ReadTailRecords(0)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	foundNoApprovalDoor := false
	for _, r := range records {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Reason == "no_approval_door" {
			foundNoApprovalDoor = true
		}
	}
	if !foundNoApprovalDoor {
		t.Fatalf("audit log missing worker_call_denied(no_approval_door)")
	}
}
