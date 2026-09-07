package herdr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewPaneRetriesOnlyBusyOnSamePane(t *testing.T) {
	calls := 0
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		if method != "agent.start" {
			t.Errorf("unexpected method %s", method)
		}
		if params["pane_id"] != "w1:p1" {
			t.Errorf("changed pane: %v", params)
		}
		calls++
		if calls < 3 {
			return nil, &Error{Code: "agent_pane_busy", Message: "not an available shell"}
		}
		return agentInfo("job-abc", "w1:p1", "idle"), nil
	})
	b := &Backend{Socket: fake.socket}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.startInNewPane(ctx, "job-abc", "pi", "w1:p1", nil, 1000); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("got %d attempts", calls)
	}
}

func TestNewPaneBusyWaitHonorsDeadline(t *testing.T) {
	fake := newFakeHerdr(t, func(string, map[string]any) (any, *Error) {
		return nil, &Error{Code: "agent_pane_busy", Message: "busy"}
	})
	b := &Backend{Socket: fake.socket}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := b.startInNewPane(ctx, "job-abc", "pi", "w1:p1", nil, 1000)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestNewPaneDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	fake := newFakeHerdr(t, func(string, map[string]any) (any, *Error) {
		calls++
		return nil, &Error{Code: "agent_start_timeout", Message: "launch may have happened"}
	})
	b := &Backend{Socket: fake.socket}
	err := b.startInNewPane(context.Background(), "job-abc", "pi", "w1:p1", nil, 1000)
	var remote *Error
	if !errors.As(err, &remote) || remote.Code != "agent_start_timeout" || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
