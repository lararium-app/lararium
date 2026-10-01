// Command cell is the Lararium cell lifecycle CLI: create, start, stop,
// run, snapshot, restore, destroy, and doctor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/cell"
	"github.com/lararium-app/lararium/internal/proxy"
)

// runSystem runs a host command, returning combined output + error.
// Args reach execve directly (no shell), so caller-supplied values cannot
// inject commands -- this is the CLI's designed control surface.
func runSystem(name string, args ...string) (string, error) {
	//nolint:gosec,noctx // G204/G702: argv slice, no shell; ctx cancels wait only
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// shellJoin prepares argv for the in-cell /bin/sh -c (agy review
// F8). Two documented forms:
//   - single part  -> passed through verbatim: a deliberate shell
//     string ("echo hi; id", pipes, redirects keep working —
//     live-verified usage).
//   - multiple parts -> each part single-quoted so word boundaries
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
  create [--no-proxy] <id>
                      Create a new cell (--no-proxy opts out of egress
                      policy enforcement; hard cage applies either way)
  start <id>          Boot the cell container
  run <id> -- <cmd>   Execute a command inside the cell
  stop <id>           Stop the cell container
  status [id]         Show cell status
  list                List all cells
  destroy <id>        Destroy the cell (stop + unmount + remove)
  snapshot <id>       Snapshot the cell's upper layer
  restore <id> <ts>   Restore from a snapshot
  net-install         Install the host network cage (requires root)

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

	// Parse: strip global --config [PATH] BEFORE the subcommand only.
	// Everything from the subcommand onward passes through untouched —
	// scanning past it let `cell run c -- curl --config f` have its
	// guest argv eaten and parsed as our config path (agy round-2 F6).
	args := os.Args[1:]
	stripped := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		arg := args[i]
		// First non-flag token is the subcommand: stop global parsing.
		if arg != "--config" && !strings.HasPrefix(arg, "--config=") {
			break
		}
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --config requires a path")
				os.Exit(2)
			}
			*configFlag = args[i+1]
			i += 2
			continue
		case strings.HasPrefix(arg, "--config="):
			*configFlag = strings.TrimPrefix(arg, "--config=")
		}
		i++
	}
	stripped = append(stripped, args[i:]...)
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
	store.ProxyPort = cfg.Proxy.Port
	store.ProxyGlobal = cfg.Proxy.Enabled != nil && *cfg.Proxy.Enabled

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
	case "net-install":
		netInstallCmd(store)
	case "proxy":
		proxyCmd(store, cmdArgs)
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
	var noProxy bool
	id := ""
	for _, a := range args {
		switch a {
		case "--no-proxy":
			noProxy = true
		default:
			if id == "" {
				id = a
			}
		}
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "error: missing cell id\n")
		os.Exit(1)
	}
	if err := store.Create(id, noProxy); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cell %s created\n", id)
}

