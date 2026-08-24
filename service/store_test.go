package service

import (
	"context"
	"encoding/json"
	"github.com/gisikw/golem/protocol"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func create(t *testing.T, s *Store) protocol.Job {
	t.Helper()
	j, e := s.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "key", Harness: "fake", Host: "host", Prompt: "go", CWD: "/tmp"})
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestCreateRejectsPlaintextProviderSecret(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	secret := "fixture-api-key"
	_, err = s.Create(context.Background(), protocol.CreateJob{
		IdempotencyKey: "secret-key", Harness: "fake", Host: "host", Prompt: "go", CWD: "/tmp",
		ProviderConfig: &protocol.ProviderConfig{Provider: "demo", ModelsJSON: json.RawMessage(`{"providers":{"demo":{"apiKey":"` + secret + `"}}}`)},
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("plaintext provider secret was accepted or echoed: %v", err)
	}
}

func TestCreateAcceptsProviderSecretReference(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Create(context.Background(), protocol.CreateJob{
		IdempotencyKey: "reference-key", Harness: "fake", Host: "host", Prompt: "go", CWD: "/tmp",
		ProviderConfig: &protocol.ProviderConfig{Provider: "demo", ModelsJSON: json.RawMessage(`{"providers":{"demo":{"apiKey":"$GOLEM_API_KEY"}}}`)},
	})
	if err != nil {
		t.Fatalf("reference rejected: %v", err)
	}
}

func TestSettlementIdempotentAndDurable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "db")
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	j := create(t, s)
	ctx := context.Background()
	for i, st := range []protocol.State{protocol.Starting, protocol.Running} {
		if e = s.Record(ctx, protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: string(rune('a' + i)), JobID: j.ID, State: st}}}); e != nil {
			t.Fatal(e)
		}
	}
	set := &protocol.Settlement{ID: "settle", JobID: j.ID, Verdict: protocol.Done, At: time.Now()}
	b := protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: "terminal", JobID: j.ID, Settlement: set}}}
	if e = s.Record(ctx, b); e != nil {
		t.Fatal(e)
	}
	if e = s.Record(ctx, b); e != nil {
		t.Fatal(e)
	}
	altered := *set
	altered.Summary = "late conflicting detail"
	if e = s.Record(ctx, protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: "terminal-retry", JobID: j.ID, Settlement: &altered}}}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e := s.Get(ctx, j.ID)
	if e != nil || got.State != protocol.Done || got.Settlement == nil || got.Settlement.Summary != "" {
		t.Fatalf("reopen/first settlement: %#v %v", got, e)
	}
}
func TestAnswerIdempotencyConflict(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	block := func(key, question string) protocol.Job {
		j, err := s.Create(ctx, protocol.CreateJob{IdempotencyKey: key, Harness: "fake", Host: "host", Prompt: "go", CWD: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		events := []protocol.ObservedEvent{
			{ID: key + "-start", JobID: j.ID, State: protocol.Starting},
			{ID: key + "-run", JobID: j.ID, State: protocol.Running},
			{ID: key + "-question", JobID: j.ID, Question: &protocol.BlockedQuestion{ID: question, Prompt: "answer", Options: []string{"yes", "no"}, At: time.Now()}},
		}
		if err = s.Record(ctx, protocol.EventBatch{Host: "host", Events: events}); err != nil {
			t.Fatal(err)
		}
		return j
	}
	first := block("answer-job-1", "q1")
	if got, e := s.Get(ctx, first.ID); e != nil || got.Question == nil || len(got.Question.Options) != 2 || got.Question.Options[0] != "yes" {
		t.Fatalf("blocked question options not persisted: %#v, %v", got.Question, e)
	}
	a := protocol.Answer{IdempotencyKey: "answer-key", QuestionID: "q1", Text: "yes"}
	if _, e = s.Answer(ctx, first.ID, a); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Answer(ctx, first.ID, a); e != nil {
		t.Fatalf("exact retry failed: %v", e)
	}
	changed := a
	changed.Text = "no"
	if _, e = s.Answer(ctx, first.ID, changed); e == nil {
		t.Fatal("reused answer key with changed text")
	}
	second := block("answer-job-2", "q2")
	if _, e = s.Answer(ctx, second.ID, protocol.Answer{IdempotencyKey: "answer-key", QuestionID: "q2", Text: "yes"}); e == nil {
		t.Fatal("reused answer key for another job/question")
	}
}

func TestReapRefusesRunningAndPollsSettledRequest(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	j := create(t, s)
	if _, e = s.Reap(ctx, j.ID); e == nil {
		t.Fatal("running job was accepted for reap")
	}
	for i, st := range []protocol.State{protocol.Starting, protocol.Running} {
		if e = s.Record(ctx, protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: string(rune('r' + i)), JobID: j.ID, State: st}}}); e != nil {
			t.Fatal(e)
		}
	}
	set := &protocol.Settlement{ID: "reap-settle", JobID: j.ID, Verdict: protocol.Done, At: time.Now()}
	if e = s.Record(ctx, protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: "reap-terminal", JobID: j.ID, Settlement: set}}}); e != nil {
		t.Fatal(e)
	}
	got, e := s.Reap(ctx, j.ID)
	if e != nil || !got.ReapRequested {
		t.Fatalf("settled reap request: %#v %v", got, e)
	}
	poll, e := s.Poll(ctx, "host")
	if e != nil || len(poll.Assignments) != 1 || !poll.Assignments[0].Job.ReapRequested {
		t.Fatalf("reap request not delivered to supervisor: %#v %v", poll, e)
	}
}

func TestServiceAssignsLogicalArtifactID(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "db"))
	defer s.Close()
	j := create(t, s)
	if j.Artifacts.ID != j.ID || j.Artifacts.Directory != "" || filepath.IsAbs(j.Artifacts.ID) {
		t.Fatalf("service leaked a filesystem artifact path: %#v", j.Artifacts)
	}
}

func TestRejectTerminalWithoutSettlement(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "db"))
	defer s.Close()
	j := create(t, s)
	e := s.Record(context.Background(), protocol.EventBatch{Host: "host", Events: []protocol.ObservedEvent{{ID: "bad", JobID: j.ID, State: protocol.Failed}}})
	if e == nil {
		t.Fatal("accepted terminal observation without settlement")
	}
}
