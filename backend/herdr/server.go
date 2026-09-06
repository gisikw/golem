package herdr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultServerStartupTimeout = 15 * time.Second
	// sockaddr_un is 104 bytes on macOS and 108 on Linux, including NUL.
	// Leave room rather than allowing platform-dependent startup failures.
	maxPrivateSocketPath = 100
)

// Server owns the private Herdr process used by one golemd. Herdr has no
// independent data-dir flag: XDG_CONFIG_HOME plus an explicit session select
// its data and socket, so all of those inputs are fixed below and inherited
// HERDR_* values are removed. This is deliberately not an attach-or-start
// helper; finding a live server in the private namespace is an error.
type Server struct {
	Binary         string
	Root           string
	Session        string
	Shell          string
	StartupTimeout time.Duration
	Logger         *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan error
	logFile *os.File
}

func (s *Server) session() string {
	if s.Session != "" {
		return s.Session
	}
	return "golem"
}

func (s *Server) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "herdr"
}

func (s *Server) shell() string {
	if s.Shell != "" {
		return s.Shell
	}
	return "/bin/sh"
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) configHome() string { return filepath.Join(s.Root, "config") }
func (s *Server) stateHome() string  { return filepath.Join(s.Root, "state") }
func (s *Server) home() string       { return filepath.Join(s.Root, "home") }
func (s *Server) configPath() string { return filepath.Join(s.configHome(), "herdr", "config.toml") }
func (s *Server) SocketPath() string {
	return filepath.Join(s.configHome(), "herdr", "sessions", s.session(), "herdr.sock")
}
func (s *Server) PiExtension() string {
	return filepath.Join(s.Root, "pi-seed", "extensions", "herdr-agent-state.ts")
}

// isolatedEnv preserves the daemon's ordinary environment (notably its
// packaged PATH and provider credentials), but makes every Herdr selector
// explicit. HOME is private too because some integrations consult it.
func (s *Server) isolatedEnv(extra ...string) []string {
	blocked := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true,
		"HERDR_CONFIG_PATH": true, "HERDR_SESSION": true,
		"HERDR_SOCKET_PATH": true, "HERDR_CLIENT_SOCKET_PATH": true,
		"PI_CODING_AGENT_DIR": true, "ENV": true, "BASH_ENV": true,
	}
	env := make([]string, 0, len(os.Environ())+8+len(extra))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] {
			env = append(env, item)
		}
	}
	env = append(env,
		"HOME="+s.home(),
		"XDG_CONFIG_HOME="+s.configHome(),
		"XDG_STATE_HOME="+s.stateHome(),
		"HERDR_CONFIG_PATH="+s.configPath(),
		"HERDR_SESSION="+s.session(),
		"HERDR_SOCKET_PATH="+s.SocketPath(),
	)
	return append(env, extra...)
}

func (s *Server) writeConfig() error {
	for _, dir := range []string{s.Root, s.configHome(), s.stateHome(), s.home(), filepath.Dir(s.configPath())} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	// non_login is necessary but not sufficient for bash (interactive bash
	// still reads bashrc), hence the default /bin/sh or packaged no-rc shell.
	body := "onboarding = false\n\n[terminal]\ndefault_shell = " + strconv.Quote(s.shell()) + "\nshell_mode = \"non_login\"\n\n" +
		"[update]\nversion_check = false\nmanifest_check = false\n\n" +
		"[session]\nresume_agents_on_restore = false\n"
	tmp := s.configPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.configPath())
}

func (s *Server) installPiIntegration(ctx context.Context) error {
	target := s.PiExtension()
	if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() {
		// Stable seed: do not silently replace bytes between jobs. Still repair
		// permissions in case an older installer created world-readable paths.
		return s.restrictPiSeed()
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seed := filepath.Join(s.Root, "pi-seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(seed, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.binary(), "integration", "install", "pi")
	cmd.Env = s.isolatedEnv("PI_CODING_AGENT_DIR=" + seed)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install bundled Herdr Pi integration: %w: %s", err, strings.TrimSpace(string(output)))
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Herdr integration install did not create regular file %s", target)
	}
	return s.restrictPiSeed()
}

func (s *Server) restrictPiSeed() error {
	for _, dir := range []string{filepath.Join(s.Root, "pi-seed"), filepath.Dir(s.PiExtension())} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict private Herdr Pi seed: %w", err)
		}
	}
	if err := os.Chmod(s.PiExtension(), 0o600); err != nil {
		return fmt.Errorf("restrict private Herdr Pi extension: %w", err)
	}
	return nil
}

