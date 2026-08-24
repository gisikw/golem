package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/gisikw/golem/client"
	piadapter "github.com/gisikw/golem/harnesses/pi"
	"github.com/gisikw/golem/supervisor"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func secondsEnv(k string, fallback time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Error("seconds environment must be a non-negative integer", "key", k)
		os.Exit(2)
	}
	return time.Duration(n) * time.Second
}
func argvEnv(k string, fallback []string) []string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		slog.Error("argv environment must be JSON array", "key", k)
		os.Exit(2)
	}
	return out
}

// settlementNotifiers assembles the configured settlement callbacks. It
// degrades gracefully: with neither variable set it returns nil and settlement
// proceeds without a courtesy notification.
//
//	GOLEM_WORKLIST_DIR: local drop-box.
//	GOLEM_SETTLEMENT_WEBHOOK: HTTP POST for cross-host supervisors.
func settlementNotifiers(host string) supervisor.Notifier {
	var ns supervisor.Notifiers
	if dir := os.Getenv("GOLEM_WORKLIST_DIR"); dir != "" {
		ns = append(ns, supervisor.WorklistNotifier{Host: host, Dir: dir})
	}
	if url := os.Getenv("GOLEM_SETTLEMENT_WEBHOOK"); url != "" {
		ns = append(ns, supervisor.WebhookNotifier{Host: host, URL: url})
	}
	if len(ns) == 0 {
		return nil
	}
	return ns
}
func main() {
	home, _ := os.UserHomeDir()
	stateDefault := filepath.Join(home, ".local", "state", "golem", "supervisor")
	host := flag.String("host", env("GOLEM_HOST", "local"), "explicit worker host name")
	endpoint := flag.String("service", env("GOLEM_ENDPOINT", "http://127.0.0.1:7337"), "service HTTP URL or unix://path")
	state := flag.String("state", env("GOLEM_SUPERVISOR_STATE", stateDefault), "local durable state directory")
	artifactRoot := flag.String("artifact-root", env("GOLEM_ARTIFACT_ROOT", ""), "host-local artifact root (default: STATE/artifacts)")
	allowedRoots := flag.String("allowed-cwd-roots", env("GOLEM_ALLOWED_CWD_ROOTS", home), "allowed CWD roots separated by the OS path-list separator")
	interval := flag.Duration("poll", 5*time.Second, "reconcile interval")
	offline := flag.Duration("offline-restart-window", 30*time.Minute, "maximum disconnected recreation window")
	linger := flag.Duration("linger", secondsEnv("GOLEM_LINGER_SECONDS", time.Hour), "settled worker tmux retention")
	pi := flag.String("pi", env("GOLEM_PI", "pi"), "pi executable")
	flag.Parse()
	if *host == "" || *offline < 0 || *linger < 0 || *allowedRoots == "" {
		slog.Error("invalid supervisor configuration")
		os.Exit(2)
	}
	if *artifactRoot == "" {
		*artifactRoot = filepath.Join(*state, "artifacts")
	}
	roots := strings.Split(*allowedRoots, string(os.PathListSeparator))
	if err := os.MkdirAll(*state, 0700); err != nil {
		slog.Error("state directory", "error", err)
		os.Exit(1)
	}
	reg, err := supervisor.OpenRegistry(filepath.Join(*state, "workers.json"))
	if err != nil {
		slog.Error("registry", "error", err)
		os.Exit(1)
	}
	tm := supervisor.Tmux{Socket: filepath.Join(*state, "tmux.sock"), Config: filepath.Join(*state, "tmux.conf"), DefaultShell: os.Getenv("GOLEM_INTERACTIVE_SHELL")}
	if err = tm.Prepare(); err != nil {
		slog.Error("tmux prepare", "error", err)
		os.Exit(1)
	}
	// A supervisor restart may adopt a server started by a previous process
	// (sessions persist: exit-empty off, settled workers linger). tmux reads a
	// config via -f only at server birth, so re-apply the current policy to any
	// already-running server now. Best-effort: a source-file failure must not
	// stop the supervisor from reconciling.
	if err = tm.ReapplyPolicy(context.Background()); err != nil {
		slog.Warn("tmux policy reapply on boot failed", "error", err)
	}
	piAdapter := piadapter.Adapter{
		Binary:        *pi,
		HookExtension: os.Getenv("GOLEM_HOOK_EXTENSION"),
		WebExtension:  os.Getenv("GOLEM_WEB_EXTENSION"),
		// SourceProfile is an optional pi dir used only for model catalog,
		// defaults, theme, and explicitly opted-in authentication. Its extension
		// list is never read by worker profiles.
		SourceProfile:   env("GOLEM_PI_SOURCE_PROFILE", os.Getenv("PI_CODING_AGENT_DIR")),
		CopyAuth:        os.Getenv("GOLEM_COPY_AUTH") == "1",
		DefaultProvider: os.Getenv("GOLEM_PI_DEFAULT_PROVIDER"),
		DefaultModel:    os.Getenv("GOLEM_PI_DEFAULT_MODEL"),
	}
	s := &supervisor.Supervisor{Host: *host, Client: client.New(*endpoint), Registry: reg, Tmux: tm, OfflineWindow: *offline, Linger: *linger, ArtifactRoot: *artifactRoot, AllowedCWDRoots: roots, Adapters: supervisor.ConfiguredAdapters(piAdapter, argvEnv("GOLEM_CLAUDE_ARGV", []string{"claude", "{prompt}"}), argvEnv("GOLEM_CODEX_ARGV", []string{"codex", "{prompt}"})), Notify: settlementNotifiers(*host)}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Reconcile with global truth before any reboot recreation. Only when the
	// service is unavailable may the bounded offline policy authorize recovery.
	if err = s.Tick(ctx); err != nil {
		slog.Warn("initial reconcile unavailable; applying offline policy", "error", err)
		s.Recover(ctx)
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err = s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("reconcile failed; workers preserved", "error", err)
		}
	}
}
