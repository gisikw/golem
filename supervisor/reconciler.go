package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gisikw/golem/client"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/harnesses/claude"
	"github.com/gisikw/golem/harnesses/codex"
	piadapter "github.com/gisikw/golem/harnesses/pi"
	"github.com/gisikw/golem/protocol"
)

type ActionKind string

const (
	Start  ActionKind = "start"
	Cancel ActionKind = "cancel"
	Forget ActionKind = "forget"
)

type Action struct {
	Kind       ActionKind
	Assignment protocol.Assignment
	JobID      string
}

// Diff is pure so reconciliation behavior can be tested without processes.
func Diff(desired []protocol.Assignment, local map[string]Worker) []Action {
	out := []Action{}
	seen := map[string]bool{}
	for _, d := range desired {
		seen[d.Job.ID] = true
		_, ok := local[d.Job.ID]
		if !ok && !d.Job.State.Terminal() && !d.Job.CancelRequested && d.DesiredState != protocol.Cancelling {
			out = append(out, Action{Kind: Start, Assignment: d, JobID: d.Job.ID})
		} else if ok && (d.Job.CancelRequested || d.DesiredState == protocol.Cancelling) {
			out = append(out, Action{Kind: Cancel, Assignment: d, JobID: d.Job.ID})
		}
	}
	for id, worker := range local {
		if !seen[id] && worker.SettledAt.IsZero() {
			out = append(out, Action{Kind: Forget, JobID: id})
		}
	}
	return out
}

type Supervisor struct {
	Host             string
	Client           *client.Client
	Registry         *Registry
	Tmux             Tmux
	OfflineWindow    time.Duration
	ArtifactRoot     string
	AllowedCWDRoots  []string
	MaxStartAttempts int
	StartBackoff     time.Duration
	Linger           time.Duration
	Adapters         map[string]harnesses.Adapter
	// Notify, when non-nil, is invoked after a durable settlement to promptly
	// wake the Golem operator. It must never block or fail the settlement.
	Notify Notifier
	Logger *slog.Logger
}

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
func (s *Supervisor) adapter(kind protocol.HarnessKind) (harnesses.Adapter, error) {
	a, ok := s.Adapters[string(kind)]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", kind)
	}
	return a, nil
}
func DefaultAdapters(piBinary string, claudeArgv, codexArgv []string) map[string]harnesses.Adapter {
	return ConfiguredAdapters(piadapter.Adapter{Binary: piBinary}, claudeArgv, codexArgv)
}

func ConfiguredAdapters(pi piadapter.Adapter, claudeArgv, codexArgv []string) map[string]harnesses.Adapter {
	return map[string]harnesses.Adapter{"pi": pi, "claude": claude.Adapter{ArgvTemplate: claudeArgv}, "codex": codex.Adapter{ArgvTemplate: codexArgv}, "fake": claude.Adapter{ArgvTemplate: []string{"sh", "-c", "printf '%s\\n' fake-worker-complete; sleep 1"}}}
}

