package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedConfigWithDoorsAndConnectors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lararium.yaml")
	os.WriteFile(p, []byte(`hearth:
  home: /hearth
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://x
custos:
  doors_sock: /h/doors.sock
  door_token: /h/door.token
  connectors:
    mcp:
    - name: fake
      transport: stdio
      command: /bin/cat
`), 0o600)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("shared config with connectors must load in hearthd: %v", err)
	}
	if len(cfg.Custos.Connectors.MCP) != 1 || cfg.Custos.DoorsSock == "" {
		t.Fatalf("fields lost: %+v", cfg.Custos)
	}
}
