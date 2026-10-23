// MCP-CALL verb tests — issue #89 (MCP-SPEC §9, amendment A3):
// MCP-TOOLS carries inputSchema (JCS canonical JSON) and MCP-CALL
// executes a connector tool, replying `OK <json>` / `ERR <reason>`.

package custos_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

type callEchoArgs struct {
	Message string `json:"message"`
}

// toolRow mirrors the MCP-TOOLS frame row shape (A3).
type toolRow struct {
	Name        string          `json:"name"`
	Flags       string          `json:"flags"`
	Title       string          `json:"title"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// newCallTestRegistry spins an in-process MCP server with one schema'd
// tool and one no-arg tool and returns a RailRegistry whose connector
// is READY.
func newCallTestRegistry(t *testing.T) *custos.RailRegistry {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "call-fake", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "echoes the message back",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args callEchoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + args.Message}},
		}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ping",
		Description: "no-argument liveness tool",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong"}},
		}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "boom",
		Description: "fails with a multi-line server error",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("server failure\nFORGED OK injected\r\nsecond line")
	})

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{
		{Name: "alpha", Transport: "http", URL: ts.URL},
	}, t.TempDir())
	conn := reg.Connector("alpha")
	if conn == nil {
		t.Fatal("registry missing alpha connector")
	}
	if err := conn.OnFirstUnlock(context.Background()); err != nil {
		t.Fatalf("OnFirstUnlock: %v", err)
	}
	if st := conn.Status(); st.State != custos.MCPStateReady {
		t.Fatalf("connector state = %q (%s), want ready", st.State, st.Reason)
	}
	return reg
}

func captureWrite(t *testing.T, got *string) func([]byte) error {
	t.Helper()
	return func(b []byte) error {
		*got += string(b)
		return nil
	}
}

// MCP-TOOLS frames carry inputSchema per tool (A3), single-line and
// JSON-parseable, with the declared property present.
func TestMCPCall_ToolsFrameCarriesInputSchema(t *testing.T) {
	reg := newCallTestRegistry(t)

	var got string
	if !custos.DispatchMCPVerb(nil, reg, "MCP-TOOLS", "alpha", true, captureWrite(t, &got)) {
		t.Fatal("DispatchMCPVerb did not handle MCP-TOOLS")
	}
	if !strings.HasPrefix(got, "OK ") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("frame shape wrong: %q", got)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(got, "OK "), "\n")
	if strings.Contains(body, "\n") {
		t.Fatalf("frame not single-line: %q", body)
	}
	var rows []toolRow
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("tools body not JSON: %v (%s)", err, body)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want echo+ping+boom", rows)
	}
	var echo *toolRow
	for i := range rows {
		if rows[i].Name == "echo" {
			echo = &rows[i]
		}
	}
	if echo == nil {
		t.Fatalf("echo row missing: %+v", rows)
	}
	if len(echo.InputSchema) == 0 {
		t.Fatalf("inputSchema missing from row: %s", body)
	}
	var schema map[string]any
	if err := json.Unmarshal(echo.InputSchema, &schema); err != nil {
		t.Fatalf("inputSchema not a JSON object: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["message"]; !ok {
		t.Fatalf("inputSchema.properties lacks message: %s", echo.InputSchema)
	}
	// Canonical form must be stable across two dispatches (JCS).
	var got2 string
	custos.DispatchMCPVerb(nil, reg, "MCP-TOOLS", "alpha", true, captureWrite(t, &got2))
	if strings.TrimPrefix(got2, "OK ") != strings.TrimPrefix(got, "OK ") {
		t.Fatalf("inputSchema not canonical-stable:\n%s\n%s", got, got2)
	}
}

// Happy path: MCP-CALL executes the tool and returns the raw result.
func TestMCPCall_Success(t *testing.T) {
	reg := newCallTestRegistry(t)

	var got string
	if !custos.DispatchMCPCall(nil, reg, "alpha", "echo", `{"message":"hi"}`, captureWrite(t, &got), custos.MCPCallGuard{}) {
		t.Fatal("DispatchMCPCall returned false")
	}
	if !strings.HasPrefix(got, "OK ") {
		t.Fatalf("want OK, got %q", got)
	}
	if !strings.Contains(got, "echo:hi") {
		t.Fatalf("result missing tool output: %q", got)
	}
}

// Empty args default to {} so no-arg tools can be called bare.
func TestMCPCall_EmptyArgsDefault(t *testing.T) {
	reg := newCallTestRegistry(t)

	var got string
	custos.DispatchMCPCall(nil, reg, "alpha", "ping", "", captureWrite(t, &got), custos.MCPCallGuard{})
	if !strings.HasPrefix(got, "OK ") || !strings.Contains(got, "pong") {
		t.Fatalf("empty args should default to {}: %q", got)
	}
}

// A server-supplied error must never break the single-line frame
// contract: newlines in Execute reasons would forge frames on the
// control channel (oneLineReason sanitizer).
func TestMCPCall_ServerErrorStaysSingleLine(t *testing.T) {
	reg := newCallTestRegistry(t)

	var got string
	custos.DispatchMCPCall(nil, reg, "alpha", "boom", "{}", captureWrite(t, &got), custos.MCPCallGuard{})
	// The SDK surfaces tool failures as an isError result frame (json-
	// marshaled, so newlines are escaped) or as a Go error (sanitized
	// by oneLineReason). Both shapes must keep the one-line contract.
	if !strings.HasPrefix(got, "ERR ") && !strings.HasPrefix(got, "OK ") {
		t.Fatalf("want ERR or OK, got %q", got)
	}
	if strings.ContainsRune(strings.TrimRight(got, "\n"), '\n') || strings.Contains(got, "\r") {
		t.Fatalf("ERR frame contains raw newlines: %q", got)
	}
}

func TestMCPCall_Errors(t *testing.T) {
	reg := newCallTestRegistry(t)

	cases := []struct {
		name, conn, tool, args, want string
	}{
		{"unknown connector", "nope", "echo", `{}`, "not found"},
		{"unknown tool", "alpha", "no_such_tool", `{}`, ""},
		{"invalid args JSON", "alpha", "echo", `{oops`, "bad_args"},
		{"missing tool name", "alpha", "", `{}`, "bad_request"},
		{"missing connector name", "", "echo", `{}`, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if !custos.DispatchMCPCall(nil, reg, tc.conn, tc.tool, tc.args, captureWrite(t, &got), custos.MCPCallGuard{}) {
				t.Fatal("DispatchMCPCall returned false")
			}
			if !strings.HasPrefix(got, "ERR ") {
				t.Fatalf("want ERR, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want ERR containing %q, got %q", tc.want, got)
			}
			if strings.ContainsRune(strings.TrimRight(got, "\n"), '\n') {
				t.Fatalf("ERR frame not single-line: %q", got)
			}
		})
	}
}
