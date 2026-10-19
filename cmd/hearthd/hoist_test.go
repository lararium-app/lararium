package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestHoistGlobalFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantErr string
	}{
		{
			name: "canonical order unchanged",
			args: []string{"-config", "lararium.yaml", "token", "create", "laptop"},
			want: []string{"-config", "lararium.yaml", "token", "create", "laptop"},
		},
		{
			name: "trailing -config path",
			args: []string{"token", "create", "laptop", "-config", "custom.yaml"},
			want: []string{"-config", "custom.yaml", "token", "create", "laptop"},
		},
		{
			name: "trailing -config=path",
			args: []string{"token", "create", "laptop", "-config=custom.yaml"},
			want: []string{"-config=custom.yaml", "token", "create", "laptop"},
		},
		{
			name: "trailing --config path",
			args: []string{"token", "create", "laptop", "--config", "custom.yaml"},
			want: []string{"--config", "custom.yaml", "token", "create", "laptop"},
		},
		{
			name: "-- stopping hoisting",
			args: []string{"token", "create", "laptop", "--", "-config", "custom.yaml"},
			want: []string{"token", "create", "laptop", "--", "-config", "custom.yaml"},
		},
		{
			name:    "-config last with no value erroring",
			args:    []string{"token", "create", "laptop", "-config"},
			wantErr: "flag needs an argument: -config",
		},
		{
			name: "trailing -model path",
			args: []string{"serve", "-model", "local/model1"},
			want: []string{"-model", "local/model1", "serve"},
		},
		{
			name: "trailing --model=path",
			args: []string{"serve", "--model=local/model1"},
			want: []string{"--model=local/model1", "serve"},
		},
		{
			name:    "--model last with no value erroring",
			args:    []string{"serve", "--model"},
			wantErr: "flag needs an argument: --model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hoistGlobalFlags(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hoistGlobalFlags(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}

	t.Run("setup --config X resolves to X", func(t *testing.T) {
		hoisted, err := hoistGlobalFlags([]string{"setup", "--config", "custom.yaml"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		cfgPath := fs.String("config", "lararium.yaml", "config path")
		if err := fs.Parse(hoisted); err != nil {
			t.Fatalf("flag parse failed: %v", err)
		}
		if *cfgPath != "custom.yaml" {
			t.Errorf("expected cfgPath = 'custom.yaml', got %q", *cfgPath)
		}
		subArgs := fs.Args()
		if len(subArgs) != 1 || subArgs[0] != "setup" {
			t.Errorf("expected subArgs = ['setup'], got %v", subArgs)
		}

		// Also verify setup subcommand flagset behavior
		setupFs := flag.NewFlagSet("setup", flag.ContinueOnError)
		setupCfg := setupFs.String("config", *cfgPath, "config path")
		if err := setupFs.Parse(subArgs[1:]); err != nil {
			t.Fatalf("setup sub-flagset parse failed: %v", err)
		}
		if *setupCfg != "custom.yaml" {
			t.Errorf("expected setup sub-flagset config = 'custom.yaml', got %q", *setupCfg)
		}
	})
}
