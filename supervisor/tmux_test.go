package supervisor

import (
	"context"
	"github.com/gisikw/golem/harnesses"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratedTmuxThemeIsComposed(t *testing.T) {
	dir := t.TempDir()
	theme := filepath.Join(dir, "theme.conf")
	if err := os.WriteFile(theme, []byte("set-option -g mode-style 'fg=colour1,bg=colour2'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOLEM_TMUX_THEME_CONFIG", theme)
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if err := tm.Prepare(); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(tm.config())
	if err != nil || !strings.Contains(string(config), "mode-style 'fg=colour1,bg=colour2'") {
		t.Fatalf("generated theme not composed: %q %v", config, err)
	}
}

func TestAttachPolicyRemovesTmuxControlBindings(t *testing.T) {
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if err := tm.Prepare(); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(tm.config())
	if err != nil {
		t.Fatal(err)
	}
	policy := string(config)
	for _, required := range []string{
		"unbind-key -a -T prefix",
		"bind-key -T prefix C-b send-prefix",
		"bind-key -T prefix d detach-client",
		"unbind-key -T root MouseDown3Pane",
		"unbind-key -T root MouseDown1Status",
	} {
		if !strings.Contains(policy, required) {
			t.Fatalf("attach restriction missing %q", required)
		}
	}
}

func TestMissingThemeIsNonBlocking(t *testing.T) {
	dir := t.TempDir()
	// A theme artifact that does not exist (or is a symlink) must be skipped so
	// workers still start on the plain policy config — theming is cosmetic.
	t.Setenv("GOLEM_TMUX_THEME_CONFIG", filepath.Join(dir, "absent.conf"))
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if err := tm.Prepare(); err != nil {
		t.Fatalf("missing theme blocked worker start: %v", err)
	}
	config, err := os.ReadFile(tm.config())
	if err != nil || !strings.Contains(string(config), "allow-passthrough on") {
		t.Fatalf("plain policy not written when theme absent: %q %v", config, err)
	}
	if strings.Contains(string(config), "canonical Golem palette") {
		t.Fatal("absent theme should not compose a palette section")
	}
}

func TestPrivateTmuxLifecycle(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if e := tm.Prepare(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	transcript := filepath.Join(dir, "out")
	s, target, e := tm.Start(ctx, "test", harnesses.Launch{Argv: []string{"sh", "-c", "printf 'visible-out\\n'; printf 'visible-err\\n' >&2; exit 7"}, Dir: dir, Transcript: transcript})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(target, s+":") {
		t.Fatal(target)
	}
	if !tm.Has(ctx, s) {
		t.Fatal("session absent")
	}
	if out, e := tm.run(ctx, "show-options", "-gv", "allow-passthrough"); e != nil || out != "on" {
		t.Fatalf("explicit config missing: %q %v", out, e)
	}
	if out, e := tm.run(ctx, "show-options", "-gv", "mouse"); e != nil || out != "on" {
		t.Fatalf("mouse arbitration missing: %q %v", out, e)
	}
	if out, e := tm.run(ctx, "list-keys", "-T", "root", "PageUp"); e != nil || !strings.Contains(out, "#{alternate_on}") {
		t.Fatalf("PageUp arbitration missing: %q %v", out, e)
	}
	var exit *int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		alive, code, paneErr := tm.Pane(ctx, target)
		if paneErr != nil {
			t.Fatal(paneErr)
		}
		if !alive {
			exit = code
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exit == nil || *exit != 7 {
		t.Fatalf("pipeline lost harness exit status: %v", exit)
	}
	pane, e := tm.run(ctx, "capture-pane", "-p", "-S", "-", "-t", target)
	if e != nil || !strings.Contains(pane, "visible-out") || !strings.Contains(pane, "visible-err") {
		t.Fatalf("harness output not visible in pane: %q %v", pane, e)
	}
	got, e := os.ReadFile(transcript)
	if e != nil || string(got) != "visible-out\nvisible-err\n" {
		t.Fatalf("transcript is not an exact output copy: %q %v", got, e)
	}
	_, _ = tm.run(ctx, "kill-server")
}

func TestInteractiveLaunchOwnsPaneWithoutTee(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if e := tm.Prepare(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _, _ = tm.run(context.Background(), "kill-server") })
	transcript := filepath.Join(dir, "out")
	// An interactive harness owns the pane PTY directly: no tee pipeline, so the
	// transcript file is never created. The pane still shows its output.
	s, target, e := tm.Start(ctx, "interactive", harnesses.Launch{Argv: []string{"sh", "-c", "printf 'tui-live\\n'; sleep 0.5"}, Dir: dir, Transcript: transcript, Interactive: true})
	if e != nil {
		t.Fatal(e)
	}
	start := tm.mustPaneCommand(ctx, t, target)
	if strings.Contains(start, "tee") || strings.Contains(start, "pipefail") {
		t.Fatalf("interactive launch wrapped harness in tee pipeline: %q", start)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("interactive launch created a transcript file: %v", err)
	}
	_ = s
}

func (t Tmux) mustPaneCommand(ctx context.Context, tb *testing.T, target string) string {
	tb.Helper()
	out, err := t.run(ctx, "display-message", "-p", "-t", target, "#{pane_start_command}")
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// A server started WITHOUT the pinned config (a foreign/older process, or a
// supervisor that predates a policy change) never applies the policy, because
// tmux reads a config via -f only at server birth. This reproduces the
// real-world defect (pi warns extended-keys off, no mouse scroll, no PageUp
// copy-mode) and asserts that starting a worker — or ReapplyPolicy — repairs it
// via source-file on the live server.
func TestPolicyReappliedToAdoptedServer(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if e := tm.Prepare(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _, _ = tm.run(context.Background(), "kill-server") })
	// Birth a server on this socket WITHOUT -f (config-less), exactly as an
	// adopted/foreign server would come up. -f /dev/null blocks any ambient
	// user config so the defaults are unambiguous.
	if out, e := tm.run(ctx, "-f", os.DevNull, "new-session", "-d", "-s", "foreign", "sh"); e != nil {
		t.Fatalf("seed foreign server: %s %v", out, e)
	}
	if out, _ := tm.run(ctx, "show-options", "-gv", "allow-passthrough"); out == "on" {
		t.Fatal("precondition failed: config-less server already has policy")
	}
	// Starting a worker on the adopted server must apply the policy.
	if _, _, e := tm.Start(ctx, "adopt", harnesses.Launch{Argv: []string{"sh", "-c", "sleep 1"}, Dir: dir, Transcript: filepath.Join(dir, "out"), Interactive: true}); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ opt, want string }{
		{"allow-passthrough", "on"},
		{"extended-keys", "on"},
		{"mouse", "on"},
	} {
		if out, e := tm.run(ctx, "show-options", "-gv", tc.opt); e != nil || out != tc.want {
			t.Fatalf("policy %s not applied to adopted server: %q %v", tc.opt, out, e)
		}
	}
	if out, e := tm.run(ctx, "list-keys", "-T", "root", "PageUp"); e != nil || !strings.Contains(out, "#{alternate_on}") {
		t.Fatalf("PageUp copy-mode binding not applied to adopted server: %q %v", out, e)
	}
}

// DefaultShell (GOLEM_INTERACTIVE_SHELL) is written as the tmux default-shell
// so windows/panes a human opens use an interactive readline bash, not the
// non-interactive Nix build bash (literal \[\] PS1). Empty preserves tmux's
// default; a path with a double quote is rejected.
func TestDefaultShellPolicy(t *testing.T) {
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock"), DefaultShell: "/nix/store/x/bin/bash"}
	if err := tm.Prepare(); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(tm.config())
	if err != nil || !strings.Contains(string(cfg), "set-option -g default-shell \"/nix/store/x/bin/bash\"") {
		t.Fatalf("default-shell not written: %q %v", cfg, err)
	}
	tm2 := Tmux{Socket: filepath.Join(dir, "tmux2.sock")}
	if err := tm2.Prepare(); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := os.ReadFile(tm2.config())
	if strings.Contains(string(cfg2), "default-shell") {
		t.Fatalf("default-shell written when unset: %q", cfg2)
	}
	tm3 := Tmux{Socket: filepath.Join(dir, "tmux3.sock"), DefaultShell: "/bin/ba\"sh"}
	if err := tm3.Prepare(); err == nil {
		t.Fatal("expected rejection of double-quoted default-shell path")
	}
}

// remain-on-exit is scoped to the worker window: the WORKER pane retains its
// dead status (exit-status capture) while a second window a human opens closes
// naturally when its command exits. The global policy default is OFF.
func TestRemainOnExitScopedToWorkerWindow(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if e := tm.Prepare(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _, _ = tm.run(context.Background(), "kill-server") })
	transcript := filepath.Join(dir, "out")
	s, target, e := tm.Start(ctx, "scope", harnesses.Launch{Argv: []string{"sh", "-c", "printf 'worker\\n'; exit 5"}, Dir: dir, Transcript: transcript})
	if e != nil {
		t.Fatal(e)
	}
	if out, e := tm.run(ctx, "show-options", "-gv", "remain-on-exit"); e != nil || out != "off" {
		t.Fatalf("global remain-on-exit must be off: %q %v", out, e)
	}
	if out, e := tm.run(ctx, "show-options", "-wv", "-t", s+":worker", "remain-on-exit"); e != nil || out != "on" {
		t.Fatalf("worker window remain-on-exit must be on: %q %v", out, e)
	}
	var exit *int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		alive, code, paneErr := tm.Pane(ctx, target)
		if paneErr != nil {
			t.Fatal(paneErr)
		}
		if !alive {
			exit = code
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exit == nil || *exit != 5 {
		t.Fatalf("worker pane lost exit status after scoping: %v", exit)
	}
	// A SECOND window a human opens must NOT remain after its command exits.
	if _, e := tm.run(ctx, "new-window", "-a", "-t", s+":worker", "-n", "inspect", "sh", "-c", "exit 0"); e != nil {
		t.Fatalf("open inspect window: %v", e)
	}
	gone := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, e := tm.run(ctx, "list-windows", "-t", s, "-F", "#{window_name}")
		if e != nil {
			t.Fatalf("list-windows: %v", e)
		}
		if !strings.Contains(out, "inspect") {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Fatal("human-opened window lingered as a dead pane after its command exited")
	}
}

// ReapplyPolicy is the boot-time repair for a supervisor that adopts a live
// server (sessions persist across restarts). It is a no-op with no server and
// idempotently loads the policy when one is running.
func TestReapplyPolicyRepairsLiveServer(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tm := Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if e := tm.Prepare(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _, _ = tm.run(context.Background(), "kill-server") })
	// No server: must be a silent no-op, not an error.
	if e := tm.ReapplyPolicy(ctx); e != nil {
		t.Fatalf("ReapplyPolicy must no-op without a server: %v", e)
	}
	if out, e := tm.run(ctx, "-f", os.DevNull, "new-session", "-d", "-s", "foreign", "sh"); e != nil {
		t.Fatalf("seed foreign server: %s %v", out, e)
	}
	if e := tm.ReapplyPolicy(ctx); e != nil {
		t.Fatalf("ReapplyPolicy on live server: %v", e)
	}
	if out, e := tm.run(ctx, "show-options", "-gv", "extended-keys"); e != nil || out != "on" {
		t.Fatalf("ReapplyPolicy did not load policy into live server: %q %v", out, e)
	}
}
