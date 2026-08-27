package pi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gisikw/golem/harnesses"
	piintegration "github.com/gisikw/golem/integrations/pi"
	"github.com/gisikw/golem/protocol"
)

// Adapter runs pi as an interactive TUI. Lifecycle is reported out-of-band by
// the agent-hooks pi extension (HookExtension) which appends durable records to
// the job's side-channel events file; Observe advances a cursor over it. This
// is the documented "hook/side-channel" pattern for harnesses without a
// machine-readable stdout lifecycle stream.
//
// WORKER PROFILE ISOLATION IS A SECURITY/CORRECTNESS BOUNDARY. Each worker runs
// under a private, per-job pi coding-agent dir written by the adapter at Start.
// That dir's settings.json enumerates ONLY the worker extension set (agent-hooks
// and, optionally, the self-contained web extension) — never the operator's
// worklist/identity/attention/zip/handoff/agents-dispatch/subscriber/telemetry
// suite. Workers therefore cannot dispatch other workers or receive operator
// inbox/orientation machinery. The adapter sets PI_CODING_AGENT_DIR explicitly
// so the operator's personal profile can never leak in through ambient env.
type Adapter struct {
	Binary string
	// HookExtension optionally overrides the built-in agent-hooks side-channel
	// extension. When empty, the embedded signed-off extension is materialized
	// into the worker's private profile; lifecycle observation is never silently
	// disabled.
	HookExtension string
	// WebExtension is the optional self-contained web (search + fetch) extension.
	// It carries no operator-specific state; its SSRF guard defaults to
	// public-only destinations, so it is safe in an isolated worker.
	WebExtension string
	// SourceProfile is a pi coding-agent dir whose model catalog
	// (models-store.json), theme, and — only when CopyAuth is set — credentials
	// (auth.json) seed each worker's isolated dir. It is NEVER used as the
	// worker's PI_CODING_AGENT_DIR; it is read for model config only, so the
	// worker gets the same model access as the operator without inheriting its
	// extension suite or personal settings.
	SourceProfile string
	// CopyAuth copies SourceProfile/auth.json into each worker dir. Off by
	// default: credentials otherwise flow through ambient provider env vars
	// (e.g. ANTHROPIC_API_KEY), keeping secrets out of per-job artifact dirs.
	CopyAuth bool
	// DefaultProvider/DefaultModel seed the worker settings default. When empty
	// they are read from SourceProfile/settings.json so the worker's default
	// matches the operator's; job.Model (--model) still overrides at launch.
	DefaultProvider string
	DefaultModel    string
	Providers       map[string]Provider
	Env             map[string]string
}

// Provider is operator-owned connection configuration. APIKeyEnv names a
// variable in golemd's environment; its value is read only while provisioning.
type Provider struct {
	BaseURL   string
	APIKeyEnv string
}

// EventsEnv names the side-channel path the hook extension writes to.
const EventsEnv = "GOLEM_EVENTS"

// TaskContextEnv names the immutable per-job context file used to re-steer a
// worker after compaction without trusting its lossy conversational memory.
const TaskContextEnv = "GOLEM_TASK_CONTEXT"

// CodingDirEnv is the pi coding-agent dir. The adapter sets it explicitly per
// worker so the ambient (operator) value can never take effect.
const CodingDirEnv = "PI_CODING_AGENT_DIR"

// blockSuffix is appended to the worker's initial prompt. Blocking is an
// explicit agent action (pi has no native ask mechanism): the agent-hooks
// extension registers agents_block, and this sentence tells the worker when and
// how to use it. Kept short by design.
const blockSuffix = "\n\nIf you are genuinely blocked on operator input (missing credentials, ambiguous requirements, or confirmation of a destructive action), call the agents_block tool with your question, then end your turn; the operator's answer will arrive as your next message."

// withBlockSuffix appends the blocked-question instruction to a worker prompt.
func withBlockSuffix(prompt string) string { return prompt + blockSuffix }

func (a Adapter) bin() string {
	if a.Binary != "" {
		return a.Binary
	}
	return "pi"
}

// paths returns the session JSONL, pane transcript, and side-channel events
// file beneath the job's artifact directory.
func paths(j protocol.Job) (string, string, string) {
	return filepath.Join(j.Artifacts.Directory, "pi-session.jsonl"),
		filepath.Join(j.Artifacts.Directory, "pi-transcript.log"),
		filepath.Join(j.Artifacts.Directory, "events.jsonl")
}

