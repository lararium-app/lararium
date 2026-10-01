package cell

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runInTemplate executes a command inside the template chroot with a
// complete guest PATH. chroot resolves argv[0] through the INHERITED
// host PATH, and usrmerged noble ships useradd/usermod/grep siblings
// under /usr/sbin — a host PATH without /usr/sbin (live-probed on
// an Arch-based host: bake died at "create keeper user: exit status 127")
// silently hid them. Pin the canonical PATH via env inside the guest.
func (s *Store) runInTemplate(dst string, args ...string) ([]byte, []byte, error) {
	full := append([]string{
		dst,
		"env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	},
		args...)
	return s.runner.Run("chroot", full...)
}

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

	// debootstrap maps the host to a Debian arch via dpkg
	// (--print-architecture). Distros without dpkg fall back to a
	// host-detection block that Arch's patched debootstrap only
	// implements for its own CARCH values — live-probed on an Arch-based host:
	// bake dies with "Unknown architecture: x86_64". Fail with the
	// remedy instead of the cryptic child error.
	if _, err := lookPath("dpkg"); err != nil {
		return fmt.Errorf("dpkg not found (required by debootstrap for " +
			"host-architecture detection): install the 'dpkg' package " +
			"and re-run (e.g. 'pacman -S dpkg' on Arch systems)")
	}

	dst := s.TemplateDir()

	// Refuse to bake onto an existing template: debootstrap wants an
	// empty dir, and a finished template is sealed read-only below —
	// an in-place re-bake would fail halfway and leave a corrupt tree.
	// Delete it explicitly to rebuild (fresh bootstraps are cheap and
	// deterministic off the same mirror).
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("template exists at %s; remove it to rebuild", dst)
	}

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
	// Refresh lists first: debootstrap's copy may predate additional
	// packages pulled from the mirror, and re-running build-template
	// after an Ubuntu point release needs current lists (cheap insurance).
	if _, _, err := s.runInTemplate(dst, "apt-get", "-y", "update"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	if _, _, err := s.runInTemplate(dst, append([]string{"apt-get"}, aptArgs...)...); err != nil {
		return fmt.Errorf("apt-get install: %w", err)
	}

	// Create keeper user (uid 1000, own group; sudo group only when
	// it exists — minbase lacks it and useradd -g sudo then fails,
	// which silently shipped keeper-less templates until the guest
	// rejected User=keeper with status=217/USER, live-probed 2026-09-30).
	if _, _, err := s.runInTemplate(dst, "useradd", "-m", "-u", "1000", "-U", "-s", "/bin/bash", "keeper"); err != nil {
		return fmt.Errorf("create keeper user: %w", err)
	}
	// Best effort: grant sudo membership when the sudo package landed.
	//nolint:errcheck // absent sudo package is a valid template
	s.runInTemplate(dst, "usermod", "-aG", "sudo", "keeper")
	// Spec §2 intent — "lets the agent install a package" — requires
	// non-interactive sudo: keeper has no password and cell run is
	// non-TTY, so plain sudo group membership fails at the prompt
	// (agy round-2 F10). NOPASSWD drop-in is in-guest only; §1's
	// userns keeps it harmless.
	if err := os.MkdirAll(filepath.Join(dst, "etc", "sudoers.d"), 0o755); err != nil {
		return fmt.Errorf("mkdir sudoers.d: %w", err)
	}
	sudoers := filepath.Join(dst, "etc", "sudoers.d", "keeper")
	if err := os.WriteFile(sudoers, []byte("keeper ALL=(ALL) NOPASSWD:ALL\n"), 0o440); err != nil {
		return fmt.Errorf("write sudoers drop-in: %w", err)
	}
	if _, _, err := s.runInTemplate(dst, "grep", "-q", "^keeper:", "/etc/passwd"); err != nil {
		return fmt.Errorf("keeper user missing after useradd: %w", err)
	}

	// Create /workspace directory in template.
	ws := filepath.Join(dst, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	// Enable in-guest units: networkd (interface config, spec §4) and
	// dbus (machine bus — `cell run` reaches the guest through it).
	if err := os.MkdirAll(filepath.Join(dst, "etc", "systemd", "system", "multi-user.target.wants"), 0o755); err != nil {
		return fmt.Errorf("mkdir wants: %w", err)
	}
	for _, unit := range []string{"systemd-networkd.service", "dbus.service"} {
		// The link TARGET must be guest-root-relative ("/lib/..."),
		// not the host path — a host-absolute target dangles inside
		// the booted guest and the unit never starts (agy review F1,
		// confirmed on the built template).
		unitPath := filepath.Join(dst, "lib", "systemd", "system", unit)
		if _, err := os.Stat(unitPath); err != nil {
			continue
		}
		enableLink := filepath.Join(dst, "etc", "systemd", "system", "multi-user.target.wants", unit)
		if _, err := os.Lstat(enableLink); os.IsNotExist(err) {
			if err := os.Symlink("/lib/systemd/system/"+unit, enableLink); err != nil {
				return fmt.Errorf("enable %s: %w", unit, err)
			}
		} else if err == nil {
			// Repair a previously-baked host-absolute link.
			os.Remove(enableLink)
			if err := os.Symlink("/lib/systemd/system/"+unit, enableLink); err != nil {
				return fmt.Errorf("relink %s: %w", unit, err)
			}
		}
	}

	// Mask NetworkManager if present.
	nmWants := filepath.Join(dst, "etc", "systemd", "system", "NetworkManager.service")
	if err := os.MkdirAll(filepath.Dir(nmWants), 0o755); err == nil {
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
	if _, _, err := s.runInTemplate(dst, "apt-get", "clean"); err != nil {
		return fmt.Errorf("apt-get clean: %w", err)
	}

	// Spec §2 manifest: distro, version, built-at, sha256 of the
	// tree. Hash over sorted relative paths + content digests of
	// regular files (symlinks hashed as targets, dirs as paths — the
	// tree is small, this runs once per bake).
	digest, err := treeSHA256(dst)
	if err != nil {
		return fmt.Errorf("hash template tree: %w", err)
	}
	manifest := struct {
		Distro  string    `json:"distro"`
		Version string    `json:"version"`
		BuiltAt time.Time `json:"built_at"`
		SHA256  string    `json:"sha256"`
	}{Distro: "ubuntu", Version: "noble", BuiltAt: time.Now().UTC(), SHA256: digest}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, "template.json"), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write template manifest: %w", err)
	}

	// Seal read-only (spec §2: "cached read-only"). The template is a
	// lowerdir; any write into it would silently corrupt every cell's
	// overlay view. Rebuild = delete + bake (see head of this func).
	if err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// Symlinks get no chmod at all: os.Chmod FOLLOWS them, and a
		// baked /dev/fd -> /proc/self/fd then points at the HOST
		// procfs mid-walk. Ubuntu's procfs tolerates a no-op chmod;
		// Arch's refuses (EPERM, live-probed on an Arch-based host) and a more
		// permissive kernel would mutate the wrong file entirely.
		// Linux ignores symlink permission bits, so skipping is
		// exactly correct for a read-only seal.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		// Strip WRITE bits only -- execute bits must survive or the
		// guest cannot run /sbin/init and every binary.
		//nolint:gosec // G122: tree is host-owned, baked by us; no hostile writer mid-bake
		if err := os.Chmod(p, info.Mode().Perm()&^0o222); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seal template: %w", err)
	}
	// Directories: readable+traversable, not writable (0555).
	if err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			//nolint:gosec // G122: host-owned baked tree; see file-chmod above
			return os.Chmod(p, 0o555)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seal template dirs: %w", err)
	}

	return nil
}

