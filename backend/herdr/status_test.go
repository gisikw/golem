package herdr

import (
	"testing"
	"time"

	"github.com/gisikw/golem/protocol"
)

func TestMapStatus(t *testing.T) {
	for _, tc := range []struct {
		herdr string
		state protocol.State
		stale bool
		known bool
	}{
		// idle and done are NOT terminal: idle means "ready for input" and done
		// means "idle after unseen work". Neither ever settles a Golem job.
		{"working", protocol.Running, false, true},
		{"idle", protocol.Running, false, true},
		{"done", protocol.Running, false, true},
		{"blocked", protocol.Blocked, false, true},
		// unknown keeps the last known state and only raises the stale flag:
		// herdr says explicitly that it does not prove completion.
		{"unknown", "", true, true},
		{"teleporting", "", true, false},
	} {
		state, stale, known := MapStatus(tc.herdr)
		if state != tc.state || stale != tc.stale || known != tc.known {
			t.Fatalf("MapStatus(%q) = %q,%t,%t; want %q,%t,%t", tc.herdr, state, stale, known, tc.state, tc.stale, tc.known)
		}
	}
}

func TestUnknownKeepsLastKnownStateAndFlagsStale(t *testing.T) {
	b := &Backend{}
	b.record("w1:p1", "working", time.Now().UTC())
	b.record("w1:p1", "unknown", time.Now().UTC())
	status, ok := b.Status("w1:p1")
	if !ok || status.State != protocol.Running || !status.Stale {
		t.Fatalf("unknown must preserve the last state and flag it stale, got %+v", status)
	}
}