func (a Adapter) launchEnv(events, workerDir, taskContext string) map[string]string {
	env := cloneEnv(a.Env)
	if env == nil {
		env = map[string]string{}
	}
	env[EventsEnv] = events
	env[TaskContextEnv] = taskContext
	// Explicit override: this wins over any ambient PI_CODING_AGENT_DIR the
	// supervisor process inherited from the operator, so the worker never loads
	// the operator's personal profile.
	env[CodingDirEnv] = workerDir
	return env
}

// workerDir is the per-job private pi coding-agent dir. It lives beneath the
// job artifact directory so it is isolated per worker (no shared mutable
// settings raced by concurrent Starts) and removed with the job's artifacts.
func workerDir(j protocol.Job) string {
	return filepath.Join(j.Artifacts.Directory, "pi")
}

func taskContextPath(j protocol.Job) string {
	return filepath.Join(j.Artifacts.Directory, "task.json")
}

// writeTaskContext persists the immutable dispatch and resolved workspace for
// compaction re-steering. It is deliberately separate from the pi session: the
// fourth-compaction guard must not depend on the memory it is policing.
func writeTaskContext(j protocol.Job) (string, error) {
	p := taskContextPath(j)
	body := struct {
		ID        string                      `json:"id"`
		Harness   protocol.HarnessKind        `json:"harness"`
		Model     string                      `json:"model,omitempty"`
		CWD       string                      `json:"cwd"`
		Prompt    string                      `json:"prompt"`
		Workspace *protocol.ResolvedWorkspace `json:"workspace,omitempty"`
	}{j.ID, j.Harness, j.Model, j.CWD, j.Prompt, j.Workspace}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(p, b, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// workerExtensions is the exact, auditable worker extension set. The leak-guard
// test pins this list; adding operator extensions here is a security change.
func (a Adapter) workerExtensions() []string {
	v := []string{}
	if a.HookExtension != "" {
		v = append(v, a.HookExtension)
	}
	if a.WebExtension != "" {
		v = append(v, a.WebExtension)
	}
	return v
}

// writeWorkerProfile materializes the isolated per-job pi dir: a fresh
// settings.json enumerating only the worker extension set plus model defaults,
// and (best-effort, non-fatal) the model catalog, theme, and optionally auth
// copied from SourceProfile. Loading agent-hooks via settings.json (not a CLI
// --extension) is deliberate: pi errors if the same tool is registered twice,
// so the extension set must live in exactly one place, and settings.json is the
// single file the leak-guard test can audit.
//
// When a model is dispatched, its operator-configured provider is provisioned
// with exactly that model and nothing else: a single-provider models.json,
// pinned defaults, and one enabledModels entry. Any API key is read from the
// daemon environment at this point and never enters protocol or store data.
func (a Adapter) writeWorkerProfile(dir, dispatchedModel string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	provider, model := a.DefaultProvider, a.DefaultModel
	if provider == "" || model == "" {
		sp, sm := sourceDefaults(a.SourceProfile)
		if provider == "" {
			provider = sp
		}
		if model == "" {
			model = sm
		}
	}
	configured := false
	if dispatchedModel != "" {
		var ok bool
		provider, model, ok = strings.Cut(dispatchedModel, "/")
		if !ok || provider == "" || model == "" {
			return fmt.Errorf("pi model %q must be provider/model", dispatchedModel)
		}
		if _, ok = a.Providers[provider]; !ok {
			return fmt.Errorf("pi provider %q is not configured", provider)
		}
		configured = true
	}
	extensions := a.workerExtensions()
	if a.HookExtension == "" {
		builtIn, err := piintegration.WriteHooks(dir)
		if err != nil {
			return err
		}
		extensions = append([]string{builtIn}, extensions...)
	}
	settings := map[string]any{
		"lastChangelogVersion": "0.84.1",
		// Omit the compaction key entirely: pi's native default (enabled) applies.
		// Workers have no custom handoff machinery; pi compacts natively.
		"extensions": extensions,
	}
	if provider != "" {
		settings["defaultProvider"] = provider
	}
	if model != "" {
		settings["defaultModel"] = model
	}
	if configured {
		p := a.Providers[provider]
		modelEntry := map[string]any{"id": model, "name": model, "reasoning": false, "input": []string{"text"}, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}, "contextWindow": 131072, "maxTokens": 16384}
		entry := map[string]any{"name": provider, "baseUrl": p.BaseURL, "api": "openai-completions", "models": []map[string]any{modelEntry}}
		if p.APIKeyEnv != "" {
			key, ok := os.LookupEnv(p.APIKeyEnv)
			if !ok || key == "" {
				return fmt.Errorf("pi provider %q requires daemon environment %s", provider, p.APIKeyEnv)
			}
			entry["apiKey"] = key
			entry["authHeader"] = true
		} else {
			// pi requires an apiKey before a model appears in /model, even for
			// keyless OpenAI-compatible servers (see pi docs/models.md). Keep the
			// documented dummy-value workaround for auth-free providers.
			entry["apiKey"] = "golem-keyless"
		}
		modelsJSON, err := json.MarshalIndent(map[string]any{"providers": map[string]any{provider: entry}}, "", "  ")
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "models.json"), modelsJSON, 0o600); err != nil {
			return err
		}
		settings["enabledModels"] = []string{provider + "/" + model}
	} else {
		copyProfileFile(a.SourceProfile, dir, "models-store.json")
	}
	// Theme is cosmetic and non-blocking.
	if copyProfileTheme(a.SourceProfile, dir) {
		settings["theme"] = "golem"
		settings["themes"] = []string{filepath.Join(dir, "themes")}
	}
	if a.CopyAuth {
		copyProfileFile(a.SourceProfile, dir, "auth.json")
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600)
}