// treeSHA256 digests a directory tree: sorted "path\0type\0content"
// stream. Deterministic for identical trees regardless of inode
// order or timestamps.
func treeSHA256(root string) (string, error) {
	type entry struct {
		rel  string
		mode fs.FileMode
		src  string // symlink target, "" for others
	}
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil // dirs carry no content; hashing them = os.Open(dir)
		}
		// Hash regular files and symlinks ONLY. Opening a device,
		// FIFO, or socket BLOCKS forever (proven the hard way: the
		// walk hung on dev/console inside the baked rootfs).
		if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		e := entry{rel: rel, mode: info.Mode().Perm()}
		if info.Mode()&os.ModeSymlink != 0 {
			tgt, terr := os.Readlink(p)
			if terr != nil {
				return terr
			}
			e.src = tgt
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%v\x00%s\x00", e.rel, e.mode, e.src)
		if e.src == "" && !strings.HasSuffix(e.rel, "/") {
			f, oerr := os.Open(filepath.Join(root, e.rel))
			if oerr != nil {
				if os.IsNotExist(oerr) {
					continue // special files (dev nodes) skipped
				}
				return "", oerr
			}
			sh := sha256.New()
			if _, cerr := io.Copy(sh, f); cerr != nil {
				f.Close()
				return "", cerr
			}
			f.Close()
			h.Write(sh.Sum(nil))
		}
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
