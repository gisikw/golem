package herdr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/golem/backend"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

// DefaultReconcile is the safety-net poll interval. Herdr's event envelopes
// carry no sequence number and offer no resumable cursor, so a dropped
// subscription is an observation gap until this poll closes it.
const DefaultReconcile = 15 * time.Second

// DefaultStartupTimeout bounds agent.start's own readiness wait.
const DefaultStartupTimeout = 60 * time.Second

var unsafeName = regexp.MustCompile(`[^a-z0-9_-]`)

// AgentName is the durable binding between a Golem job and a Herdr agent.
// Herdr names must match [a-z][a-z0-9_-]{0,31} and be unique among live
// agents, so a restarted golemd can re-find its agent by name via agent.list.
func AgentName(jobID string) string {
	id := unsafeName.ReplaceAllString(strings.ToLower(jobID), "-")
	name := "job-" + strings.TrimPrefix(id, "job-")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// Backend runs Golem jobs inside one Herdr session on this host. Session and
// target handles are the agent name and the pane id; the supervisor persists
// both in its durable worker registry, which is the job binding.
type Backend struct {
	// Socket is the session's Unix socket path.
	Socket string
	// Kinds maps a Golem harness (argv[0] base name) to a Herdr agent kind.
	// Only pi is supported in the minimum viable cut.
	Kinds map[string]string
	// StartupTimeout bounds agent.start; Reconcile is the safety-net poll.
	StartupTimeout time.Duration
	Reconcile      time.Duration
	Logger         *slog.Logger
	// Owner is non-nil only for golemd's private child server. Shutdown stops
	// that exact process; no Herdr CLI stop command or ambient socket is used.
	Owner interface{ Shutdown(context.Context) error }

	mu       sync.Mutex
	states   map[string]backend.Status // pane id -> last observation
	watching map[string]context.CancelFunc
	base     context.Context
}

func (b *Backend) log() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

func (b *Backend) kind(harness string) (string, bool) {
	kinds := b.Kinds
	if len(kinds) == 0 {
		kinds = map[string]string{"pi": "pi"}
	}
	k, ok := kinds[harness]
	return k, ok
}

// call runs exactly one Herdr request on its own connection. herdr 0.8.1
// closes a connection as soon as it has answered one request (verified
// against the live server: a second write gets EPIPE), so there is no control
// connection to keep alive; only a subscription keeps its socket. Herdr error
// frames are returned as-is: they are answers, not transport failures.
func (b *Backend) call(ctx context.Context, f func(*Conn) error) error {
	c, err := Dial(ctx, b.Socket)
	if err != nil {
		return fmt.Errorf("herdr socket %s: %w", b.Socket, err)
	}
	defer c.Close()
	return f(c)
}

// Prepare is the startup capability check: the socket must be reachable and
// speak exactly protocol 20. golemd falls back to tmux (loudly) when it fails.
func (b *Backend) Prepare() error {
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version string
	var proto int
	err := b.call(ctx2, func(c *Conn) error {
		var e error
		version, proto, e = c.Ping(ctx2)
		return e
	})
	if err != nil {
		return fmt.Errorf("herdr ping: %w", err)
	}
	if proto != Protocol {
		return fmt.Errorf("herdr protocol %d unsupported (golem speaks %d; server version %s)", proto, Protocol, version)
	}
	b.log().Info("herdr backend ready", "socket", b.Socket, "version", version, "protocol", proto)
	return nil
}

// Run owns the asynchronous half: it holds the context every per-pane event
// subscription lives under and runs the reconcile poll that closes the gaps a
// dropped subscription leaves behind.
func (b *Backend) Run(ctx context.Context) {
	b.mu.Lock()
	b.base = ctx
	b.mu.Unlock()
	interval := b.Reconcile
	if interval <= 0 {
		interval = DefaultReconcile
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcile(ctx)
		}
	}
}

// reconcile is the safety net and the restart-adoption read: one agent.list,
// stamped with the time the response was read.
func (b *Backend) reconcile(ctx context.Context) {
	var agents []AgentInfo
	err := b.call(ctx, func(c *Conn) error {
		var e error
		agents, e = c.AgentList(ctx)
		return e
	})
	if err != nil {
		if ctx.Err() == nil {
			b.log().Warn("herdr reconcile failed", "error", err)
		}
		return
	}
	at := time.Now().UTC()
	live := map[string]bool{}
	for _, a := range agents {
		live[a.PaneID] = true
		b.record(a.PaneID, a.AgentStatus, at)
		b.watch(a.PaneID)
	}
	b.mu.Lock()
	for pane := range b.states {
		if !live[pane] {
			status := b.states[pane]
			status.Present, status.AsOf = false, at
			b.states[pane] = status
		}
	}
	b.mu.Unlock()
}

