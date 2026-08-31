// Package claude runs a pinned Claude Code CLI as an interactive worker.
// Claude Code has no stable stdout lifecycle stream, so this adapter installs
// a per-job hook settings file and observes its append-only side channel.
package claude

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
	"github.com/gisikw/golem/protocol"
)

const RouterBaseURL = "https://router.gisi.network"

// Adapter.Binary must point at the deployment's pinned Claude Code binary.
// Keeping it injectable makes the pin explicit in packaging and testable
// without requiring Claude or network credentials in unit tests.
type Adapter struct {
	Binary       string
	ArgvTemplate []string
}

func (a Adapter) generic() harnesses.TemplateAdapter {
	return harnesses.TemplateAdapter{Kind: "claude", ArgvTemplate: a.ArgvTemplate}
}

func (a Adapter) bin() string {
	if a.Binary != "" {
		return a.Binary
	}
	return "claude"
}
func paths(j protocol.Job) (string, string, string) {
	d := j.Artifacts.Directory
	return filepath.Join(d, "claude"), filepath.Join(d, "claude-transcript.log"), filepath.Join(d, "events.jsonl")
}
func withBlock(p string) string {
	return p + "\n\nIf you are genuinely blocked on operator input, use the available permission/confirmation prompt rather than guessing, then wait for the operator."
}

// The command hook is deliberately generated per job: it has no ambient path
// or job-id dependency and can only append to this worker's event file.
func writeHooks(dir, events string) (string, error) {
	p := filepath.Join(dir, "claude-hooks.sh")
	script := `#!/bin/sh
set -eu
event="$1"
input=$(cat 2>/dev/null || true)
ts=$(date +%s%3N 2>/dev/null || date +%s000)
case "$event" in
 SessionStart|UserPromptSubmit) type=running ;;
 PreToolUse|PostToolUse) type=progress ;;
 Stop) type=settled ;;
 Notification)
   case "$input" in *permission_prompt*|*"permission"*) type=blocked ;; *) type=progress ;; esac ;;
 *) type=progress ;;
esac
# Keep hook input opaque in detail; it is useful for diagnosis but never used
# as a command. Stop's summary is intentionally conservative: the transcript
# is authoritative for usage and the daemon owns the final settlement.
printf '{"type":"%s","ts":%s,"message":"claude %s"' "$type" "$ts" "$event" >> ` + shellQuote(events) + `
if [ "$type" = blocked ]; then printf ',"id":"permission-%s","prompt":"Claude is waiting for permission or confirmation"' "$ts" >> ` + shellQuote(events) + `; fi
printf ',"detail":%s}\n' "$(printf %s "$input" | sed 's/[\\\\\"]/[\\\\&]/g' | awk '{printf "\\\"%s\\\"",$0}')" >> ` + shellQuote(events) + `
`
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		return "", err
	}
	return p, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func (a Adapter) Start(ctx context.Context, j protocol.Job) (harnesses.Launch, error) {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Start(ctx, j)
	}
	if j.Artifacts.Directory == "" {
		return harnesses.Launch{}, errors.New("claude requires artifact directory")
	}
	if err := os.MkdirAll(j.Artifacts.Directory, 0700); err != nil {
		return harnesses.Launch{}, err
	}
	configDir, transcript, events := paths(j)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return harnesses.Launch{}, err
	}
	hook, err := writeHooks(configDir, events)
	if err != nil {
		return harnesses.Launch{}, err
	}
	settings := map[string]any{"hooks": map[string]any{
		"SessionStart":     []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " SessionStart"}}}},
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " UserPromptSubmit"}}}},
		"PreToolUse":       []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " PreToolUse"}}}},
		"PostToolUse":      []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " PostToolUse"}}}},
		"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " Stop"}}}},
		"Notification":     []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook + " Notification"}}}},
	}}
	sb, _ := json.Marshal(settings)
	settingsPath := filepath.Join(configDir, "settings.json")
	if err = os.WriteFile(settingsPath, sb, 0600); err != nil {
		return harnesses.Launch{}, err
	}
	// CLAUDE_CONFIG_DIR prevents ~/.claude transcripts/settings from being used.
	env := map[string]string{"ANTHROPIC_BASE_URL": RouterBaseURL, "CLAUDE_CONFIG_DIR": configDir, "GOLEM_EVENTS": events}
	argv := []string{a.bin(), "--settings", settingsPath, "--permission-mode", "default"}
	if j.Model != "" {
		argv = append(argv, "--model", j.Model)
	}
	argv = append(argv, withBlock(j.Prompt))
	return harnesses.Launch{Argv: argv, Dir: j.CWD, Env: env, Transcript: transcript, Events: events, Interactive: true}, nil
}
func (a Adapter) Prompt(ctx context.Context, r *harnesses.Runtime, p string) error {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Prompt(ctx, r, p)
	}
	if r.SendText == nil {
		return harnesses.ErrUnsupported
	}
	return r.SendText(ctx, p)
}
func (a Adapter) Answer(ctx context.Context, r *harnesses.Runtime, x protocol.Answer) error {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Answer(ctx, r, x)
	}
	if r.SendText == nil {
		return harnesses.ErrUnsupported
	}
	return r.SendText(ctx, x.Text)
}
func (a Adapter) Cancel(ctx context.Context, r *harnesses.Runtime) error {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Cancel(ctx, r)
	}
	if r.Cancel == nil {
		return harnesses.ErrUnsupported
	}
	return r.Cancel(ctx)
}
func (a Adapter) Resume(ctx context.Context, j protocol.Job, l harnesses.Launch) (harnesses.Launch, error) {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Resume(ctx, j, l)
	}
	return harnesses.Launch{}, harnesses.ErrUnsupported
}

