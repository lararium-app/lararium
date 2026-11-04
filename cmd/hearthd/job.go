package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/surface"
)

func resolveSocketPath(cfgPath string) (string, error) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return "", err
	}
	cfg.Hearth.Home = custos.ResolveHearthHome("", cfg.Hearth.Home)
	return filepath.Join(cfg.Hearth.Home, "hearthd.sock"), nil
}

func reorderFlags(args []string) []string {
	var flags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if arg == "--disable" || arg == "--enable" || arg == "--json" ||
				arg == "-disable" || arg == "-enable" || arg == "-json" {
				continue
			}
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}

func printJobUsage() {
	fmt.Fprintln(os.Stderr, `Usage: hearthd job add [--name <name>] (--every <dur> | --schedule <json>) [--prompt <text>] [--channel <dest>]
       hearthd job list [--json]
       hearthd job pause <id>
       hearthd job resume <id>
       hearthd job remove <id>
       hearthd job run <id>
       hearthd job edit <id> [--every <dur> | --schedule <json>] [--prompt <text>] [--enable | --disable | --enabled true|false]`)
}

func jobCmd(cfgPath string, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printJobUsage()
		if len(args) == 0 {
			exitFunc(2)
		}
		exitFunc(0)
	}

	switch args[0] {
	case "add":
		jobAddCmd(cfgPath, args[1:])
	case "list":
		jobListCmd(cfgPath, args[1:])
	case "pause":
		jobLifecycleCmd(cfgPath, "pause", args[1:])
	case "resume":
		jobLifecycleCmd(cfgPath, "resume", args[1:])
	case "remove":
		jobLifecycleCmd(cfgPath, "remove", args[1:])
	case "run":
		jobLifecycleCmd(cfgPath, "run", args[1:])
	case "edit":
		jobEditCmd(cfgPath, args[1:])
	default:
		printJobUsage()
		exitFunc(2)
	}
}

func jobAddCmd(cfgPath string, args []string) {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("job add", flag.ContinueOnError)
	nameFlag := fs.String("name", "", "job name")
	everyFlag := fs.String("every", "", "interval shorthand (e.g. 30m)")
	scheduleFlag := fs.String("schedule", "", "full schedule JSON")
	promptFlag := fs.String("prompt", "", "task prompt")
	userPromptFlag := fs.String("user-prompt", "", "task prompt alias")
	channelFlag := fs.String("channel", "", "announcement destination channel")
	announceFlag := fs.String("announce", "", "announcement destination channel")

	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}

	prompt := *promptFlag
	if prompt == "" {
		prompt = *userPromptFlag
	}
	name := *nameFlag
	channel := *channelFlag
	if channel == "" {
		channel = *announceFlag
	}

	pos := fs.Args()
	// SPEC-SILENCE: job add accepts name and prompt positionally:
	// "job add --every 30m <name> <prompt>" or "job add --name <name> --every 30m <prompt>"
	if name == "" && len(pos) > 0 {
		name = pos[0]
		pos = pos[1:]
	}
	if prompt == "" && len(pos) > 0 {
		prompt = pos[0]
	}

	if prompt == "" {
		fmt.Fprintln(os.Stderr, "prompt is required")
		exitFunc(2)
		return
	}

	var sched fasti.Schedule
	switch {
	case *everyFlag != "":
		sched = fasti.Schedule{Kind: "interval", Every: *everyFlag}
	case *scheduleFlag != "":
		if err := json.Unmarshal([]byte(*scheduleFlag), &sched); err != nil {
			fmt.Fprintf(os.Stderr, "invalid schedule JSON: %v\n", err)
			exitFunc(2)
			return
		}
	default:
		fmt.Fprintln(os.Stderr, "schedule is required: specify --every <dur> or --schedule <json>")
		exitFunc(2)
		return
	}

	req := map[string]any{
		"verb":     "add",
		"entity":   "job",
		"name":     name,
		"schedule": sched,
		"prompt":   prompt,
	}
	if channel != "" {
		req["announce"] = channel
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal request: %v\n", err)
		exitFunc(1)
		return
	}
	sockPath, err := resolveSocketPath(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	resp, err := surface.FastiMutationViaSocket(sockPath, reqData)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	var ack struct {
		ID string `json:"id"`
	}
	ackJSON := strings.TrimSpace(strings.TrimPrefix(resp, "OK"))
	if err := json.Unmarshal([]byte(ackJSON), &ack); err != nil {
		fmt.Fprintf(os.Stderr, "unmarshal ack: %v\n", err)
		exitFunc(1)
		return
	}
	fmt.Printf("%s enabled\n", ack.ID)
}

