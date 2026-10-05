// Package main implements the minimal custosd daemon per CUSTOS-SPEC.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/custos"
)

type ConfigFile struct {
	Hearth struct {
		Home string `yaml:"home"`
	} `yaml:"hearth"`
	Custos custos.Config `yaml:"custos"`
}

func loadConfig(cfgPath, hearthOverride string) (string, *custos.Config) {
	var c ConfigFile
	if raw, err := os.ReadFile(cfgPath); err == nil { //nolint:gosec // operator-provided config path
		_ = yaml.Unmarshal(raw, &c)
	}

	home := c.Hearth.Home
	if hearthOverride != "" {
		home = hearthOverride
	}
	if home == "" {
		if env := os.Getenv("HEARTH_HOME"); env != "" {
			home = env
		} else {
			home = "."
		}
	}

	stateDir := filepath.Join(home, "custos")
	c.Custos.Normalize()
	return stateDir, &c.Custos
}

func main() {
	defaultCfg := "lararium.yaml"
	if e := os.Getenv("LARARIUM_CONFIG"); e != "" {
		defaultCfg = e
	}

	cfgFlag := flag.String("config", defaultCfg, "path to lararium.yaml")
	hearthFlag := flag.String("hearth", "", "override hearth root directory")
	flag.Parse()

	args := flag.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	stateDir, cfg := loadConfig(*cfgFlag, *hearthFlag)

	switch cmd {
	case "serve":
		d := custos.NewDaemon(stateDir, cfg)
		if err := d.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "custosd: start: %v\n", err)
			os.Exit(1)
		}

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			d.Stop()
		}()

		d.Wait()

	case "status":
		client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "custosd: status: %v\n", err)
			os.Exit(1)
		}
		resp, err := client.RoundTrip("STATUS")
		if err != nil {
			fmt.Fprintf(os.Stderr, "custosd: status: %v\n", err)
			os.Exit(1)
		}

		var st custos.Status
		if err := json.Unmarshal([]byte(resp), &st); err != nil {
			fmt.Println(resp)
			return
		}

		if st.State == custos.StateDegraded {
			fmt.Printf("%s (listeners_failed: %d)\n", st.State, st.ListenersFailed)
		} else {
			fmt.Println(st.State)
		}

	case "shutdown":
		client, err := custos.NewCtlClient(stateDir, cfg.LockWaitTimeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "custosd: shutdown: %v\n", err)
			os.Exit(1)
		}
		resp, err := client.RoundTrip("SHUTDOWN")
		if err != nil {
			fmt.Fprintf(os.Stderr, "custosd: shutdown: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(resp)

	default:
		fmt.Fprintf(os.Stderr, "usage: custosd [serve|status|shutdown]\n")
		os.Exit(2)
	}
}
