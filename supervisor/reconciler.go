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
	"strings"
	"sync"
	"time"

	"github.com/gisikw/golem/artifacts"
	"github.com/gisikw/golem/backend"
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
	Backend          backend.Backend
	OfflineWindow    time.Duration
	ArtifactRoot     string
	AllowedCWDRoots  []string
	MaxStartAttempts int
	StartBackoff     time.Duration
	Linger           time.Duration
	Adapters         map[string]harnesses.Adapter
	AttachHost       string
	AttachPort       int
	Logger           *slog.Logger

	terminalMu         sync.Mutex
	terminalCandidates map[string]terminalCandidate
}

type terminalCandidate struct {
	target              string
	generationStartedAt time.Time
	generationCursor    int64
	observation         harnesses.Observation
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
	ca := claude.Adapter{Binary: "claude"}
	if len(claudeArgv) > 0 {
		ca.ArgvTemplate = claudeArgv
	} // explicit test/deployment override
	return map[string]harnesses.Adapter{"pi": pi, "claude": ca, "codex": codex.Adapter{ArgvTemplate: codexArgv}, "fake": claude.Adapter{ArgvTemplate: []string{"sh", "-c", "printf '%s\\n' fake-worker-complete; printf '%s\\n' fake-artifact >\"$GOLEM_ARTIFACT_DIR/result.txt\"; sleep 1"}}}
}