func jobListCmd(cfgPath string, args []string) {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "print raw JSON rows one per line")
	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}

	sockPath, err := resolveSocketPath(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	req := map[string]string{
		"verb":   "list",
		"entity": "job",
	}
	reqData, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal request: %v\n", err)
		exitFunc(1)
		return
	}
	lines, err := surface.FastiStreamViaSocket(sockPath, reqData)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	if *jsonFlag {
		for _, line := range lines {
			fmt.Println(line)
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSCHEDULE\tENABLED\tNEXT DUE")
	for _, line := range lines {
		var row surface.JobSummary
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		schedStr := row.Schedule.Kind
		if row.Schedule.Every != "" {
			schedStr += " " + row.Schedule.Every
		} else if row.Schedule.Expr != "" {
			schedStr += " " + row.Schedule.Expr
		}
		nextDue := row.NextDue
		if nextDue == "" {
			nextDue = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n", row.ID, row.Name, schedStr, row.Enabled, nextDue)
	}
	_ = w.Flush()
}

func jobLifecycleCmd(cfgPath, op string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: hearthd job %s <id>\n", op)
		exitFunc(2)
		return
	}
	id := args[0]
	sockPath, err := resolveSocketPath(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	req := map[string]string{
		"verb":   op,
		"entity": "job",
		"id":     id,
	}
	reqData, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal request: %v\n", err)
		exitFunc(1)
		return
	}
	_, err = surface.FastiMutationViaSocket(sockPath, reqData)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
	fmt.Println(id)
}

func jobEditCmd(cfgPath string, args []string) {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("job edit", flag.ContinueOnError)
	everyFlag := fs.String("every", "", "interval shorthand (e.g. 30m)")
	scheduleFlag := fs.String("schedule", "", "full schedule JSON")
	promptFlag := fs.String("prompt", "", "task prompt")
	userPromptFlag := fs.String("user-prompt", "", "task prompt alias")
	enabledFlag := fs.String("enabled", "", "enable or disable job (true/false)")
	disableFlag := fs.Bool("disable", false, "disable job")
	enableFlag := fs.Bool("enable", false, "enable job")

	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}

	pos := fs.Args()
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "job id is required")
		exitFunc(2)
		return
	}
	id := pos[0]

	req := map[string]any{
		"verb":   "edit",
		"entity": "job",
		"id":     id,
	}

	prompt := *promptFlag
	if prompt == "" {
		prompt = *userPromptFlag
	}
	if prompt != "" {
		req["prompt"] = prompt
	}

	if *everyFlag != "" {
		req["schedule"] = fasti.Schedule{Kind: "interval", Every: *everyFlag}
	} else if *scheduleFlag != "" {
		var s fasti.Schedule
		if err := json.Unmarshal([]byte(*scheduleFlag), &s); err != nil {
			fmt.Fprintf(os.Stderr, "invalid schedule JSON: %v\n", err)
			exitFunc(2)
			return
		}
		req["schedule"] = s
	}

	switch {
	case *disableFlag:
		req["enabled"] = false
	case *enableFlag:
		req["enabled"] = true
	case *enabledFlag != "":
		switch strings.ToLower(*enabledFlag) {
		case "true", "1", "yes":
			req["enabled"] = true
		case "false", "0", "no":
			req["enabled"] = false
		default:
			fmt.Fprintf(os.Stderr, "invalid --enabled value %q: must be true or false\n", *enabledFlag)
			exitFunc(2)
			return
		}
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal request: %v\n", err)
		exitFunc(1)
		return
	}
	sockPath, err := resolveSocketPath(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}

	_, err = surface.FastiMutationViaSocket(sockPath, reqData)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFunc(1)
		return
	}
	fmt.Println(id)
}
