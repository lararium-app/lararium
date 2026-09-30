package cell

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MountOverlay mounts an overlayfs for the given cell.
// lowerdir=templates/noble,upperdir=cells/<id>/upper,workdir=cells/<id>/work
func (s *Store) MountOverlay(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	lower := s.TemplateDir()
	upper := s.UpperDir(id)
	work := s.WorkDir(id)
	merged := s.MergedDir(id)

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)

	_, _, err := s.runner.Run("/bin/mount", "-t", "overlay", "overlay", merged, "-o", opts)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	return nil
}

// UnmountOverlay unmounts the overlay for the given cell.
// Plain umount only, retry up to 3 times with 1s spacing.
// If still busy, returns error with PIDs from fuser -m.
// NO umount -l (lazy unmount forbidden).
func (s *Store) UnmountOverlay(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	merged := s.MergedDir(id)

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(1 * time.Second)
		}

		_, stderr, err := s.runner.Run("umount", merged)
		if err == nil {
			// Verify it's actually unmounted.
			if !s.isMounted(merged) {
				return nil
			}
		}

		if attempt == 2 {
			// Final attempt failed — get PIDs from fuser.
			stdout, _, _ := s.runner.Run("fuser", "-m", merged)
			pids := strings.TrimSpace(string(stdout))
			if pids != "" {
				return fmt.Errorf("cell busy: %s", pids)
			}
			return fmt.Errorf("umount failed: %s", strings.TrimSpace(string(stderr)))
		}
	}

	return nil
}

// isActive reports whether the cell's systemd unit is active (running).
func (s *Store) isActive(id string) bool {
	_, _, err := s.runner.Run("systemctl", "is-active", "--quiet", UnitName(id))
	return err == nil
}

// IsActive reports whether the cell's systemd unit is active (running).
// Exported for cmd/ precondition checks.
func (s *Store) IsActive(id string) bool { return s.isActive(id) }

// Snapshot renames the current upper to snapshots/<ts>/upper and creates
// a fresh empty upper, then remounts the overlay. cell.json is untouched.
func (s *Store) Snapshot(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	// A snapshot renames upper out from under the mount — the cell
	// must be stopped or a live container keeps writing to orphaned
	// inodes (split-brain).
	if s.isActive(id) {
		return fmt.Errorf("cell %s is running; stop it before snapshot", id)
	}

	upper := s.UpperDir(id)
	snapshotsDir := s.SnapshotsDir(id)

	// Unmount overlay if a stop left it mounted.
	if s.isMounted(s.MergedDir(id)) {
		if err := s.UnmountOverlay(id); err != nil {
			return fmt.Errorf("unmount for snapshot: %w", err)
		}
	}

	// Rename upper to snapshots/<ts>/upper.
	ts := time.Now().UTC().Format("20060102T150405Z")
	snapUpper := filepath.Join(snapshotsDir, ts, "upper")
	if err := os.MkdirAll(filepath.Dir(snapUpper), 0755); err != nil {
		return fmt.Errorf("mkdir snapshots: %w", err)
	}
	if err := os.Rename(upper, snapUpper); err != nil {
		return fmt.Errorf("rename upper to snapshot: %w", err)
	}

	// Create fresh empty upper, re-seed it, and reset work/.
	// Spec §2 order is mkdir → seed → mount: the guest .network
	// drop-in lives in upper/, so a bare fresh upper would boot the
	// cell without its interface config (agy review F5). Stale work/
	// from the rotated layer risks overlayfs index inconsistency.
	if err := os.MkdirAll(upper, 0755); err != nil {
		return fmt.Errorf("mkdir fresh upper: %w", err)
	}
	if err := os.RemoveAll(s.WorkDir(id)); err != nil {
		return fmt.Errorf("clean work dir: %w", err)
	}
	if err := os.MkdirAll(s.WorkDir(id), 0755); err != nil {
		return fmt.Errorf("mkdir work: %w", err)
	}
	if err := s.seedUpper(id); err != nil {
		return fmt.Errorf("seed fresh upper: %w", err)
	}

	// Remount overlay.
	if err := s.MountOverlay(id); err != nil {
		// Restore snapshot on failure.
		os.RemoveAll(upper)
		os.Rename(snapUpper, upper)
		return fmt.Errorf("remount after snapshot: %w", err)
	}

	return nil
}

// IsOverlayMounted reports whether the cell's merged dir is currently
// mounted (exported for cmd/ restore/snapshot preconditions).
func (s *Store) IsOverlayMounted(id string) bool {
	return s.isMounted(s.MergedDir(id))
}

// isMounted checks if a path appears in /proc/mounts.
func (s *Store) isMounted(mp string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == mp {
			return true
		}
	}
	return false
}

// CreateOverlay sets up overlay directories and mounts the overlay.
// Create = mkdir 3 dirs → seed upper/ (spec: seed BEFORE mount) → mount.
func (s *Store) CreateOverlay(id string) error {
	if err := validateID(id); err != nil {
		return err
	}

	// mkdir three dirs.
	for _, d := range []string{s.UpperDir(id), s.WorkDir(id), s.MergedDir(id)} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Seed upper/ BEFORE mount: write guest .network file.
	if err := s.seedUpper(id); err != nil {
		return fmt.Errorf("seed upper: %w", err)
	}

	// Mount overlay.
	if err := s.MountOverlay(id); err != nil {
		return err
	}

	return nil
}

// seedUpper writes the guest .network file into upper/ before mount.
func (s *Store) seedUpper(id string) error {
	networkDir := filepath.Join(s.UpperDir(id), "etc", "systemd", "network")
	if err := os.MkdirAll(networkDir, 0755); err != nil {
		return fmt.Errorf("mkdir network dir: %w", err)
	}

	networkContent := `[Match]
OriginalName=host0

[Network]
Address=10.91.0.2/28
Gateway=10.91.0.1
`
	networkFile := filepath.Join(networkDir, "10-lararium.network")
	if err := os.WriteFile(networkFile, []byte(networkContent), 0644); err != nil {
		return fmt.Errorf("write .network: %w", err)
	}

	return nil
}