type event struct {
	Type                                  string `json:"type"`
	Ts                                    int64  `json:"ts"`
	Message, Summary, Verdict, ID, Prompt string
	Options                               []string        `json:"options,omitempty"`
	Detail                                json.RawMessage `json:"detail,omitempty"`
}

func (a Adapter) Observe(ctx context.Context, j protocol.Job, r *harnesses.Runtime) (harnesses.Observation, error) {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().Observe(ctx, j, r)
	}
	if r.Alive == nil {
		return harnesses.Observation{}, harnesses.ErrUnsupported
	}
	alive, code, err := r.Alive(ctx)
	if err != nil {
		return harnesses.Observation{}, err
	}
	o := harnesses.Observation{State: protocol.Running, ExitCode: code, Cursor: r.ObservationCursor}
	f, err := os.Open(r.Launch.Events)
	if os.IsNotExist(err) {
		if !alive {
			o.State = protocol.Failed
		}
		return o, nil
	}
	if err != nil {
		return o, err
	}
	defer f.Close()
	if _, err = f.Seek(o.Cursor, io.SeekStart); err != nil {
		return o, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		start := o.Cursor
		o.Cursor += int64(len(line)) + 1
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		at := time.UnixMilli(e.Ts).UTC()
		if e.Ts == 0 {
			at = time.Now()
		}
		switch e.Type {
		case "settled":
			o.Settled = true
			o.Verdict = mapVerdict(e.Verdict)
			o.Summary = e.Summary
			if o.Summary == "" {
				o.Summary = "claude turn completed"
			}
		case "blocked":
			o.Question = &protocol.BlockedQuestion{ID: e.ID, Prompt: e.Prompt, Options: e.Options, At: at, Detail: e.Detail}
		default:
			h := sha256.Sum256(line)
			o.Progresses = append(o.Progresses, &protocol.Progress{ID: fmt.Sprintf("%s-claude-%d-%s", j.ID, start, hex.EncodeToString(h[:8])), JobID: j.ID, At: at, Message: e.Message, Detail: e.Detail})
		}
	}
	if err = sc.Err(); err != nil {
		return o, err
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
	}
	return protocol.Done
}
func (a Adapter) CollectSettlement(ctx context.Context, j protocol.Job, l harnesses.Launch, o harnesses.Observation) (*protocol.Settlement, error) {
	if len(a.ArgvTemplate) > 0 {
		return a.generic().CollectSettlement(ctx, j, l, o)
	}
	var s *protocol.Settlement
	if o.Settled {
		s = harnesses.SideChannelSettlement(j, l, o)
	} else {
		var err error
		s, err = harnesses.BasicSettlement(j, l, o)
		if err != nil {
			return nil, err
		}
	}
	if err := transcriptUsage(filepath.Join(filepath.Dir(l.Transcript), "claude"), &s.Usage); err != nil {
		return nil, err
	}
	return s, nil
}
func transcriptUsage(dir string, u *protocol.Usage) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) != nil {
				continue
			}
			collectUsage(v, u)
		}
		return sc.Err()
	})
}
func collectUsage(v map[string]any, u *protocol.Usage) {
	role, _ := v["role"].(string)
	if m, ok := v["message"].(map[string]any); ok {
		if role == "" {
			role, _ = m["role"].(string)
		}
		if x, ok := m["usage"].(map[string]any); ok && role == "assistant" {
			addUsage(x, u)
		}
	}
	if x, ok := v["usage"].(map[string]any); ok && role == "assistant" {
		addUsage(x, u)
	}
}
func addUsage(x map[string]any, u *protocol.Usage) {
	if n, ok := x["input_tokens"].(float64); ok {
		u.InputTokens += int64(n)
	}
	if n, ok := x["output_tokens"].(float64); ok {
		u.OutputTokens += int64(n)
	}
	if n, ok := x["cost"].(float64); ok {
		u.CostMicros += int64(math.Round(n * 1_000_000))
	}
}