// Recover adopts surviving sessions. Only pi (currently the only resumable
// adapter) is recreated while disconnected, and never after RestartUntil.
func (s *Supervisor) Recover(ctx context.Context) {
	for _, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() {
			continue
		}
		if s.Backend.Has(ctx, w.Session) {
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
	// ArtifactMetadata.Directory is deliberately excluded from JSON because it
	// is host-local. Consequently it is empty after workers.json is restored;
	// rehydrate it from the configured root and immutable logical ID before the
	// adapter rebuilds task.json and its private profile. Without this step Pi
	// interprets "task.json" relative to golemd's cwd.
	w.Job, err = s.localJob(w.Job)
	if err != nil {
		s.log().Warn("worker cannot resume", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}

	// Read pending side-channel records before creating another target. A
	// successful completion is authoritative even though the old target has
	// disappeared, and publishing it here avoids a duplicate resume. An ordinary
	// failed record is ambiguous: Pi emits the same verdict during orderly Herdr
	// shutdown, so it cannot be accepted at this boundary. Leave it unread; the
	// generation boundary below will identify it without inspecting its summary.
	if settled, inspectErr := s.settlePendingBeforeResume(ctx, a, &w); inspectErr != nil {
		s.log().Warn("worker pre-resume observation failed", "job", w.Job.ID, "mode", mode, "error", inspectErr)
		return
	} else if settled {
		return
	}

	launch, err := a.Resume(ctx, w.Job, w.Launch)
	if err != nil {
		s.log().Warn("worker cannot resume", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}
	generationCursor, err := sideChannelBoundary(launch.Events)
	if err != nil {
		s.log().Warn("worker cannot establish resume generation", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}
	generationStartedAt := time.Now().UTC()
	session, target, err := s.Backend.Start(ctx, w.Job.ID, launch)
	if err != nil {
		s.log().Error("worker resume failed", "job", w.Job.ID, "mode", mode, "error", err)
		return
	}
	w.Launch, w.Session, w.Target = launch, session, target
	w.GenerationStartedAt, w.GenerationCursor = generationStartedAt, generationCursor
	w.StartedAt = time.Now().UTC()
	if err = s.Registry.Put(w); err != nil {
		s.log().Error("worker resume registration failed", "job", w.Job.ID, "mode", mode, "error", err)
		if teardownErr := s.Backend.Teardown(ctx, session, target); teardownErr != nil {
			s.log().Warn("unregistered resumed worker teardown failed", "job", w.Job.ID, "error", teardownErr)
		}
		return
	}
	s.log().Info("worker resumed", "job", w.Job.ID, "mode", mode)
}

func sideChannelBoundary(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// settlePendingBeforeResume preserves a completed old turn rather than
// launching a duplicate. Failed is deliberately excluded: an ordinary failed
// completion and Pi's orderly-shutdown failure are identical at this API
// boundary. Exhaustion remains authoritative because Terminate is explicit
// structured policy evidence, not a summary-string convention.
func (s *Supervisor) settlePendingBeforeResume(ctx context.Context, a harnesses.Adapter, w *Worker) (bool, error) {
	runtime := s.runtime(*w)
	obs, err := a.Observe(ctx, w.Job, &runtime)
	if err != nil {
		return false, err
	}
	if !obs.Settled || obs.Verdict == protocol.Failed && !obs.Terminate {
		return false, nil
	}
	for _, progress := range observationProgresses(obs) {
		event := protocol.ObservedEvent{ID: progress.ID, JobID: w.Job.ID, Progress: progress, ObservedAt: time.Now().UTC()}
		if err = s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
			return false, err
		}
	}
	if w.LastState == protocol.Starting {
		if err = s.publishState(ctx, w, protocol.Running); err != nil {
			return false, err
		}
	}
	settlement, err := a.CollectSettlement(ctx, w.Job, w.Launch, obs)
	if err != nil {
		return false, err
	}
	if len(obs.Detail) > 0 {
		settlement.Detail = obs.Detail
	}
	s.enrichSettlement(w.Job, settlement, obs)
	event := protocol.ObservedEvent{ID: w.Job.ID + "-settlement", JobID: w.Job.ID, Settlement: settlement, ObservedAt: time.Now().UTC()}
	if err = s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		return false, err
	}
	w.ObservationCursor = obs.Cursor
	s.settleWorker(*w, settlement.At, settlement.State)
	s.clearTerminal(w.Job.ID)
	return true, nil
}

func observationProgresses(obs harnesses.Observation) []*protocol.Progress {
	if len(obs.Progresses) != 0 {
		return obs.Progresses
	}
	if obs.Progress != nil {
		return []*protocol.Progress{obs.Progress}
	}
	return nil
}

func (s *Supervisor) Tick(ctx context.Context) error {
	known := map[string]protocol.State{}
	for id, w := range s.Registry.Snapshot() {
		known[id] = w.LastState
	}
	poll, err := s.Client.Poll(ctx, known)
	if err != nil {
		return err
	} // existing workers are untouched
	// Reap before recovery. Recovery can discover and publish a pending genuine
	// completion; reaping that newly settled worker against this poll's stale
	// nonterminal assignment would let Diff launch a duplicate in the same Tick.
	s.reapExpired(ctx, time.Now())
	// A current desired assignment authorizes recovery regardless of the
	// disconnected deadline; the adapter must still provide honest resume.
	local := s.Registry.Snapshot()
	for _, d := range poll.Assignments {
		if w, ok := local[d.Job.ID]; ok && d.Job.ReapRequested && !w.SettledAt.IsZero() {
			_ = s.Backend.Teardown(ctx, w.Session, w.Target)
			_ = s.Registry.Delete(d.Job.ID)
			continue
		}
		if w, ok := local[d.Job.ID]; ok && w.SettledAt.IsZero() && !s.terminalPending(d.Job.ID) && !s.Backend.Has(ctx, w.Session) && d.DesiredState != protocol.Cancelling {
			s.resumeWorker(ctx, w, "confirmed")
		}
	}
	// Reassert the terminal endpoint for any live worker whose service record
	// lacks it. The endpoint (host + private socket + deterministic target) is
	// durable, not a lifecycle-transition side effect: if the Starting/Running
	// event that originally carried it was lost while the service was briefly
	// unavailable, the row would otherwise stay activation-less forever (its
	// tmux session is genuinely live but the viewer, correctly, cannot target
	// it). Redelivery is idempotent (stable event id, deterministic endpoint)
	// and stops once the record matches, so a healthy worker emits nothing.
	for _, d := range poll.Assignments {
		s.reassertTerminal(ctx, d.Job)
	}
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
	// Deliver queued interaction input after assignment reconciliation. Answers
	// retain adapter semantics; steers use the same runtime SendText callback, so
	// both paths converge on one tmux bracketed-paste-plus-Enter implementation.
	for _, d := range poll.Assignments {
		w, ok := s.Registry.Snapshot()[d.Job.ID]
		if !ok {
			continue
		}
		answerPending := d.Job.Question != nil && d.Job.Question.Answer != nil && w.AnsweredKey != d.Job.Question.Answer.IdempotencyKey
		if answerPending {
			var e error
			if screenQuestion(d.Job.Question) {
				if answerer, ok := s.Backend.(backend.Answerer); ok {
					e = answerer.Answer(ctx, w.Target, w.Job.Harness, d.Job.Question.Answer.Text)
				} else {
					e = harnesses.ErrUnsupported
				}
			} else if adapter, adapterErr := s.adapter(w.Job.Harness); adapterErr == nil {
				runtime := s.runtime(w)
				e = adapter.Answer(ctx, &runtime, *d.Job.Question.Answer)
			} else {
				e = adapterErr
			}
			if e == nil {
				w.AnsweredKey = d.Job.Question.Answer.IdempotencyKey
				answerPending = false
				_ = s.Registry.Put(w)
			} else if !errors.Is(e, harnesses.ErrUnsupported) {
				s.log().Warn("answer delivery failed", "job", w.Job.ID, "error", e)
			}
		}
		if answerPending {
			continue
		}
		start := 0
		if w.SteeredKey != "" {
			for i := range d.Job.Steers {
				if d.Job.Steers[i].ID == w.SteeredKey {
					start = i + 1
					break
				}
			}
		}
		for _, steer := range d.Job.Steers[start:] {
			if e := s.sendText(ctx, w, steer.Text); e != nil {
				s.log().Warn("steer delivery failed", "job", w.Job.ID, "steer", steer.ID, "error", e)
				break
			}
			w.SteeredKey = steer.ID
			_ = s.Registry.Put(w)
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
	set := protocol.Settlement{ID: j.ID + "-start-failed", JobID: j.ID, State: protocol.Failed, Verdict: protocol.Failed, Summary: fmt.Sprintf("worker failed to start after %d attempt(s): %s", attempt.Count, attempt.Reason), At: time.Now().UTC()}
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
	j, err = s.localJob(j)
	if err != nil {
		return permanentStart("%v", err)
	}
	if err = os.MkdirAll(j.Artifacts.Directory, 0o700); err != nil {
		return err
	}
	launch, err := a.Start(ctx, j)
	if err != nil {
		return err
	}
	if launch.Env == nil {
		launch.Env = map[string]string{}
	}
	launch.Env["GOLEM_ARTIFACT_DIR"] = j.Artifacts.Directory
	generationCursor, err := sideChannelBoundary(launch.Events)
	if err != nil {
		return err
	}
	generationStartedAt := time.Now().UTC()
	session, target, err := s.Backend.Start(ctx, j.ID, launch)
	if err != nil {
		return err
	}
	w := Worker{Job: j, Launch: launch, Session: session, Target: target, RestartUntil: time.Now().Add(s.OfflineWindow), LastState: protocol.Starting, GenerationStartedAt: generationStartedAt, GenerationCursor: generationCursor, StartedAt: time.Now().UTC()}
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

// localJob restores and validates the host-local paths that are intentionally
// absent from service and registry JSON. It is shared by first launch and
// restart resume so both execute from the same canonical workspace/artifact
// context regardless of golemd's process cwd.
func (s *Supervisor) localJob(j protocol.Job) (protocol.Job, error) {
	cwd, err := filepath.EvalSymlinks(j.CWD)
	if err != nil {
		return j, fmt.Errorf("invalid cwd %q: %v", j.CWD, err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil || !withinAny(cwd, s.AllowedCWDRoots) {
		return j, fmt.Errorf("cwd %q is outside configured allowed roots", j.CWD)
	}
	if j.Artifacts.ID == "" || filepath.Base(j.Artifacts.ID) != j.Artifacts.ID || j.Artifacts.ID == "." || j.Artifacts.ID == ".." {
		return j, fmt.Errorf("invalid logical artifact id %q", j.Artifacts.ID)
	}
	if s.ArtifactRoot == "" {
		return j, errors.New("invalid supervisor artifact root")
	}
	root, err := filepath.Abs(s.ArtifactRoot)
	if err != nil {
		return j, errors.New("invalid supervisor artifact root")
	}
	j.CWD = cwd
	j.Artifacts.Directory = filepath.Join(root, j.Artifacts.ID)
	return j, nil
}

// observedAt is the honest timestamp for a state observation. tmux state is
// read synchronously, so it is simply now. A substrate that watches state
// asynchronously (herdr, whose events carry no timestamp of their own) reports
// when golemd received the observation; never a backdated guess about when the
// transition actually happened.
func (s *Supervisor) observedAt(target string) time.Time {
	now := time.Now().UTC()
	observer, ok := s.Backend.(backend.Observer)
	if !ok || target == "" {
		return now
	}
	status, found := observer.Status(target)
	if !found || status.AsOf.IsZero() || status.AsOf.After(now) {
		return now
	}
	return status.AsOf.UTC()
}

func (s *Supervisor) publishState(ctx context.Context, w *Worker, state protocol.State) error {
	event := protocol.ObservedEvent{ID: w.Job.ID + "-" + string(state), JobID: w.Job.ID, State: state, ObservedAt: s.observedAt(w.Target)}
	if state == protocol.Starting || state == protocol.Running {
		event.Terminal = s.Backend.Endpoint(s.Host, w.Target)
		if s.AttachPort > 0 {
			host := s.AttachHost
			if host == "" {
				host = s.Host
			}
			event.Activation = &protocol.Activation{Type: "ssh", Host: host, Port: s.AttachPort, User: w.Job.ID}
		}
	}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		return err
	}
	w.LastState = state
	return s.Registry.Put(*w)
}

// reassertTerminal idempotently ensures the service record for a live local
// worker carries the exact tmux terminal endpoint. The endpoint is durable and
// deterministic (host + private socket + "worker-<jobid>:0.0" target), so a
// terminal-only event with a stable id is safe to redeliver: the store dedups
// by event id and applies ev.Terminal independent of lifecycle state. This is
// the recovery path for the case where the Starting/Running event that
// originally carried the endpoint was lost (service briefly unavailable) and
// the worker's LastState has since advanced past Starting, so the
// Starting-retry self-heal in observe can no longer fire. Without it a
// genuinely live agent row stays activation-less and the viewer, correctly,
// cannot target it.
//
// It fabricates nothing: it acts only for a worker that is present locally,
// unsettled, and whose tmux session is actually alive. A dead/stale/absent
// session is skipped, so those rows stay nonactionable. It also no-ops once the
// service record already matches, so a healthy worker emits no extra events and
// the normal Starting-transition path stays authoritative.
func (s *Supervisor) reassertTerminal(ctx context.Context, job protocol.Job) {
	w, ok := s.Registry.Snapshot()[job.ID]
	if !ok || !w.SettledAt.IsZero() || w.Target == "" {
		return
	}
	// Still Starting: the observe self-heal owns delivery via the honest
	// Starting->Running transition; do not race it with a bare terminal event.
	if w.LastState == protocol.Starting {
		return
	}
	endpoint := s.Backend.Endpoint(s.Host, w.Target)
	if endpoint == nil {
		return // this substrate publishes no host-local terminal (herdr)
	}
	want := *endpoint
	var activation *protocol.Activation
	if s.AttachPort > 0 {
		host := s.AttachHost
		if host == "" {
			host = s.Host
		}
		activation = &protocol.Activation{Type: "ssh", Host: host, Port: s.AttachPort, User: job.ID}
	}
	if job.Terminal != nil && *job.Terminal == want && (activation == nil || job.Activation != nil && *job.Activation == *activation) {
		return
	}
	if !s.Backend.Has(ctx, w.Session) {
		return // no live terminal exists; never fabricate a target
	}
	eventID := job.ID + "-terminal"
	if activation != nil {
		eventID += fmt.Sprintf("-ssh-%d", activation.Port)
	}
	event := protocol.ObservedEvent{ID: eventID, JobID: job.ID, Terminal: &want, Activation: activation, ObservedAt: time.Now().UTC()}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
		s.log().Warn("terminal endpoint reassertion deferred", "job", job.ID, "error", err)
	}
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
func (s *Supervisor) sendText(ctx context.Context, w Worker, text string) error {
	return s.Backend.Send(ctx, w.Target, text)
}

func screenQuestion(q *protocol.BlockedQuestion) bool {
	if q == nil || len(q.Detail) == 0 {
		return false
	}
	var detail struct {
		Source     string `json:"source"`
		Structured *bool  `json:"structured"`
	}
	return json.Unmarshal(q.Detail, &detail) == nil && detail.Source == "screen" && detail.Structured != nil && !*detail.Structured
}

func (s *Supervisor) runtime(w Worker) harnesses.Runtime {
	answer := func(ctx context.Context, text string) error { return s.sendText(ctx, w, text) }
	if backendAnswer, ok := s.Backend.(backend.Answerer); ok {
		answer = func(ctx context.Context, text string) error {
			return backendAnswer.Answer(ctx, w.Target, w.Job.Harness, text)
		}
	}
	return harnesses.Runtime{Launch: w.Launch, ObservationCursor: w.ObservationCursor, SendText: func(ctx context.Context, text string) error { return s.sendText(ctx, w, text) }, AnswerText: answer, Cancel: func(ctx context.Context) error { return s.Backend.Teardown(ctx, w.Session, w.Target) }, Alive: func(ctx context.Context) (bool, *int, error) { return s.Backend.Pane(ctx, w.Target) }}
}
func (s *Supervisor) reapExpired(ctx context.Context, now time.Time) {
	linger := s.Linger
	if linger < 0 {
		linger = time.Hour
	}
	for id, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() && now.Sub(w.SettledAt) >= linger {
			_ = s.Backend.Teardown(ctx, w.Session, w.Target)
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
	if err := s.Backend.Cancel(ctx, w.Session, w.Target); err != nil {
		s.log().Warn("backend cancel incomplete", "job", id, "error", err)
	}
	set := protocol.Settlement{ID: id + "-cancelled", JobID: id, State: protocol.Cancelled, Verdict: protocol.Cancelled, Summary: "cancelled by requested state", At: time.Now().UTC()}
	if err := s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{{ID: id + "-cancel-settlement", JobID: id, Settlement: &set}}}); err == nil {
		s.settleWorker(w, set.At, set.State)
	}
}
func (s *Supervisor) forget(ctx context.Context, id string) {
	if w, ok := s.Registry.Snapshot()[id]; ok {
		_ = s.Backend.Teardown(ctx, w.Session, w.Target)
		_ = s.Registry.Delete(id)
	}
}

// Herdr's owned server and its agents receive systemd's SIGTERM in the same
// control group as golemd. The child can therefore disappear (and Pi's Herdr
// extension can append its shutdown settlement) before Go has delivered the
// same signal to NotifyContext. A context check alone cannot distinguish that
// ordering from a spontaneous substrate crash.
//
// Hold an ambiguous failure observation in daemon-local memory. If golemd
// remains up, the next reconcile confirms and publishes it, preserving crashes
// with one poll of latency. Structured done/exhaustion can publish immediately,
// but is held until that publication succeeds so cursor advancement cannot lose
// retry content. Confirmation always observes again: side-channel bytes written
// after the candidate supersede it. This matters
// when a replacement daemon resumes a worker before observing a shutdown-only
// record left by the old process; the resumed worker's running or settlement
// record is newer evidence from the current target.
//
// During orderly replacement there is no next reconcile: the durable cursor
// has consumed the shutdown-only side event, while the deliberately unsettled
// registry entry is resumed by the new daemon. This is intentionally not
// persisted; persisting it would turn the old daemon's shutdown evidence into
// a terminal decision after restart.
func (s *Supervisor) holdHerdrTerminal(id string, w Worker, obs harnesses.Observation) bool {
	if s.Backend.Policy().Name != "herdr" {
		return false
	}
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if s.terminalCandidates == nil {
		s.terminalCandidates = make(map[string]terminalCandidate)
	}
	s.terminalCandidates[id] = terminalCandidate{target: w.Target, generationStartedAt: w.GenerationStartedAt, generationCursor: w.GenerationCursor, observation: obs}
	return true
}

func (s *Supervisor) herdrTerminal(id string, w Worker) (harnesses.Observation, bool) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	candidate, ok := s.terminalCandidates[id]
	if !ok {
		return harnesses.Observation{}, false
	}
	if candidate.target != w.Target || !candidate.generationStartedAt.Equal(w.GenerationStartedAt) || candidate.generationCursor != w.GenerationCursor {
		delete(s.terminalCandidates, id)
		return harnesses.Observation{}, false
	}
	return candidate.observation, true
}

func (s *Supervisor) terminalPending(id string) bool {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	_, ok := s.terminalCandidates[id]
	return ok
}

func (s *Supervisor) clearTerminal(id string) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	delete(s.terminalCandidates, id)
}

// terminalPredatesGeneration recognizes side-channel settlement bytes that
// cannot belong to the current target. The cursor handles complete (and even
// timestamp-less) records already present at launch. Event time closes the
// narrow case where an old writer completes a record after the cursor snapshot
// but before Backend.Start establishes the replacement target.
func terminalPredatesGeneration(w Worker, obs harnesses.Observation) bool {
	if !obs.Settled || w.GenerationStartedAt.IsZero() {
		return false
	}
	if obs.TerminalCursor > 0 && obs.TerminalCursor <= w.GenerationCursor {
		return true
	}
	return !obs.TerminalAt.IsZero() && obs.TerminalAt.Before(w.GenerationStartedAt)
}

func discardTerminal(obs *harnesses.Observation) {
	obs.Settled = false
	obs.Terminate = false
	obs.Verdict = ""
	obs.Summary = ""
	obs.Usage = nil
	obs.TerminalAt = time.Time{}
	obs.TerminalCursor = 0
}

func (s *Supervisor) observe(ctx context.Context) error {
	for id, w := range s.Registry.Snapshot() {
		if !w.SettledAt.IsZero() {
			s.clearTerminal(id)
			continue
		}
		a, err := s.adapter(w.Job.Harness)
		if err != nil {
			return err
		}
		candidate, confirming := s.herdrTerminal(id, w)
		runtime := s.runtime(w)
		obs, observeErr := a.Observe(ctx, w.Job, &runtime)
		// Shutdown cancellation is not evidence about the worker. In particular,
		// the owned Herdr child may disappear while Pane is being observed and
		// honestly return absent rather than a context error. Preserve resumable
		// registry state for the next daemon instead of publishing failure.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if observeErr == nil {
			for _, progress := range observationProgresses(obs) {
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
		if observeErr == nil && terminalPredatesGeneration(w, obs) {
			// Keep the cursor/progress from this scan, but terminal content from an
			// older target has no authority over the live replacement generation.
			discardTerminal(&obs)
			s.clearTerminal(id)
			confirming = false
		}
		if confirming {
			if observeErr == nil && obs.Cursor > candidate.Cursor {
				// The append-only side channel orders this observation after the
				// candidate. A live observation cancels the candidate; a newer
				// terminal observation replaces it before publication so a failed
				// publication retries the right settlement on the next Tick.
				if !obs.Settled && obs.State == protocol.Running {
					s.clearTerminal(id)
				} else {
					s.holdHerdrTerminal(id, w, obs)
				}
			} else {
				// No newer durable evidence arrived. Confirm exactly the held
				// observation, including a spontaneous crash whose adapter check
				// continues to fail and therefore has no side-channel cursor.
				obs = candidate
				observeErr = nil
			}
		}
		// A worker keeps running when the adapter reports it alive (State Running)
		// and has not settled over the side channel. Interactive workers settle via
		// obs.Settled while their TUI stays alive; minimal adapters settle when the
		// process exits (State != Running). A dead pane is the supervisor's own
		// crash boundary handled below.
		if observeErr == nil && !obs.Settled && obs.State == protocol.Running {
			s.clearTerminal(id)
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
			// Pi questions remain structured side-channel actions. For other
			// Herdr agents, a screen-detected blocked status is projected from a
			// bounded passive snapshot and explicitly marked unstructured.
			question := obs.Question
			substrateBlocked := false
			if observer, ok := s.Backend.(backend.Observer); ok {
				if status, found := observer.Status(w.Target); found && !status.Stale && status.State == protocol.Blocked {
					substrateBlocked = true
					if w.Job.Harness != protocol.HarnessPi {
						// A screen-authority hook notification is not a parsed
						// question. Use only the honest screen projection here.
						question = nil
					}
					if w.Job.Harness != protocol.HarnessPi && w.LastState != protocol.Blocked {
						if projector, supported := s.Backend.(backend.BlockedQuestioner); supported {
							projected, projectErr := projector.BlockedQuestion(ctx, w.Target, w.Job.Harness)
							if projectErr != nil {
								s.log().Warn("blocked screen projection failed", "job", w.Job.ID, "error", projectErr)
							} else if projected != nil {
								question = projected
							}
						}
					}
				}
			}
			if question != nil && w.LastState != protocol.Blocked {
				if err = s.publishBlocked(ctx, &w, question); err != nil {
					return err
				}
			} else if question == nil && w.LastState == protocol.Blocked && !substrateBlocked {
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
			name := s.Backend.Policy().Name
			boundary := "worker tmux target unavailable"
			serverBoundary := "private tmux server unavailable"
			if name != "" && name != "tmux" {
				boundary, serverBoundary = "worker "+name+" target unavailable", name+" substrate unavailable"
			}
			if !s.Backend.ServerAlive(ctx) {
				boundary = serverBoundary
			}
			obs = harnesses.Observation{State: protocol.Failed, ExitCode: &one}
			detail, _ := json.Marshal(map[string]string{"failure_boundary": boundary})
			obs.Detail = detail
		}
		trustedSideSettlement := observeErr == nil && obs.Settled && (obs.Verdict != protocol.Failed || obs.Terminate)
		if !confirming && trustedSideSettlement {
			// Publish authoritative done/exhaustion immediately, but retain an
			// in-memory copy until publication succeeds because the durable cursor
			// was advanced above. The next Tick can therefore retry exact content.
			s.holdHerdrTerminal(id, w, obs)
		} else if !confirming && s.holdHerdrTerminal(id, w, obs) {
			s.log().Info("herdr terminal observation awaiting confirmation", "job", id)
			continue
		}
		settlement, err := a.CollectSettlement(ctx, w.Job, w.Launch, obs)
		if err != nil {
			return err
		}
		if len(obs.Detail) > 0 {
			settlement.Detail = obs.Detail
		}
		s.enrichSettlement(w.Job, settlement, obs)
		event := protocol.ObservedEvent{ID: id + "-settlement", JobID: id, Settlement: settlement, ObservedAt: time.Now().UTC()}
		if err = s.Client.Events(ctx, protocol.EventBatch{Host: s.Host, Events: []protocol.ObservedEvent{event}}); err != nil {
			return err
		}
		s.settleWorker(w, settlement.At, settlement.State)
		s.clearTerminal(id)
		if obs.Terminate {
			// Policy exhaustion is an immediate process boundary, not ordinary
			// settlement linger. The artifacts/session remain retained by Golem.
			if killErr := s.Backend.Teardown(ctx, w.Session, w.Target); killErr != nil {
				s.log().Warn("exhausted worker kill failed", "job", id, "error", killErr)
			}
		}
	}
	return nil
}

// enrichSettlement adds daemon-observable facts without asking an adapter to
// claim semantics it does not have. Artifact paths are relative, bounded, and
// deterministic; worktree state is sampled at settlement time.
func (s *Supervisor) enrichSettlement(job protocol.Job, set *protocol.Settlement, obs harnesses.Observation) {
	if set.State == "" {
		set.State = set.Verdict
	}
	if verdict := []rune(set.Summary); len(verdict) > 4096 {
		set.Summary = string(verdict[:4096])
	}
	if set.Verdict == "" {
		set.Verdict = set.State
	}
	if set.ExitStatus == nil {
		set.ExitStatus = obs.ExitCode
	}
	listing := artifacts.List(job.Artifacts.Directory)
	set.Artifacts = listing.Artifacts
	set.ArtifactsTruncated = listing.ArtifactsTruncated
	if job.Workspace != nil {
		wt := &protocol.WorktreeSettlement{Name: job.Workspace.Worktree}
		if out, err := exec.Command("git", "-C", job.CWD, "rev-parse", "--short", "HEAD").Output(); err == nil {
			wt.Head = strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("git", "-C", job.CWD, "status", "--porcelain").Output(); err == nil {
			wt.Dirty = len(out) > 0
		}
		set.Worktree = wt
	}
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
