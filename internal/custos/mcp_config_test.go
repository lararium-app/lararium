package custos_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestMCPConfigLoaderHelper runs as a subprocess when
// GO_WANT_HELPER_PROCESS=1. It loads config from TEST_CONFIG_PATH, which
// prints errors to stderr and exits non-zero per MCP-SPEC §3.
func TestMCPConfigLoaderHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	cfgPath := os.Getenv("TEST_CONFIG_PATH")
	_ = custos.LoadConfigFile(cfgPath)
	os.Exit(0)
}

func runLoaderSubprocess(t *testing.T, yamlContent string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	//nolint:gosec // helper executes test binary as subprocess
	cmd := exec.Command(os.Args[0], "-test.run=TestMCPConfigLoaderHelper", "--")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"TEST_CONFIG_PATH="+cfgPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run subprocess: %v", err)
		}
	}
	return stderr.String(), exitCode
}

func createFakeExecutable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-mcp-bin")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
	return p
}

// V2's loader-rejection table: one subtest per reason name, each
// asserting the reason string appears on stderr and exit is non-zero
// (MCP-SPEC §3, §12 V2).
func TestV2_LoaderRejectionTable(t *testing.T) {
	fakeExec := createFakeExecutable(t)

	tests := []struct {
		reason string
		yaml   string
	}{
		{
			reason: custos.ReasonUnknownKey,
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        unknown_key_field: true
`, fakeExec),
		},
		{
			reason: custos.ReasonDuplicateName,
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: dup-server
        transport: stdio
        command: %s
      - name: dup-server
        transport: stdio
        command: %s
`, fakeExec, fakeExec),
		},
		{
			reason: custos.ReasonReservedName,
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: gmail
        transport: stdio
        command: %s
`, fakeExec),
		},
		{
			reason: custos.ReasonBadTransport,
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: sse
        url: https://mcp.example.com/mcp
`,
		},
		{
			reason: custos.ReasonMissingKey,
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
`,
		},
		{
			reason: custos.ReasonBadCommand,
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: relative/path/to/binary
`,
		},
		{
			reason: custos.ReasonBadURL,
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: http
        url: http://example.com/mcp
`,
		},
		{
			reason: custos.ReasonBadMaxConcurrent,
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        max_concurrent_calls: 17
`, fakeExec),
		},
		{
			reason: custos.ReasonBadName,
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: Bad_Name_Upper
        transport: stdio
        command: %s
`, fakeExec),
		},
	}

	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			stderr, exitCode := runLoaderSubprocess(t, tc.yaml)
			if exitCode == 0 {
				t.Fatalf("reason %s: expected non-zero exit code, got 0; stderr: %q",
					tc.reason, stderr)
			}
			if !strings.Contains(stderr, tc.reason) {
				t.Fatalf("reason %s: expected %q in stderr; got stderr: %q",
					tc.reason, tc.reason, stderr)
			}
		})
	}
}

// In-process validation of each rejection reason per MCP-SPEC §3, §12 V2.
func TestV2_LoaderRejections_InProcess(t *testing.T) {
	fakeExec := createFakeExecutable(t)

	tests := []struct {
		name       string
		yaml       string
		wantReason string
	}{
		{
			name: "unknown_key inside mcp entry",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        bogus_key: 123
`, fakeExec),
			wantReason: custos.ReasonUnknownKey,
		},
		{
			name: "unknown_key inside connectors",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    unknown_connector: true
    mcp:
      - name: test
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonUnknownKey,
		},
		{
			name: "unknown_key inside custos",
			yaml: `
custos:
  unknown_custos_field: true
`,
			wantReason: custos.ReasonUnknownKey,
		},
		{
			name: "duplicate_name",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: foo
        transport: stdio
        command: %s
      - name: foo
        transport: stdio
        command: %s
`, fakeExec, fakeExec),
			wantReason: custos.ReasonDuplicateName,
		},
		{
			name: "reserved_name gmail",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: gmail
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonReservedName,
		},
		{
			name: "reserved_name custos",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: custos
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonReservedName,
		},
		{
			name: "bad_transport sse",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: sse
        url: https://mcp.example.com/mcp
`,
			wantReason: custos.ReasonBadTransport,
		},
		{
			name: "bad_transport empty",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: ""
        url: https://mcp.example.com/mcp
`,
			wantReason: custos.ReasonBadTransport,
		},
		{
			name: "missing_key stdio command",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
`,
			wantReason: custos.ReasonMissingKey,
		},
		{
			name: "missing_key http url",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: http
`,
			wantReason: custos.ReasonMissingKey,
		},
		{
			name: "bad_command relative",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: relative/bin
`,
			wantReason: custos.ReasonBadCommand,
		},
		{
			name: "bad_command nonexistent",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: /path/that/does/not/exist/ever
`,
			wantReason: custos.ReasonBadCommand,
		},
		{
			name: "bad_url public http",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: http
        url: http://example.com/mcp
`,
			wantReason: custos.ReasonBadURL,
		},
		{
			name: "bad_url invalid scheme ftp",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: http
        url: ftp://127.0.0.1/mcp
`,
			wantReason: custos.ReasonBadURL,
		},
		{
			name: "bad_url unparseable",
			yaml: `
custos:
  connectors:
    mcp:
      - name: test
        transport: http
        url: "://invalid"
`,
			wantReason: custos.ReasonBadURL,
		},
		{
			name: "bad_max_concurrent 0",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        max_concurrent_calls: 0
`, fakeExec),
			wantReason: custos.ReasonBadMaxConcurrent,
		},
		{
			name: "bad_max_concurrent 17",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        max_concurrent_calls: 17
`, fakeExec),
			wantReason: custos.ReasonBadMaxConcurrent,
		},
		{
			name: "bad_max_concurrent negative",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test
        transport: stdio
        command: %s
        max_concurrent_calls: -3
`, fakeExec),
			wantReason: custos.ReasonBadMaxConcurrent,
		},
		{
			name: "bad_name uppercase",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: TestServer
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name slash forbidden",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test/server
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name starts with digit",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: 1server
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name starts with underscore",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: _server
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name starts with dash",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: -server
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name contains dot",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: server.name
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name empty",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: ""
        transport: stdio
        command: %s
`, fakeExec),
			wantReason: custos.ReasonBadName,
		},
		{
			name: "bad_name too long (33 chars)",
			yaml: fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: %s
        transport: stdio
        command: %s
`, strings.Repeat("a", 33), fakeExec),
			wantReason: custos.ReasonBadName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := custos.ParseConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error for %s, got cfg: %+v", tc.name, cfg)
			}
			gotReason := custos.ErrorReason(err)
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q; err: %v", gotReason, tc.wantReason, err)
			}
		})
	}
}

