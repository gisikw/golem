package tmux

import (
	"context"

	"github.com/gisikw/golem/backend"
	"github.com/gisikw/golem/protocol"
)

// The tmux substrate is the historical default, so its policy is the zero
// value in every respect that matters: every configured harness runs, attach
// and steer stay available.
func (t Tmux) Policy() backend.Policy { return backend.Policy{Name: "tmux"} }

// Endpoint is the same host-local descriptor the supervisor has always
// published: this daemon's host, its private tmux socket, and the pane target.
func (t Tmux) Endpoint(host, target string) *protocol.TerminalEndpoint {
	return &protocol.TerminalEndpoint{Host: host, Socket: t.Socket, Target: target}
}

// Cancel keeps the historical behaviour exactly: Ctrl-C into the worker pane
// so it becomes a retained dead pane under the owned tmux policy. Teardown
// remains Kill's job.
func (t Tmux) Cancel(ctx context.Context, _, target string) error { return t.Interrupt(ctx, target) }

// Shutdown is the private tmux server teardown golemd has always performed on
// SIGINT/SIGTERM: it owns the server, so it kills it and every worker pane.
func (t Tmux) Shutdown(ctx context.Context) error { return t.KillServer(ctx) }

var _ backend.Backend = Tmux{}

// Run executes a raw tmux command against the private server. It exists so
// tests in other packages can assert on server-side state; nothing in the
// daemon path uses it.
func (t Tmux) Run(ctx context.Context, args ...string) (string, error) { return t.run(ctx, args...) }

// SafeName exposes the session-name sanitiser for tests that must predict a
// worker's deterministic tmux target.
func SafeName(id string) string { return safeName.ReplaceAllString(id, "-") }

// Teardown is the historical Kill: destroy the worker's tmux session.
func (t Tmux) Teardown(ctx context.Context, session, _ string) error { return t.Kill(ctx, session) }