// Recover adopts surviving sessions. Only pi (currently the only resumable
// adapter) is recreated while disconnected, and never after RestartUntil.
func (s *Supervisor) Recover(ctx context.Context) {
	for _, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() {
			continue
		}
		if s.Tmux.Has(ctx, w.Session) {
			continue
		}
		if time.Now().After(w.RestartUntil) {
			s.log().Warn("offline restart window expired", "job", w.Job.ID)
			continue
		}
		s.resumeWorker(ctx, w, "offline")
	}
}
func (s *Supervisor) resumeWorker(ctx context.Context, w Worker, mode string) {
	a, err := s.adapter(w.Job.Harness)
	if err != nil {
		return
	}
	launch, err := a.Resume(ctx, w.Job, w.Launch)
	if err != nil {
		s.log().Warn("worker cannot resume", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}
	session, target, err := s.Tmux.Start(ctx, w.Job.ID, launch)
	if err != nil {
		s.log().Error("worker resume failed", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}
	w.Launch, w.Session, w.Target, w.StartedAt = launch, session, target, time.Now().UTC()
	_ = s.Registry.Put(w)
	s.log().Info("worker resumed", "job", w.Job.ID, "mode", mode)
}

func (s *Supervisor) Tick(ctx context.Context) error {
	known := map[string]protocol.State{}
	for id, w := range s.Registry.Snapshot() {
		known[id] = w.LastState
	}
	poll, err := s.Client.Poll(ctx, s.Host, known)
	if err != nil {
		return err
	} // existing workers are untouched
	// A current desired assignment authorizes recovery regardless of the
	// disconnected deadline; the adapter must still provide honest resume.
	local := s.Registry.Snapshot()
	for _, d := range poll.Assignments {
		if w, ok := local[d.Job.ID]; ok && d.Job.ReapRequested && !w.SettledAt.IsZero() {
			_ = s.Tmux.Kill(ctx, w.Session)
			_ = s.Registry.Delete(d.Job.ID)
			continue
		}
		if w, ok := local[d.Job.ID]; ok && w.SettledAt.IsZero() && !s.Tmux.Has(ctx, w.Session) && d.DesiredState != protocol.Cancelling {
			s.resumeWorker(ctx, w, "confirmed")
		}
	}
	s.reapExpired(ctx, time.Now())
	for _, a := range Diff(poll.Assignments, s.Registry.Snapshot()) {
		switch a.Kind {
		case Start:
			if err = s.reconcileStart(ctx, a.Assignment.Job); err != nil {
				s.log().Error("start failed", "job", a.JobID, "error", err)
			}
		case Cancel:
			s.cancel(ctx, a.JobID)
		case Forget:
			s.forget(ctx, a.JobID)
		}
	}
	// Deliver answered blocked questions after assignment reconciliation.
	for _, d := range poll.Assignments {
		w, ok := s.Registry.Snapshot()[d.Job.ID]
		if !ok || d.Job.Question == nil || d.Job.Question.Answer == nil || w.AnsweredKey == d.Job.Question.Answer.IdempotencyKey {
			continue
		}
		adapter, e := s.adapter(w.Job.Harness)
		if e != nil {
			continue
		}
		runtime := s.runtime(w)
		if e = adapter.Answer(ctx, &runtime, *d.Job.Question.Answer); e == nil {
			w.AnsweredKey = d.Job.Question.Answer.IdempotencyKey
			_ = s.Registry.Put(w)
		} else if !errors.Is(e, harnesses.ErrUnsupported) {
			s.log().Warn("answer delivery failed", "job", w.Job.ID, "error", e)
		}
	}
	return s.observe(ctx)
}

type startError struct {
	err       error
	permanent bool
}

func (e *startError) Error() string { return e.err.Error() }
func permanentStart(format string, args ...any) error {
	return &startError{err: fmt.Errorf(format, args...), permanent: true}
}

func (s *Supervisor) reconcileStart(ctx context.Context, j protocol.Job) error {
	now := time.Now().UTC()
	attempt, exists := s.Registry.Attempt(j.ID)
	if exists && attempt.SettlementPending {
		return s.publishStartFailure(ctx, j, attempt)
	}
	if exists && now.Before(attempt.NextAttempt) {
		return nil
	}
	err := s.start(ctx, j)
	if err == nil {
		return s.Registry.ClearAttempt(j.ID)
	}
	attempt.Count++
	attempt.Reason = err.Error()
	max := s.MaxStartAttempts
	if max <= 0 {
		max = 3
	}
	var classified *startError
	permanent := errors.As(err, &classified) && classified.permanent
	if permanent || attempt.Count >= max {
		attempt.SettlementPending = true
	} else {
		backoff := s.StartBackoff
		if backoff <= 0 {
			backoff = 5 * time.Second
		}
		attempt.NextAttempt = now.Add(backoff * time.Duration(1<<(attempt.Count-1)))
	}
	if saveErr := s.Registry.PutAttempt(j.ID, attempt); saveErr != nil {
		return saveErr
	}
	if attempt.SettlementPending {
		return s.publishStartFailure(ctx, j, attempt)
	}
	return err
}

func (s *Supervisor) publishStartFailure(ctx context.Context, j protocol.Job, attempt StartAttempt) error {
	set := protocol.Settlement{ID: j.ID + "-start-failed", JobID: j.ID, Verdict: protocol.Failed, Summary: fmt.Sprintf("worker failed to start after %d attempt(s): %s", attempt.Count, attempt.Reason), At: time.Now().UTC()}
	detail, _ := json.Marshal(map[string]any{"failure_boundary": "worker_start", "attempts": attempt.Count, "reason": attempt.Reason})
	set.Detail = detail
	event := protocol.ObservedEvent{ID: j.ID + "-start-failed-event", JobID: j.ID, Settlement: &set, ObservedAt: time.Now().UTC()}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		return err // pending settlement remains durable locally for redelivery
	}
	return s.Registry.ClearAttempt(j.ID)
}

func (s *Supervisor) start(ctx context.Context, j protocol.Job) error {
	a, err := s.adapter(j.Harness)
	if err != nil {
		return permanentStart("%v", err)
	}
	cwd, err := filepath.EvalSymlinks(j.CWD)
	if err != nil {
		return permanentStart("invalid cwd %q: %v", j.CWD, err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil || !withinAny(cwd, s.AllowedCWDRoots) {
		return permanentStart("cwd %q is outside configured allowed roots", j.CWD)
	}
	j.CWD = cwd
	if j.Artifacts.ID == "" || filepath.Base(j.Artifacts.ID) != j.Artifacts.ID || j.Artifacts.ID == "." || j.Artifacts.ID == ".." {
		return permanentStart("invalid logical artifact id %q", j.Artifacts.ID)
	}
	root, err := filepath.Abs(s.ArtifactRoot)
	if err != nil || s.ArtifactRoot == "" {
		return permanentStart("invalid supervisor artifact root")
	}
	j.Artifacts.Directory = filepath.Join(root, j.Artifacts.ID)
	if err = os.MkdirAll(j.Artifacts.Directory, 0o700); err != nil {
		return err
	}
	worktree := ""
	if j.Isolation == protocol.IsolationWorktree {
		worktree = filepath.Join(j.Artifacts.Directory, "worktree")
		if out, worktreeErr := exec.CommandContext(ctx, "git", "-C", j.CWD, "worktree", "add", "--detach", worktree, "HEAD").CombinedOutput(); worktreeErr != nil {
			return fmt.Errorf("git worktree: %s: %w", out, worktreeErr)
		}
		j.CWD = worktree
	}
	launch, err := a.Start(ctx, j)
	if err != nil {
		return err
	}
	session, target, err := s.Tmux.Start(ctx, j.ID, launch)
	if err != nil {
		return err
	}
	w := Worker{Job: j, Launch: launch, Session: session, Target: target, Worktree: worktree, RestartUntil: time.Now().Add(s.OfflineWindow), LastState: protocol.Starting, StartedAt: time.Now().UTC()}
	if err = s.Registry.Put(w); err != nil {
		return err
	}
	// Registration is the process-reality boundary. A failed service delivery
	// is retried by observation and must not be misclassified as a start failure.
	if err = s.publishState(ctx, &w, protocol.Starting); err != nil {
		s.log().Warn("starting observation deferred", "job", j.ID, "error", err)
	}
	return nil
}
func (s *Supervisor) publishState(ctx context.Context, w *Worker, state protocol.State) error {
	event := protocol.ObservedEvent{ID: w.Job.ID + "-" + string(state), JobID: w.Job.ID, State: state, ObservedAt: time.Now().UTC()}
	if state == protocol.Starting || state == protocol.Running {
		event.Terminal = &protocol.TerminalEndpoint{Host: s.Host, Socket: s.Tmux.Socket, Target: w.Target}
	}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		return err
	}
	w.LastState = state
	return s.Registry.Put(*w)
}

// publishBlocked reports a side-channel blocked question and moves the worker to
// the Blocked state. Delivery of the eventual answer is handled in Tick.
func (s *Supervisor) publishBlocked(ctx context.Context, w *Worker, q *protocol.BlockedQuestion) error {
	event := protocol.ObservedEvent{ID: w.Job.ID + "-blocked-" + q.ID, JobID: w.Job.ID, State: protocol.Blocked, Question: q, ObservedAt: time.Now().UTC()}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		return err
	}
	w.LastState = protocol.Blocked
	return s.Registry.Put(*w)
}
func (s *Supervisor) runtime(w Worker) harnesses.Runtime {
	return harnesses.Runtime{Launch: w.Launch, ObservationCursor: w.ObservationCursor, SendText: func(ctx context.Context, text string) error { return s.Tmux.Send(ctx, w.Target, text) }, Cancel: func(ctx context.Context) error { return s.Tmux.Kill(ctx, w.Session) }, Alive: func(ctx context.Context) (bool, *int, error) { return s.Tmux.Pane(ctx, w.Target) }}
}
func (s *Supervisor) reapExpired(ctx context.Context, now time.Time) {
	linger := s.Linger
	if linger < 0 {
		linger = time.Hour
	}
	for id, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() && now.Sub(w.SettledAt) >= linger {
			_ = s.Tmux.Kill(ctx, w.Session)
			_ = s.Registry.Delete(id)
		}
	}
}

