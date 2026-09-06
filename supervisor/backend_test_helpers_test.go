package supervisor

import tmuxbackend "github.com/gisikw/golem/backend/tmux"

// tmux returns the tmux backend a test installed on this supervisor. The
// substrate is an interface now; these tests still drive the real tmux one.
func (s *Supervisor) tmux() tmuxbackend.Tmux { return s.Backend.(tmuxbackend.Tmux) }
