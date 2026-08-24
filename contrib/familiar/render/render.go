// Package render projects Golem jobs into Familiar's deliberately small,
// host-rendered semantic tree contract.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/golem/protocol"
)

const maxItems = 10
const RenderAPI = 1
const RenderTarget = "left-nav"

type Activation struct {
	Type    string `json:"type"`
	Socket  string `json:"socket"`
	Session string `json:"session"`
}

// Node is the entire render vocabulary. Tree and branch nodes have Children;
// item leaves have Label, Status, and an optional typed Activation. The host
// alone decides pixels, layout, colors, and input behavior.
type Node struct {
	Kind       string      `json:"kind"`
	ID         string      `json:"id"`
	Label      string      `json:"label,omitempty"`
	Status     string      `json:"status,omitempty"`
	Children   *[]Node     `json:"children,omitempty"`
	Activation *Activation `json:"activation,omitempty"`
}

type Document struct {
	RenderAPI int    `json:"render_api"`
	Revision  uint64 `json:"revision"`
	TTLMillis int64  `json:"ttl_ms"`
	Target    string `json:"target"`
	Content   Node   `json:"content"`
}

type JobSource interface {
	List(context.Context, protocol.State) ([]protocol.Job, error)
}
type LiveFunc func(context.Context, protocol.TerminalEndpoint) bool
type InvalidateFunc func(context.Context) error

// Callback sends the scoped host invalidation signal. The POST is empty: the
// host already knows which plugin owns the callback URL.
func Callback(url string, client *http.Client) InvalidateFunc {
	if url == "" {
		return nil
	}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode/100 != 2 {
			return fmt.Errorf("render invalidation callback returned %s", res.Status)
		}
		return nil
	}
}

// Project preserves the old Familiar sidebar ordering and labels: active jobs
// before settled jobs, newest first in each partition, capped at ten, then
// grouped lexically by the final CWD component. Dead terminals remain visible
// but intentionally have no activation target.
func Project(ctx context.Context, jobs []protocol.Job, live LiveFunc) Node {
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
	byBranch := map[string][]Node{}
	for _, j := range ordered {
		item := Node{Kind: "item", ID: "job:" + j.ID, Label: label(j.Prompt, j.ID), Status: string(j.State)}
		if j.Terminal != nil && j.Terminal.Socket != "" && j.Terminal.Target != "" && live(ctx, *j.Terminal) {
			item.Activation = &Activation{Type: "terminal", Socket: j.Terminal.Socket, Session: session(j.Terminal.Target)}
		}
		branch := workspace(j.CWD)
		byBranch[branch] = append(byBranch[branch], item)
	}
	labels := make([]string, 0, len(byBranch))
	for label := range byBranch {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	branches := make([]Node, 0, len(labels))
	for _, label := range labels {
		children := byBranch[label]
		branches = append(branches, Node{Kind: "branch", ID: "workspace:" + label, Label: label, Children: &children})
	}
	return Node{Kind: "tree", ID: "golem:jobs", Label: "agents", Children: &branches}
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

// Server maintains the cached render document. Revision changes exactly when
// the semantic tree changes. One invalidation is emitted per host-observed
// render, coalescing further changes until the host refetches.
type Server struct {
	source     JobSource
	live       LiveFunc
	invalidate InvalidateFunc
	mu         sync.Mutex
	doc        Document
	cacheFresh bool
}

func New(source JobSource, live LiveFunc, invalidate InvalidateFunc, ttl time.Duration) *Server {
	children := []Node{}
	root := Node{Kind: "tree", ID: "golem:jobs", Label: "agents", Children: &children}
	return &Server{source: source, live: live, invalidate: invalidate, doc: Document{RenderAPI: RenderAPI, Revision: 1, TTLMillis: ttl.Milliseconds(), Target: RenderTarget, Content: root}}
}
func (s *Server) Refresh(ctx context.Context) error {
	jobs, err := s.source.List(ctx, "")
	if err != nil {
		return err
	}
	root := Project(ctx, jobs, s.live)
	encoded, _ := json.Marshal(root)
	s.mu.Lock()
	prior, _ := json.Marshal(s.doc.Content)
	changed := string(encoded) != string(prior)
	poke := changed && s.cacheFresh && s.invalidate != nil
	if changed {
		s.doc.Revision++
		s.doc.Content = root
	}
	if poke {
		s.cacheFresh = false
	}
	s.mu.Unlock()
	if poke {
		// Delivery is advisory. Failure leaves TTL expiry as the fallback and
		// never makes refresh or the adapter fail.
		_ = s.invalidate(ctx)
	}
	return nil
}
func (s *Server) Document() Document { s.mu.Lock(); defer s.mu.Unlock(); return s.doc }
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /v1/render", s.render)
	return m
}
func (s *Server) render(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	doc := s.doc
	s.cacheFresh = true
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
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