func (s *Supervisor) settleWorker(w Worker, at time.Time, state protocol.State) {
	w.LastState = state
	w.SettledAt = at.UTC()
	_ = s.Registry.Put(w)
}

func (s *Supervisor) cancel(ctx context.Context, id string) {
	w, ok := s.Registry.Snapshot()[id]
	if !ok {
		return
	}
	// Ctrl-C lets the worker pane become a retained dead pane under the owned
	// tmux policy, preserving its output for the linger window.
	_ = s.Tmux.Interrupt(ctx, w.Target)
	set := protocol.Settlement{ID: id + "-cancelled", JobID: id, Verdict: protocol.Cancelled, Summary: "cancelled by requested state", At: time.Now().UTC()}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{{ID: id + "-cancel-settlement", JobID: id, Settlement: &set}}}); err == nil {
		s.settleWorker(w, set.At, set.Verdict)
	}
}
func (s *Supervisor) forget(ctx context.Context, id string) {
	if w, ok := s.Registry.Snapshot()[id]; ok {
		_ = s.Tmux.Kill(ctx, w.Session)
		_ = s.Registry.Delete(id)
	}
}
func (s *Supervisor) observe(ctx context.Context) error {
	for id, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() {
			continue
		}
		a, err := s.adapter(w.Job.Harness)
		if err != nil {
			return err
		}
		runtime := s.runtime(w)
		obs, observeErr := a.Observe(ctx, w.Job, &runtime)
		if observeErr == nil {
			progresses := obs.Progresses
			if len(progresses) == 0 && obs.Progress != nil {
				progresses = []*protocol.Progress{obs.Progress}
			}
			for _, progress := range progresses {
				event := protocol.ObservedEvent{ID: progress.ID, JobID: id, Progress: progress, ObservedAt: time.Now().UTC()}
				if err = s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
					return err
				}
			}
			if obs.Cursor != w.ObservationCursor {
				w.ObservationCursor = obs.Cursor
				if err = s.Registry.Put(w); err != nil {
					return err
				}
			}
		}
		// A worker keeps running when the adapter reports it alive (State Running)
		// and has not settled over the side channel. Interactive workers settle via
		// obs.Settled while their TUI stays alive; minimal adapters settle when the
		// process exits (State != Running). A dead pane is the supervisor's own
		// crash boundary handled below.
		if observeErr == nil && !obs.Settled && obs.State == protocol.Running {
			if w.LastState == protocol.Starting {
				// Retry the idempotent starting event first: its original response may
				// have been lost even though the worker was successfully created.
				if err = s.publishState(ctx, &w, protocol.Starting); err != nil {
					return err
				}
				if err = s.publishState(ctx, &w, protocol.Running); err != nil {
					return err
				}
			}
			// Blocked questions are reported over the side channel; project them so
			// the operator/operator can answer. Answer delivery happens in Tick.
			if obs.Question != nil && w.LastState != protocol.Blocked {
				if err = s.publishBlocked(ctx, &w, obs.Question); err != nil {
					return err
				}
			} else if obs.Question == nil && w.LastState == protocol.Blocked {
				if err = s.publishState(ctx, &w, protocol.Running); err != nil {
					return err
				}
			}
			continue
		}
		// A side-channel settlement is still preceded by Running so the lifecycle
		// is honest (Starting -> Running -> terminal).
		if observeErr == nil && obs.Settled && w.LastState == protocol.Starting {
			if err = s.publishState(ctx, &w, protocol.Running); err != nil {
				return err
			}
		}
		if observeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			one := 1
			boundary := "worker tmux target unavailable"
			if !s.Tmux.ServerAlive(ctx) {
				boundary = "private tmux server unavailable"
			}
			obs = harnesses.Observation{State: protocol.Failed, ExitCode: &one}
			detail, _ := json.Marshal(map[string]string{"failure_boundary": boundary})
			obs.Detail = detail
		}
		settlement, err := a.CollectSettlement(ctx, w.Job, w.Launch, obs)
		if err != nil {
			return err
		}
		if len(obs.Detail) > 0 {
			settlement.Detail = obs.Detail
		}
		event := protocol.ObservedEvent{ID: id + "-settlement", JobID: id, Settlement: settlement, ObservedAt: time.Now().UTC()}
		if err = s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
			return err
		}
		s.settleWorker(w, settlement.At, settlement.Verdict)
		// Settlement is durable; notification is a best-effort courtesy that must
		// never fail or delay the settlement itself.
		s.notifySettlement(ctx, w.Job, settlement)
	}
	return nil
}

