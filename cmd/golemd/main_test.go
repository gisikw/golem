package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/supervisor"
)

func TestValidateTCPAuthLoopbackPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1", "[::1]:1", "localhost:1"} {
		if err := validateTCPAuth(address, nil); err != nil {
			t.Fatalf("loopback %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:1", ":1", "192.0.2.1:1"} {
		if err := validateTCPAuth(address, nil); err == nil {
			t.Fatalf("non-loopback %q accepted without tokens", address)
		}
		if err := validateTCPAuth(address, []string{"token"}); err != nil {
			t.Fatalf("authenticated %q rejected: %v", address, err)
		}
	}
}

func TestShutdownDaemonKillsPrivateTmuxAndWorker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tmux := supervisor.Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if err := tmux.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pidFile := filepath.Join(dir, "worker.pid")
	_, _, err := tmux.Start(ctx, "shutdown", harnesses.Launch{
		Argv:        []string{"sh", "-c", "echo $$ >" + pidFile + "; exec sleep 60"},
		Dir:         dir,
		Transcript:  filepath.Join(dir, "transcript"),
		Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tmux.KillServer(context.Background()) })

	var pid int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		contents, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			pid, readErr = strconv.Atoi(strings.TrimSpace(string(contents)))
			if readErr == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("worker pid was not written")
	}

	shutdownDaemon(nil, nil, tmux)
	if tmux.ServerAlive(context.Background()) {
		t.Fatal("private tmux server survived daemon shutdown")
	}
	if _, err = os.Lstat(tmux.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private tmux socket remains after shutdown: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err = syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker process %d survived private server shutdown", pid)
}
