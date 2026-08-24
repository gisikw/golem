// Package codex is a minimal Codex process adapter. Codex lifecycle, resume,
// and steering are not claimed until a stable machine API is integrated.
package codex

import (
	"context"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

type Adapter struct{ ArgvTemplate []string }

func (a Adapter) impl() harnesses.TemplateAdapter {
	return harnesses.TemplateAdapter{Kind: "codex", ArgvTemplate: a.ArgvTemplate}
}
func (a Adapter) Start(c context.Context, j protocol.Job) (harnesses.Launch, error) {
	return a.impl().Start(c, j)
}
func (a Adapter) Prompt(c context.Context, r *harnesses.Runtime, s string) error {
	return a.impl().Prompt(c, r, s)
}
func (a Adapter) Observe(c context.Context, j protocol.Job, r *harnesses.Runtime) (harnesses.Observation, error) {
	return a.impl().Observe(c, j, r)
}
func (a Adapter) Answer(c context.Context, r *harnesses.Runtime, x protocol.Answer) error {
	return a.impl().Answer(c, r, x)
}
func (a Adapter) Cancel(c context.Context, r *harnesses.Runtime) error { return a.impl().Cancel(c, r) }
func (a Adapter) Resume(c context.Context, j protocol.Job, l harnesses.Launch) (harnesses.Launch, error) {
	return a.impl().Resume(c, j, l)
}
func (a Adapter) CollectSettlement(c context.Context, j protocol.Job, l harnesses.Launch, o harnesses.Observation) (*protocol.Settlement, error) {
	return a.impl().CollectSettlement(c, j, l, o)
}

var _ harnesses.Adapter = Adapter{}
