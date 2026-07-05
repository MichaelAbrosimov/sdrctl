package api

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/systemd/systemdtest"
)

// shortSocketDir returns a directory whose paths fit the unix socket
// length limit (~104 bytes on darwin) — t.TempDir can exceed it.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sdrctl-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// SDR-P2-03: a second agent must not steal the socket of a live one; only
// a genuinely stale socket file is removed.
func TestRunSocketRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "sdrctl.sock")

	// "First agent": a live unix listener on the path.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	srv := newServer(testConfig(), systemdtest.New(idleUnits()))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.RunSocket(ctx, path, "staff"); err == nil {
		t.Fatal("second agent took over a live socket")
	}

	// Stale socket (listener closed, file left behind): must be replaced.
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("listener close removed the socket file on this platform: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.RunSocket(ctx2, path, "staff") }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if socketAlive(path) {
			break
		}
		if time.Now().After(deadline) {
			cancel2()
			t.Fatal("stale socket was not replaced by the new agent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	if err := <-done; err != nil {
		t.Fatalf("RunSocket over a stale socket: %v", err)
	}
}
