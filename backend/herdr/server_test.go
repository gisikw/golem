package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHerdrHelperProcess is re-executed through helperBinary. It behaves like
// exactly the two Herdr commands Server owns, allowing lifecycle tests to use
// real Unix sockets and OS processes without depending on an installed Herdr.
func TestHerdrHelperProcess(t *testing.T) {
	if os.Getenv("GOLEM_HERDR_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := 0
	for i, arg := range args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	args = args[separator:]
	if len(args) >= 3 && args[0] == "integration" && args[1] == "install" && args[2] == "pi" {
		dir := filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "extensions")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "herdr-agent-state.ts"), []byte("// helper lifecycle extension\n"), 0o600)
		os.Exit(0)
	}
	if len(args) >= 3 && args[0] == "--session" && args[2] == "server" {
		if os.Getenv("GOLEM_HERDR_HELPER_FAIL_SERVER") == "1" {
			os.Exit(23)
		}
		record := map[string]string{}
		for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "HERDR_CONFIG_PATH", "HERDR_SESSION", "HERDR_SOCKET_PATH"} {
			record[key] = os.Getenv(key)
		}
		if path := os.Getenv("GOLEM_HERDR_HELPER_RECORD"); path != "" {
			body, _ := json.Marshal(record)
			_ = os.WriteFile(path, body, 0o600)
		}
		socket := os.Getenv("HERDR_SOCKET_PATH")
		_ = os.MkdirAll(filepath.Dir(socket), 0o700)
		_ = os.Remove(socket)
		listener, err := net.Listen("unix", socket)
		if err != nil {
			os.Exit(24)
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				os.Exit(0)
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				var req struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(line, &req)
				fmt.Fprintf(conn, "{\"id\":%q,\"result\":{\"version\":\"test\",\"protocol\":20}}\n", req.ID)
			}()
		}
	}
	os.Exit(2)
}

func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "herdr-helper")
	body := "#!/bin/sh\nexport GOLEM_HERDR_HELPER=1\nexec " + fmt.Sprintf("%q", exe) + " -test.run=TestHerdrHelperProcess -- \"$@\"\n"
	if err = os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOwnedServerIsolationSeedLifecycleAndNoAttach(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gh-") // stay below sockaddr_un's small path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	userDir, err := os.MkdirTemp("/tmp", "gh-user-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(userDir) })
	record := filepath.Join(root, "environment.json")
	userSocket := filepath.Join(userDir, "user-herdr.sock")
	userListener, err := net.Listen("unix", userSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer userListener.Close()

	t.Setenv("HERDR_SESSION", "personal")
	t.Setenv("HERDR_SOCKET_PATH", userSocket)
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(t.TempDir(), "personal.toml"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "personal-config"))
	t.Setenv("HOME", filepath.Join(t.TempDir(), "personal-home"))
	t.Setenv("GOLEM_HERDR_HELPER_RECORD", record)

	s := &Server{Binary: helperBinary(t), Root: root, Session: "golem-private", StartupTimeout: 3 * time.Second}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	if s.SocketPath() == userSocket || !strings.HasPrefix(s.SocketPath(), root+string(os.PathSeparator)) {
		t.Fatalf("private socket escaped root: private=%q user=%q", s.SocketPath(), userSocket)
	}
	if !liveSocket(s.SocketPath(), time.Second) {
		t.Fatal("owned private socket is not live")
	}
	seed, err := os.ReadFile(s.PiExtension())
	if err != nil || !strings.Contains(string(seed), "helper lifecycle") {
		t.Fatalf("stable lifecycle seed missing: %q, %v", seed, err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(root, "pi-seed"): 0o700,
		filepath.Dir(s.PiExtension()):  0o700,
		s.PiExtension():                0o600,
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat private seed path %s: %v", path, statErr)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("private seed mode for %s = %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	config, err := os.ReadFile(s.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"shell_mode = \"non_login\"", "manifest_check = false", "resume_agents_on_restore = false"} {
		if !strings.Contains(string(config), policy) {
			t.Fatalf("private config lacks %q:\n%s", policy, config)
		}
	}
	var got map[string]string
	body, err := os.ReadFile(record)
	if err != nil || json.Unmarshal(body, &got) != nil {
		t.Fatalf("helper environment unavailable: %s: %v", body, err)
	}
	if got["HERDR_SOCKET_PATH"] != s.SocketPath() || got["HERDR_SESSION"] != "golem-private" || got["HOME"] == os.Getenv("HOME") {
		t.Fatalf("Herdr inherited operator selectors: %#v", got)
	}

	// Starting a second owner must refuse the live private server, not attach.
	second := &Server{Binary: s.Binary, Root: root, Session: s.Session, StartupTimeout: time.Second}
	if err = second.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing to attach") {
		t.Fatalf("second owner attached or gave unclear error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if liveSocket(s.SocketPath(), 100*time.Millisecond) {
		t.Fatal("private socket survived owner shutdown")
	}

	// A restart preserves seed bytes across a bundled Herdr upgrade, while
	// repairing permissions an older installer may have left too broad.
	stable := []byte("// deliberately retained seed\n")
	if err = os.WriteFile(s.PiExtension(), stable, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(s.PiExtension(), 0o644); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{Binary: s.Binary, Root: root, Session: s.Session, StartupTimeout: time.Second}
	if err = restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotSeed, err := os.ReadFile(restarted.PiExtension())
	if err != nil || string(gotSeed) != string(stable) {
		t.Fatalf("restart replaced stable lifecycle seed: %q, %v", gotSeed, err)
	}
	info, err := os.Stat(restarted.PiExtension())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restart did not restrict stable lifecycle seed: %v, %v", info, err)
	}
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err = restarted.Stop(restartCtx); err != nil {
		restartCancel()
		t.Fatal(err)
	}
	restartCancel()

	if _, err = os.Stat(userSocket); err != nil {
		t.Fatalf("user Herdr socket was touched: %v", err)
	}
}

func TestOwnedServerRefusesReachableNonHerdrSocketWithoutUnlinking(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gh-occupied-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	s := &Server{Binary: helperBinary(t), Root: root, Session: "occupied", StartupTimeout: time.Second}
	if err = os.MkdirAll(filepath.Dir(s.SocketPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	before, err := os.Lstat(s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}

	err = s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to attach") {
		t.Fatalf("reachable non-Herdr socket was not refused: %v", err)
	}
	after, statErr := os.Lstat(s.SocketPath())
	if statErr != nil || !os.SameFile(before, after) {
		t.Fatalf("pre-existing live socket was replaced: before=%v after=%v error=%v", before, after, statErr)
	}
}

func TestOwnedServerStartupFailureIsBoundedAndCleaned(t *testing.T) {
	t.Setenv("GOLEM_HERDR_HELPER_FAIL_SERVER", "1")
	root, err := os.MkdirTemp("/tmp", "gh-fail-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	s := &Server{Binary: helperBinary(t), Root: root, StartupTimeout: time.Second}
	started := time.Now()
	err = s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited before ready") {
		t.Fatalf("startup failure not surfaced: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("startup failure was not bounded: %s", time.Since(started))
	}
	if s.cmd != nil {
		t.Fatal("failed child remains owned")
	}
	if stopErr := s.Stop(context.Background()); stopErr != nil {
		t.Fatalf("cleanup after failed start is not idempotent: %v", stopErr)
	}
}
