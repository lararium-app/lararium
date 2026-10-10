package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/memindex"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

// BK17: Offline stopped-daemon: persona files, memory/, archive/, nuntius/ byte-identical after round-trip.
func TestBK17_OfflineByteIdentity(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "b4-bk17-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	srcRoot := filepath.Join(tmpDir, "hearth")
	if err := os.MkdirAll(srcRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Persona files (representative fixtures with distinct modes)
	personaFiles := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"SOUL.md":      {content: []byte("# Soul\nCore soul personality.\n"), mode: 0o644},
		"IDENTITY.md":  {content: []byte("# Identity\nAssistant identity details.\n"), mode: 0o644},
		"USER.md":      {content: []byte("# User\nUser preferences and profile.\n"), mode: 0o644},
		"MEMORY.md":    {content: []byte("# Memory\nDurable memory store.\n"), mode: 0o600},
		"HEARTBEAT.md": {content: []byte("# Heartbeat\nPeriodic maintenance orders.\n"), mode: 0o644},
		"keys.json":    {content: []byte(`{"anthropic":"sk-ant-bk17-key"}`), mode: 0o600},
		"tokens.json":  {content: []byte(`{"label_bk17":"tok_val_bk17"}`), mode: 0o600},
	}
	for rel, f := range personaFiles {
		p := filepath.Join(srcRoot, rel)
		if err := os.WriteFile(p, f.content, f.mode); err != nil {
			t.Fatalf("write persona file %s: %v", rel, err)
		}
	}

	// 2. memory/ files (incl. at least one nested dir)
	memFiles := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"notes.md":                 {content: []byte("# Notes\nRoot memory note.\n"), mode: 0o644},
		"projects/deep/project.md": {content: []byte("# Project\nDeep project plan.\n"), mode: 0o600},
		"people/alice.md":          {content: []byte("# Alice\nLead architect.\n"), mode: 0o644},
	}
	memDirModes := map[string]os.FileMode{
		"memory":               0o755,
		"memory/projects":      0o755,
		"memory/projects/deep": 0o750,
		"memory/people":        0o755,
	}
	for d, m := range memDirModes {
		if err := os.MkdirAll(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
	}
	for rel, f := range memFiles {
		p := filepath.Join(srcRoot, "memory", rel)
		if err := os.WriteFile(p, f.content, f.mode); err != nil {
			t.Fatalf("write memory file %s: %v", rel, err)
		}
	}

	// 3. archive/ files (incl. at least one nested dir)
	archFiles := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"old-log.txt":            {content: []byte("2026-09-01: historical log text\n"), mode: 0o644},
		"nested/2026/summary.md": {content: []byte("# 2026 Summary\nArchived summary.\n"), mode: 0o640},
	}
	archDirModes := map[string]os.FileMode{
		"archive":             0o755,
		"archive/nested":      0o755,
		"archive/nested/2026": 0o750,
	}
	for d, m := range archDirModes {
		if err := os.MkdirAll(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
	}
	for rel, f := range archFiles {
		p := filepath.Join(srcRoot, "archive", rel)
		if err := os.WriteFile(p, f.content, f.mode); err != nil {
			t.Fatalf("write archive file %s: %v", rel, err)
		}
	}

	// 4. nuntius/ files (incl. at least one nested dir)
	nuntiusFiles := map[string]struct {
		content []byte
		mode    os.FileMode
	}{
		"paired.json":         {content: []byte(`{"paired":true,"owner":"user_123"}`), mode: 0o600},
		"state/sessions.json": {content: []byte(`{"active":"main","history":["main"]}`), mode: 0o644},
	}
	nuntiusDirModes := map[string]os.FileMode{
		"nuntius":       0o755,
		"nuntius/state": 0o750,
	}
	for d, m := range nuntiusDirModes {
		if err := os.MkdirAll(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(srcRoot, d), m); err != nil {
			t.Fatal(err)
		}
	}
	for rel, f := range nuntiusFiles {
		p := filepath.Join(srcRoot, "nuntius", rel)
		if err := os.WriteFile(p, f.content, f.mode); err != nil {
			t.Fatalf("write nuntius file %s: %v", rel, err)
		}
	}

	// Valid session tree so root passes verification
	sessDir := filepath.Join(srcRoot, "sessions", "main")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "session.json"), []byte(`{"id":"main"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := `{"seq":1,"t":"msg","ts":"2026-10-09T18:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "events.jsonl"), []byte(ev), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := createTestConfig(t, srcRoot)

	// Create backup via stopped-daemon offline path
	outBundle := filepath.Join(tmpDir, "bk17.lararium-backup")
	if err := backup.CreateOffline(srcRoot, cfgPath, outBundle, false, nil); err != nil {
		t.Fatalf("CreateOffline failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(outBundle, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Verify failed: err=%v, findings=%+v", err, findings)
	}

	// RestoreTo a fresh root
	restoredRoot := filepath.Join(tmpDir, "restored-hearth")
	if err := backup.RestoreTo(outBundle, restoredRoot); err != nil {
		t.Fatalf("RestoreTo failed: %v", err)
	}

	// Assert byte-identical and modes preserved for every file in persona files, memory/, archive/, nuntius/
	assertFileCategory := func(category string, relFiles []string) {
		for _, rel := range relFiles {
			srcPath := filepath.Join(srcRoot, rel)
			dstPath := filepath.Join(restoredRoot, rel)

			srcInfo, err := os.Stat(srcPath)
			if err != nil {
				t.Fatalf("[%s] stat source file %s: %v", category, rel, err)
			}
			dstInfo, err := os.Stat(dstPath)
			if err != nil {
				t.Fatalf("[%s] missing restored file %s: %v", category, rel, err)
			}

			// Mode preserved
			if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
				t.Fatalf("[%s] mode mismatch for %s: got %o, want %o", category, rel, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
			}

			// Byte identity
			srcBytes, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			dstBytes, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(srcBytes, dstBytes) {
				t.Fatalf("[%s] content mismatch for %s", category, rel)
			}
		}
	}

	var personaList []string
	for rel := range personaFiles {
		personaList = append(personaList, rel)
	}
	assertFileCategory("persona", personaList)

	var memList []string
	for rel := range memFiles {
		memList = append(memList, filepath.Join("memory", rel))
	}
	assertFileCategory("memory", memList)

	var archList []string
	for rel := range archFiles {
		archList = append(archList, filepath.Join("archive", rel))
	}
	assertFileCategory("archive", archList)

	var nuntiusList []string
	for rel := range nuntiusFiles {
		nuntiusList = append(nuntiusList, filepath.Join("nuntius", rel))
	}
	assertFileCategory("nuntius", nuntiusList)

	// Verify directory modes preserved for all nested dirs in memory/, archive/, nuntius/
	allDirModes := make(map[string]os.FileMode)
	for d, m := range memDirModes {
		allDirModes[d] = m
	}
	for d, m := range archDirModes {
		allDirModes[d] = m
	}
	for d, m := range nuntiusDirModes {
		allDirModes[d] = m
	}
	for dirRel, wantMode := range allDirModes {
		dInfo, err := os.Stat(filepath.Join(restoredRoot, dirRel))
		if err != nil {
			t.Fatalf("missing restored directory %s: %v", dirRel, err)
		}
		if dInfo.Mode().Perm() != wantMode {
			t.Fatalf("directory mode mismatch for %s: got %o, want %o", dirRel, dInfo.Mode().Perm(), wantMode)
		}
	}
}

type bk4GatedProvider struct {
	mu             sync.Mutex
	step           int
	midTurnReached chan struct{}
	gateUnblock    chan struct{}
}

func (p *bk4GatedProvider) Name() string { return "bk4-provider" }
func (p *bk4GatedProvider) Capabilities(_ context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (p *bk4GatedProvider) Complete(ctx context.Context, _ []router.Message, _ router.Options) (*router.Completion, error) {
	p.mu.Lock()
	step := p.step
	p.step++
	p.mu.Unlock()

	switch step {
	case 0:
		// Pre-backup Turn 1 reply
		return &router.Completion{Text: "turn 1 reply"}, nil
	case 1:
		// In-flight Turn 2 step 1: trigger a tool call to write tool_call + tool_result events
		return &router.Completion{
			ToolCalls: []router.ToolCall{{
				ID:       "call_bk4_1",
				Name:     "dummy_action",
				ArgsJSON: `{}`,
			}},
		}, nil
	case 2:
		// In-flight Turn 2 step 2: signal mid-turn reached and block on gate
		select {
		case p.midTurnReached <- struct{}{}:
		default:
		}
		select {
		case <-p.gateUnblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &router.Completion{Text: "turn 2 final reply"}, nil
	default:
		return &router.Completion{Text: "done"}, nil
	}
}

func (p *bk4GatedProvider) StreamComplete(ctx context.Context, msgs []router.Message, opts router.Options, _ string, onDelta router.StreamHandler) (*router.Completion, error) {
	comp, err := p.Complete(ctx, msgs, opts)
	if err != nil {
		return nil, err
	}
	if comp.Text != "" && onDelta != nil {
		_ = onDelta(comp.Text)
	}
	return comp, nil
}

// BK4: Mid-turn: long turn + socket backup → restored root: every events.jsonl seq-contiguous; in-flight turn whole-or-absent; all pre-backup turns present.
func TestBK4_MidTurnBackupCoherence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "b4-bk4-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	home := filepath.Join(tmpDir, "hearth")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("# Soul\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "IDENTITY.md"), []byte("# Identity\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "USER.md"), []byte("# User\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "MEMORY.md"), []byte("# Memory\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte("# Orders\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "keys.json"), []byte(`{"test":"key"}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, "tokens.json"), []byte(`{"test":"tok"}`), 0o600)

	if _, err := penatus.CreateSession(home, "main", "main"); err != nil {
		t.Fatal(err)
	}

	cfgPath := createTestConfig(t, home)
	sockPath := filepath.Join(home, "hearthd.sock")

	provider := &bk4GatedProvider{
		midTurnReached: make(chan struct{}, 1),
		gateUnblock:    make(chan struct{}),
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: provider}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: provider}}})

	dummyTool := loop.Tool{
		Spec: router.ToolSpec{
			Name:             "dummy_action",
			Description:      "dummy tool for BK4 test",
			ParamsJSONSchema: `{"type":"object"}`,
		},
		Trusted: true,
		Run: func(_ context.Context, _ string) (string, error) {
			return "dummy result", nil
		},
	}

	ap := surface.NewApprovalHub(5 * time.Second)
	serveCfg := surface.ServeConfig{
		ApprovalTimeout: 5 * time.Second,
		TurnTimeout:     10 * time.Second,
	}
	hub := surface.NewHub(serveCfg, home, 1000, 80, []loop.Tool{dummyTool}, rt, ap)
	srv := &surface.Server{Hub: hub}
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, hub.SingleWriterLock, progress)
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}
	defer closer.Close()

	if err := surface.PingViaSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("PingViaSocket: %v", err)
	}

	// 1. Run Pre-backup Turn 1 (present in full before in-flight turn starts)
	err = hub.RunHeadlessTurn(context.Background(), "main", "pre-backup turn 1 message", "api", 0, nil)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	// Verify pre-backup turn took backup snapshot where in-flight turn is absent
	outAbsent := filepath.Join(tmpDir, "bk4-absent.lararium-backup")
	if err := surface.BackupViaSocket(sockPath, outAbsent, false, nil); err != nil {
		t.Fatalf("backup before turn 2 failed: %v", err)
	}

	// 2. Start in-flight Turn 2 (long turn writing user msg, tool_call, tool_result, then gated before assistant terminal event)
	turn2ErrCh := make(chan error, 1)
	go func() {
		turn2ErrCh <- hub.RunHeadlessTurn(context.Background(), "main", "turn 2 long in-flight message", "api", 0, nil)
	}()

	// Wait until Turn 2 reaches mid-turn step
	select {
	case <-provider.midTurnReached:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for turn 2 to reach mid-turn")
	}

	// 3. Fire socket backup verb mid-turn:
	// Because single-writer lock is held by the in-flight turn, daemon quiesce law MUST refuse with busy
	outMidTurn := filepath.Join(tmpDir, "bk4-midturn.lararium-backup")
	errMid := surface.BackupViaSocket(sockPath, outMidTurn, false, nil)
	if errMid == nil || !strings.Contains(errMid.Error(), "busy") {
		t.Fatalf("expected busy refusal for mid-turn socket backup, got: %v", errMid)
	}

	// 4. Unblock Turn 2 to finish
	close(provider.gateUnblock)
	select {
	case err := <-turn2ErrCh:
		if err != nil {
			t.Fatalf("turn 2 failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for turn 2 completion")
	}

	// 5. Fire socket backup after Turn 2 finishes
	outWhole := filepath.Join(tmpDir, "bk4-whole.lararium-backup")
	if err := surface.BackupViaSocket(sockPath, outWhole, false, nil); err != nil {
		t.Fatalf("backup after turn 2 failed: %v", err)
	}

	// Verification helper to assert PENATUS §4.5.6 BK4 coherence properties on restored root
	verifyRestoredCoherence := func(bundlePath, restoredDir string, expectTurn2 bool) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		findings, err := backup.Verify(bundlePath, &stdout, &stderr)
		if err != nil || len(findings) != 0 {
			t.Fatalf("Verify bundle %s failed: err=%v, findings=%+v", bundlePath, err, findings)
		}

		if err := backup.RestoreTo(bundlePath, restoredDir); err != nil {
			t.Fatalf("RestoreTo failed: %v", err)
		}

		eventsPath := filepath.Join(restoredDir, "sessions", "main", "events.jsonl")
		raw, err := os.ReadFile(eventsPath)
		if err != nil {
			t.Fatalf("read restored events.jsonl: %v", err)
		}

		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) == 0 {
			t.Fatal("restored events.jsonl is empty")
		}

		var events []penatus.Event
		for idx, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev penatus.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal event line %d: %v", idx, err)
			}
			events = append(events, ev)
		}

		// (a) Every events.jsonl seq-contiguous from 1
		for i, ev := range events {
			expectedSeq := int64(i + 1)
			if ev.Seq != expectedSeq {
				t.Fatalf("seq continuity broken at index %d: got seq=%d, want=%d", i, ev.Seq, expectedSeq)
			}
		}

		// (b) All pre-backup turns present in full (Turn 1: user msg + assistant msg)
		if len(events) < 2 {
			t.Fatalf("expected at least 2 events for pre-backup turn 1, got %d", len(events))
		}
		var f0, f1 struct {
			Role string `json:"role"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(events[0].Fields["role"], &f0.Role)
		_ = json.Unmarshal(events[0].Fields["text"], &f0.Text)
		_ = json.Unmarshal(events[1].Fields["role"], &f1.Role)
		_ = json.Unmarshal(events[1].Fields["text"], &f1.Text)

		if f0.Role != "user" || f0.Text != "pre-backup turn 1 message" {
			t.Fatalf("pre-backup turn 1 user msg mismatch: %+v", f0)
		}
		if f1.Role != "assistant" || f1.Text != "turn 1 reply" {
			t.Fatalf("pre-backup turn 1 assistant msg mismatch: %+v", f1)
		}

		// (c) In-flight turn whole-or-absent (no half-written turn)
		var turn2Events []penatus.Event
		for _, ev := range events[2:] {
			turn2Events = append(turn2Events, ev)
		}

		if expectTurn2 {
			// Whole: user msg (seq 3), tool_call (seq 4), tool_result (seq 5), assistant msg (seq 6)
			if len(turn2Events) != 4 {
				t.Fatalf("expected whole in-flight turn with 4 events, got %d", len(turn2Events))
			}
			var terminal struct {
				Role string `json:"role"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(turn2Events[3].Fields["role"], &terminal.Role)
			_ = json.Unmarshal(turn2Events[3].Fields["text"], &terminal.Text)
			if terminal.Role != "assistant" || terminal.Text != "turn 2 final reply" {
				t.Fatalf("terminal assistant event missing or corrupted: %+v", terminal)
			}
		} else {
			// Absent: zero events of turn 2
			if len(turn2Events) != 0 {
				t.Fatalf("expected absent in-flight turn (0 events), got %d events", len(turn2Events))
			}
		}
	}

	// Assert whole case
	restoredWhole := filepath.Join(tmpDir, "restored-whole")
	verifyRestoredCoherence(outWhole, restoredWhole, true)

	// Assert absent case
	restoredAbsent := filepath.Join(tmpDir, "restored-absent")
	verifyRestoredCoherence(outAbsent, restoredAbsent, false)
}

