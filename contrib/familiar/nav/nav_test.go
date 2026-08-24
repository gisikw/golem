package nav

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gisikw/golem/protocol"
)

type fakeSource struct{ jobs []protocol.Job }

func (f *fakeSource) List(context.Context, protocol.State) ([]protocol.Job, error) {
	return f.jobs, nil
}

func TestProjectGroupsLabelsAndLiveTerminal(t *testing.T) {
	now := time.Now()
	jobs := []protocol.Job{
		{ID: "job-a", CWD: "/work/zeta", Prompt: "  Fix\nsidebar now and later", State: protocol.Running, UpdatedAt: now, Terminal: &protocol.TerminalEndpoint{Socket: "/run/tmux.sock", Target: "worker-job-a:0.0"}},
		{ID: "job-deadbeef", CWD: "/work/alpha", State: protocol.Done, UpdatedAt: now.Add(time.Minute), Terminal: &protocol.TerminalEndpoint{Socket: "/run/tmux.sock", Target: "worker-dead:0.0"}},
	}
	groups := Project(context.Background(), jobs, func(_ context.Context, endpoint protocol.TerminalEndpoint) bool {
		return endpoint.Target == "worker-job-a:0.0"
	})
	if len(groups) != 2 || groups[0].Label != "alpha" || groups[1].Label != "zeta" {
		t.Fatalf("groups = %#v", groups)
	}
	if groups[0].Items[0].Attach != nil {
		t.Fatal("dead terminal published attach")
	}
	item := groups[1].Items[0]
	if item.Label != "Fix sidebar now " || item.Attach == nil || item.Attach.Session != "worker-job-a" {
		t.Fatalf("live item = %#v", item)
	}
}

func TestGenerationWakeAndTimeout(t *testing.T) {
	source := &fakeSource{}
	s := New(source, func(context.Context, protocol.TerminalEndpoint) bool { return false }, 40*time.Millisecond)
	initial := s.Snapshot().Generation

	start := time.Now()
	r := httptest.NewRequest("GET", "/v1/nav?generation=1", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("long poll did not wait for timeout")
	}
	var timed Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &timed); err != nil || timed.Generation != initial {
		t.Fatalf("timeout snapshot: %#v %v", timed, err)
	}

	done := make(chan Snapshot, 1)
	go func() {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/nav?generation=1", nil))
		var got Snapshot
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		done <- got
	}()
	time.Sleep(10 * time.Millisecond)
	source.jobs = []protocol.Job{{ID: "job-new", CWD: "/w/a", Prompt: "new", State: protocol.Pending}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Generation <= initial {
			t.Fatalf("generation did not increase: %d", got.Generation)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll did not wake")
	}
}

func TestNoTerminalMeansNoAttach(t *testing.T) {
	groups := Project(context.Background(), []protocol.Job{{ID: "job-a", CWD: "/w", State: protocol.Running}}, func(context.Context, protocol.TerminalEndpoint) bool { return true })
	if groups[0].Items[0].Attach != nil {
		t.Fatal("job without terminal published attach")
	}
}