// Start provisions the stable Pi lifecycle seed and starts Herdr in the
// foreground. A live pre-existing private socket is refused rather than
// adopted; a socket outside Root is never inspected or removed.
func (s *Server) Start(ctx context.Context) error {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return errors.New("private Herdr root must be an absolute path")
	}
	if !validPrivateSession(s.session()) {
		return fmt.Errorf("invalid private Herdr session %q", s.session())
	}
	if len(s.SocketPath()) > maxPrivateSocketPath {
		return fmt.Errorf("private Herdr socket path is too long (%d > %d bytes): shorten herdr.root or herdr.session", len(s.SocketPath()), maxPrivateSocketPath)
	}
	// Check before writing config: even Golem's own namespace may contain an
	// orphan from a hard crash, and refusing ownership must be non-mutating.
	if socketReachable(s.SocketPath(), 500*time.Millisecond) {
		return fmt.Errorf("private Herdr already running at %s; refusing to attach", s.SocketPath())
	}
	if info, err := os.Lstat(s.SocketPath()); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket private Herdr path %s", s.SocketPath())
		}
		if err = os.Remove(s.SocketPath()); err != nil {
			return fmt.Errorf("remove stale private Herdr socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.writeConfig(); err != nil {
		return fmt.Errorf("write private Herdr config: %w", err)
	}
	installCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := s.installPiIntegration(installCtx)
	cancel()
	if err != nil {
		return err
	}

	logPath := filepath.Join(s.Root, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.binary(), "--session", s.session(), "server")
	cmd.Env = s.isolatedEnv()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start private Herdr: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	s.mu.Lock()
	s.cmd, s.done, s.logFile = cmd, done, logFile
	s.mu.Unlock()

	timeout := s.StartupTimeout
	if timeout <= 0 {
		timeout = defaultServerStartupTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if liveSocket(s.SocketPath(), 250*time.Millisecond) {
			s.log().Info("private Herdr started", "pid", cmd.Process.Pid, "socket", s.SocketPath(), "root", s.Root)
			return nil
		}
		select {
		case waitErr := <-done:
			s.clearProcess()
			return fmt.Errorf("private Herdr exited before ready (log %s): %w", logPath, waitErr)
		case <-ctx.Done():
			s.stopAfterFailedStart()
			return ctx.Err()
		case <-deadline.C:
			s.stopAfterFailedStart()
			return fmt.Errorf("private Herdr socket %s not ready after %s (log %s)", s.SocketPath(), timeout, logPath)
		case <-ticker.C:
		}
	}
}

func (s *Server) stopAfterFailedStart() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
}

// socketReachable deliberately performs no Herdr request. Any process that is
// accepting connections at the private path owns it; even an incompatible or
// non-Herdr listener must not be unlinked as though it were a stale inode.
func socketReachable(path string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func liveSocket(path string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := Dial(ctx, path)
	if err != nil {
		return false
	}
	defer c.Close()
	_, protocol, err := c.Ping(ctx)
	return err == nil && protocol == Protocol
}

func (s *Server) clearProcess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	s.cmd, s.done, s.logFile = nil, nil, nil
}

// Stop signals only the exact child process Start created. It never invokes
// `herdr server stop`, performs a process-name kill, or consults an ambient
// socket, all of which could target a user's Herdr.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		s.clearProcess()
		removeSocket(s.SocketPath())
		if err != nil && !terminatedBy(err, syscall.SIGTERM) {
			return err
		}
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		s.clearProcess()
		removeSocket(s.SocketPath())
		return fmt.Errorf("private Herdr forced down after shutdown deadline: %w", ctx.Err())
	}
}

func terminatedBy(err error, signal syscall.Signal) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal
}

func removeSocket(path string) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
}

func validPrivateSession(s string) bool {
	if s == "" || s == "default" || s == "." || s == ".." || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func (s *Server) Shutdown(ctx context.Context) error { return s.Stop(ctx) }