// Accept cases: full stdio entry, full http entry, defaults, env forms.
func TestAcceptCases(t *testing.T) {
	fakeExec := createFakeExecutable(t)

	yamlContent := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: full-stdio
        transport: stdio
        command: %s
        args: ["--root", "/srv/books", "--verbose"]
        env:
          VAULT_KEY: ${vault:fs-key}
          ENV_PATH: ${env:PATH}
          LITERAL: literal-api-secret
        tool_allowlist: ["read_file", "search"]
        tool_denylist: ["delete_file"]
        max_concurrent_calls: 8
        list_timeout: 45s
      - name: full-http
        transport: http
        url: https://mcp.example.com/mcp
        bearer: ${vault:example-mcp-token}
        tool_allowlist: ["fetch"]
        tool_denylist: []
        max_concurrent_calls: 16
        list_timeout: 10s
      - name: defaults-stdio
        transport: stdio
        command: %s
`, fakeExec, fakeExec)

	cfg, err := custos.ParseConfig([]byte(yamlContent))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if len(cfg.Connectors.MCP) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(cfg.Connectors.MCP))
	}

	// 1. Full stdio entry assertions
	s0 := cfg.Connectors.MCP[0]
	if s0.Name != "full-stdio" {
		t.Errorf("Name = %q, want full-stdio", s0.Name)
	}
	if s0.Transport != "stdio" {
		t.Errorf("Transport = %q, want stdio", s0.Transport)
	}
	if s0.Command != fakeExec {
		t.Errorf("Command = %q, want %q", s0.Command, fakeExec)
	}
	if len(s0.Args) != 2 || s0.Args[0] != "--root" || s0.Args[1] != "/srv/books" {
		// Note: args slice had 3 elements in yaml
		if len(s0.Args) != 3 || s0.Args[2] != "--verbose" {
			t.Errorf("Args = %v, want [--root /srv/books --verbose]", s0.Args)
		}
	}
	if s0.Env["VAULT_KEY"] != "${vault:fs-key}" {
		t.Errorf("Env[VAULT_KEY] = %q, want ${vault:fs-key}", s0.Env["VAULT_KEY"])
	}
	if s0.Env["ENV_PATH"] != "${env:PATH}" {
		t.Errorf("Env[ENV_PATH] = %q, want ${env:PATH}", s0.Env["ENV_PATH"])
	}
	if s0.Env["LITERAL"] != "literal-api-secret" {
		t.Errorf("Env[LITERAL] = %q, want literal-api-secret", s0.Env["LITERAL"])
	}
	if len(s0.ToolAllowlist) != 2 || s0.ToolAllowlist[0] != "read_file" {
		t.Errorf("ToolAllowlist = %v, want [read_file search]", s0.ToolAllowlist)
	}
	if len(s0.ToolDenylist) != 1 || s0.ToolDenylist[0] != "delete_file" {
		t.Errorf("ToolDenylist = %v, want [delete_file]", s0.ToolDenylist)
	}
	if s0.MaxConcurrentCalls != 8 {
		t.Errorf("MaxConcurrentCalls = %d, want 8", s0.MaxConcurrentCalls)
	}
	if s0.ListTimeout != 45*time.Second {
		t.Errorf("ListTimeout = %v, want 45s", s0.ListTimeout)
	}

	// 2. Full http entry assertions
	s1 := cfg.Connectors.MCP[1]
	if s1.Name != "full-http" {
		t.Errorf("Name = %q, want full-http", s1.Name)
	}
	if s1.Transport != "http" {
		t.Errorf("Transport = %q, want http", s1.Transport)
	}
	if s1.URL != "https://mcp.example.com/mcp" {
		t.Errorf("URL = %q, want https://mcp.example.com/mcp", s1.URL)
	}
	if s1.Bearer != "${vault:example-mcp-token}" {
		t.Errorf("Bearer = %q, want ${vault:example-mcp-token}", s1.Bearer)
	}
	if s1.MaxConcurrentCalls != 16 {
		t.Errorf("MaxConcurrentCalls = %d, want 16", s1.MaxConcurrentCalls)
	}
	if s1.ListTimeout != 10*time.Second {
		t.Errorf("ListTimeout = %v, want 10s", s1.ListTimeout)
	}

	// 3. Defaults entry assertions
	s2 := cfg.Connectors.MCP[2]
	if s2.Name != "defaults-stdio" {
		t.Errorf("Name = %q, want defaults-stdio", s2.Name)
	}
	if s2.MaxConcurrentCalls != 4 {
		t.Errorf("default MaxConcurrentCalls = %d, want 4", s2.MaxConcurrentCalls)
	}
	if s2.ListTimeout != 30*time.Second {
		t.Errorf("default ListTimeout = %v, want 30s", s2.ListTimeout)
	}
}

// I8 security invariant: http host gate per MCP-SPEC §11 I8.
// http:// allowed ONLY for loopback/RFC1918 hosts; https always allowed;
// public http:// rejected with bad_url.
func TestI8_HTTPHostGate(t *testing.T) {
	tests := []struct {
		url        string
		wantAccept bool
	}{
		// Spec-specified cases in brief:
		{"http://127.0.0.1:8080/mcp", true},
		{"http://192.168.1.7:9/mcp", true},
		{"http://example.com/mcp", false},
		{"https://example.com/mcp", true},

		// Additional loopback cases:
		{"http://localhost:8080/mcp", true},
		{"http://[::1]:8080/mcp", true},
		{"http://sub.localhost:8080/mcp", true},

		// Additional RFC 1918 cases:
		{"http://10.0.0.1:8080/mcp", true},
		{"http://10.255.255.254:8080/mcp", true},
		{"http://172.16.0.1:8080/mcp", true},
		{"http://172.31.255.254:8080/mcp", true},
		{"http://192.168.0.1:8080/mcp", true},

		// Public HTTP cases (rejected bad_url):
		{"http://8.8.8.8:8080/mcp", false},
		{"http://1.1.1.1/mcp", false},
		{"http://172.15.0.1/mcp", false},  // outside 172.16..172.31
		{"http://172.32.0.1/mcp", false},  // outside 172.16..172.31
		{"http://192.167.1.1/mcp", false}, // outside 192.168
	}

	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			yamlContent := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test-server
        transport: http
        url: %q
`, tc.url)
			cfg, err := custos.ParseConfig([]byte(yamlContent))
			if tc.wantAccept {
				if err != nil {
					t.Fatalf("expected url %q to be accepted, got error: %v", tc.url, err)
				}
				if len(cfg.Connectors.MCP) != 1 || cfg.Connectors.MCP[0].URL != tc.url {
					t.Fatalf("url not preserved properly: %+v", cfg.Connectors.MCP)
				}
			} else {
				if err == nil {
					t.Fatalf("expected url %q to be rejected, but succeeded", tc.url)
				}
				if reason := custos.ErrorReason(err); reason != custos.ReasonBadURL {
					t.Fatalf("expected reason %q for url %q, got %q (err: %v)",
						custos.ReasonBadURL, tc.url, reason, err)
				}
			}
		})
	}
}

