package supervisor

import (
	"encoding/json"
	"errors"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Worker struct {
	Job               protocol.Job     `json:"job"`
	Launch            harnesses.Launch `json:"launch"`
	Session           string           `json:"session"`
	Target            string           `json:"target"`
	Worktree          string           `json:"worktree,omitempty"`
	RestartUntil      time.Time        `json:"restart_until"`
	LastState         protocol.State   `json:"last_state"`
	LastExit          *int             `json:"last_exit,omitempty"`
	AnsweredKey       string           `json:"answered_key,omitempty"`
	ObservationCursor int64            `json:"observation_cursor,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	SettledAt         time.Time        `json:"settled_at,omitempty"`
}

type StartAttempt struct {
	Count             int       `json:"count"`
	NextAttempt       time.Time `json:"next_attempt,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	SettlementPending bool      `json:"settlement_pending,omitempty"`
}

type Registry struct {
	path     string
	mu       sync.Mutex
	Workers  map[string]Worker       `json:"workers"`
	Attempts map[string]StartAttempt `json:"start_attempts,omitempty"`
}

func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, Workers: map[string]Worker{}, Attempts: map[string]StartAttempt{}}
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return r, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, r); e != nil {
		return nil, e
	}
	if r.Workers == nil {
		r.Workers = map[string]Worker{}
	}
	if r.Attempts == nil {
		r.Attempts = map[string]StartAttempt{}
	}
	return r, nil
}
func (r *Registry) Snapshot() map[string]Worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := map[string]Worker{}
	for k, v := range r.Workers {
		x[k] = v
	}
	return x
}
func (r *Registry) Put(w Worker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Workers[w.Job.ID] = w
	return r.save()
}
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Workers, id)
	delete(r.Attempts, id)
	return r.save()
}
func (r *Registry) Attempt(id string) (StartAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.Attempts[id]
	return a, ok
}
func (r *Registry) PutAttempt(id string, a StartAttempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Attempts[id] = a
	return r.save()
}
func (r *Registry) ClearAttempt(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Attempts, id)
	return r.save()
}
func (r *Registry) save() error {
	if e := os.MkdirAll(filepath.Dir(r.path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	tmp := r.path + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, r.path)
}
