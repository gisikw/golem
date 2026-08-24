package harnesses

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gisikw/golem/protocol"
)

// TemplateAdapter is the honest minimum used by harnesses without a stable
// machine-readable lifecycle API. It launches configured argv and observes only
// process exit plus an optional transcript.
//
// The documented path for a harness that CAN report lifecycle is the
// hook/side-channel pattern (see harnesses/pi): the adapter instruments its
// harness with a sibling "hook adapter" that appends durable JSON records to
// Launch.Events, and Observe advances a durable cursor over that file instead
// of parsing harness stdout. A crash boundary (pane death) is always the
// supervisor's concern and needs no side-channel coverage. TemplateAdapter
// deliberately does neither: it claims no steering, blocked-question, usage, or
// resume semantics, returning ErrUnsupported for operations it cannot honor.
type TemplateAdapter struct {
	Kind         string
	ArgvTemplate []string
}

func (a TemplateAdapter) Start(_ context.Context, j protocol.Job) (Launch, error) {
	if len(a.ArgvTemplate) == 0 {
		return Launch{}, errors.New(a.Kind + " argv template is empty")
	}
	if err := os.MkdirAll(j.Artifacts.Directory, 0o700); err != nil {
		return Launch{}, err
	}
	repl := strings.NewReplacer("{prompt}", j.Prompt, "{model}", j.Model, "{cwd}", j.CWD, "{job_id}", j.ID, "{artifacts}", j.Artifacts.Directory)
	argv := make([]string, len(a.ArgvTemplate))
	for i, x := range a.ArgvTemplate {
		argv[i] = repl.Replace(x)
	}
	return Launch{Argv: argv, Dir: j.CWD, Transcript: filepath.Join(j.Artifacts.Directory, a.Kind+"-transcript.log")}, nil
}
func (a TemplateAdapter) Prompt(context.Context, *Runtime, string) error { return ErrUnsupported }
func (a TemplateAdapter) Answer(context.Context, *Runtime, protocol.Answer) error {
	return ErrUnsupported
}
func (a TemplateAdapter) Cancel(ctx context.Context, r *Runtime) error {
	if r.Cancel == nil {
		return ErrUnsupported
	}
	return r.Cancel(ctx)
}
func (a TemplateAdapter) Observe(ctx context.Context, _ protocol.Job, r *Runtime) (Observation, error) {
	if r.Alive == nil {
		return Observation{}, ErrUnsupported
	}
	alive, code, err := r.Alive(ctx)
	if err != nil {
		return Observation{}, err
	}
	if alive {
		return Observation{State: protocol.Running}, nil
	}
	return Observation{State: protocol.Failed, ExitCode: code}, nil
}
func (a TemplateAdapter) Resume(context.Context, protocol.Job, Launch) (Launch, error) {
	return Launch{}, ErrUnsupported
}
func (a TemplateAdapter) CollectSettlement(_ context.Context, j protocol.Job, l Launch, o Observation) (*protocol.Settlement, error) {
	return BasicSettlement(j, l, o)
}