// Reserved name and duplicate name checks per MCP-SPEC §3.
func TestReservedNameAndDuplicateName(t *testing.T) {
	fakeExec := createFakeExecutable(t)

	// 1. Reserved name "gmail"
	gmailYAML := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: gmail
        transport: stdio
        command: %s
`, fakeExec)
	_, err := custos.ParseConfig([]byte(gmailYAML))
	if err == nil || custos.ErrorReason(err) != custos.ReasonReservedName {
		t.Fatalf("expected reserved_name for gmail, got: %v", err)
	}

	// 2. Reserved name "custos"
	custosYAML := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: custos
        transport: stdio
        command: %s
`, fakeExec)
	_, err = custos.ParseConfig([]byte(custosYAML))
	if err == nil || custos.ErrorReason(err) != custos.ReasonReservedName {
		t.Fatalf("expected reserved_name for custos, got: %v", err)
	}

	// 3. Duplicate name
	dupYAML := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: my-server
        transport: stdio
        command: %s
      - name: my-server
        transport: stdio
        command: %s
`, fakeExec, fakeExec)
	_, err = custos.ParseConfig([]byte(dupYAML))
	if err == nil || custos.ErrorReason(err) != custos.ReasonDuplicateName {
		t.Fatalf("expected duplicate_name, got: %v", err)
	}
}

// TestBadCommandDetailed checks command verification rules:
// absolute path, existing regular file, executable permissions.
func TestBadCommandDetailed(t *testing.T) {
	dir := t.TempDir()

	// Non-executable file (mode 0644)
	nonExecPath := filepath.Join(dir, "non-exec-file")
	if err := os.WriteFile(nonExecPath, []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Directory
	dirPath := filepath.Join(dir, "sub-dir")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		command string
	}{
		{"relative command", "my-exec"},
		{"non-existent command", filepath.Join(dir, "absent-bin")},
		{"non-executable file", nonExecPath},
		{"directory", dirPath},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yamlContent := fmt.Sprintf(`
custos:
  connectors:
    mcp:
      - name: test-cmd
        transport: stdio
        command: %q
`, tc.command)
			_, err := custos.ParseConfig([]byte(yamlContent))
			if err == nil {
				t.Fatalf("%s: expected bad_command error, got nil", tc.name)
			}
			if reason := custos.ErrorReason(err); reason != custos.ReasonBadCommand {
				t.Fatalf("%s: reason = %q, want %q; err: %v",
					tc.name, reason, custos.ReasonBadCommand, err)
			}
		})
	}
}
