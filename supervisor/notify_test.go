package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gisikw/golem/protocol"
)

func TestWorklistNotifierWritesAtomicEnvelope(t *testing.T) {
	dir := t.TempDir()
	n := WorklistNotifier{Host: "host", Dir: dir}
	job := protocol.Job{ID: "job-123"}
	set := &protocol.Settlement{JobID: "job-123", Verdict: protocol.Done, Summary: "did the thing"}
	if err := n.Settled(context.Background(), job, set); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "incoming", "subagent-job-123-settlement.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("envelope not written: %v", err)
	}
	var env map[string]any
	if err = json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if env["id"] != "subagent-job-123-settlement" || env["priority"].(float64) != 2 || env["body"] != "did the thing" {
		t.Fatalf("envelope fields wrong: %#v", env)
	}
	// No leftover temp files (atomic rename cleaned up).
	entries, _ := os.ReadDir(filepath.Join(dir, "incoming"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly one whole file, got %d", len(entries))
	}
	// Failed/timeout are more urgent (P1).
	set.Verdict = protocol.Failed
	if err = n.Settled(context.Background(), job, set); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(file)
	_ = json.Unmarshal(b, &env)
	if env["priority"].(float64) != 1 {
		t.Fatalf("failed settlement not P1: %#v", env)
	}
}

func TestWebhookNotifierPostsNotice(t *testing.T) {
	var got settlementNotice
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	n := WebhookNotifier{Host: "laptop", URL: server.URL}
	job := protocol.Job{ID: "abc"}
	set := &protocol.Settlement{JobID: "abc", Verdict: protocol.Timeout, Summary: "ran out"}
	if err := n.Settled(context.Background(), job, set); err != nil {
		t.Fatal(err)
	}
	if got.JobID != "abc" || got.Verdict != "timeout" || got.Priority != 1 || got.Host != "laptop" || got.Body != "ran out" {
		t.Fatalf("webhook received wrong notice: %#v", got)
	}
}

func TestWebhookNotifierReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	n := WebhookNotifier{Host: "h", URL: server.URL}
	err := n.Settled(context.Background(), protocol.Job{ID: "x"}, &protocol.Settlement{JobID: "x", Verdict: protocol.Done})
	if err == nil {
		t.Fatal("expected an error on 500 so the caller can log it")
	}
}

func TestNotifiersFanOutAndTolerateFailure(t *testing.T) {
	dir := t.TempDir()
	// One good (worklist) + one failing (unreachable webhook). Fan-out returns
	// the first error but still delivers to the healthy transport.
	ns := Notifiers{
		WorklistNotifier{Host: "h", Dir: dir},
		WebhookNotifier{Host: "h", URL: "http://127.0.0.1:1"},
	}
	_ = ns.Settled(context.Background(), protocol.Job{ID: "j"}, &protocol.Settlement{JobID: "j", Verdict: protocol.Done})
	if _, err := os.Stat(filepath.Join(dir, "incoming", "subagent-j-settlement.json")); err != nil {
		t.Fatalf("healthy transport did not deliver despite sibling failure: %v", err)
	}
}
