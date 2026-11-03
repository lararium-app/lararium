package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lararium-app/lararium/internal/surface"
)

func printGoalUsage() {
	fmt.Fprintln(os.Stderr, `Usage: hearthd goal new [<title>] --objective <req> [--turns <n>] [--session <s>] [--step-after <dur>]
       hearthd goal list [--json]
       hearthd goal pause <id>
       hearthd goal resume <id>
       hearthd goal cancel <id>
       hearthd goal close <id>`)
}

func goalCmd(cfgPath string, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printGoalUsage()
		if len(args) == 0 {
			exitFunc(2)
		}
		exitFunc(0)
	}

	switch args[0] {
	case "new":
		goalNewCmd(cfgPath, args[1:])
	case "list":
		goalListCmd(cfgPath, args[1:])
	case "pause":
		goalLifecycleCmd(cfgPath, "pause", args[1:])
	case "resume":
		goalLifecycleCmd(cfgPath, "resume", args[1:])
	case "cancel":
		goalLifecycleCmd(cfgPath, "cancel", args[1:])
	case "close":
		goalLifecycleCmd(cfgPath, "close", args[1:])
	default:
		printGoalUsage()
		exitFunc(2)
	}
}

func goalNewCmd(cfgPath string, args []string) {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("goal new", flag.ContinueOnError)
	var objectives stringSliceFlag
	fs.Var(&objectives, "objective", "goal objective / requirement (repeatable)")
	var requirements stringSliceFlag
	fs.Var(&requirements, "requirement", "goal requirement (repeatable)")
	turnsFlag := fs.Int("turns", 20, "turn budget (default 20, cap 100)")
	budgetFlag := fs.Int("budget", 0, "turn budget alias")
	sessionFlag := fs.String("session", "", "dedicated session ID (default goal-<id>)")
	stepAfterFlag := fs.String("step-after", "", "step cadence (default 15m)")

	if err := fs.Parse(args); err != nil {
		exitFunc(2)
		return
	}

	// SPEC-SILENCE: CLI accepts both --objective and --requirement, --turns and --budget for goal new
	var reqs []string
	reqs = append(reqs, objectives...)
	reqs = append(reqs, requirements...)

	pos := fs.Args()
	title := ""
	if len(pos) > 0 {
		title = pos[0]
	}
	if title == "" && len(reqs) > 0 {
		title = reqs[0]
	}

	if len(reqs) == 0 {
		fmt.Fprintln(os.Stderr, "at least one --objective is required")
		exitFunc(2)
		return
	}

	turns := *turnsFlag
	if *budgetFlag > 0 {
		turns = *budgetFlag
	}
	if turns > 100 {
		turns = 100
	}
	if turns <= 0 {
		turns = 20
	}

	req := map[string]any{
		"verb":         "new",
		"entity":       "goal",
		"title":        title,
		"requirements": reqs,
		"turn_budget":  turns,
	}
	if *sessionFlag != "" {
		req["session"] = *sessionFlag
	}
	if *stepAfterFlag != "" {
		req["step_after"] = *stepAfterFlag
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
	fmt.Println(ack.ID)
}

func goalListCmd(cfgPath string, args []string) {
	fs := flag.NewFlagSet("goal list", flag.ContinueOnError)
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
		"entity": "goal",
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
	fmt.Fprintln(w, "ID\tSTATUS\tTURNS\tBUDGET\tNEXT DUE")
	for _, line := range lines {
		var row surface.GoalSummary
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		nextDue := row.NextDue
		if nextDue == "" {
			nextDue = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", row.ID, row.Status, row.TurnCount, row.TurnBudget, nextDue)
	}
	_ = w.Flush()
}

func goalLifecycleCmd(cfgPath, op string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: hearthd goal %s <id>\n", op)
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
		"entity": "goal",
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
