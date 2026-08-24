// Package nav projects Golem jobs into Familiar's deliberately small nav seam.
package nav

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/golem/protocol"
)

const maxItems = 10

type Attach struct {
	Socket  string `json:"socket"`
	Session string `json:"session"`
}
type Item struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	State  string  `json:"state"`
	Attach *Attach `json:"attach,omitempty"`
}
type Group struct {
	Label string `json:"label"`
	Items []Item `json:"items"`
}
type Snapshot struct {
	Version    int     `json:"version"`
	Generation uint64  `json:"generation"`
	Groups     []Group `json:"groups"`
}

type JobSource interface {
	List(context.Context, protocol.State) ([]protocol.Job, error)
}
type LiveFunc func(context.Context, protocol.TerminalEndpoint) bool

// Project preserves the old Familiar sidebar ordering and labels: active jobs
// before settled jobs, newest first in each partition, capped at ten, then
// grouped lexically by the final CWD component. Dead terminals remain visible
// but intentionally have no attach target.
func Project(ctx context.Context, jobs []protocol.Job, live LiveFunc) []Group {
	active, terminal := make([]protocol.Job, 0, len(jobs)), make([]protocol.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.State.Terminal() {
			terminal = append(terminal, j)
		} else {
			active = append(active, j)
		}
	}
	newest := func(xs []protocol.Job) {
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].UpdatedAt.After(xs[j].UpdatedAt) })
	}
	newest(active)
	newest(terminal)
	ordered := append(active, terminal...)
	if len(ordered) > maxItems {
		ordered = ordered[:maxItems]
	}
	byGroup := map[string][]Item{}
	for _, j := range ordered {
		item := Item{ID: j.ID, Label: label(j.Prompt, j.ID), State: string(j.State)}
		if j.Terminal != nil && j.Terminal.Socket != "" && j.Terminal.Target != "" && live(ctx, *j.Terminal) {
			item.Attach = &Attach{Socket: j.Terminal.Socket, Session: session(j.Terminal.Target)}
		}
		group := workspace(j.CWD)
		byGroup[group] = append(byGroup[group], item)
	}
	labels := make([]string, 0, len(byGroup))
	for label := range byGroup {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	groups := make([]Group, 0, len(labels))
	for _, label := range labels {
		groups = append(groups, Group{Label: label, Items: byGroup[label]})
	}
	return groups
}

func label(prompt, id string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if s == "" {
		parts := strings.Split(id, "-")
		s = parts[len(parts)-1]
		r := []rune(s)
		if len(r) > 8 {
			s = string(r[len(r)-8:])
		}
	}
	r := []rune(s)
	if len(r) > 16 {
		s = string(r[:16])
	}
	return s
}
func workspace(cwd string) string {
	s := strings.Join(strings.Fields(filepath.Base(filepath.Clean(cwd))), " ")
	if s == "" || s == "." || s == string(filepath.Separator) {
		return "unknown"
	}
	return s
}
func session(target string) string {
	if i := strings.IndexByte(target, ':'); i >= 0 {
		return target[:i]
	}
	return target
}

// Server owns only snapshot invalidation. Generation changes exactly when the
// projected groups change and is monotonic for this adapter process.
type Server struct {
	source   JobSource
	live     LiveFunc
	wait     time.Duration
	mu       sync.Mutex
	snapshot Snapshot
	changed  chan struct{}
}

func New(source JobSource, live LiveFunc, wait time.Duration) *Server {
	return &Server{source: source, live: live, wait: wait, snapshot: Snapshot{Version: 1, Generation: 1, Groups: []Group{}}, changed: make(chan struct{})}
}
func (s *Server) Refresh(ctx context.Context) error {
	jobs, err := s.source.List(ctx, "")
	if err != nil {
		return err
	}
	groups := Project(ctx, jobs, s.live)
	encoded, _ := json.Marshal(groups)
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, _ := json.Marshal(s.snapshot.Groups)
	if string(encoded) != string(prior) {
		s.snapshot.Generation++
		s.snapshot.Groups = groups
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}
func (s *Server) Snapshot() Snapshot { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot }
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /v1/nav", s.nav)
	return m
}
func (s *Server) nav(w http.ResponseWriter, r *http.Request) {
	known, hasKnown := uint64(0), false
	if raw := r.URL.Query().Get("generation"); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			known, hasKnown = n, true
		} else {
			http.Error(w, "invalid generation", 400)
			return
		}
	}
	s.mu.Lock()
	snap, changed := s.snapshot, s.changed
	s.mu.Unlock()
	if hasKnown && known == snap.Generation {
		timer := time.NewTimer(s.wait)
		defer timer.Stop()
		select {
		case <-changed:
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		s.mu.Lock()
		snap = s.snapshot
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}
func (s *Server) RunRefresh(ctx context.Context, interval time.Duration) {
	_ = s.Refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Refresh(ctx)
		}
	}
}
