// Package backend defines the run substrate contract: everything golemd needs
// to give a job a live harness process, observe it, deliver text to it, cancel
// it, and tear it down. The supervisor owns lifecycle policy and the ledger;
// a Backend owns process reality.
//
// Two implementations exist: backend/tmux (the original private tmux server,
// unchanged) and backend/herdr (a Herdr session driven over its nd-JSON socket
// API). tmux remains the default; herdr is selected only when [herdr] is
// configured.
package backend

import (
	"context"
	"time"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

// Backend is the substrate a supervisor drives. Session and target are opaque
// substrate handles the supervisor persists in its durable worker registry
// (tmux: session name and pane target; herdr: agent name and pane id).
type Backend interface {
	// Prepare validates/creates whatever the substrate needs before any job
	// starts. It runs once at daemon boot.
	Prepare() error
	// Start launches the harness for a job and returns its durable handles. It
	// must be idempotent for an already-live job (adoption).
	Start(ctx context.Context, id string, l harnesses.Launch) (session string, target string, err error)
	// Has reports whether the substrate still holds this session.
	Has(ctx context.Context, session string) bool
	// Kill tears the job's substrate down (teardown, reap, forget).
	Kill(ctx context.Context, session string) error
	// Cancel is the interactive cancellation path: interrupt the harness and,
	// where the substrate requires it, verify the process is gone.
	Cancel(ctx context.Context, session, target string) error
	// Send delivers text to the harness as one atomic submitted message.
	Send(ctx context.Context, target, text string) error
	// Pane reports liveness and, when the substrate has one, an exit status.
	Pane(ctx context.Context, target string) (bool, *int, error)
	// ServerAlive reports substrate health (used for failure-boundary detail).
	ServerAlive(ctx context.Context) bool
	// Shutdown tears down substrate-owned processes at daemon shutdown. A
	// substrate golemd does not own (herdr) does nothing here.
	Shutdown(ctx context.Context) error
	// Endpoint is the host-local terminal descriptor for a live target, or nil
	// when the substrate publishes none.
	Endpoint(host, target string) *protocol.TerminalEndpoint
	// Policy is the substrate's public capability surface.
	Policy() Policy
}

// Policy is a zero-value-is-today's-behaviour description of what the active
// substrate supports. The zero value means "tmux": every configured harness,
// attach and steer available.
type Policy struct {
	// Name identifies the substrate in logs and errors ("tmux", "herdr").
	Name string
	// Harnesses, when non-nil, is the exact set of harness kinds this substrate
	// can run. Dispatch of any other kind is rejected with 400.
	Harnesses map[string]bool
	// NoAttach/NoSteer make those verbs return 501 with an explicit message.
	NoAttach bool
	NoSteer  bool
}

// Status is one observation of a target by a substrate that watches state
// asynchronously. AsOf is when golemd learned it, never a backdated guess.
type Status struct {
	State   protocol.State
	Stale   bool
	Present bool
	AsOf    time.Time
}

// Observer is implemented by substrates that maintain their own asynchronous
// view of a target (herdr's event subscription plus reconcile poll). The
// supervisor uses it only to stamp an honest as_of on published state events.
type Observer interface {
	Status(target string) (Status, bool)
}
