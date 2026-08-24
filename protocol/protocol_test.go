package protocol

import "testing"

func TestTransitions(t *testing.T) {
	legal := [][2]State{{Pending, Assigned}, {Assigned, Starting}, {Starting, Running}, {Running, Blocked}, {Blocked, Running}, {Running, Cancelling}}
	for _, p := range legal {
		if !CanTransition(p[0], p[1], false) {
			t.Errorf("expected legal %s -> %s", p[0], p[1])
		}
	}
	if CanTransition(Assigned, Running, false) {
		t.Error("assignment skipped starting")
	}
	if CanTransition(Running, Done, false) {
		t.Error("terminal accepted without settlement")
	}
	if !CanTransition(Running, Done, true) {
		t.Error("terminal settlement rejected")
	}
	if CanTransition(Done, Running, true) {
		t.Error("terminal state escaped")
	}
}