func withinAny(path string, roots []string) bool {
	for _, configured := range roots {
		root, err := filepath.EvalSymlinks(configured)
		if err != nil {
			continue
		}
		root, err = filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !filepath.IsAbs(rel) && !(len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// GCSettled removes only service-confirmed terminal job artifacts, honoring a
// per-job retention override. root bounds deletion and must contain each path.
func GCSettled(jobs []protocol.Job, root string, now time.Time, defaultAge time.Duration) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.State.Terminal() || j.Artifacts.ID == "" || filepath.Base(j.Artifacts.ID) != j.Artifacts.ID {
			continue
		}
		path, err := filepath.Abs(filepath.Join(absRoot, j.Artifacts.ID))
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator) {
			continue
		}
		age := defaultAge
		if j.Artifacts.RetentionDays > 0 {
			age = time.Duration(j.Artifacts.RetentionDays) * 24 * time.Hour
		}
		at := j.UpdatedAt
		if j.Settlement != nil && !j.Settlement.At.IsZero() {
			at = j.Settlement.At
		}
		if !at.IsZero() && now.Sub(at) >= age {
			if err = os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// GC is a low-level age-based helper retained for host administration. The CLI
// uses GCSettled so running jobs can never be removed by semantic GC.
func GC(root string, before time.Time) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, e := entry.Info()
		if e == nil && info.ModTime().Before(before) {
			if e = os.RemoveAll(filepath.Join(root, entry.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}
