package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gisikw/golem/attachssh"
	"github.com/gisikw/golem/client"
	golemconfig "github.com/gisikw/golem/config"
	piadapter "github.com/gisikw/golem/harnesses/pi"
	"github.com/gisikw/golem/service"
	"github.com/gisikw/golem/supervisor"
	gliderssh "github.com/gliderlabs/ssh"
)

const version = "0.1.0"

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func secondsEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		slog.Error("seconds environment must be non-negative", "key", key)
		os.Exit(2)
	}
	return time.Duration(n) * time.Second
}
func argvEnv(key string, fallback []string) []string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	var out []string
	if json.Unmarshal([]byte(value), &out) != nil {
		slog.Error("argv environment must be JSON array", "key", key)
		os.Exit(2)
	}
	return out
}
func main() {
	home, _ := os.UserHomeDir()
	defaultState := filepath.Join(home, ".local", "state", "golem")
	configPath := flag.String("config", "", "operator TOML config (required)")
	state := flag.String("state", env("GOLEM_STATE", defaultState), "durable daemon state directory")
	db := flag.String("db", env("GOLEM_DB", ""), "SQLite path (default: STATE/golem.db)")
	socket := flag.String("unix", env("GOLEM_SOCKET", ""), "Unix socket (default: STATE/golemd.sock; empty via --listen-only)")
	listen := flag.String("listen", env("GOLEM_LISTEN", ""), "TCP address; empty disables")
	listenOnly := flag.Bool("listen-only", false, "disable the default Unix listener")
	artifactRoot := flag.String("artifact-root", env("GOLEM_ARTIFACT_ROOT", ""), "artifact root (default: STATE/artifacts)")
	allowedRoots := flag.String("allowed-cwd-roots", env("GOLEM_ALLOWED_CWD_ROOTS", home), "allowed CWD roots separated by OS path-list separator")
	interval := flag.Duration("poll", time.Second, "reconcile interval")
	offline := flag.Duration("offline-restart-window", 30*time.Minute, "maximum disconnected recreation window")
	linger := flag.Duration("linger", secondsEnv("GOLEM_LINGER_SECONDS", time.Hour), "settled tmux retention")
	piBinary := flag.String("pi", env("GOLEM_PI", "pi"), "pi executable")
	flag.Parse()

	cfg, err := golemconfig.Load(*configPath)
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(2)
	}
	if *db == "" {
		*db = filepath.Join(*state, "golem.db")
	}
	if *socket == "" && !*listenOnly {
		*socket = filepath.Join(*state, "golemd.sock")
	}
	if *artifactRoot == "" {
		*artifactRoot = filepath.Join(*state, "artifacts")
	}
	if err = validateTCPAuth(*listen, cfg.APIBearerTokens); err != nil {
		slog.Error("config", "error", err)
		os.Exit(2)
	}
	if *listen != "" && len(cfg.APIBearerTokens) == 0 {
		slog.Warn("loopback TCP listener has no bearer tokens; use only for local development", "address", *listen)
	}
	if *offline < 0 || *linger < 0 || *interval <= 0 || *allowedRoots == "" {
		slog.Error("invalid daemon configuration")
		os.Exit(2)
	}
	if err = os.MkdirAll(*state, 0o700); err != nil {
		slog.Error("state directory", "error", err)
		os.Exit(1)
	}

	if err = os.MkdirAll(*artifactRoot, 0o700); err != nil {
		slog.Error("artifact root", "error", err)
		os.Exit(1)
	}

	store, err := service.Open(*db)
	if err != nil {
		slog.Error("open store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	piProviders := make(map[string]piadapter.Provider, len(cfg.Providers))
	providerNames := make(map[string]bool, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		piProviders[name] = piadapter.Provider{Kind: provider.Kind, BaseURL: provider.BaseURL, APIKeyEnv: provider.APIKeyEnv}
		providerNames[name] = true
	}
	piEnv := map[string]string{}
	for _, name := range []string{"GOLEM_TIAMAT_URL", "GOLEM_TIAMAT_TOKEN_FILE"} {
		if value := os.Getenv(name); value != "" {
			piEnv[name] = value
		}
	}
	piAdapter := piadapter.Adapter{Binary: *piBinary, HookExtension: os.Getenv("GOLEM_HOOK_EXTENSION"), WebExtension: os.Getenv("GOLEM_WEB_EXTENSION"), SourceProfile: env("GOLEM_PI_SOURCE_PROFILE", os.Getenv("PI_CODING_AGENT_DIR")), CopyAuth: os.Getenv("GOLEM_COPY_AUTH") == "1", DefaultProvider: os.Getenv("GOLEM_PI_DEFAULT_PROVIDER"), DefaultModel: os.Getenv("GOLEM_PI_DEFAULT_MODEL"), Providers: piProviders, Env: piEnv}
	adapters := supervisor.ConfiguredAdapters(piAdapter, argvEnv("GOLEM_CLAUDE_ARGV", []string{"claude", "{prompt}"}), argvEnv("GOLEM_CODEX_ARGV", []string{"codex", "{prompt}"}))
	for harness := range cfg.Harnesses {
		if _, ok := adapters[harness]; !ok {
			slog.Error("configured harness has no adapter", "harness", harness)
			os.Exit(2)
		}
	}

	projects := make(map[string]string, len(cfg.Projects))
	for name, project := range cfg.Projects {
		projects[name] = project.Path
	}
	workspaceResolver := &service.WorkspaceResolver{State: *state, Projects: projects, CloneEnabled: cfg.CloneEnabled}
	api := service.API{Store: store, Capabilities: cfg.Capabilities(version), Workspaces: workspaceResolver, PiProviders: providerNames, ArtifactRoot: *artifactRoot}
	servers, listeners, err := serve(api.Handler(), *socket, *listen, cfg.APIBearerTokens)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	if len(servers) == 0 {
		slog.Error("no listeners configured")
		os.Exit(1)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	registry, err := supervisor.OpenRegistry(filepath.Join(*state, "workers.json"))
	if err != nil {
		slog.Error("registry", "error", err)
		os.Exit(1)
	}
	tmux := supervisor.Tmux{Socket: filepath.Join(*state, "tmux.sock"), Config: filepath.Join(*state, "tmux.conf"), DefaultShell: os.Getenv("GOLEM_INTERACTIVE_SHELL")}
	if err = tmux.Prepare(); err != nil {
		slog.Error("tmux prepare", "error", err)
		os.Exit(1)
	}
	if err = tmux.ReapplyPolicy(context.Background()); err != nil {
		slog.Warn("tmux policy reapply on boot failed", "error", err)
	}
	roots := strings.Split(*allowedRoots, string(os.PathListSeparator))
	for _, project := range cfg.Projects {
		roots = append(roots, project.Path)
	}
	roots = append(roots, filepath.Join(*state, "clones"))
	internalEndpoint := "unix://" + *socket
	if *socket == "" {
		internalEndpoint = "http://" + *listen
	}
	internalClient := client.New(internalEndpoint)
	if *socket == "" && len(cfg.APIBearerTokens) > 0 {
		internalClient.Token = cfg.APIBearerTokens[0]
	}
	sup := &supervisor.Supervisor{Host: cfg.Name, Client: internalClient, Registry: registry, Tmux: tmux, OfflineWindow: *offline, Linger: *linger, ArtifactRoot: *artifactRoot, AllowedCWDRoots: roots, Adapters: adapters, AttachHost: cfg.Name, AttachPort: cfg.AttachSSH.Port}

	var sshServer *attachssh.Server
	if cfg.AttachSSH.Port != 0 {
		hostSigner, keyErr := attachssh.LoadOrCreateHostKey(cfg.AttachSSH.HostKeyPath)
		if keyErr != nil {
			slog.Error("SSH host key", "error", keyErr)
			os.Exit(1)
		}
		authorized, keysErr := attachssh.LoadAuthorizedKeys(cfg.AttachSSH.AuthorizedKeysPath)
		if keysErr != nil {
			slog.Error("SSH authorized keys", "error", keysErr)
			os.Exit(1)
		}
		sshListener, listenErr := net.Listen("tcp", fmt.Sprintf(":%d", cfg.AttachSSH.Port))
		if listenErr != nil {
			slog.Error("SSH attach listen", "error", listenErr)
			os.Exit(1)
		}
		sshServer = attachssh.New(registry, tmux, hostSigner, authorized)
		go func() {
			slog.Info("listening", "component", "attach-ssh", "address", sshListener.Addr())
			if serveErr := sshServer.Serve(sshListener); serveErr != nil && !errors.Is(serveErr, gliderssh.ErrServerClosed) {
				slog.Error("SSH attach serve", "error", serveErr)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err = sup.Tick(ctx); err != nil {
		slog.Warn("initial reconcile unavailable; applying offline policy", "error", err)
		sup.Recover(ctx)
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownDaemon(servers, sshServer, tmux)
			return
		case <-ticker.C:
			if err = sup.Tick(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("reconcile failed; workers preserved", "error", err)
			}
		}
	}
}

// shutdownDaemon runs only after the signal context has stopped the
// supervisor loop. First stop accepting API/attach traffic, then tear down the
// private tmux process boundary and every worker pane it owns. Store state is
// deliberately untouched: boot reconciliation either resumes a capable
// adapter or records a vanished non-resumable worker as failed; shutdown never
// fabricates successful completion.
func shutdownDaemon(servers []*http.Server, sshServer *attachssh.Server, tmux supervisor.Tmux) {
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			slog.Warn("HTTP shutdown incomplete", "error", err)
		}
	}
	if sshServer != nil {
		if err := sshServer.Shutdown(shutdown); err != nil {
			slog.Warn("SSH attach shutdown incomplete", "error", err)
		}
	}
	cancel()

	// Use a fresh bound so a slow HTTP drain cannot prevent worker teardown.
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if err := tmux.KillServer(killCtx); err != nil {
		slog.Error("private tmux shutdown failed", "error", err)
	}
}

func validateTCPAuth(address string, tokens []string) error {
	if address == "" || len(tokens) > 0 {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid TCP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("TCP listen beyond loopback requires api_bearer_tokens")
}

func serve(handler http.Handler, socket, address string, tokens []string) ([]*http.Server, []net.Listener, error) {
	var servers []*http.Server
	var listeners []net.Listener
	start := func(network, target string) error {
		if network == "unix" {
			absolute, err := filepath.Abs(target)
			if err != nil {
				return err
			}
			target = absolute
			if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if info, statErr := os.Lstat(target); statErr == nil {
				if info.Mode()&os.ModeSocket == 0 {
					return errors.New("refusing to replace non-socket unix path")
				}
				_ = os.Remove(target)
			}
		}
		listener, err := net.Listen(network, target)
		if err != nil {
			return err
		}
		if network == "unix" {
			_ = os.Chmod(target, 0o600)
		}
		serverHandler := handler
		if network == "tcp" && len(tokens) > 0 {
			serverHandler = service.BearerAuth(tokens, handler)
		}
		server := &http.Server{Handler: serverHandler, ReadHeaderTimeout: 5 * time.Second}
		servers, listeners = append(servers, server), append(listeners, listener)
		go func() {
			slog.Info("listening", "component", "golemd", "network", network, "address", target)
			if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				slog.Error("serve", "error", serveErr)
			}
		}()
		return nil
	}
	if socket != "" {
		if err := start("unix", socket); err != nil {
			return nil, nil, err
		}
	}
	if address != "" {
		if err := start("tcp", address); err != nil {
			return nil, nil, err
		}
	}
	return servers, listeners, nil
}
