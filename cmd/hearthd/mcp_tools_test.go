package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/loop"
)

// fakeRailDoor speaks enough doors protocol for the real custosdoor
// client: HELLO → OK snapshot, MCP-STATUS + MCP-TOOLS pushes, and an
// echo responder for MCP-CALL (issue #83 wiring).
func fakeRailDoor(t *testing.T, sockPath string) func() {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake rail door listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "HELLO ") {
					return
				}
				_, _ = c.Write([]byte("OK []\n"))
				_, _ = c.Write([]byte(`MCP-STATUS [{"name":"demo","state":"ready","tools":1,"restarts":0,"reason":"ok"}]` + "\n"))
				_, _ = c.Write([]byte(`MCP-TOOLS demo [{"name":"echo","title":"Echo the input","inputSchema":{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}}]` + "\n"))
				for {
					select {
					case <-done:
						return
					default:
					}
					_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					line, err := br.ReadString('\n')
					if err != nil {
						var ne net.Error
						if errors.As(err, &ne) && ne.Timeout() {
							continue
						}
						return
					}
					if strings.HasPrefix(line, "MCP-CALL demo echo ") {
						_, _ = c.Write([]byte(`OK {"content":[{"type":"text","text":"echo: hi"}],"isError":false}` + "\n"))
					}
				}
			}(conn)
		}
	}()
	return func() { close(done); ln.Close() }
}

func TestMCPToolSource_EndToEnd(t *testing.T) {
	dir := shortTmp(t)
	sockPath := filepath.Join(dir, "doors.sock")
	tokenPath := filepath.Join(dir, "door.token")
	if err := os.WriteFile(tokenPath, []byte("test-door-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanUp := fakeRailDoor(t, sockPath)
	defer cleanUp()

	client, err := custosdoor.New(custosdoor.Options{
		SockPath:  sockPath,
		TokenPath: tokenPath,
		Name:      "wiring-test",
	})
	if err != nil {
		t.Fatalf("custosdoor.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	src := mcpToolSource(client)
	var tools []loop.Tool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tools = src()
		if len(tools) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(tools) != 1 {
		t.Fatalf("tool source = %+v, want one demo echo tool", tools)
	}
	lt := tools[0]
	if lt.Spec.Name != "mcp__demo__echo" {
		t.Fatalf("name = %q, want mcp__demo__echo", lt.Spec.Name)
	}
	if lt.Spec.Description != "Echo the input" {
		t.Fatalf("description = %q", lt.Spec.Description)
	}
	if !strings.Contains(lt.Spec.ParamsJSONSchema, `"msg"`) {
		t.Fatalf("schema lost inputSchema: %q", lt.Spec.ParamsJSONSchema)
	}
	if lt.Trusted {
		t.Fatal("MCP tools must never be Trusted")
	}

	out, err := lt.Run(context.Background(), `{"msg":"hi"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "echo: hi" {
		t.Fatalf("Run output = %q, want %q", out, "echo: hi")
	}
}

func TestMCPToolName(t *testing.T) {
	if got := mcpToolName("github-rail", "create_pr"); got != "mcp__github-rail__create_pr" {
		t.Fatalf("plain name = %q", got)
	}
	if got := mcpToolName("a b", "tool.x!"); got != "mcp__a_b__tool_x_" {
		t.Fatalf("sanitized name = %q", got)
	}
	long := mcpToolName(strings.Repeat("c", 40), strings.Repeat("t", 40))
	if len(long) != 64 {
		t.Fatalf("long name len = %d, want 64", len(long))
	}
	if long != mcpToolName(strings.Repeat("c", 40), strings.Repeat("t", 40)) {
		t.Fatal("truncated name not deterministic")
	}
	// distinct long names must not collide
	other := mcpToolName(strings.Repeat("c", 40), strings.Repeat("t", 39)+"z")
	if other == long {
		t.Fatal("long-name collision")
	}
}

func TestRenderMCPResult(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, "a\nb"},
		{`{"content":[{"type":"text","text":"boom"}],"isError":true}`, "MCP tool error: boom"},
		{`{"content":[{"type":"image","data":"..."}]}`, "[image content]"},
		{`{"content":[]}`, "(no output)"},
		{`not json`, "not json"},
	}
	for _, c := range cases {
		if got := renderMCPResult([]byte(c.in)); got != c.want {
			t.Errorf("renderMCPResult(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
