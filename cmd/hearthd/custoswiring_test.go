package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestStartCustosDoorUnsetIsInert(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "doors.sock")
	if stop := startCustosDoor(t.Context(), CustosConfig{}, nil); stop != nil {
		t.Fatal("unset config started a door client")
	}
	// Half-set: warned and disabled, never started.
	if stop := startCustosDoor(t.Context(), CustosConfig{DoorsSock: sock}, nil); stop != nil {
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
	stop := startCustosDoor(t.Context(), CustosConfig{DoorsSock: filepath.Join(dir, "doors.sock"), DoorToken: tok}, nil)
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
