package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gisikw/golem/harnesses"
)

// Tmux addresses only the supervisor-owned server. No command may use the user
// default socket, and server creation always supplies the pinned config.
//
// DefaultShell, when set, is the interactive shell tmux uses for windows/panes a
// human opens while inspecting a worker (the WORKER pane execs the harness argv
// directly and is unaffected). It exists because tmux would otherwise spawn the
// non-interactive Nix build bash (no readline, literal \[\] PS1). The supervisor
// wires it from GOLEM_INTERACTIVE_SHELL (the flake exports
// pkgs.bashInteractive); empty preserves tmux's default.
type Tmux struct{ Binary, Socket, Config, DefaultShell string }

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func (t Tmux) binary() string {
	if t.Binary != "" {
		return t.Binary
	}
	return "tmux"
}
func (t Tmux) config() string {
	if t.Config != "" {
		return t.Config
	}
	return filepath.Join(filepath.Dir(t.Socket), "tmux.conf")
}
func (t Tmux) Prepare() error {
	if !filepath.IsAbs(t.Socket) {
		return errors.New("tmux socket must be absolute")
	}
	dir := filepath.Dir(t.Socket)
	cfg := t.config()
	if !filepath.IsAbs(cfg) || filepath.Dir(cfg) != dir {
		return errors.New("tmux config must be in private state directory")
	}
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink tmux state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(t.Socket); err == nil && fi.Mode()&os.ModeSocket == 0 {
		return errors.New("tmux socket path is not a socket")
	}
	if fi, err := os.Lstat(cfg); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink tmux config")
	}
	// remain-on-exit is scoped to the WORKER pane/window only (set at Start via
	// set-option -w on the worker window). Globally it is OFF so any window a
	// human opens to inspect a worker closes naturally when its process exits
	// (the operator hates lingering dead panes). The worker pane still needs
	// remain-on-exit so the supervisor can read pane_dead/exit-status after death
	// (see Pane()); that is applied per-window, not globally.
	policy := "# Golem Agent Supervisor complete private tmux policy.\nset-option -g status off\nset-option -g pane-border-status off\nset-option -g remain-on-exit off\nset-option -g exit-empty off\nset-option -g destroy-unattached off\nset-option -g allow-rename off\nset-option -g prefix C-b\nunbind-key -a -T prefix\nbind-key -T prefix C-b send-prefix\nunbind-key -T root MouseDown3Pane\nunbind-key -T root M-MouseDown3Pane\nunbind-key -T root MouseDown1Status\nunbind-key -T root MouseDown3Status\nunbind-key -T root MouseDown3StatusLeft\nunbind-key -T root M-MouseDown3Status\nunbind-key -T root M-MouseDown3StatusLeft\nset-option -g mouse on\nbind-key -n PageUp if-shell -F '#{alternate_on}' 'send-keys PageUp' 'copy-mode -eu'\nset-option -g allow-passthrough on\nset-option -g extended-keys on\nset-option -g extended-keys-format csi-u\nset-option -g terminal-features 'xterm*:RGB:extkeys,screen*:extkeys,tmux*:RGB:extkeys,kitty*:RGB:extkeys,ghostty*:RGB:extkeys,xterm-ghostty:RGB:extkeys'\n"
	// default-shell: prefer an interactive bash for windows/panes a human opens.
	// Empty preserves tmux's compiled default. The path must contain no double
	// quote (it is embedded in a quoted set-option value).
	if sh := t.DefaultShell; sh != "" {
		if strings.Contains(sh, "\"") {
			return errors.New("tmux default-shell path must not contain a double quote")
		}
		policy += fmt.Sprintf("set-option -g default-shell \"%s\"\n", sh)
	}
	// Golem theming is COSMETIC AND NON-BLOCKING: a missing, symlinked,
	// irregular, or unreadable theme artifact is skipped so workers still start
	// on the plain policy config. Never hardcode palette values here; the styles
	// come from golem-theme.sh via GOLEM_TMUX_THEME_CONFIG.
	if theme := os.Getenv("GOLEM_TMUX_THEME_CONFIG"); theme != "" {
		if styles, ok := readThemeStyles(theme); ok {
			policy += "# Generated from the canonical Golem palette.\n" + styles
		}
	}
	return os.WriteFile(cfg, []byte(policy), 0o600)
}

func readThemeStyles(theme string) (string, bool) {
	fi, err := os.Lstat(theme)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return "", false
	}
	styles, err := os.ReadFile(theme)
	if err != nil {
		return "", false
	}
	return string(styles), true
}
func (t Tmux) run(ctx context.Context, args ...string) (string, error) {
	a := append([]string{"-S", t.Socket}, args...)
	c := exec.CommandContext(ctx, t.binary(), a...)
	configureCommand(c)
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return strings.TrimSpace(b.String()), err
}