// sourceDefaults reads only defaultProvider/defaultModel from a source pi
// settings.json. It never reads the source extension list — that is the whole
// point of isolation.
func sourceDefaults(sourceProfile string) (string, string) {
	if sourceProfile == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(sourceProfile, "settings.json"))
	if err != nil {
		return "", ""
	}
	var s struct {
		DefaultProvider string `json:"defaultProvider"`
		DefaultModel    string `json:"defaultModel"`
	}
	if json.Unmarshal(b, &s) != nil {
		return "", ""
	}
	return s.DefaultProvider, s.DefaultModel
}

// copyProfileFile copies a single non-symlink regular file between pi dirs,
// best-effort. A missing source is not an error: model/auth config is optional.
func copyProfileFile(sourceProfile, dir, name string) {
	if sourceProfile == "" {
		return
	}
	src := filepath.Join(sourceProfile, name)
	fi, err := os.Lstat(src)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, name), b, 0o600)
}

// copyProfileTheme copies SourceProfile/themes/golem.json into the worker
// dir. Returns true when the theme is available so settings can select it.
func copyProfileTheme(sourceProfile, dir string) bool {
	if sourceProfile == "" {
		return false
	}
	src := filepath.Join(sourceProfile, "themes", "golem.json")
	fi, err := os.Lstat(src)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if os.MkdirAll(filepath.Join(dir, "themes"), 0o700) != nil {
		return false
	}
	return os.WriteFile(filepath.Join(dir, "themes", "golem.json"), b, 0o600) == nil
}

func (a Adapter) Start(_ context.Context, j protocol.Job) (harnesses.Launch, error) {
	if j.Artifacts.Directory == "" {
		return harnesses.Launch{}, errors.New("pi requires artifact directory")
	}
	if e := os.MkdirAll(j.Artifacts.Directory, 0700); e != nil {
		return harnesses.Launch{}, e
	}
	s, t, ev := paths(j)
	wd := workerDir(j)
	taskContext, err := writeTaskContext(j)
	if err != nil {
		return harnesses.Launch{}, err
	}
	if err := a.writeWorkerProfile(wd, j.Model); err != nil {
		return harnesses.Launch{}, err
	}
	// Interactive TUI: no --mode json --print. The positional prompt is
	// delivered as pi's initial message so the agent starts immediately — this
	// is verified to begin work under an isolated profile (see pi/README notes).
	// The worker extension set lives in the per-job settings.json, NOT on the
	// command line, because pi rejects a tool registered twice.
	v := []string{a.bin(), "--session", s, "--no-context-files", "--no-skills"}
	if m := j.Model; m != "" {
		v = append(v, "--model", m)
	}
	v = append(v, withBlockSuffix(j.Prompt))
	return harnesses.Launch{Argv: v, Dir: j.CWD, Env: a.launchEnv(ev, wd, taskContext), Transcript: t, Session: s, Events: ev, Interactive: true}, nil
}

