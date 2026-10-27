package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cd") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestStartCustosDoorUnsetIsInert(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "doors.sock")
	if stop := startCustosDoor(t.Context(), CustosConfig{}, nil, nil, nil); stop != nil {
		t.Fatal("unset config started a door client")
	}
	// Half-set: warned and disabled, never started.
	if stop := startCustosDoor(t.Context(), CustosConfig{DoorsSock: sock}, nil, nil, nil); stop != nil {
		t.Fatal("half-set config started a door client")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket path touched: %v", err)
	}
}

func TestStartCustosDoorSurvivesAbsentDaemonAndStops(t *testing.T) {
	dir := shortTmp(t)
	tok := filepath.Join(dir, "door.token")
	if err := os.WriteFile(tok, []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startCustosDoor(t.Context(), CustosConfig{DoorsSock: filepath.Join(dir, "doors.sock"), DoorToken: tok}, nil, nil, nil)
	if stop == nil {
		t.Fatal("configured door client not started")
	}
	done := make(chan struct{})
	go func() { stop(); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return")
	}
}

func TestLoadConfigCustosKeys(t *testing.T) {
	dir := shortTmp(t)
	p := filepath.Join(dir, "lararium.yaml")
	body := "hearth:\n  home: " + dir + "\nmodels:\n  default: [local/m]\nproviders:\n  - name: local\n    base_url: http://127.0.0.1:1\ncustos:\n  doors_sock: /run/x/doors.sock\n  door_token: /run/x/door.token\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Custos.DoorsSock != "/run/x/doors.sock" || c.Custos.DoorToken != "/run/x/door.token" || c.Custos.DoorName != "" {
		t.Fatalf("custos = %+v", c.Custos)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCustosDoor_ConnectAndDisconnectLogging(t *testing.T) {
	var logBuf syncBuffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOutput)

	dir := shortTmp(t)
	sockPath := filepath.Join(dir, "doors.sock")
	tokenPath := filepath.Join(dir, "door.token")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statusFrame := "MCP-STATUS [{\"name\":\"demo\",\"state\":\"ready\",\"tools\":1,\"restarts\":0,\"reason\":\"ok\"}]\n" +
		`MCP-TOOLS demo [{"name":"echo","title":"Echo the input"}]`
	cleanUp := fakeDoor(t, sockPath, statusFrame)

	stop := startCustosDoor(context.Background(), CustosConfig{DoorsSock: sockPath, DoorToken: tokenPath}, nil, nil, nil)
	if stop == nil {
		cleanUp()
		t.Fatal("startCustosDoor returned nil stop")
	}
	defer stop()

	// Wait for connect and rail tool update logs
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out := logBuf.String()
		if strings.Contains(out, "custos door: connected") &&
			strings.Contains(out, "custos door: 1 rail tools available") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	out := logBuf.String()
	if !strings.Contains(out, "custos door: connected") {
		t.Fatalf("expected 'custos door: connected' in log, got:\n%s", out)
	}
	if !strings.Contains(out, "custos door: 1 rail tools available") {
		t.Fatalf("expected 'custos door: 1 rail tools available' in log, got:\n%s", out)
	}

	// Trigger disconnect by terminating fake door
	cleanUp()

	// Wait for disconnect log
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "custos door: disconnected") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	out = logBuf.String()
	if !strings.Contains(out, "custos door: disconnected") {
		t.Fatalf("expected 'custos door: disconnected' in log, got:\n%s", out)
	}

	// Let retry loops run briefly to verify disconnect is not spammed
	time.Sleep(250 * time.Millisecond)
	count := strings.Count(logBuf.String(), "custos door: disconnected")
	if count != 1 {
		t.Fatalf("expected 'custos door: disconnected' to be logged exactly once, got %d times\nLog:\n%s", count, logBuf.String())
	}
}

func TestREPLBanner_WithCustosConfig(t *testing.T) {
	cfg := &Config{
		Custos: CustosConfig{
			DoorsSock: "/tmp/doors.sock",
			DoorToken: "/tmp/door.token",
		},
	}
	var buf bytes.Buffer
	replBanner(&buf, cfg)
	want := "note: rail tools (mcp__*) work in the web chat via 'hearthd serve', not in this REPL.\n"
	if buf.String() != want {
		t.Fatalf("replBanner with custos = %q, want %q", buf.String(), want)
	}
}

func TestREPLBanner_NoCustosConfig(t *testing.T) {
	cfg := &Config{}
	var buf bytes.Buffer
	replBanner(&buf, cfg)
	if buf.String() != "" {
		t.Fatalf("replBanner without custos = %q, want empty", buf.String())
	}

	buf.Reset()
	replBanner(&buf, nil)
	if buf.String() != "" {
		t.Fatalf("replBanner with nil cfg = %q, want empty", buf.String())
	}
}
