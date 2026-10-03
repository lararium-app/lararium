package cell

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Sentinel errors returned by the cell Store.
var (
	ErrInvalidID    = errors.New("invalid cell id")
	ErrCellNotFound = errors.New("cell not found")
	ErrNotRunning   = errors.New("cell not running")
	ErrAlreadyExist = errors.New("cell already exists")
	ErrNotRoot      = errors.New("must be run as root")
)

// Runner abstracts subprocess execution so tests can inject fakes.
type Runner interface {
	// Run executes cmd and returns separated stdout/stderr.
	Run(cmd string, args ...string) (stdout, stderr []byte, err error)
	// RunCombined executes cmd and returns merged stdout+stderr.
	RunCombined(cmd string, args ...string) (output []byte, err error)
	// StartDetached executes cmd in the background and returns its PID.
	StartDetached(cmd string, args ...string) (pid int, err error)
}

// CmdRunner is the production Runner that uses os/exec.
type CmdRunner struct{}

// Run executes cmd via execve (no shell); argv values from the CLI
// cannot inject commands by construction.
func (r *CmdRunner) Run(cmd string, args ...string) ([]byte, []byte, error) {
	//nolint:noctx // argv slice, no shell; ctx cancels wait only
	cmdExec := exec.Command(cmd, args...)
	var stdout, stderr bytes.Buffer
	cmdExec.Stdout = &stdout
	cmdExec.Stderr = &stderr
	err := cmdExec.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// RunCombined runs a command capturing merged stdout+stderr. Contexts are
// intentionally absent: lifecycle commands are supervised by systemd itself;
// a Go ctx would only cancel the wait, not the unit.
//
//nolint:noctx // see doc: cancellation is systemd's job.
func (r *CmdRunner) RunCombined(cmd string, args ...string) ([]byte, error) {
	cmdExec := exec.Command(cmd, args...)
	var output bytes.Buffer
	cmdExec.Stdout = &output
	cmdExec.Stderr = &output
	err := cmdExec.Run()
	return output.Bytes(), err
}

// StartDetached spawns a command that outlives this call and returns its PID.
//
//nolint:noctx // detached spawn must outlive this call by design.
func (r *CmdRunner) StartDetached(cmd string, args ...string) (int, error) {
	cmdExec := exec.Command(cmd, args...)
	cmdExec.Stdout = nil
	cmdExec.Stderr = nil
	cmdExec.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err := cmdExec.Start()
	return cmdExec.Process.Pid, err
}

// Cell represents a single sandbox cell record.
type Cell struct {
	ID          string    `json:"id"`
	SubUIDBase  int       `json:"subuid_base"`
	Created     time.Time `json:"created"`
	SubUIDCount int       `json:"subuid_count"`
	// GuestRootFixed records that fixGuestRootFiles has been applied
	// for this cell (agy round-3 F6): without it, a crash between
	// recordUIDMap and the first-boot restart would leave every later
	// idempotent Start short-circuiting on isActive with a broken sudo.
	GuestRootFixed bool `json:"guest_root_fixed,omitempty"`
	// Limits overrides spec §5 per-cell: any non-zero/non-empty
	// field replaces the lararium.yaml global at start; nil/zero
	// inherits.
	Limits *Limits `json:"limits,omitempty"`
	// Network state (spec §4, T5). SubnetIndex is allocated at
	// create under flock; VethHost is the deterministic host-end
	// name (erratum E1). Gateway/CellIPAddr mirror 10.91.<n>.1/.2
	// so consumers (proxy unit, tests) read one record, never
	// re-derive.
	SubnetIndex int    `json:"subnet_index"`
	VethHost    string `json:"veth_host"`
	Gateway     string `json:"gateway"`
	CellIPAddr  string `json:"cell_ip"`
	// ProxyEnabled defaults TRUE: nil (legacy/pre-T5 records) and
	// absent both mean proxied; explicit false comes from create
	// --no-proxy (C8's isolated cell).
	ProxyEnabled *bool `json:"proxy_enabled,omitempty"`
}

// ProxyOn reports the effective proxy policy (default true).
func (c *Cell) ProxyOn() bool {
	return c.ProxyEnabled == nil || *c.ProxyEnabled
}

// SubnetAllocated reports whether the record carries a T5 subnet claim
// (0 is a valid index).
func (c *Cell) SubnetAllocated() bool { return c.VethHost != "" }

// effectiveLimits merges per-cell overrides over the globals.
func (s *Store) effectiveLimits(c *Cell) Limits {
	l := s.Limits
	if c == nil || c.Limits == nil {
		return l
	}
	if c.Limits.MemoryMB > 0 {
		l.MemoryMB = c.Limits.MemoryMB
	}
	if c.Limits.CPUQuota != "" {
		l.CPUQuota = c.Limits.CPUQuota
	}
	if c.Limits.TasksMax > 0 {
		l.TasksMax = c.Limits.TasksMax
	}
	return l
}

// Store manages cell lifecycle operations.
type Store struct {
	Root   string
	Hearth string
	Limits Limits
	// ProxyPort overrides DefaultProxyPort (0 = default). Config
	// file [proxy] port / tests inject here.
	ProxyPort int
	// ProxyGlobal is false when lararium.yaml disables the proxy
	// fleet-wide (spec §4 fail-closed: filter installed, nat omitted).
	ProxyGlobal bool
	runner      Runner
}

// NewStore creates a Store with the given root, hearth path, limits, and runner.
func NewStore(root, hearth string, limits Limits, runner Runner) *Store {
	return &Store{
		Root:        root,
		Hearth:      hearth,
		Limits:      limits,
		ProxyGlobal: true, // config may flip off; default is proxied fleet
		runner:      runner,
	}
}

// SetRunner replaces the runner. Used by tests to inject a fake.
func (s *Store) SetRunner(r Runner) {
	s.runner = r
}

// validateID enforces a strict whitelist: 1-64 chars of
// [A-Za-z0-9._-], not starting with "." or "-". A leading dash turns
// into a bogus --machine=-- flag; "." passes prefix/separators checks
// yet CellDir(".") = cells/ itself, so Destroy(".") would wipe every
// cell (agy round-2 F3). Anything else is ErrInvalidID.
func validateID(id string) error {
	if id == "" || len(id) > 64 {
		return ErrInvalidID
	}
	if id[0] == '.' || id[0] == '-' || strings.Contains(id, "..") {
		return ErrInvalidID
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return ErrInvalidID
		}
	}
	return nil
}