// netInstallCmd installs the host-side cage (spec §4): filter table
// first, then ip_forward — the ORDER is the contract. Root, idempotent.
func netInstallCmd(store *cell.Store) {
	if err := store.InstallNetwork(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("cage installed: filter table + forwarding sysctls (nat regenerates per boot)")
}

// proxyCmd runs the dumb forward proxy in the foreground — it is started
// by systemd-run as the transient lararium-proxy@<id> unit (spec §4);
// it lives and dies with that unit. Not for direct human use.
func proxyCmd(store *cell.Store, args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "", "gateway addr:port to bind (the cell's own gateway)")
	peer := fs.String("peer", "", "the cell's own address (only peer accepted)")
	logPath := fs.String("log", store.ProxyLogPath(), "access log path")
	//nolint:errcheck // ExitOnError handles parse failures
	fs.Parse(args)
	if *listen == "" || *peer == "" {
		fmt.Fprintln(os.Stderr, "error: proxy requires --listen and --peer")
		os.Exit(2)
	}
	srv := &proxy.Server{Listen: *listen, Peer: *peer, LogPath: *logPath}
	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: %v\n", err)
		os.Exit(1)
	}
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
			timeout, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: --timeout wants an integer, got %q\n", args[1])
				os.Exit(2)
			}
			opts.Timeout = timeout
			args = args[2:]
		case "--env":
			if len(args) < 2 {
				fmt.Fprintf(os.Stderr, "error: --env requires KEY=VALUE\n")
				os.Exit(1)
			}
			opts.Env = append(opts.Env, args[1])
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
	// Spec §6: stdin is piped through. Read it only when not a TTY
	// (a pipe/heredoc/daemon hand-off); an interactive TTY means the
	// caller has nothing to send and blocking on read would hang.
	if fi, serr := os.Stdin.Stat(); serr == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		if data, rerr := io.ReadAll(os.Stdin); rerr == nil {
			opts.Stdin = data
		}
	}
	res, err := store.Run(id, cmdStr, opts)
	if errors.Is(err, cell.ErrNotRunning) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err != nil {
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

	// id/ts both feed filesystem paths: ts must parse as the exact
	// snapshot layout (rejects "../evil", globs; agy round-2 F7 —
	// validateID inside store methods covers id at use, but the
	// snapshot path here is built in main).
	if _, err := time.Parse("20060102T150405Z", ts); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid snapshot timestamp %q\n", ts)
		os.Exit(1)
	}

	// Restore replaces upper — the cell must be stopped (same
	// split-brain hazard as snapshot).
	if store.IsActive(id) {
		fmt.Fprintf(os.Stderr, "error: cell %s is running; stop it before restore\n", id)
		os.Exit(1)
	}

	// ts was strictly validated against 20060102T150405Z above; CellDir is
	// root+validated-id. No traversal input remains.
	snapUpper := fmt.Sprintf("%s/snapshots/%s/upper", store.CellDir(id), ts)
	currentUpper := store.UpperDir(id)

	if _, err := os.Stat(snapUpper); os.IsNotExist(err) { //nolint:gosec // G703: validated id+ts
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
	os.RemoveAll(staging)                          //nolint:gosec // G703: validated id+ts derived path
	if err := os.RemoveAll(oldUpper); err != nil { //nolint:gosec // G703: validated id derived path
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	//nolint:gosec // G703: snapUpper built from validated id+ts (see above)
	if _, err := runSystem("cp", "-a", snapUpper, staging); err != nil {
		os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "error: copy snapshot: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(currentUpper, oldUpper); err != nil { //nolint:gosec // G703: validated id derived
		os.RemoveAll(staging) //nolint:gosec // G703: validated id+ts derived
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(staging, currentUpper); err != nil { //nolint:gosec // G703: validated id+ts derived
		// Roll back the swap (best-effort; the primary error is reported).
		//nolint:gosec // rollback rename; validated id paths
		os.Rename(oldUpper, currentUpper) //nolint:errcheck // primary error wins
		//nolint:gosec // rollback cleanup; validated id paths
		os.RemoveAll(staging)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	//nolint:gosec // best-effort cleanup; validated id path
	os.RemoveAll(oldUpper)

	// work/ holds index whiteouts tied to the REPLACED upper —
	// remounting it against the restored tree risks overlayfs index
	// inconsistency (same reasoning as snapshot's work/ reset; agy
	// round-2 F7).
	if err := os.RemoveAll(store.WorkDir(id)); err != nil { //nolint:gosec // G703: validated id path
		fmt.Fprintf(os.Stderr, "error: clean work dir: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(store.WorkDir(id), 0o755); err != nil { //nolint:gosec // G703: validated id path
		fmt.Fprintf(os.Stderr, "error: mkdir work dir: %v\n", err)
		os.Exit(1)
	}

	if err := store.MountOverlay(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("cell %s restored from %s\n", id, ts)
}
