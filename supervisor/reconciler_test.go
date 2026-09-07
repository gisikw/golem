package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gisikw/golem/backend"
	tmuxbackend "github.com/gisikw/golem/backend/tmux"
	"github.com/gisikw/golem/client"
	"github.com/gisikw/golem/harnesses"
	piadapter "github.com/gisikw/golem/harnesses/pi"
	"github.com/gisikw/golem/protocol"
	"github.com/gisikw/golem/service"
)

func testSupervisor(t *testing.T, cwd, artifactRoot string) (*Supervisor, *service.Store, *client.Client) {
	t.Helper()
	store, err := service.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(service.API{Store: store, Capabilities: protocol.Capabilities{Name: "host", Harnesses: map[string]protocol.HarnessCapability{"fake": {}, "unknown": {}}}}.Handler())
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() { _ = store.Close() })
	registry, err := OpenRegistry(filepath.Join(t.TempDir(), "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(httpServer.URL)
	s := &Supervisor{Host: "host", Client: c, Registry: registry, ArtifactRoot: artifactRoot, AllowedCWDRoots: []string{cwd}, Adapters: DefaultAdapters("", nil, nil), MaxStartAttempts: 2, StartBackoff: time.Nanosecond}
	return s, store, c
}

type recordingBackend struct {
	mu       sync.Mutex
	sent     []string
	status   backend.Status
	hasState bool
	question *protocol.BlockedQuestion
}

func (*recordingBackend) Prepare() error { return nil }
func (*recordingBackend) Start(context.Context, string, harnesses.Launch) (string, string, error) {
	return "agent", "w1:p1", nil
}
func (*recordingBackend) Has(context.Context, string) bool               { return true }
func (*recordingBackend) Teardown(context.Context, string, string) error { return nil }
func (*recordingBackend) Cancel(context.Context, string, string) error   { return nil }
func (b *recordingBackend) Send(_ context.Context, _ string, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, text)
	return nil
}
func (b *recordingBackend) Answer(_ context.Context, _ string, _ protocol.HarnessKind, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, "answer:"+text)
	return nil
}
func (*recordingBackend) Pane(context.Context, string) (bool, *int, error)   { return true, nil, nil }
func (*recordingBackend) ServerAlive(context.Context) bool                   { return true }
func (*recordingBackend) Shutdown(context.Context) error                     { return nil }
func (*recordingBackend) Endpoint(string, string) *protocol.TerminalEndpoint { return nil }
func (*recordingBackend) Policy() backend.Policy                             { return backend.Policy{Name: "herdr"} }
func (b *recordingBackend) Status(string) (backend.Status, bool)             { return b.status, b.hasState }
func (b *recordingBackend) BlockedQuestion(context.Context, string, protocol.HarnessKind) (*protocol.BlockedQuestion, error) {
	return b.question, nil
}

type restartBackend struct {
	launches []harnesses.Launch
}

func (*restartBackend) Prepare() error { return nil }
func (b *restartBackend) Start(_ context.Context, _ string, launch harnesses.Launch) (string, string, error) {
	b.launches = append(b.launches, launch)
	return "job-restored", "w-new:p1", nil
}
func (*restartBackend) Has(context.Context, string) bool                   { return false }
func (*restartBackend) Teardown(context.Context, string, string) error     { return nil }
func (*restartBackend) Cancel(context.Context, string, string) error       { return nil }
func (*restartBackend) Send(context.Context, string, string) error         { return nil }
func (*restartBackend) Pane(context.Context, string) (bool, *int, error)   { return false, nil, nil }
func (*restartBackend) ServerAlive(context.Context) bool                   { return false }
func (*restartBackend) Shutdown(context.Context) error                     { return nil }
func (*restartBackend) Endpoint(string, string) *protocol.TerminalEndpoint { return nil }
func (*restartBackend) Policy() backend.Policy                             { return backend.Policy{Name: "herdr"} }

