package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisikw/golem/backend"
	"github.com/gisikw/golem/protocol"
)

// A substrate that cannot run a harness or proxy a terminal says so on the
// wire. Herdr still accepts steering because agent.prompt supports working
// agents. The zero-value policy (tmux) must keep behaving exactly as before.
func TestBackendPolicyRejectsUnsupportedVerbsAndHarnesses(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job, err := store.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "k", Harness: "pi", CWD: "/tmp", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	caps := protocol.Capabilities{Harnesses: map[string]protocol.HarnessCapability{"pi": {}, "claude": {}}}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = store.Record(context.Background(), protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: fmt.Sprintf("state-%d", i), JobID: job.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	policy := backend.Policy{Name: "herdr", Harnesses: map[string]bool{"pi": true}, NoAttach: true}
	herdrSrv := httptest.NewServer(API{Store: store, Capabilities: caps, Backend: policy}.Handler())
	defer herdrSrv.Close()
	tmuxSrv := httptest.NewServer(API{Store: store, Capabilities: caps}.Handler())
	defer tmuxSrv.Close()

	post := func(base, path, body string) (int, string) {
		res, postErr := http.Post(base+path, "application/json", bytes.NewBufferString(body))
		if postErr != nil {
			t.Fatal(postErr)
		}
		defer res.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		return res.StatusCode, buf.String()
	}
	get := func(base, path string) (int, string) {
		res, getErr := http.Get(base + path)
		if getErr != nil {
			t.Fatal(getErr)
		}
		defer res.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		return res.StatusCode, buf.String()
	}

	code, body := post(herdrSrv.URL, "/v1/jobs", `{"idempotency_key":"c1","harness":"claude","cwd":"/tmp","prompt":"go"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "not supported on herdr backend") {
		t.Fatalf("claude dispatch on herdr: %d %s", code, body)
	}
	code, body = post(herdrSrv.URL, "/v1/jobs/"+job.ID+"/steer", `{"text":"go left"}`)
	if code != http.StatusOK {
		t.Fatalf("running steer on herdr: %d %s", code, body)
	}
	code, body = get(herdrSrv.URL, "/v1/jobs/"+job.ID+"/attach")
	if code != http.StatusNotImplemented || !strings.Contains(body, "not supported on herdr backend") {
		t.Fatalf("attach on herdr: %d %s", code, body)
	}

	// tmux (zero-value policy) is untouched: steer reaches the store, attach
	// reaches the endpoint check, and claude dispatch is allowed.
	if code, body = post(tmuxSrv.URL, "/v1/jobs/"+job.ID+"/steer", `{"text":"go left"}`); code == http.StatusNotImplemented {
		t.Fatalf("steer regressed on the tmux backend: %d %s", code, body)
	}
	if code, body = get(tmuxSrv.URL, "/v1/jobs/"+job.ID+"/attach"); code == http.StatusNotImplemented {
		t.Fatalf("attach regressed on the tmux backend: %d %s", code, body)
	}
	if code, body = post(tmuxSrv.URL, "/v1/jobs", `{"idempotency_key":"c2","harness":"claude","cwd":"/tmp","prompt":"go"}`); code == http.StatusBadRequest {
		t.Fatalf("claude dispatch regressed on the tmux backend: %d %s", code, body)
	}
}