// BK11: Post-restore first boot: index rebuilt; CUSTOS-SPEC §8.1a clean; audit verify green; snapshots listable.
func TestBK11_PostRestoreFirstBoot(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "b4-bk11-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	home := filepath.Join(tmpDir, "hearth")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("# Soul\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "IDENTITY.md"), []byte("# Identity\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "USER.md"), []byte("# User\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "MEMORY.md"), []byte("# Memory\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte("# Orders\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "keys.json"), []byte(`{"anthropic":"sk-ant-bk11"}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, "tokens.json"), []byte(`{"bk11_tok":"val"}`), 0o600)

	// Valid session
	sessDir := filepath.Join(home, "sessions", "main")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "session.json"), []byte(`{"id":"main"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := `{"seq":1,"t":"msg","ts":"2026-10-09T18:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "events.jsonl"), []byte(ev), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. memory/ files
	memNotes := filepath.Join(home, "memory", "notes")
	memPeople := filepath.Join(home, "memory", "people")
	if err := os.MkdirAll(memNotes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(memPeople, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memNotes, "architecture.md"), []byte("---\ntitle: Architecture\ntype: note\n---\nScalable event log design.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memPeople, "alice.md"), []byte("---\ntitle: Alice\ntype: person\n---\nLead architect on Lararium project.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. index.db present pre-backup
	preIx, err := memindex.Open(home)
	if err != nil {
		t.Fatalf("pre-backup memindex.Open: %v", err)
	}
	if _, err := preIx.Sync(context.Background()); err != nil {
		t.Fatalf("pre-backup memindex.Sync: %v", err)
	}
	_ = preIx.Close()

	preIndexDB := filepath.Join(home, "memory", "index.db")
	if _, err := os.Stat(preIndexDB); err != nil {
		t.Fatalf("pre-backup index.db missing: %v", err)
	}

	// 3. Custos snapshots pre-backup
	passphrase := "custos-secret-passphrase-bk11"
	custosStateDir := filepath.Join(home, "custos")
	v := custos.NewVault(custosStateDir, 0)
	if err := v.Init(passphrase); err != nil {
		t.Fatalf("custos Init: %v", err)
	}
	// Mutate creates gen 2 and snapshots gen 1
	err = v.Mutate(passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-openai-bk11"}
		return []string{"openai"}, nil
	}, false)
	if err != nil {
		t.Fatalf("custos Mutate: %v", err)
	}
	v.Lock()

	preGens, err := v.Snapshots().ListGenerations()
	if err != nil {
		t.Fatalf("pre-backup ListGenerations: %v", err)
	}
	if len(preGens) == 0 {
		t.Fatal("expected pre-backup snapshot generations")
	}

	cfgPath := createTestConfig(t, home)

	// 4. Create backup bundle → verify
	outBundle := filepath.Join(tmpDir, "bk11.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, outBundle, false, nil); err != nil {
		t.Fatalf("CreateOffline failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(outBundle, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Verify failed: err=%v, findings=%+v", err, findings)
	}

	// 5. RestoreTo a fresh root
	restoredRoot := filepath.Join(tmpDir, "restored-hearth")
	if err := backup.RestoreTo(outBundle, restoredRoot); err != nil {
		t.Fatalf("RestoreTo failed: %v", err)
	}

	// Assert index.db was excluded from bundle and is initially absent in restored root
	restoredIndexDB := filepath.Join(restoredRoot, "memory", "index.db")
	if _, err := os.Stat(restoredIndexDB); !os.IsNotExist(err) {
		t.Fatalf("expected index.db to be absent immediately after restore, got err=%v", err)
	}

	// 6. First boot of RESTORED root
	// (a) Swap recovery runs on startup
	if err := backup.RecoverSwap(restoredRoot); err != nil {
		t.Fatalf("RecoverSwap on restored root: %v", err)
	}

	// (b) Memindex rebuild path runs on boot/tool invocation: index.db regenerated and queryable
	tools := buildTools(restoredRoot)
	var memSearchTool *loop.Tool
	for i := range tools {
		if tools[i].Spec.Name == "memory_search" {
			memSearchTool = &tools[i]
			break
		}
	}
	if memSearchTool == nil {
		t.Fatal("memory_search tool not found in buildTools")
	}

	toolOut, err := memSearchTool.Run(context.Background(), `{"query":"Architecture"}`)
	if err != nil {
		t.Fatalf("memory_search tool execution failed: %v", err)
	}
	if !strings.Contains(toolOut, "architecture.md") {
		t.Fatalf("expected memory_search output to match architecture.md, got: %s", toolOut)
	}

	// Assert index.db now exists and is queryable
	if info, err := os.Stat(restoredIndexDB); err != nil || info.Size() == 0 {
		t.Fatalf("expected regenerated non-empty index.db at %s: err=%v", restoredIndexDB, err)
	}

	postIx, err := memindex.Open(restoredRoot)
	if err != nil {
		t.Fatalf("memindex.Open on regenerated DB: %v", err)
	}
	defer postIx.Close()
	hits, err := postIx.Search(context.Background(), "Alice", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search query on regenerated index failed: err=%v, hits=%+v", err, hits)
	}

	// (c) CUSTOS §8.1a invariant clean on first-unlock recovery
	restoredVault := custos.NewVault(filepath.Join(restoredRoot, "custos"), 0)
	if err := restoredVault.Unlock(passphrase, false); err != nil {
		t.Fatalf("restored vault first unlock failed: %v", err)
	}
	defer restoredVault.Lock()

	dangling, err := restoredVault.Audit().FindDanglingIntents()
	if err != nil {
		t.Fatalf("FindDanglingIntents failed: %v", err)
	}
	if len(dangling) != 0 {
		t.Fatalf("CUSTOS §8.1a invariant violated: found %d dangling intents: %+v", len(dangling), dangling)
	}

	// (d) audit verify green via CLI/Go entry point
	key, err := restoredVault.LoadInstanceKey()
	if err != nil {
		t.Fatalf("LoadInstanceKey failed: %v", err)
	}
	verifyRes, err := restoredVault.Audit().Verify(key)
	if err != nil {
		t.Fatalf("Audit().Verify failed: %v", err)
	}
	if verifyRes.IsBroken {
		t.Fatalf("audit verify reports broken chain: %s", verifyRes.Format())
	}
	if verifyRes.IsDangling {
		t.Fatalf("audit verify reports dangling intents: %s", verifyRes.Format())
	}

	// (e) Snapshots listable via the list path used elsewhere in tests
	restoredGens, err := restoredVault.Snapshots().ListGenerations()
	if err != nil {
		t.Fatalf("restored Snapshots().ListGenerations failed: %v", err)
	}
	if len(restoredGens) == 0 {
		t.Fatal("expected snapshots to be listable on restored root")
	}
	if restoredGens[0] != preGens[0] {
		t.Fatalf("restored snapshot generation mismatch: got %v, want %v", restoredGens, preGens)
	}
}