func TestRestartRestoreRehydratesPiArtifactPathsBeforeHerdrStart(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	jobDir := filepath.Join(root, "artifact-restored")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Resume must not inherit an accidentally permissive historical mode.
	if err := os.WriteFile(filepath.Join(jobDir, "task.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(t.TempDir(), "workers.json")
	registry, err := OpenRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	job := protocol.Job{ID: "restored", Harness: protocol.HarnessPi, CWD: cwd, Prompt: "continue", Artifacts: protocol.ArtifactMetadata{ID: "artifact-restored", Directory: jobDir}}
	oldLaunch := harnesses.Launch{Session: "pi-session.jsonl", Events: "events.jsonl", Transcript: "pi-transcript.log", Dir: "."}
	if err = registry.Put(Worker{Job: job, Launch: oldLaunch, Session: "job-restored", Target: "w-old:p1", RestartUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Snapshot()[job.ID].Job.Artifacts.Directory; got != "" {
		t.Fatalf("host-local directory unexpectedly survived registry JSON: %q", got)
	}

	runtime := &restartBackend{}
	s := &Supervisor{Registry: restored, Backend: runtime, ArtifactRoot: root, AllowedCWDRoots: []string{cwd}, Adapters: map[string]harnesses.Adapter{"pi": piadapter.Adapter{Binary: "pi"}}}
	s.Recover(context.Background())
	if len(runtime.launches) != 1 {
		t.Fatalf("resume starts = %d, want one", len(runtime.launches))
	}
	launch := runtime.launches[0]
	wantSession := filepath.Join(jobDir, "pi-session.jsonl")
	wantEvents := filepath.Join(jobDir, "events.jsonl")
	wantTask := filepath.Join(jobDir, "task.json")
	if launch.Session != wantSession || launch.Events != wantEvents || launch.Transcript != filepath.Join(jobDir, "pi-transcript.log") || launch.Dir != cwd {
		t.Fatalf("resume retained cwd-relative paths: %#v", launch)
	}
	if launch.Env[piadapter.TaskContextEnv] != wantTask || launch.Env[piadapter.CodingDirEnv] != filepath.Join(jobDir, "pi") {
		t.Fatalf("resume environment not rooted in artifact directory: %#v", launch.Env)
	}
	info, err := os.Stat(wantTask)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("task context mode = %o, want 0600", info.Mode().Perm())
	}
	persisted := restored.Snapshot()[job.ID]
	if persisted.Job.Artifacts.Directory != jobDir || persisted.Target != "w-new:p1" {
		t.Fatalf("rehydrated worker was not persisted: %#v", persisted)
	}
}

func TestCancelledObservationDoesNotSettleVanishedResumableWorker(t *testing.T) {
	registry, err := OpenRegistry(filepath.Join(t.TempDir(), "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	job := protocol.Job{ID: "shutdown", Harness: protocol.HarnessPi, Artifacts: protocol.ArtifactMetadata{ID: "artifact-shutdown"}}
	if err = registry.Put(Worker{Job: job, Launch: harnesses.Launch{Events: filepath.Join(t.TempDir(), "events.jsonl")}, Session: "job-shutdown", Target: "w:p"}); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{Registry: registry, Backend: &restartBackend{}, Adapters: map[string]harnesses.Adapter{"pi": piadapter.Adapter{}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // model golemd shutdown racing the owned Herdr child's disappearance
	if err = s.observe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown observation = %v, want context cancellation", err)
	}
	if got := registry.Snapshot()[job.ID]; !got.SettledAt.IsZero() || got.LastState.Terminal() {
		t.Fatalf("shutdown disappearance settled resumable worker: %#v", got)
	}
}

func TestScreenDetectedClaudeBlockBecomesDispatcherQuestion(t *testing.T) {
	ctx := context.Background()
	cwd, artifacts := t.TempDir(), t.TempDir()
	s, store, _ := testSupervisor(t, cwd, artifacts)
	detail := json.RawMessage(`{"source":"screen","structured":false}`)
	recorder := &recordingBackend{hasState: true, status: backend.Status{State: protocol.Blocked, Present: true, AsOf: time.Now()}, question: &protocol.BlockedQuestion{ID: "screen-1", Prompt: "Allow this command?", At: time.Now(), Detail: detail}}
	s.Backend = recorder
	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "claude-screen-block", Harness: "claude", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: fmt.Sprintf("claude-state-%d", i), JobID: job.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	job, _ = store.Get(ctx, job.ID)
	if err = s.Registry.Put(Worker{Job: job, Launch: harnesses.Launch{Events: filepath.Join(cwd, "absent-events")}, Session: "agent", Target: "w1:p1", LastState: protocol.Running}); err != nil {
		t.Fatal(err)
	}
	if err = s.observe(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.State != protocol.Blocked || got.Question == nil || got.Question.Prompt != "Allow this command?" {
		t.Fatalf("screen block was not dispatcher-visible: %#v %v", got, err)
	}
	var metadata map[string]any
	if json.Unmarshal(got.Question.Detail, &metadata) != nil || metadata["source"] != "screen" || metadata["structured"] != false {
		t.Fatalf("screen block claimed structured semantics: %s", got.Question.Detail)
	}
	if _, err = store.Answer(ctx, job.ID, protocol.Answer{IdempotencyKey: "screen-answer", QuestionID: got.Question.ID, Text: "1 enter"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.sent) != 1 || recorder.sent[0] != "answer:1 enter" {
		t.Fatalf("screen answer did not use backend answer routing exactly once: %v", recorder.sent)
	}
}

func TestHerdrStyleSteersDeliverInOrderExactlyOnce(t *testing.T) {
	ctx := context.Background()
	cwd, artifacts := t.TempDir(), t.TempDir()
	s, store, _ := testSupervisor(t, cwd, artifacts)
	recorder := &recordingBackend{}
	s.Backend = recorder
	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "herdr-steer-order", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: fmt.Sprintf("herdr-state-%d", i), JobID: job.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	job, _ = store.Get(ctx, job.ID)
	if err = s.Registry.Put(Worker{Job: job, Launch: harnesses.Launch{}, Session: "agent", Target: "w1:p1", LastState: protocol.Running}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if _, err = store.Steer(ctx, job.ID, protocol.Steer{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if strings.Join(recorder.sent, ",") != "first,second" {
		t.Fatalf("ordered/idempotent delivery = %v", recorder.sent)
	}
	persisted, _ := store.Get(ctx, job.ID)
	if got := s.Registry.Snapshot()[job.ID].SteeredKey; got != persisted.Steers[1].ID {
		t.Fatalf("delivery cursor %q, want %q", got, persisted.Steers[1].ID)
	}
}

func TestTickDeliversPersistedSteersInOrder(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd, artifacts := t.TempDir(), t.TempDir()
	s, store, _ := testSupervisor(t, cwd, artifacts)
	s.Backend = tmuxbackend.Tmux{Socket: filepath.Join(t.TempDir(), "tmux.sock")}
	if err := s.tmux().Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.tmux().Run(context.Background(), "kill-server") })
	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "steer-order", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: fmt.Sprintf("steer-state-%d", i), JobID: job.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	job, _ = store.Get(ctx, job.ID)
	launch := harnesses.Launch{Argv: []string{"bash", "--noprofile", "--norc"}, Dir: cwd, Interactive: true}
	session, target, err := s.tmux().Start(ctx, job.ID, launch)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Registry.Put(Worker{Job: job, Launch: launch, Session: session, Target: target, LastState: protocol.Running, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(cwd, "steered")
	if _, err = store.Steer(ctx, job.ID, protocol.Steer{Text: "printf first >>" + out}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Steer(ctx, job.ID, protocol.Steer{Text: "printf second >>" + out}); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if got, readErr := os.ReadFile(out); readErr == nil && string(got) == "firstsecond" {
			worker := s.Registry.Snapshot()[job.ID]
			persisted, _ := store.Get(ctx, job.ID)
			if worker.SteeredKey != persisted.Steers[1].ID {
				t.Fatalf("delivery cursor not persisted: %#v", worker)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := os.ReadFile(out)
	t.Fatalf("steers not delivered in order: %q", got)
}

func TestPiSideChannelCompletionSettlesWhileTUIIsAlive(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd, artifacts := t.TempDir(), t.TempDir()
	s, store, _ := testSupervisor(t, cwd, artifacts)
	s.Backend = tmuxbackend.Tmux{Socket: filepath.Join(t.TempDir(), "tmux.sock")}
	if err := s.tmux().Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.tmux().Run(context.Background(), "kill-server") })
	s.Adapters["pi"] = piadapter.Adapter{}
	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "pi-side", Harness: "pi", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: "pi-start", JobID: job.ID, State: protocol.Starting}}}); err != nil {
		t.Fatal(err)
	}
	job.Artifacts.Directory = filepath.Join(artifacts, job.Artifacts.ID)
	if err = os.MkdirAll(job.Artifacts.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(job.Artifacts.Directory, "events.jsonl")
	launch := harnesses.Launch{Argv: []string{"sh", "-c", "sleep 30"}, Dir: cwd, Events: events, Interactive: true}
	session, target, err := s.tmux().Start(ctx, job.ID, launch)
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{Job: job, Launch: launch, Session: session, Target: target, LastState: protocol.Starting, StartedAt: time.Now()}
	if err = s.Registry.Put(worker); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(events, []byte(`{"type":"settled","ts":1,"verdict":"done","summary":"finished","usage":{"input":3,"output":2,"cost":0}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.observe(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.State != protocol.Done || got.Settlement == nil || got.Settlement.Summary != "finished" {
		t.Fatalf("pi completion did not propagate: %#v %v", got, err)
	}
	alive, _, err := s.tmux().Pane(ctx, target)
	if err != nil || !alive {
		t.Fatalf("interactive pane should still be alive at side-channel settlement: %v %v", alive, err)
	}
}

func TestFakeWorkerOutputIsVisibleCapturedAndSettled(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd := t.TempDir()
	artifacts := t.TempDir()
	s, store, c := testSupervisor(t, cwd, artifacts)
	s.Backend = tmuxbackend.Tmux{Socket: filepath.Join(t.TempDir(), "tmux.sock")}
	if err := s.tmux().Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.tmux().Run(context.Background(), "kill-server") })
	job, err := c.Create(ctx, protocol.CreateJob{IdempotencyKey: "fake-live", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileStart(ctx, job); err != nil {
		t.Fatal(err)
	}
	worker := s.Registry.Snapshot()[job.ID]
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err = s.observe(ctx); err != nil {
			t.Fatal(err)
		}
		got, getErr := store.Get(ctx, job.ID)
		if getErr == nil && got.State == protocol.Done && got.Settlement != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil || got.State != protocol.Done || got.Settlement == nil {
		t.Fatalf("fake worker did not settle from captured transcript: %#v %v", got, err)
	}
	if got.Settlement.ExitStatus == nil || *got.Settlement.ExitStatus != 0 || len(got.Settlement.Artifacts) == 0 {
		t.Fatalf("fake settlement lacks exit status/artifact listing: %#v", got.Settlement)
	}
	pane, err := s.tmux().Run(ctx, "capture-pane", "-p", "-S", "-", "-t", worker.Target)
	if err != nil || !strings.Contains(pane, "fake-worker-complete") {
		t.Fatalf("fake worker output not visible in pane: %q %v", pane, err)
	}
	transcript, err := os.ReadFile(worker.Launch.Transcript)
	if err != nil || string(transcript) != "fake-worker-complete\n" {
		t.Fatalf("fake transcript was not captured exactly: %q %v", transcript, err)
	}
}

func TestBootReconciliationFailsVanishedNonResumableWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd, artifactRoot := t.TempDir(), t.TempDir()
	s, store, c := testSupervisor(t, cwd, artifactRoot)
	s.Backend = tmuxbackend.Tmux{Socket: filepath.Join(t.TempDir(), "tmux.sock")}
	if err := s.tmux().Prepare(); err != nil {
		t.Fatal(err)
	}
	job, err := c.Create(ctx, protocol.CreateJob{IdempotencyKey: "boot-missing", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: fmt.Sprintf("boot-state-%d", i), JobID: job.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	job, err = store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	launch := harnesses.Launch{Argv: []string{"sh", "-c", "sleep 60"}, Dir: cwd, Transcript: filepath.Join(artifactRoot, "transcript")}
	if err = s.Registry.Put(Worker{Job: job, Launch: launch, Session: "worker-" + job.ID, Target: "worker-" + job.ID + ":0.0", LastState: protocol.Running, RestartUntil: time.Now().Add(time.Minute), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// This is the next-boot path after graceful shutdown killed tmux but left
	// durable job/worker state untouched. Fake is deliberately non-resumable.
	if err = s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != protocol.Failed || got.Settlement == nil {
		t.Fatalf("vanished worker was not failed during boot reconciliation: %#v", got)
	}
	if !strings.Contains(string(got.Settlement.Detail), "private tmux server unavailable") {
		t.Fatalf("missing honest crash boundary: %s", got.Settlement.Detail)
	}
}

func TestPermanentStartFailureSettlesImmediatelyAndRejectsCWD(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	s, store, c := testSupervisor(t, allowed, t.TempDir())
	j, err := c.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "outside", Harness: "fake", Host: "host", Prompt: "go", CWD: outside})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileStart(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), j.ID)
	if err != nil || got.State != protocol.Failed || got.Settlement == nil || !strings.Contains(got.Settlement.Summary, "outside configured allowed roots") {
		t.Fatalf("path rejection was not durably settled: %#v, %v", got, err)
	}
}

func TestTransientStartFailureIsBoundedAndAttemptPersists(t *testing.T) {
	cwd := t.TempDir()
	s, store, c := testSupervisor(t, cwd, filepath.Join("/dev/null", "artifacts"))
	j, err := c.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "bounded", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileStart(context.Background(), j); err == nil {
		t.Fatal("expected retryable filesystem failure")
	}
	attempt, ok := s.Registry.Attempt(j.ID)
	if !ok || attempt.Count != 1 || attempt.SettlementPending {
		t.Fatalf("attempt not persisted: %#v", attempt)
	}
	registry, err := OpenRegistry(s.Registry.path)
	if err != nil {
		t.Fatal(err)
	}
	s.Registry = registry
	time.Sleep(time.Millisecond)
	if err = s.reconcileStart(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), j.ID)
	if err != nil || got.State != protocol.Failed || got.Settlement == nil || !strings.Contains(got.Settlement.Summary, "after 2 attempt") {
		t.Fatalf("bounded failure was not durably settled: %#v, %v", got, err)
	}
}

func TestUnknownHarnessSettlesWithoutRetry(t *testing.T) {
	cwd := t.TempDir()
	s, store, c := testSupervisor(t, cwd, t.TempDir())
	j, err := c.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "unknown", Harness: "unknown", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileStart(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(context.Background(), j.ID)
	if got.State != protocol.Failed || got.Settlement == nil || !strings.Contains(got.Settlement.Summary, "unknown harness") {
		t.Fatalf("unknown harness was retried: %#v", got)
	}
}

func TestDiffDoesNotRestartOrForgetSettledWorkers(t *testing.T) {
	now := time.Now()
	desired := []protocol.Assignment{{Job: protocol.Job{ID: "historical", State: protocol.Done}, DesiredState: protocol.Done}}
	local := map[string]Worker{"lingering": {Job: protocol.Job{ID: "lingering"}, SettledAt: now}}
	if actions := Diff(desired, local); len(actions) != 0 {
		t.Fatalf("settled jobs produced actions: %#v", actions)
	}
}

func TestDiff(t *testing.T) {
	desired := []protocol.Assignment{{Job: protocol.Job{ID: "new", State: protocol.Assigned}}, {Job: protocol.Job{ID: "stop", State: protocol.Cancelling, CancelRequested: true}, DesiredState: protocol.Cancelling}}
	local := map[string]Worker{"stop": {Job: protocol.Job{ID: "stop"}}, "revoked": {Job: protocol.Job{ID: "revoked"}}}
	a := Diff(desired, local)
	seen := map[ActionKind]int{}
	for _, x := range a {
		seen[x.Kind]++
	}
	if seen[Start] != 1 || seen[Cancel] != 1 || seen[Forget] != 1 {
		t.Fatalf("actions %#v", a)
	}
}