// CellDir returns the absolute path for a cell's directory.
func (s *Store) CellDir(id string) string {
	return filepath.Join(s.Root, "cells", id)
}

// UpperDir returns the path to the cell's overlay upper layer.
func (s *Store) UpperDir(id string) string {
	return filepath.Join(s.CellDir(id), "upper")
}

// WorkDir returns the path to the cell's overlay work directory.
func (s *Store) WorkDir(id string) string {
	return filepath.Join(s.CellDir(id), "work")
}

// MergedDir returns the path to the cell's overlay mount point.
func (s *Store) MergedDir(id string) string {
	return filepath.Join(s.CellDir(id), "merged")
}

// WorkspaceDir returns the path to the cell's workspace directory.
func (s *Store) WorkspaceDir(id string) string {
	return filepath.Join(s.CellDir(id), "workspace")
}

// BinDir returns the path to the cell's bin directory.
func (s *Store) BinDir(id string) string {
	return filepath.Join(s.CellDir(id), "bin")
}

// SnapshotsDir returns the path to the cell's snapshots directory.
func (s *Store) SnapshotsDir(id string) string {
	return filepath.Join(s.CellDir(id), "snapshots")
}

// LogDir returns the path to the cell's log directory.
func (s *Store) LogDir(id string) string {
	return filepath.Join(s.CellDir(id), "log")
}

// TemplateDir returns the path to the built template directory.
func (s *Store) TemplateDir() string {
	return filepath.Join(s.Root, "templates", "noble")
}

// CellJSONPath returns the path to the cell.json file.
func (s *Store) CellJSONPath(id string) string {
	return filepath.Join(s.CellDir(id), "cell.json")
}

// UnitName returns the systemd unit name for a cell.
func UnitName(id string) string {
	return "lararium-cell-" + id
}

// Load reads cell.json for the given id.
func (s *Store) Load(id string) (*Cell, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.CellJSONPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCellNotFound
		}
		return nil, fmt.Errorf("read cell.json: %w", err)
	}
	var c Cell
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse cell.json: %w", err)
	}
	return &c, nil
}

// Save atomically writes cell.json (temp file + rename).
func (s *Store) Save(c *Cell) error {
	dir := s.CellDir(c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir cell dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cell.json: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".cell.json.*")
	if err != nil {
		return fmt.Errorf("create temp cell.json: %w", err)
	}
	tmp.Close()
	if err := os.WriteFile(tmp.Name(), data, 0o644); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write cell.json: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.CellJSONPath(c.ID)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename cell.json: %w", err)
	}
	return nil
}

// List returns all cell IDs found under the cells directory.
func (s *Store) List() ([]string, error) {
	cellsDir := filepath.Join(s.Root, "cells")
	entries, err := os.ReadDir(cellsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cells dir: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jsonPath := filepath.Join(cellsDir, e.Name(), "cell.json")
		if _, err := os.Stat(jsonPath); err == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
