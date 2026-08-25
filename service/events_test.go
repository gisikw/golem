package service

import (
	"bufio"
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

func TestEventSequenceReplayAndSettleOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	j, err := s.Create(ctx, protocol.CreateJob{IdempotencyKey: "event", Harness: "fake", CWD: "/tmp", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: string(rune('a' + i)), JobID: j.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	set := &protocol.Settlement{ID: "first", JobID: j.ID, State: protocol.Done, Verdict: protocol.Done, At: time.Now()}
	if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: "settle", JobID: j.ID, Settlement: set}}}); err != nil {
		t.Fatal(err)
	}
	late := &protocol.Settlement{ID: "late", JobID: j.ID, State: protocol.Failed, Verdict: protocol.Failed, At: time.Now()}
	if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: "late-settle", JobID: j.ID, Settlement: late}}}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, 0, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("non-monotonic events: %#v", events)
		}
	}
	if len(events) < 5 || events[len(events)-1].Kind != "job.settled" || events[len(events)-1].Settlement.State != protocol.Done {
		t.Fatalf("unexpected lifecycle: %#v", events)
	}
	replay, err := s.Events(ctx, events[1].Seq, j.ID)
	if err != nil || len(replay) != len(events)-2 || replay[0].Seq != events[2].Seq {
		t.Fatalf("bad replay: %#v %v", replay, err)
	}
}

func TestSSEReplaysThenStreamsBlockedAndSettlement(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "sse", Harness: "fake", CWD: "/tmp", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(API{Store: s}.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events?since=0&job="+j.ID, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	type result struct {
		event protocol.Event
		err   error
	}
	got := make(chan result, 8)
	go func() {
		scan := bufio.NewScanner(res.Body)
		for scan.Scan() {
			if strings.HasPrefix(scan.Text(), "data: ") {
				var e protocol.Event
				x := json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &e)
				got <- result{e, x}
			}
		}
	}()
	first := <-got
	if first.err != nil || first.event.Kind != "job.created" {
		t.Fatalf("no replay: %#v", first)
	}
	for i, state := range []protocol.State{protocol.Starting, protocol.Running} {
		if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: string(rune('x' + i)), JobID: j.ID, State: state}}}); err != nil {
			t.Fatal(err)
		}
	}
	q := &protocol.BlockedQuestion{ID: "q", Prompt: "choose", At: time.Now()}
	if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: "blocked", JobID: j.ID, State: protocol.Blocked, Question: q}}}); err != nil {
		t.Fatal(err)
	}
	for {
		r := <-got
		if r.event.State == protocol.Blocked {
			if r.event.Question == nil || r.event.Question.Prompt != "choose" {
				t.Fatalf("blocked payload missing: %#v", r.event)
			}
			break
		}
	}
	set := &protocol.Settlement{ID: "done", JobID: j.ID, State: protocol.Done, Verdict: protocol.Done, At: time.Now()}
	if err = s.Record(ctx, protocol.EventBatch{Events: []protocol.ObservedEvent{{ID: "done", JobID: j.ID, Settlement: set}}}); err != nil {
		t.Fatal(err)
	}
	for {
		r := <-got
		if r.event.Kind == "job.settled" {
			if r.event.Settlement == nil || r.event.Settlement.State != protocol.Done {
				t.Fatalf("settlement payload missing: %#v", r.event)
			}
			break
		}
	}
}
