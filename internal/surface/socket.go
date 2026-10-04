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

	sc := &socketCloser{ln: ln, path: path, done: make(chan struct{})}
	go sc.acceptLoop(reload)
	return sc, nil
}

type socketCloser struct {
	ln   net.Listener
	path string
	done chan struct{}
}

func (c *socketCloser) acceptLoop(reload func() error) {
	defer close(c.done)
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go handleSocketConn(conn, reload)
	}
}

func handleSocketConn(conn net.Conn, reload func() error) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socketTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	switch trimCR(line) {
	case socketReloadCmd:
		if err := reload(); err != nil {
			// reload errors are ours; they never carry key material.
			log.Printf("hearthd socket: reload failed: %v", err)
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		fmt.Fprint(conn, socketAck)
	default:
		fmt.Fprint(conn, "ERR unknown command\n")
	}
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
