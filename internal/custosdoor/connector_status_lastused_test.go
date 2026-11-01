package custosdoor

import (
	"encoding/json"
	"testing"
)

// last_used on the MCP-STATUS row is additive (CONNECTOR-LIFE-SPEC D6): rows
// from an old custosd parse unchanged, new rows expose it, and a re-marshal of
// an unused row stays byte-identical to the old shape.
func TestConnectorStatusLastUsedAdditive(t *testing.T) {
	old := `{"name":"alpha","state":"ready","tools":1,"restarts":0,"reason":"ok"}`
	var st ConnectorStatus
	if err := json.Unmarshal([]byte(old), &st); err != nil {
		t.Fatal(err)
	}
	if st.LastUsed != nil {
		t.Fatalf("old row grew last_used: %v", *st.LastUsed)
	}
	if b, _ := json.Marshal(st); string(b) != old {
		t.Fatalf("old shape changed: %s", b)
	}

	var st2 ConnectorStatus
	newRow := `{"name":"alpha","state":"disabled","tools":0,"restarts":0,"reason":"user_disabled","last_used":"2026-10-31T12:00:00Z"}`
	if err := json.Unmarshal([]byte(newRow), &st2); err != nil {
		t.Fatal(err)
	}
	if st2.LastUsed == nil || *st2.LastUsed != "2026-10-31T12:00:00Z" || st2.Reason != "user_disabled" {
		t.Fatalf("new row = %+v", st2)
	}

	// An old client (no LastUsed field) must ignore the new key.
	var legacy struct {
		Name   string `json:"name"`
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(newRow), &legacy); err != nil || legacy.State != "disabled" {
		t.Fatalf("legacy parse: %+v %v", legacy, err)
	}
}
