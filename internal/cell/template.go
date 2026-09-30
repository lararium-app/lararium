package cell

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildTemplate builds a debootstrap-based rootfs template at Root/templates/noble.
// Requires root (euid 0) and debootstrap binary.
func (s *Store) BuildTemplate() error {
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}

	// Verify debootstrap exists.
	if _, err := lookPath("debootstrap"); err != nil {
		return fmt.Errorf("debootstrap not found: %w", err)
	}

	dst := s.TemplateDir()

	// Run debootstrap.
	mirror := os.Getenv("LARARIUM_DEBIAN_MIRROR")
	args := []string{"--variant=minbase", "noble", dst}
	if mirror != "" {
		args = append(args, mirror)
	}
	if _, _, err := s.runner.Run("debootstrap", args...); err != nil {
		return fmt.Errorf("debootstrap: %w", err)
	}

	// Bake: chroot-install required packages.
	// NOTE: apt has no -r flag (live-probed: "not understood"); the
	// chroot must wrap apt itself.
	// NOTE: "systemd-networkd" is not a package on Ubuntu — networkd
	// ships inside "systemd"; "systemd-sysv" provides /sbin/init (PID 1),
	// without which nspawn cannot boot the template. Live-probed on noble.
	packages := []string{
		"bash", "curl", "python3", "iputils-ping", "iproute2",
		"ca-certificates", "sudo", "systemd", "systemd-sysv", "dbus",
	}
	aptArgs := append([]string{"-y", "install"}, packages...)
	if _, _, err := s.runner.Run("chroot", append([]string{dst, "apt-get"}, aptArgs...)...); err != nil {
		return fmt.Errorf("apt-get install: %w", err)
	}

	// Create keeper user (uid 1000, in sudo group).
	if _, _, err := s.runner.Run("chroot", dst, "useradd", "-m", "-u", "1000", "-g", "sudo", "-s", "/bin/bash", "keeper"); err != nil {
		return fmt.Errorf("create keeper user: %w", err)
	}

	// Create /workspace directory in template.
	ws := filepath.Join(dst, "workspace")
	if err := os.MkdirAll(ws, 0755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	// Enable in-guest units: networkd (interface config, spec §4) and
	// dbus (machine bus — `cell run` reaches the guest through it).
	if err := os.MkdirAll(filepath.Join(dst, "etc", "systemd", "system", "multi-user.target.wants"), 0755); err != nil {
		return fmt.Errorf("mkdir wants: %w", err)
	}
	for _, unit := range []string{"systemd-networkd.service", "dbus.service"} {
		unitPath := filepath.Join(dst, "lib", "systemd", "system", unit)
		if _, err := os.Stat(unitPath); err != nil {
			continue
		}
		enableLink := filepath.Join(dst, "etc", "systemd", "system", "multi-user.target.wants", unit)
		if _, err := os.Lstat(enableLink); os.IsNotExist(err) {
			if err := os.Symlink(unitPath, enableLink); err != nil {
				return fmt.Errorf("enable %s: %w", unit, err)
			}
		}
	}

	// Mask NetworkManager if present.
	nmWants := filepath.Join(dst, "etc", "systemd", "system", "NetworkManager.service")
	if err := os.MkdirAll(filepath.Dir(nmWants), 0755); err == nil {
		os.Remove(nmWants)
		if err := os.Symlink("/dev/null", nmWants); err != nil {
			return fmt.Errorf("mask NetworkManager: %w", err)
		}
	}

	// Zero out machine-id.
	machineID := filepath.Join(dst, "etc", "machine-id")
	os.Remove(machineID)
	if f, err := os.Create(machineID); err == nil {
		f.Close()
	}

	// APT cleanup.
	if _, _, err := s.runner.Run("chroot", dst, "apt-get", "clean"); err != nil {
		return fmt.Errorf("apt-get clean: %w", err)
	}

	return nil
}
