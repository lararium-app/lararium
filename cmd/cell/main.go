package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lararium-app/lararium/internal/cell"
)

// runSystem runs a host command, returning combined output + error.
func runSystem(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// shellJoin prepares argv for the in-cell /bin/sh -c (agy review
// F8). Two documented forms:
//   - single part  → passed through verbatim: a deliberate shell
//     string ("echo hi; id", pipes, redirects keep working —
//     live-verified usage).
//   - multiple parts → each part single-quoted so word boundaries
//     survive ('ls "my dir"' stays two words).
func shellJoinQuoted(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: cell <command> [arguments]

Commands:
  doctor              Check environment prerequisites
  build-template      Build rootfs template (requires root)
  create <id>         Create a new cell
  start <id>          Boot the cell container
  run <id> -- <cmd>   Execute a command inside the cell
  stop <id>           Stop the cell container
  status [id]         Show cell status
  list                List all cells
  destroy <id>        Destroy the cell (stop + unmount + remove)
  snapshot <id>       Snapshot the cell's upper layer
  restore <id> <ts>   Restore from a snapshot

Flags:
  --config <path>     Path to lararium.yaml (default: /etc/lararium/lararium.yaml)
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	configFlag := flag.String("config", "/etc/lararium/lararium.yaml", "path to lararium.yaml")

	var cmd string
	var cmdArgs []string

	// Parse: strip global --config [PATH], first non-flag is the
	// subcommand, everything after it passes through untouched.
	args := os.Args[1:]
	stripped := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --config requires a path")
				os.Exit(2)
			}
			*configFlag = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			*configFlag = strings.TrimPrefix(arg, "--config=")
		default:
			stripped = append(stripped, arg)
		}
	}
	if len(stripped) > 0 {
		cmd = stripped[0]
		cmdArgs = stripped[1:]
	}

	if cmd == "" {
		usage()
		os.Exit(2)
	}

	// Load config.
	cfg, err := cell.LoadConfig(*configFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Determine root directory.
	root := "/var/lib/lararium"
	if env := os.Getenv("LARARIUM_CELL_ROOT"); env != "" {
		root = env
	}

	runner := &cell.CmdRunner{}
	store := cell.NewStore(root, cfg.Hearth, cfg.Cell, runner)

	switch cmd {
	case "doctor":
		doctorCmd(store)
	case "build-template":
		buildTemplateCmd(store)
	case "create":
		createCmd(store, cmdArgs)
	case "start":
		startCmd(store, cmdArgs)
	case "run":
		runCmd(store, cmdArgs)
	case "stop":
		stopCmd(store, cmdArgs)
	case "status":
		statusCmd(store, cmdArgs)
	case "list":
		listCmd(store)
	case "destroy":
		destroyCmd(store, cmdArgs)
	case "snapshot":
		snapshotCmd(store, cmdArgs)
	case "restore":
		restoreCmd(store, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
}

func doctorCmd(store *cell.Store) {
	results, err := store.Doctor()
	for _, r := range results {
		fmt.Printf("%-20s %s  %s\n", r.Name, r.Status, r.Detail)
	}
	if err != nil {
		os.Exit(1)
	}
}

func buildTemplateCmd(store *cell.Store) {
	if err := store.BuildTemplate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("template built successfully")
}

func createCmd(store *cell.Store, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	id := args[0]
	if err := store.Create(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s created\n", id)
}

func startCmd(store *cell.Store, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	id := args[0]
	if err := store.Start(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s started\n", id)
}

func runCmd(store *cell.Store, args []string) {
	// Parse: run <id> [--cwd P] [--timeout S] -- <cmd>
	if len(args) < 1 || args[0] == "--" || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "error: missing cell id (usage: cell run <id> [-- cmd...])\n")
		os.Exit(1)
	}

	id := args[0]
	args = args[1:]

	opts := cell.RunOpts{}
	var cmdParts []string

	for len(args) > 0 {
		switch args[0] {
		case "--cwd":
			if len(args) < 2 {
				fmt.Fprintf(os.Stderr, "error: --cwd requires a value\n")
				os.Exit(1)
			}
			opts.CWD = args[1]
			args = args[2:]
		case "--timeout":
			if len(args) < 2 {
				fmt.Fprintf(os.Stderr, "error: --timeout requires a value\n")
				os.Exit(1)
			}
			// Parse timeout.
			timeoutStr := args[1]
			var timeout int
			fmt.Sscanf(timeoutStr, "%d", &timeout)
			opts.Timeout = timeout
			args = args[2:]
		case "--":
			args = args[1:]
			cmdParts = args
			args = nil
		default:
			cmdParts = append(cmdParts, args[0])
			args = args[1:]
		}
	}

	if len(cmdParts) == 0 {
		fmt.Fprintf(os.Stderr, "error: missing command\n")
		os.Exit(1)
	}

	// Quote each argv part for the in-cell /bin/sh -c so word
	// boundaries survive (agy review F8: a bare join mangled
	// `ls "my dir"` into two args). Single quotes make the parts
	// literal; '|'/'>' typed by the user stay OUTSIDE quotes as the
	// spec's shell-string form intends.
	cmdStr := shellJoinQuoted(cmdParts)
	res, err := store.Run(id, cmdStr, opts)
	if err != nil {
		if err == cell.ErrNotRunning {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Streams stay separated; the in-cell exit code is ours.
	os.Stdout.Write(res.Stdout)
	os.Stderr.Write(res.Stderr)
	code := res.ExitCode
	if code < 0 {
		code = 1 // killed by signal inside the cell
	}
	os.Exit(code)
}

func stopCmd(store *cell.Store, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	id := args[0]
	if err := store.Stop(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s stopped\n", id)
}

func statusCmd(store *cell.Store, args []string) {
	if len(args) == 0 {
		// List all.
		statuses, err := store.ListStatus()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		for _, st := range statuses {
			state := "stopped"
			if st.Running {
				state = "running"
			}
			fmt.Printf("%-15s %-10s mounted=%v\n", st.ID, state, st.Mounted)
		}
		return
	}

	id := args[0]
	st, err := store.Status(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	state := "stopped"
	if st.Running {
		state = "running"
	}
	fmt.Printf("%-15s %-10s mounted=%v\n", st.ID, state, st.Mounted)
}

func listCmd(store *cell.Store) {
	ids, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	for _, id := range ids {
		fmt.Println(id)
	}
}

func destroyCmd(store *cell.Store, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	id := args[0]
	if err := store.Destroy(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s destroyed\n", id)
}

func snapshotCmd(store *cell.Store, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	id := args[0]
	if err := store.Snapshot(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s snapshotted\n", id)
}

func restoreCmd(store *cell.Store, args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "error: usage: cell restore <id> <timestamp>\n")
		os.Exit(1)
	}
	id := args[0]
	ts := args[1]

	// Restore replaces upper — the cell must be stopped (same
	// split-brain hazard as snapshot).
	if store.IsActive(id) {
		fmt.Fprintf(os.Stderr, "error: cell %s is running; stop it before restore\n", id)
		os.Exit(1)
	}

	snapUpper := fmt.Sprintf("%s/snapshots/%s/upper", store.CellDir(id), ts)
	currentUpper := store.UpperDir(id)

	if _, err := os.Stat(snapUpper); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: snapshot %s not found\n", ts)
		os.Exit(1)
	}

	// Unmount if still mounted (a stopped cell is already unmounted),
	// replace upper, remount.
	if store.IsOverlayMounted(id) {
		if err := store.UnmountOverlay(id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// The snapshot must SURVIVE the restore (spec §2: "historical
	// uppers kept for restore" — agy review F4: a rename consumed it,
	// so re-restoring the same ts failed). Copy the snapshot tree
	// (cp -a: mode/ownership/xattrs), then drop the old upper only
	// after the copy completed, via an out-of-the-way rename so a
	// crash mid-delete never leaves upper/ half-removed.
	staging := currentUpper + ".restoring"
	oldUpper := currentUpper + ".old"
	os.RemoveAll(staging)
	if err := os.RemoveAll(oldUpper); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if _, err := runSystem("cp", "-a", snapUpper, staging); err != nil {
		os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "error: copy snapshot: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(currentUpper, oldUpper); err != nil {
		os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(staging, currentUpper); err != nil {
		// Roll back the swap.
		os.Rename(oldUpper, currentUpper)
		os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	os.RemoveAll(oldUpper)

	if err := store.MountOverlay(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("cell %s restored from %s\n", id, ts)
}
