package surface

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// Control socket (KEYS-SPEC K6): a line protocol on a unix socket in
// the hearth dir. The CLI writes keys.json itself, then asks a running
// daemon to re-read it. One command: RELOAD-KEYS.
const (
	socketReloadCmd = "RELOAD-KEYS"
	socketAck       = "OK\n"
	socketTimeout   = 5 * time.Second
)

// ServeSocket starts the control socket at path and returns a Closer
// that stops accepting and removes the socket file. Stale-file rule
// (spec K6): if something is listening on path, refuse; if the file is
// there but dead, unlink and bind. reload runs on each RELOAD-KEYS
// line; its error text must never contain key material.
func (s *Server) ServeSocket(path string, reload func() error) (io.Closer, error) {
	if _, err := os.Stat(path); err == nil {
		d := net.Dialer{Timeout: socketTimeout}
		conn, dialErr := d.DialContext(context.Background(), "unix", path)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another hearthd is listening on %s", path)
		}
		// Dead socket file (listener gone): reclaim it.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, fmt.Errorf("control socket mode: %w", err)
	}

	sc := &socketCloser{ln: ln, path: path, done: make(chan struct{}), s: s}
	go sc.acceptLoop(reload)
	return sc, nil
}

type socketCloser struct {
	ln   net.Listener
	path string
	done chan struct{}
	s    *Server
}

func (c *socketCloser) acceptLoop(reload func() error) {
	defer close(c.done)
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go c.s.handleSocketConn(conn, reload)
	}
}

func (s *Server) handleSocketConn(conn net.Conn, reload func() error) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socketTimeout))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return
	}
	cmd := trimCR(line)
	switch {
	case cmd == socketReloadCmd:
		if err := reload(); err != nil {
			// reload errors are ours; they never carry key material.
			log.Printf("hearthd socket: reload failed: %v", err)
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		fmt.Fprint(conn, socketAck)
	case cmd == "PING":
		fmt.Fprint(conn, "OK\n")
	case strings.HasPrefix(cmd, "backup ") || cmd == "backup":
		s.handleBackupSocket(conn, reader, cmd)
	default:
		fmt.Fprint(conn, "ERR unknown command\n")
	}
}

func (s *Server) handleBackupSocket(conn net.Conn, reader *bufio.Reader, line string) {
	if s.Backup == nil {
		fmt.Fprint(conn, "ERR backup not supported\n")
		return
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		fmt.Fprint(conn, "ERR usage: backup <out_abs> [--no-config]\n")
		return
	}
	outAbs := parts[1]
	noConfig := false
	for _, p := range parts[2:] {
		if p == "--no-config" {
			noConfig = true
		}
	}

	// 1. Ack request line immediately (§4.5.3.1)
	if _, err := fmt.Fprint(conn, "OK\n"); err != nil {
		return
	}

	// Extend deadline for backup operations
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))

	progress := func(msg string) {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
		_, _ = fmt.Fprintf(conn, "PROGRESS %s\n", msg)
	}

	if err := s.Backup(outAbs, noConfig, progress); err != nil {
		_, _ = fmt.Fprintf(conn, "ERR %s\n", err.Error())
		return
	}
	_, _ = fmt.Fprint(conn, "DONE\n")
}

func trimCR(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Close stops the accept loop and removes the socket file.
func (c *socketCloser) Close() error {
	err := c.ln.Close()
	<-c.done
	os.Remove(c.path)
	return err
}

// ErrNotListening marks "no daemon on the socket" (dial failure:
// missing file, ECONNREFUSED on a stale file, permission error) as
// distinct from "connected but the ack failed" — the CLI prints a
// different line for each (spec K6), both exit 0.
var ErrNotListening = errors.New("not listening")

// ReloadViaSocket sends RELOAD-KEYS to a running daemon and waits for
// the ack. Dial failures wrap ErrNotListening; ack failures (timeout,
// ERR line) are plain errors. The CLI treats every failure as success
// of its own write — the ack is convenience, never the contract.
func ReloadViaSocket(path string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(conn, "%s\n", socketReloadCmd); err != nil {
		return err
	}
	ack, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reload ack: %w", err)
	}
	ack = trimCR(ack)
	if ack != "OK" {
		return fmt.Errorf("reload failed: %s", ack)
	}
	return nil
}

// PingViaSocket sends PING to a running daemon and waits for OK/PONG.
func PingViaSocket(path string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprint(conn, "PING\n"); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("ping read: %w", err)
	}
	line = trimCR(line)
	if line != "OK" && line != "PONG" {
		return fmt.Errorf("ping unexpected response: %s", line)
	}
	return nil
}

// BackupViaSocket invokes the control socket verb backup <out_abs> [--no-config] per §4.5.3.1.
// Single request line, ack, progress lines, done/error.
func BackupViaSocket(path string, outAbs string, noConfig bool, progress func(string)) error {
	d := net.Dialer{Timeout: socketTimeout}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotListening, err)
	}
	defer conn.Close()

	cmd := "backup " + outAbs
	if noConfig {
		cmd += " --no-config"
	}
	_ = conn.SetDeadline(time.Now().Add(socketTimeout))
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return err
	}

	r := bufio.NewReader(conn)
	ack, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("backup ack: %w", err)
	}
	ack = trimCR(ack)
	if strings.HasPrefix(ack, "ERR ") {
		return errors.New(strings.TrimPrefix(ack, "ERR "))
	}
	if ack != "OK" {
		return fmt.Errorf("unexpected backup ack: %s", ack)
	}

	for {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("connection closed before backup finished")
			}
			return fmt.Errorf("read backup line: %w", err)
		}
		line = trimCR(line)
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			if progress != nil {
				progress(strings.TrimPrefix(line, "PROGRESS "))
			}
		case line == "DONE":
			return nil
		case strings.HasPrefix(line, "ERR "):
			return errors.New(strings.TrimPrefix(line, "ERR "))
		}
	}
}