// record applies one observation. unknown keeps the last known state and only
// raises the stale flag, exactly as the mapping table requires.
func (b *Backend) record(pane, herdrStatus string, at time.Time) {
	state, stale, known := MapStatus(herdrStatus)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		b.states = map[string]backend.Status{}
	}
	status := b.states[pane]
	status.Present, status.AsOf, status.Stale = true, at, stale
	if !known {
		b.log().Warn("unrecognised herdr agent status", "pane", pane, "status", herdrStatus)
	}
	if state != "" {
		status.State = state
	}
	b.states[pane] = status
}

// Status implements backend.Observer: the supervisor uses it only to stamp an
// honest as_of on the state events it publishes.
func (b *Backend) Status(target string) (backend.Status, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[target]
	return s, ok
}

// watch subscribes to one pane's agent status changes. Protocol 20 requires a
// pane_id per subscription, so this is one connection per live job. as_of is
// the receipt time of the pushed line; a drop ends the goroutine and the
// reconcile poll re-establishes it.
func (b *Backend) watch(pane string) {
	b.mu.Lock()
	if b.watching == nil {
		b.watching = map[string]context.CancelFunc{}
	}
	base := b.base
	_, already := b.watching[pane]
	if already || base == nil {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(base)
	b.watching[pane] = cancel
	b.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			b.mu.Lock()
			delete(b.watching, pane)
			b.mu.Unlock()
		}()
		c, err := Dial(ctx, b.Socket)
		if err != nil {
			b.log().Warn("herdr subscription dial failed", "pane", pane, "error", err)
			return
		}
		events, err := c.Subscribe(ctx, []map[string]any{{"type": "pane.agent_status_changed", "pane_id": pane}})
		if err != nil {
			_ = c.Close()
			b.log().Warn("herdr subscribe failed", "pane", pane, "error", err)
			return
		}
		for event := range events {
			var data struct {
				PaneID      string `json:"pane_id"`
				AgentStatus string `json:"agent_status"`
			}
			if json.Unmarshal(event.Data, &data) != nil || data.PaneID == "" {
				continue
			}
			b.record(data.PaneID, data.AgentStatus, event.ReceivedAt)
		}
	}()
}

