package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gisikw/golem/protocol"
)

func TestSteerRouteStatesPersistenceAndEvent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	makeJob := func(key string, state protocol.State) protocol.Job {
		j, createErr := store.Create(ctx, protocol.CreateJob{IdempotencyKey: key, Harness: "fake", CWD: "/tmp", Prompt: "go"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if state == protocol.Starting || state == protocol.Running || state == protocol.Blocked || state.Terminal() {
			if createErr = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: key + "-starting", JobID: j.ID, State: protocol.Starting}}}); createErr != nil {
				t.Fatal(createErr)
			}
		}
		if state == protocol.Running || state == protocol.Blocked || state.Terminal() {
			if createErr = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: key + "-running", JobID: j.ID, State: protocol.Running}}}); createErr != nil {
				t.Fatal(createErr)
			}
		}
		if state == protocol.Blocked {
			q := &protocol.BlockedQuestion{ID: "q", Prompt: "question", At: time.Now()}
			if createErr = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: key + "-blocked", JobID: j.ID, State: protocol.Blocked, Question: q}}}); createErr != nil {
				t.Fatal(createErr)
			}
		}
		if state.Terminal() {
			settlement := &protocol.Settlement{ID: key + "-settlement", JobID: j.ID, State: state, Verdict: state, At: time.Now()}
			if createErr = store.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: key + "-done", JobID: j.ID, Settlement: settlement}}}); createErr != nil {
				t.Fatal(createErr)
			}
		}
		j, _ = store.Get(ctx, j.ID)
		return j
	}
	starting := makeJob("starting", protocol.Starting)
	running := makeJob("running", protocol.Running)
	blocked := makeJob("blocked", protocol.Blocked)
	done := makeJob("done", protocol.Done)

	srv := httptest.NewServer(API{Store: store}.Handler())
	defer srv.Close()
	post := func(id, text string) (int, string) {
		res, postErr := http.Post(srv.URL+"/v1/jobs/"+id+"/steer", "application/json", strings.NewReader(`{"text":`+text+`}`))
		if postErr != nil {
			t.Fatal(postErr)
		}
		defer res.Body.Close()
		var body bytes.Buffer
		_, _ = body.ReadFrom(res.Body)
		return res.StatusCode, body.String()
	}
	if status, _ := post(starting.ID, `"early"`); status != http.StatusOK {
		t.Fatalf("starting steer status %d", status)
	}
	if status, _ := post(running.ID, `"first"`); status != http.StatusOK {
		t.Fatalf("running steer status %d", status)
	}
	if status, _ := post(running.ID, `"second"`); status != http.StatusOK {
		t.Fatalf("second steer status %d", status)
	}
	if status, body := post(blocked.ID, `"wrong surface"`); status != http.StatusConflict || !strings.Contains(body, "use answer") {
		t.Fatalf("blocked steer: %d %s", status, body)
	}
	if status, _ := post(done.ID, `"late"`); status != http.StatusConflict {
		t.Fatalf("terminal steer status %d", status)
	}
	if status, _ := post("missing", `"lost"`); status != http.StatusNotFound {
		t.Fatalf("unknown steer status %d", status)
	}
	if status, _ := post(running.ID, `""`); status != http.StatusBadRequest {
		t.Fatalf("empty steer status %d", status)
	}
	persisted, err := store.Get(ctx, running.ID)
	if err != nil || len(persisted.Steers) != 2 || persisted.Steers[0].Text != "first" || persisted.Steers[1].Text != "second" {
		t.Fatalf("steers not persisted in order: %#v %v", persisted.Steers, err)
	}
	events, err := store.Events(ctx, 0, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	progress := 0
	for _, event := range events {
		if event.Kind == "job.progress" && event.Progress != nil && strings.Contains(event.Progress.Message, "steering") {
			progress++
		}
	}
	if progress != 2 {
		t.Fatalf("got %d steering progress events: %#v", progress, events)
	}
}

func TestCapabilitiesAndDispatchValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caps := protocol.Capabilities{Name: "local-daemon", Version: "test", Harnesses: map[string]protocol.HarnessCapability{"pi": {Models: []string{"openai/gpt-5.6", "missing/model"}}, "fake": {Models: []string{}}}}
	server := httptest.NewServer(API{Store: store, Capabilities: caps, PiProviders: map[string]bool{"openai": true}}.Handler())
	defer server.Close()
	res, err := http.Get(server.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.Capabilities
	if err = json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if got.Name != caps.Name || got.Harnesses["pi"].Models[0] != "openai/gpt-5.6" {
		t.Fatalf("unexpected capabilities: %#v", got)
	}

	post := func(harness, model string) int {
		body, _ := json.Marshal(protocol.CreateJob{IdempotencyKey: harness + model, Harness: protocol.HarnessKind(harness), Model: model, CWD: "/tmp", Prompt: "go"})
		response, requestErr := http.Post(server.URL+"/v1/jobs", "application/json", bytes.NewReader(body))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := post("claude", ""); status < 400 || status >= 500 {
		t.Fatalf("unknown harness status %d", status)
	}
	if status := post("pi", "other/model"); status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown model status %d", status)
	}
	if status := post("pi", "missing/model"); status != http.StatusUnprocessableEntity {
		t.Fatalf("missing provider status %d", status)
	}
	if status := post("fake", ""); status != http.StatusCreated {
		t.Fatalf("model-less fake status %d", status)
	}
}
