package main

import "testing"

func TestStrayConnectorsWarning(t *testing.T) {
	if w := strayConnectorsWarning([]byte("connectors:\n  - name: x\n")); w == "" {
		t.Fatal("stray top-level connectors must warn")
	}
	if w := strayConnectorsWarning([]byte("custos:\n  connectors:\n    - name: x\n")); w != "" {
		t.Fatalf("correct shape must stay silent, got %q", w)
	}
	if w := strayConnectorsWarning([]byte("not: [valid: yaml: {{{")); w != "" {
		t.Fatalf("unparseable must stay silent, got %q", w)
	}
}