// ReapplyPolicy re-loads the pinned policy config into a live server. tmux reads
// a config via -f ONLY when the SERVER first starts; a server adopted across a
// supervisor restart (sessions persist because exit-empty is off and settled
// workers linger) keeps whatever config it was born with. When the operator
// changes the policy — e.g. adds extended-keys — an already-running server never
// picks it up, so pi warns "extended-keys off", the mouse wheel does not scroll,
// and PageUp copy-mode is unbound. source-file applies the current policy
// synchronously to the running server (idempotent set-options/bindings). No-op
// (nil) when no server is running yet. Called at supervisor boot and after
// every session start so the policy is reliable on every server start path.
func (t Tmux) ReapplyPolicy(ctx context.Context) error {
	if !t.ServerAlive(ctx) {
		return nil
	}
	_, err := t.run(ctx, "source-file", t.config())
	return err
}
func (t Tmux) ServerAlive(ctx context.Context) bool {
	_, err := t.run(ctx, "show-options", "-g", "status")
	return err == nil
}
func (t Tmux) Has(ctx context.Context, session string) bool {
	_, err := t.run(ctx, "has-session", "-t", session)
	return err == nil
}
func (t Tmux) Start(ctx context.Context, id string, l harnesses.Launch) (string, string, error) {
	if len(l.Argv) == 0 {
		return "", "", errors.New("empty harness argv")
	}
	session := "worker-" + safeName.ReplaceAllString(id, "-")
	if t.Has(ctx, session) {
		return session, session + ":0.0", nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Transcript), 0o700); err != nil {
		return "", "", err
	}
	parts := make([]string, 0, len(l.Env)+len(l.Argv))
	for k, v := range l.Env {
		parts = append(parts, quote(k+"="+v))
	}
	for _, v := range l.Argv {
		parts = append(parts, quote(v))
	}
	var cmd string
	if l.Interactive {
		// An interactive TUI owns the pane PTY directly. Its scrollback is the
		// human record; semantics arrive over the side channel, so there is no
		// tee-to-file pipeline (raw escape soup is not a useful transcript).
		cmd = "exec env " + strings.Join(parts, " ")
	} else {
		// Keep the harness off the PTY so its transcript bytes remain identical to
		// direct file redirection, while tee mirrors those bytes into the pane.
		// pipefail preserves the harness exit status instead of reporting tee's.
		pipeline := "exec env " + strings.Join(parts, " ") + " 2>&1 | tee -a " + quote(l.Transcript)
		cmd = "exec bash -o pipefail -c " + quote(pipeline)
	}
	args := []string{"new-session", "-d", "-s", session, "-n", "worker", "-c", l.Dir, cmd}
	fresh := !t.ServerAlive(ctx)
	if fresh {
		if fi, err := os.Lstat(t.Socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(t.Socket)
		}
		// -f applies the policy at server birth so it is born with exit-empty off
		// etc.; a config is read via -f ONLY on the server-spawning invocation.
		args = append([]string{"-f", t.config()}, args...)
	}
	if out, err := t.run(ctx, args...); err != nil {
		return "", "", fmt.Errorf("tmux start: %s: %w", out, err)
	}
	// Always source-file the policy after the session exists. On a fresh server
	// this forces the -f config to be fully applied SYNCHRONOUSLY before Start
	// returns (closing the rare race where an immediate query saw the PageUp
	// binding missing — the known TestPrivateTmuxLifecycle flake). On an adopted
	// server (supervisor restart) it re-applies the current policy the -f flag
	// could not, so a config change always takes effect on the next start.
	if _, err := t.run(ctx, "source-file", t.config()); err != nil {
		return "", "", fmt.Errorf("tmux source-file: %w", err)
	}
	// Scope remain-on-exit to the WORKER window only. The global policy keeps it
	// OFF so human-opened inspection windows close naturally on process exit;
	// the worker pane must retain its dead status so Pane() can read the harness
	// exit code after death (crash → failed settlement). set-option -w targets
	// exactly this window on the live server, applied idempotently on every start
	// path (fresh or adopted).
	if _, err := t.run(ctx, "set-option", "-w", "-t", session+":worker", "remain-on-exit", "on"); err != nil {
		return "", "", fmt.Errorf("tmux remain-on-exit scope: %w", err)
	}
	return session, session + ":0.0", nil
}
func (t Tmux) Kill(ctx context.Context, session string) error {
	_, err := t.run(ctx, "kill-session", "-t", session)
	return err
}

// KillServer tears down the complete private process boundary. All worker
// panes belong to this server, so killing it also kills every worker process.
// It is intentionally idempotent for shutdown paths where no worker ever
// caused the lazy tmux server to be created.
func (t Tmux) KillServer(ctx context.Context) error {
	if !t.ServerAlive(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.removeSocket()
		return nil
	}
	if _, err := t.run(ctx, "kill-server"); err != nil {
		return err
	}
	// Some tmux versions leave the now-inert Unix socket node behind briefly.
	// Remove only a socket at the private prepared path; never unlink an
	// unexpected replacement.
	t.removeSocket()
	return nil
}

func (t Tmux) removeSocket() {
	if fi, err := os.Lstat(t.Socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(t.Socket)
	}
}
func (t Tmux) Interrupt(ctx context.Context, target string) error {
	_, err := t.run(ctx, "send-keys", "-t", target, "C-c")
	return err
}

// Send injects text into an interactive TUI as a bracketed paste followed by
// Enter. Bracketed paste keeps multi-line text a single atomic paste in pi's
// input box instead of submitting a turn on every embedded newline.
func (t Tmux) Send(ctx context.Context, target, text string) error {
	if _, err := t.run(ctx, "send-keys", "-t", target, "-l", "\x1b[200~"+text+"\x1b[201~"); err != nil {
		return err
	}
	_, err := t.run(ctx, "send-keys", "-t", target, "Enter")
	return err
}
func (t Tmux) Pane(ctx context.Context, target string) (bool, *int, error) {
	out, err := t.run(ctx, "display-message", "-p", "-t", target, "#{pane_dead} #{pane_dead_status}")
	if err != nil {
		return false, nil, err
	}
	f := strings.Fields(out)
	if len(f) > 0 && f[0] == "0" {
		return true, nil, nil
	}
	if len(f) > 1 {
		if n, e := strconv.Atoi(f[1]); e == nil {
			return false, &n, nil
		}
	}
	return false, nil, nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
