package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// RunSocket serves SocketHandler on a unix socket until the context is
// cancelled. This is the CLI's primary channel: always available while the
// agent runs, independent of the network API configuration.
func (s *Server) RunSocket(ctx context.Context, path, group string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Remove a stale socket left by an unclean shutdown — but only a STALE
	// one: a socket that still accepts connections belongs to a live agent,
	// and stealing it would silently run two observers/auto-restores in
	// parallel. Never delete something that is not a socket either.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		if socketAlive(path) {
			return fmt.Errorf("another agent is already serving %s — refusing to take over its socket", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := restrictSocket(path, group); err != nil {
		log.Printf("socket: %v — socket stays owner-only", err)
	}

	srv := &http.Server{Handler: s.SocketHandler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		_ = srv.Close()
		_ = os.Remove(path)
		return nil
	case err := <-errc:
		return err
	}
}

// socketAlive reports whether something is still accepting connections on
// the unix socket. A stale file from an unclean shutdown refuses the dial;
// a live listener accepts it.
func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// restrictSocket narrows the fresh socket to root:<group> 0660: file
// permissions replace token auth on this channel. chmod runs first so the
// brief pre-chown window is already group-closed.
func restrictSocket(path, group string) error {
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("group %q not found (create it: groupadd %s)", group, group)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("group %q has non-numeric gid %q", group, g.Gid)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return fmt.Errorf("chown root:%s: %w", group, err)
	}
	return nil
}