func (a Adapter) Resume(_ context.Context, j protocol.Job, l harnesses.Launch) (harnesses.Launch, error) {
	if l.Session == "" {
		return harnesses.Launch{}, errors.New("pi resume requires session")
	}
	if l.Events == "" {
		_, _, l.Events = paths(j)
	}
	wd := workerDir(j)
	taskContext, err := writeTaskContext(j)
	if err != nil {
		return harnesses.Launch{}, err
	}
	if err := a.writeWorkerProfile(wd, j.Model); err != nil {
		return harnesses.Launch{}, err
	}
	l.Argv = []string{a.bin(), "--session", l.Session, "--no-context-files", "--no-skills"}
	if m := j.Model; m != "" {
		l.Argv = append(l.Argv, "--model", m)
	}
	l.Argv = append(l.Argv, "Continue the interrupted delegated task from the existing session.")
	l.Env = a.launchEnv(l.Events, wd, taskContext)
	l.Interactive = true
	return l, nil
}

func cloneEnv(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (Adapter) Prompt(ctx context.Context, r *harnesses.Runtime, p string) error {
	if r.SendText == nil {
		return harnesses.ErrUnsupported
	}
	return r.SendText(ctx, p)
}
func (Adapter) Answer(ctx context.Context, r *harnesses.Runtime, a protocol.Answer) error {
	if r.SendText == nil {
		return harnesses.ErrUnsupported
	}
	return r.SendText(ctx, a.Text)
}
func (Adapter) Cancel(ctx context.Context, r *harnesses.Runtime) error {
	if r.Cancel == nil {
		return harnesses.ErrUnsupported
	}
	return r.Cancel(ctx)
}

// sideEvent is one line of the append-only events.jsonl side channel written by
// the agent-hooks extension.
type sideEvent struct {
	Type    string     `json:"type"`
	Ts      int64      `json:"ts"` // epoch millis
	Turn    *int       `json:"turn,omitempty"`
	Message string     `json:"message,omitempty"`
	Summary string     `json:"summary,omitempty"`
	Verdict string     `json:"verdict,omitempty"`
	ID      string     `json:"id,omitempty"`
	Prompt  string     `json:"prompt,omitempty"`
	Options []string   `json:"options,omitempty"`
	Count   int        `json:"count,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	Usage   *sideUsage `json:"usage,omitempty"`
}
type sideUsage struct {
	Input  int64   `json:"input"`
	Output int64   `json:"output"`
	Cost   float64 `json:"cost"`
}

// Observe advances a durable byte cursor over the side-channel events file. It
// never parses pi TUI output. Process death is the supervisor's concern.
func (Adapter) Observe(ctx context.Context, j protocol.Job, r *harnesses.Runtime) (harnesses.Observation, error) {
	if r.Alive == nil {
		return harnesses.Observation{}, harnesses.ErrUnsupported
	}
	alive, code, err := r.Alive(ctx)
	if err != nil {
		return harnesses.Observation{}, err
	}
	o := harnesses.Observation{State: protocol.Running, ExitCode: code, Cursor: r.ObservationCursor}
	f, e := os.Open(r.Launch.Events)
	if e == nil {
		defer f.Close()
		if _, err = f.Seek(r.ObservationCursor, io.SeekStart); err != nil {
			return o, err
		}
		reader := bufio.NewReaderSize(f, 64<<10)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return o, readErr
			}
			// A writer may be mid-record; do not advance the durable cursor until
			// its newline makes the JSON object complete.
			if !strings.HasSuffix(line, "\n") {
				break
			}
			lineOffset := o.Cursor
			o.Cursor += int64(len(line))
			b := []byte(strings.TrimSuffix(line, "\n"))
			var ev sideEvent
			if json.Unmarshal(b, &ev) != nil || ev.Type == "" {
				if errors.Is(readErr, io.EOF) {
					break
				}
				continue
			}
			at := time.UnixMilli(ev.Ts).UTC()
			if ev.Ts == 0 {
				at = time.Now().UTC()
			}
			switch ev.Type {
			case "settled":
				// Exhaustion is terminal policy state. A graceful pi shutdown may
				// append an ordinary settled record afterward; it must not overwrite
				// the failed verdict or immediate-kill request.
				if !o.Terminate {
					o.Settled = true
					o.Verdict = mapVerdict(ev.Verdict)
					o.Summary = ev.Summary
					if ev.Usage != nil {
						o.Usage = &protocol.Usage{InputTokens: ev.Usage.Input, OutputTokens: ev.Usage.Output, CostMicros: int64(math.Round(ev.Usage.Cost * 1_000_000))}
					}
				}
			case "blocked":
				o.Question = &protocol.BlockedQuestion{ID: ev.ID, Prompt: ev.Prompt, Options: ev.Options, At: at, Detail: json.RawMessage(append([]byte(nil), b...))}
			case "exhausted":
				o.Settled = true
				o.Terminate = true
				o.Verdict = protocol.Failed
				o.Summary = fmt.Sprintf("worker exhausted compaction budget after %d attempts", ev.Count)
			default:
				h := sha256.Sum256(b)
				msg := ev.Message
				if msg == "" {
					msg = "pi " + ev.Type
				}
				o.Progresses = append(o.Progresses, &protocol.Progress{ID: fmt.Sprintf("%s-pi-%d-%s", j.ID, lineOffset, hex.EncodeToString(h[:8])), JobID: j.ID, At: at, Message: msg, Detail: json.RawMessage(append([]byte(nil), b...))})
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
	} else if !os.IsNotExist(e) {
		return o, e
	}
	if len(o.Progresses) > 0 {
		o.Progress = o.Progresses[len(o.Progresses)-1]
	}
	if !alive {
		o.State = protocol.Failed
	}
	return o, nil
}

func mapVerdict(v string) protocol.State {
	switch protocol.State(v) {
	case protocol.Done, protocol.Failed, protocol.Cancelled, protocol.Timeout:
		return protocol.State(v)
	default:
		return protocol.Done
	}
}

// CollectSettlement builds the settlement. A side-channel "settled" event
// carries the verdict, final assistant message, and usage directly; otherwise
// (e.g. a crash boundary) BasicSettlement infers the verdict from process exit.
// In both cases usage is reconciled against the pi session JSONL, whose
// per-operation records are the authoritative cumulative total.
func (Adapter) CollectSettlement(_ context.Context, j protocol.Job, l harnesses.Launch, o harnesses.Observation) (*protocol.Settlement, error) {
	var s *protocol.Settlement
	if o.Settled {
		s = harnesses.SideChannelSettlement(j, l, o)
	} else {
		var err error
		if s, err = harnesses.BasicSettlement(j, l, o); err != nil {
			return nil, err
		}
	}
	// Reconcile usage from the session JSONL when the side channel did not
	// report it (the cumulative-usage logic is the authoritative source).
	if o.Usage == nil {
		if err := sessionUsage(l.Session, &s.Usage); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func sessionUsage(session string, u *protocol.Usage) error {
	f, err := os.Open(session)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	for scan.Scan() {
		var v any
		if json.Unmarshal(scan.Bytes(), &v) == nil {
			collectUsage(v, u)
		}
	}
	return scan.Err()
}

type piUsage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cost   struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}
type sessionRecord struct {
	Type    string   `json:"type"`
	Usage   *piUsage `json:"usage,omitempty"`
	Message *struct {
		Role  string   `json:"role"`
		Usage *piUsage `json:"usage,omitempty"`
	} `json:"message,omitempty"`
}

// Session usage records are per-operation deltas, not cumulative snapshots.
// We add only usage-bearing fields defined by pi's session schema and return
// the final cumulative total. Nested details/retainedTail copies are ignored.
func collectUsage(v any, u *protocol.Usage) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	var record sessionRecord
	if json.Unmarshal(b, &record) != nil {
		return
	}
	var usage *piUsage
	switch record.Type {
	case "message":
		if record.Message != nil && (record.Message.Role == "assistant" || record.Message.Role == "toolResult") {
			usage = record.Message.Usage
		}
	case "compaction", "branch_summary":
		usage = record.Usage
	}
	if usage != nil {
		u.InputTokens += usage.Input
		u.OutputTokens += usage.Output
		u.CostMicros += int64(math.Round(usage.Cost.Total * 1_000_000))
	}
}
