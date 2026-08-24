package render

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gisikw/golem/protocol"
)

type fakeSource struct{ jobs []protocol.Job }

func (f *fakeSource) List(context.Context, protocol.State) ([]protocol.Job, error) {
	return f.jobs, nil
}
func children(n Node) []Node {
	if n.Children == nil {
		return nil
	}
	return *n.Children
}

func TestProjectTreeOrderLabelsAndLiveTerminal(t *testing.T) {
	now := time.Now()
	jobs := []protocol.Job{
		{ID: "job-a", CWD: "/work/zeta", Prompt: "  Fix\nsidebar now and later", State: protocol.Running, UpdatedAt: now, Terminal: &protocol.TerminalEndpoint{Socket: "/run/tmux.sock", Target: "worker-job-a:0.0"}},
		{ID: "job-deadbeef", CWD: "/work/alpha", State: protocol.Done, UpdatedAt: now.Add(time.Minute), Terminal: &protocol.TerminalEndpoint{Socket: "/run/tmux.sock", Target: "worker-dead:0.0"}},
	}
	root := Project(context.Background(), jobs, func(_ context.Context, endpoint protocol.TerminalEndpoint) bool {
		return endpoint.Target == "worker-job-a:0.0"
	})
	branches := children(root)
	if root.Kind != "tree" || root.ID != "golem:jobs" || len(branches) != 2 {
		t.Fatalf("root = %#v", root)
	}
	if branches[0].Kind != "branch" || branches[0].Label != "alpha" || branches[1].Label != "zeta" {
		t.Fatalf("branches = %#v", branches)
	}
	if children(branches[0])[0].Activation != nil {
		t.Fatal("dead terminal published activation")
	}
	item := children(branches[1])[0]
	if item.Kind != "item" || item.Label != "Fix sidebar now " || item.Status != "running" || item.Activation == nil || item.Activation.Type != "terminal" || item.Activation.Session != "worker-job-a" {
		t.Fatalf("live item = %#v", item)
	}
}

func TestRenderIncludesExactAPIRevisionTTLAndContainerChildren(t *testing.T) {
	s := New(&fakeSource{}, func(context.Context, protocol.TerminalEndpoint) bool { return false }, nil, 30*time.Second)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/render", nil))
	var doc Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.RenderAPI != 1 || doc.Revision != 1 || doc.TTLMillis != 30000 || doc.Target != "left-nav" || doc.Content.Kind != "tree" || doc.Content.Children == nil {
		t.Fatalf("render document: %#v %v", doc, err)
	}
}

func TestChangedStatePostsOnceAndUnchangedDoesNotPoke(t *testing.T) {
	source := &fakeSource{}
	pokes := 0
	s := New(source, func(context.Context, protocol.TerminalEndpoint) bool { return false }, func(context.Context) error { pokes++; return nil }, 30*time.Second)
	// A render marks the host cache fresh.
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/render", nil))
	if err := s.Refresh(context.Background()); err != nil || pokes != 0 {
		t.Fatalf("unchanged refresh pokes=%d err=%v", pokes, err)
	}
	source.jobs = []protocol.Job{{ID: "job-a", CWD: "/w", State: protocol.Running}}
	if err := s.Refresh(context.Background()); err != nil || pokes != 1 {
		t.Fatalf("changed refresh pokes=%d err=%v", pokes, err)
	}
	if err := s.Refresh(context.Background()); err != nil || pokes != 1 {
		t.Fatalf("unchanged refresh repeated poke=%d err=%v", pokes, err)
	}
	// Changes coalesce until Familiar refetches.
	source.jobs[0].State = protocol.Blocked
	if err := s.Refresh(context.Background()); err != nil || pokes != 1 {
		t.Fatalf("unobserved change was not coalesced: %d %v", pokes, err)
	}
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/render", nil))
	source.jobs[0].State = protocol.Running
	if err := s.Refresh(context.Background()); err != nil || pokes != 2 {
		t.Fatalf("post-refetch change pokes=%d err=%v", pokes, err)
	}
}

func TestCallbackIsEmptyPOSTAndFailureDoesNotFailRefresh(t *testing.T) {
	requests := 0
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || len(body) != 0 || r.ContentLength > 0 {
			t.Errorf("callback method=%s length=%d body=%q", r.Method, r.ContentLength, body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	if err := Callback(callback.URL, callback.Client())(context.Background()); err != nil || requests != 1 {
		t.Fatalf("callback requests=%d err=%v", requests, err)
	}

	source := &fakeSource{}
	s := New(source, func(context.Context, protocol.TerminalEndpoint) bool { return false }, func(context.Context) error { return errors.New("host unavailable") }, time.Second)
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/render", nil))
	source.jobs = []protocol.Job{{ID: "job-a", CWD: "/w", State: protocol.Running}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("callback failure escaped refresh: %v", err)
	}
}

func TestNoTerminalMeansNoActivation(t *testing.T) {
	root := Project(context.Background(), []protocol.Job{{ID: "job-a", CWD: "/w", State: protocol.Running}}, func(context.Context, protocol.TerminalEndpoint) bool { return true })
	if children(children(root)[0])[0].Activation != nil {
		t.Fatal("job without terminal published activation")
	}
}
