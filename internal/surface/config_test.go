package surface

import (
	"testing"
	"time"
)

func TestServeConfigNormalize(t *testing.T) {
	tests := []struct {
		name        string
		cfg         ServeConfig
		wantListen  string
		wantApprove time.Duration
		wantTurn    time.Duration
		wantErr     bool
	}{
		{
			name:        "defaults",
			cfg:         ServeConfig{},
			wantListen:  "127.0.0.1:7717",
			wantApprove: 5 * time.Minute,
			wantTurn:    10 * time.Minute,
			wantErr:     false,
		},
		{
			name:        "custom loopback",
			cfg:         ServeConfig{Listen: "127.0.0.1:8080"},
			wantListen:  "127.0.0.1:8080",
			wantApprove: 5 * time.Minute,
			wantTurn:    10 * time.Minute,
			wantErr:     false,
		},
		{
			name:        "localhost",
			cfg:         ServeConfig{Listen: "localhost:7717"},
			wantListen:  "localhost:7717",
			wantApprove: 5 * time.Minute,
			wantTurn:    10 * time.Minute,
			wantErr:     false,
		},
		{
			name:        "ipv6 loopback",
			cfg:         ServeConfig{Listen: "[::1]:7717"},
			wantListen:  "[::1]:7717",
			wantApprove: 5 * time.Minute,
			wantTurn:    10 * time.Minute,
			wantErr:     false,
		},
		{
			name:    "non-loopback without allowed_hosts fails",
			cfg:     ServeConfig{Listen: "0.0.0.0:7717"},
			wantErr: true,
		},
		{
			name:        "non-loopback with allowed_hosts succeeds",
			cfg:         ServeConfig{Listen: "0.0.0.0:7717", AllowedHosts: []string{"example.com"}},
			wantListen:  "0.0.0.0:7717",
			wantApprove: 5 * time.Minute,
			wantTurn:    10 * time.Minute,
			wantErr:     false,
		},
		{
			name:        "custom timeouts",
			cfg:         ServeConfig{Listen: "127.0.0.1:7717", ApprovalTimeout: 2 * time.Minute, TurnTimeout: 3 * time.Minute},
			wantListen:  "127.0.0.1:7717",
			wantApprove: 2 * time.Minute,
			wantTurn:    3 * time.Minute,
			wantErr:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := cfg.Normalize()
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if cfg.Listen != tc.wantListen {
				t.Errorf("Listen = %q, want %q", cfg.Listen, tc.wantListen)
			}
			if cfg.ApprovalTimeout != tc.wantApprove {
				t.Errorf("ApprovalTimeout = %v, want %v", cfg.ApprovalTimeout, tc.wantApprove)
			}
			if cfg.TurnTimeout != tc.wantTurn {
				t.Errorf("TurnTimeout = %v, want %v", cfg.TurnTimeout, tc.wantTurn)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"0.0.0.0", false},
		{"example.com", false},
		{"192.168.1.1", false},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			got := isLoopbackHost(tc.host)
			if got != tc.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
