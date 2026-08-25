package supervisor

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gisikw/golem/client"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/harnesses/claude"
	"github.com/gisikw/golem/protocol"
	"github.com/gisikw/golem/service"
)

// liveSupervisor builds a supervisor whose tmux socket is prepared and whose
// fake harness stays alive (like an interactive pi worker), so a started worker
// has a genuinely live terminal to target.
func liveSupervisor(t *testing.T) (*Supervisor, *service.Store, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	cwd := t.TempDir()
	s, store, _ := testSupervisor(t, cwd, t.TempDir())
	s.Adapters = map[string]harnesses.Adapter{"fake": claude.Adapter{ArgvTemplate: []string{"sh", "-c", "sleep 30"}}}
	s.Tmux = Tmux{Socket: filepath.Join(t.TempDir(), "tmux.sock")}
	if err := s.Tmux.Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Tmux.run(context.Background(), "kill-server") })
	return s, store, cwd
}

// A worker whose terminal-bearing Starting/Running event was lost (service
// briefly unavailable) and whose LastState has since advanced past Starting is
// exactly the Azula state: state=assigned, terminal=nil, tmux session alive.
// The observe self-heal only fires while LastState==Starting, so before the fix
// the row stayed activation-less forever. This exercises the persistence/restart
// seam (registry reloaded from disk into a fresh supervisor) rather than a
// constructor happy path, and asserts the exact socket/target is recovered.
func TestTerminalEndpointReassertedAfterLostStartingEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, store, cwd := liveSupervisor(t)

	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "k", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}

	// Service is down when the worker starts: the live tmux session comes up but
	// the terminal-carrying Starting event is dropped.
	s.Client = client.New("http://127.0.0.1:1")
	if err = s.reconcileStart(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, job.ID); got.Terminal != nil {
		t.Fatalf("precondition: terminal unexpectedly present: %#v", got.Terminal)
	}

	// Supervisor restart: reload the durable worker registry from disk. Simulate
	// that the worker's heartbeat had already advanced LastState past Starting,
	// so the Starting-retry self-heal in observe can no longer fire.
	regPath := s.Registry.path
	w := s.Registry.Snapshot()[job.ID]
	w.LastState = protocol.Running
	if err = s.Registry.Put(w); err != nil {
		t.Fatal(err)
	}
	reg2, err := OpenRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(service.API{Store: store}.Handler())
	defer httpServer.Close()
	s2 := &Supervisor{Host: "host", Client: client.New(httpServer.URL), Registry: reg2, Tmux: s.Tmux, ArtifactRoot: s.ArtifactRoot, AllowedCWDRoots: []string{cwd}, Adapters: s.Adapters, MaxStartAttempts: 2, StartBackoff: time.Nanosecond}

	for i := 0; i < 3; i++ {
		if e := s2.Tick(ctx); e != nil {
			t.Logf("tick: %v", e)
		}
		time.Sleep(30 * time.Millisecond)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terminal == nil {
		t.Fatal("live worker never regained a terminal endpoint: row stays nonclickable")
	}
	wantTarget := "worker-" + safeName.ReplaceAllString(job.ID, "-") + ":0.0"
	if got.Terminal.Host != "host" || got.Terminal.Socket != s.Tmux.Socket || got.Terminal.Target != wantTarget {
		t.Fatalf("reasserted endpoint not exact: %#v (want socket=%q target=%q)", got.Terminal, s.Tmux.Socket, wantTarget)
	}
}

// A dead terminal must never be given a fabricated activation target: if the
// tmux session is gone, reassertion is a no-op and the service record stays
// terminal-less, so the row remains correctly nonactionable.
func TestDeadWorkerGetsNoTerminalReassertion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, store, cwd := liveSupervisor(t)

	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "k", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	s.Client = client.New("http://127.0.0.1:1") // terminal event lost at start
	if err = s.reconcileStart(ctx, job); err != nil {
		t.Fatal(err)
	}
	w := s.Registry.Snapshot()[job.ID]
	w.LastState = protocol.Running
	if err = s.Registry.Put(w); err != nil {
		t.Fatal(err)
	}

	// Kill the tmux session: the terminal is now dead/stale.
	if err = s.Tmux.Kill(ctx, w.Session); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	if s.Tmux.Has(ctx, w.Session) {
		t.Fatal("session still alive after kill")
	}

	// Service recovers; reassertion must decline to invent a target.
	s.Client = client.New(mustURL(t, store))
	current, _ := store.Get(ctx, job.ID)
	s.reassertTerminal(ctx, current)

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terminal != nil {
		t.Fatalf("dead worker fabricated a terminal target: %#v", got.Terminal)
	}
}

// A healthy worker whose service record already carries the exact endpoint must
// not re-emit terminal events (no churn, no revision thrash downstream).
func TestReassertTerminalIsIdempotentWhenRecordMatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, store, cwd := liveSupervisor(t)
	s.Client = client.New(mustURL(t, store))

	job, err := store.Create(ctx, protocol.CreateJob{IdempotencyKey: "k", Harness: "fake", Host: "host", Prompt: "go", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileStart(ctx, job); err != nil {
		t.Fatal(err)
	}
	// One observe drives Starting->Running, recording the endpoint the normal way.
	_ = s.observe(ctx)
	before, _ := store.Get(ctx, job.ID)
	if before.Terminal == nil {
		t.Fatal("precondition: normal path did not record terminal")
	}
	updated := before.UpdatedAt

	// Reassertion should recognise the record already matches and do nothing:
	// the job body (and its UpdatedAt) must be untouched.
	s.reassertTerminal(ctx, before)
	after, _ := store.Get(ctx, job.ID)
	if !after.UpdatedAt.Equal(updated) {
		t.Fatalf("idempotent reassertion mutated the record: %v -> %v", updated, after.UpdatedAt)
	}
}

func mustURL(t *testing.T, store *service.Store) string {
	t.Helper()
	srv := httptest.NewServer(service.API{Store: store}.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}