func (b *Backend) unwatch(pane string) {
	b.mu.Lock()
	cancel := b.watching[pane]
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Start resolves the job's agent, creating the workspace and launching the
// harness when it is not already live. It is idempotent: an existing agent
// with this job's name is adopted (restart adoption), never duplicated.
func (b *Backend) Start(ctx context.Context, id string, l harnesses.Launch) (string, string, error) {
	if len(l.Argv) == 0 {
		return "", "", errors.New("empty harness argv")
	}
	name := AgentName(id)
	harness := filepath.Base(l.Argv[0])
	kind, ok := b.kind(harness)
	if !ok {
		return "", "", fmt.Errorf("harness %q not supported on herdr backend", harness)
	}
	existing, found, err := b.agent(ctx, name)
	if err != nil {
		return "", "", err
	}
	if found {
		b.record(existing.PaneID, existing.AgentStatus, time.Now().UTC())
		b.watch(existing.PaneID)
		b.log().Info("adopted live herdr agent", "job", id, "agent", name, "pane", existing.PaneID)
		return name, existing.PaneID, nil
	}

	args, prompt := splitPrompt(l)
	env := launchEnv(l)
	timeout := int(b.startupTimeout() / time.Millisecond)
	var pane PaneInfo
	err = b.call(ctx, func(c *Conn) error {
		var e error
		_, pane, e = c.WorkspaceCreate(ctx, l.Dir, "golem/"+id, env)
		return e
	})
	if err != nil {
		return "", "", fmt.Errorf("herdr start %s: workspace.create: %w", id, err)
	}
	// agent.start blocks until Herdr has detected the agent and considers it
	// interactive-ready, so it gets its own deadline from the configured
	// startup timeout rather than the caller's reconcile-tick context.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.startupTimeout()+10*time.Second)
	defer cancel()
	err = b.startInNewPane(startCtx, name, kind, pane.PaneID, args, timeout)
	if err != nil {
		// The workspace exists but has no agent: close it so a retry does not
		// leak one workspace per attempt.
		if closeErr := b.closeWorkspace(ctx, workspaceOf(pane.PaneID)); closeErr != nil {
			b.log().Warn("herdr workspace cleanup after failed start", "job", id, "error", closeErr)
		}
		return "", "", fmt.Errorf("herdr start %s: agent.start: %w", id, err)
	}
	if prompt != "" {
		if err = b.promptWhenReady(ctx, name, prompt); err != nil {
			// A workspace with an unprompted agent in it is worse than none: the
			// next attempt would adopt an idle pi that was never given its task.
			if closeErr := b.closeWorkspace(ctx, workspaceOf(pane.PaneID)); closeErr != nil {
				b.log().Warn("herdr workspace cleanup after failed prompt", "job", id, "error", closeErr)
			}
			return "", "", fmt.Errorf("herdr start %s: agent.prompt: %w", id, err)
		}
	}
	b.record(pane.PaneID, "working", time.Now().UTC())
	b.watch(pane.PaneID)
	b.log().Info("herdr agent started", "job", id, "agent", name, "pane", pane.PaneID, "workspace", workspaceOf(pane.PaneID))
	return name, pane.PaneID, nil
}

// startInNewPane bridges workspace creation and shell readiness. Herdr can
// return a pane before its shell reaches the foreground prompt; agent.start's
// timeout only covers readiness AFTER launch. Retry only its explicit pre-launch
// busy rejection, in the SAME newly-created pane, under the startup deadline.
// Never retry transport/ambiguous launch errors (which could duplicate an agent).
func (b *Backend) startInNewPane(ctx context.Context, name, kind, pane string, args []string, timeout int) error {
	for {
		err := b.call(ctx, func(c *Conn) error {
			_, e := c.AgentStart(ctx, name, kind, pane, args, timeout)
			return e
		})
		var remote *Error
		if !errors.As(err, &remote) || remote.Code != "agent_pane_busy" {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for new pane %s shell readiness: %w", pane, ctx.Err())
		case <-timer.C:
		}
	}
}

// closeWorkspace tears a workspace down on a cleanup path where the caller's
// context may already be cancelled.
func (b *Backend) closeWorkspace(ctx context.Context, workspace string) error {
	if workspace == "" {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	return b.call(ctx, func(c *Conn) error { return c.WorkspaceClose(ctx, workspace) })
}

// promptWhenReady submits the initial task once Herdr has bound the agent to
// its name. agent.start returns as soon as it has detected the expected agent,
// but the named binding can lag a beat: agent.prompt then answers
// agent_not_ready ("not an active named agent"), observed against herdr 0.8.1.
// Golem waits for the binding instead of failing a perfectly good worker.
func (b *Backend) promptWhenReady(ctx context.Context, name, prompt string) error {
	deadline := time.Now().Add(b.startupTimeout())
	var lastErr error
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		info, found, err := b.agent(ctx, name)
		if err != nil {
			lastErr = err
			continue
		}
		if !found || info.LaunchPending || !info.InteractiveReady {
			lastErr = fmt.Errorf("agent %s not interactive-ready yet", name)
			continue
		}
		err = b.call(ctx, func(c *Conn) error { return c.AgentPrompt(ctx, name, prompt) })
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsCode(err, "agent_not_ready") && !IsCode(err, "agent_not_found") {
			return err
		}
	}
	return lastErr
}

// agent is one agent.get by name or pane.
func (b *Backend) agent(ctx context.Context, target string) (AgentInfo, bool, error) {
	var info AgentInfo
	var found bool
	err := b.call(ctx, func(c *Conn) error {
		var e error
		info, found, e = c.AgentGet(ctx, target)
		return e
	})
	return info, found, err
}

func (b *Backend) startupTimeout() time.Duration {
	if b.StartupTimeout > 0 {
		return b.StartupTimeout
	}
	return DefaultStartupTimeout
}

// splitPrompt separates the harness's initial task text from its argv. Herdr's
// agent.start takes only args and waits for interactive readiness, so the
// prompt is submitted afterwards through agent.prompt. The split happens only
// when the final argv element is exactly the adapter's declared prompt;
// otherwise argv is passed through untouched and nothing is submitted.
func splitPrompt(l harnesses.Launch) ([]string, string) {
	args := append([]string{}, l.Argv[1:]...)
	if l.Prompt == "" || len(args) == 0 || args[len(args)-1] != l.Prompt {
		return args, ""
	}
	return args[:len(args)-1], l.Prompt
}

// launchEnv is the environment for the workspace's root pane: the harness's
// own environment plus a PATH that puts the operator-configured harness binary
// first, because Herdr resolves the agent kind's executable through PATH.
// Secrets ride here, never in argv.
func launchEnv(l harnesses.Launch) map[string]string {
	env := map[string]string{}
	for k, v := range l.Env {
		env[k] = v
	}
	if dir := filepath.Dir(l.Argv[0]); strings.Contains(l.Argv[0], string(os.PathSeparator)) {
		env["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
	} else if path := os.Getenv("PATH"); path != "" {
		env["PATH"] = path
	}
	return env
}

func workspaceOf(pane string) string {
	if id, _, ok := strings.Cut(pane, ":"); ok {
		return id
	}
	return ""
}

// Has reports whether this job's agent is still live in the session.
func (b *Backend) Has(ctx context.Context, session string) bool {
	_, found, err := b.agent(ctx, session)
	return err == nil && found
}

// Pane reports liveness of the job's agent. Herdr exposes no exit status
// anywhere in the pane API (PR sketch B in the brief), so the exit code is
// always nil: absence, not a status code, is the observable fact.
func (b *Backend) Pane(ctx context.Context, target string) (bool, *int, error) {
	info, found, err := b.agent(ctx, target)
	if err != nil {
		return false, nil, err
	}
	if !found {
		b.mu.Lock()
		if status, ok := b.states[target]; ok {
			status.Present, status.AsOf = false, time.Now().UTC()
			b.states[target] = status
		}
		b.mu.Unlock()
		return false, nil, nil
	}
	b.record(target, info.AgentStatus, time.Now().UTC())
	return true, nil, nil
}

// Send is the steering path. Herdr 0.8.2 explicitly accepts agent.prompt for
// working agents and atomically submits the message. If the agent became
// blocked after /steer accepted the input, agent_blocked is returned without
// writing anything; raw dialog input would be unsafe and is never a fallback.
func (b *Backend) Send(ctx context.Context, target, text string) error {
	return b.call(ctx, func(c *Conn) error { return c.AgentPrompt(ctx, target, text) })
}

// Answer distinguishes Pi's structured next-message block from a Herdr
// screen-detected dialog. Pi retains its agents_block side channel and needs a
// submitted user message even while Herdr's lifecycle authority says blocked.
// Other harnesses receive only an explicit sequence of logical UI keys.
func (b *Backend) Answer(ctx context.Context, target string, harness protocol.HarnessKind, text string) error {
	if harness == protocol.HarnessPi {
		err := b.Send(ctx, target, text)
		if !IsCode(err, "agent_blocked") {
			return err
		}
		b.log().Info("delivering structured Pi answer through blocked pane", "pane", target)
		if err = b.call(ctx, func(c *Conn) error {
			return c.Call(ctx, "pane.send_text", map[string]any{"pane_id": target, "text": text}, nil)
		}); err != nil {
			return err
		}
		return b.call(ctx, func(c *Conn) error {
			return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": target, "keys": []string{"enter"}}, nil)
		})
	}
	keys, err := dialogKeys(text)
	if err != nil {
		return err
	}
	return b.call(ctx, func(c *Conn) error { return c.AgentSendKeys(ctx, target, keys...) })
}

func dialogKeys(text string) ([]string, error) {
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 || len(fields) > 4 {
		return nil, errors.New("screen answer must be 1-4 deliberate keys (for example `1 enter`, `down enter`, or `esc`)")
	}
	allowed := map[string]bool{"enter": true, "esc": true, "escape": true, "up": true, "down": true, "left": true, "right": true, "tab": true, "space": true, "y": true, "n": true}
	for _, key := range fields {
		if len(key) == 1 && key[0] >= '0' && key[0] <= '9' {
			continue
		}
		if !allowed[key] {
			return nil, fmt.Errorf("unsafe screen answer %q: use deliberate UI keys such as `1 enter`, `down enter`, or `esc`", text)
		}
	}
	return fields, nil
}

// BlockedQuestion projects only non-Pi screen-detected blocks. Pi question
// semantics remain exclusively owned by its structured agents_block channel.
func (b *Backend) BlockedQuestion(ctx context.Context, target string, harness protocol.HarnessKind) (*protocol.BlockedQuestion, error) {
	if harness == protocol.HarnessPi {
		return nil, nil
	}
	var read ReadResult
	err := b.call(ctx, func(c *Conn) error {
		var e error
		read, e = c.AgentRead(ctx, target, "detection", 80)
		return e
	})
	if err != nil {
		return nil, err
	}
	text, clipped := boundedRunes(strings.TrimSpace(read.Text), 8192)
	if text == "" {
		text = "Herdr detected a blocking approval or question UI, but its bounded detection snapshot was empty."
	}
	detail, _ := json.Marshal(map[string]any{
		"source": "screen", "structured": false, "detector": "herdr",
		"snapshot_source": "detection", "revision": read.Revision,
		"truncated": read.Truncated || clipped,
	})
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", target, read.Revision, text)))
	return &protocol.BlockedQuestion{
		ID: "screen-" + hex.EncodeToString(digest[:8]), Prompt: text,
		At: time.Now().UTC(), Detail: detail,
	}, nil
}

func boundedRunes(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	return string(runes[:limit]), true
}

// Cancel is the verified cancellation path from §3.4: esc, ctrl+c, settle
// briefly, close the workspace, then prove absence. A close response is not
// itself evidence that descendants died, so an unverified cancel is reported
// as an error rather than quietly claimed as success.
func (b *Backend) Cancel(ctx context.Context, session, target string) error {
	err := b.call(ctx, func(c *Conn) error {
		if e := c.AgentSendKeys(ctx, session, "esc"); e != nil && !IsCode(e, "agent_not_found") {
			return e
		}
		return nil
	})
	if err != nil {
		b.log().Warn("herdr cancel esc failed", "agent", session, "error", err)
	}
	err = b.call(ctx, func(c *Conn) error {
		if e := c.AgentSendKeys(ctx, session, "ctrl+c"); e != nil && !IsCode(e, "agent_not_found") {
			return e
		}
		return nil
	})
	if err != nil {
		b.log().Warn("herdr cancel interrupt failed", "agent", session, "error", err)
	}
	// Give the harness a moment to leave working state before the workspace
	// goes away, so it can flush its own side channel.
	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
	}
	return b.Teardown(ctx, session, target)
}

// Teardown closes the job's workspace and verifies that both the agent and the
// workspace are afterwards absent.
func (b *Backend) Teardown(ctx context.Context, session, target string) error {
	workspace := workspaceOf(target)
	if workspace == "" {
		return fmt.Errorf("herdr kill %s: no workspace in target %q", session, target)
	}
	b.unwatch(target)
	if err := b.call(ctx, func(c *Conn) error { return c.WorkspaceClose(ctx, workspace) }); err != nil {
		return fmt.Errorf("herdr workspace.close %s: %w", workspace, err)
	}
	var agentGone, workspaceGone bool
	err := b.call(ctx, func(c *Conn) error {
		_, found, e := c.AgentGet(ctx, session)
		agentGone = !found
		return e
	})
	if err == nil {
		err = b.call(ctx, func(c *Conn) error {
			_, found, e := c.WorkspaceGet(ctx, workspace)
			workspaceGone = !found
			return e
		})
	}
	if err != nil {
		return fmt.Errorf("herdr teardown verification %s: %w", session, err)
	}
	if !agentGone || !workspaceGone {
		return fmt.Errorf("herdr teardown unverified for %s: agent_present=%t workspace_present=%t", session, !agentGone, !workspaceGone)
	}
	b.mu.Lock()
	delete(b.states, target)
	b.mu.Unlock()
	return nil
}

// ServerAlive pings the session socket.
func (b *Backend) ServerAlive(ctx context.Context) bool {
	err := b.call(ctx, func(c *Conn) error {
		_, _, e := c.Ping(ctx)
		return e
	})
	return err == nil
}

// Shutdown stops only the private Herdr child started by this golemd. A
// backend constructed without an owner (unit tests and legacy callers) never
// attempts a socket-level stop, so it cannot target an unrelated server.
func (b *Backend) Shutdown(ctx context.Context) error {
	if b.Owner == nil {
		return nil
	}
	return b.Owner.Shutdown(ctx)
}

// Endpoint publishes no host-local terminal: reaching a job is ordinary SSH
// plus herdr, not a tmux socket Golem hands out.
func (b *Backend) Endpoint(string, string) *protocol.TerminalEndpoint { return nil }

// Policy restricts this substrate to explicitly mapped harnesses and disables
// attach. Steering is supported through agent.prompt for starting/running jobs.
func (b *Backend) Policy() backend.Policy {
	harnesses := map[string]bool{}
	kinds := b.Kinds
	if len(kinds) == 0 {
		kinds = map[string]string{"pi": "pi"}
	}
	for golem := range kinds {
		harnesses[golem] = true
	}
	return backend.Policy{Name: "herdr", Harnesses: harnesses, NoAttach: true}
}

var (
	_ backend.Backend           = (*Backend)(nil)
	_ backend.Observer          = (*Backend)(nil)
	_ backend.Answerer          = (*Backend)(nil)
	_ backend.BlockedQuestioner = (*Backend)(nil)
)
