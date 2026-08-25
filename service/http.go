package service

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gisikw/golem/artifacts"
	"github.com/gisikw/golem/protocol"
)

type API struct {
	Store        *Store
	Logger       *slog.Logger
	Capabilities protocol.Capabilities
	Workspaces   *WorkspaceResolver
	PiProviders  map[string]bool
	ArtifactRoot string
}

func (a API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		output(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.Ready(r.Context()); err != nil {
			failure(w, err, http.StatusServiceUnavailable)
			return
		}
		output(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	m.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		output(w, http.StatusOK, a.Capabilities)
	})
	m.HandleFunc("POST /v1/jobs", a.create)
	m.HandleFunc("GET /v1/jobs", a.list)
	m.HandleFunc("GET /v1/jobs/{id}", a.get)
	m.HandleFunc("GET /v1/jobs/{id}/artifacts", a.listArtifacts)
	m.HandleFunc("GET /v1/jobs/{id}/artifacts/{path...}", a.getArtifact)
	m.HandleFunc("POST /v1/jobs/{id}/cancel", a.cancel)
	m.HandleFunc("POST /v1/jobs/{id}/reap", a.reap)
	m.HandleFunc("POST /v1/jobs/{id}/answer", a.answer)
	m.HandleFunc("POST /v1/jobs/{id}/steer", a.steer)
	m.HandleFunc("POST /v1/jobs/poll", a.poll)
	m.HandleFunc("POST /v1/events", a.events)
	m.HandleFunc("GET /v1/events", a.streamEvents)
	return a.logging(artifactPathGuard(m))
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
func output(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, err error, status int) {
	output(w, status, map[string]string{"error": err.Error()})
}
func (a API) create(w http.ResponseWriter, r *http.Request) {
	var x protocol.CreateJob
	if err := decode(w, r, &x); err != nil {
		failure(w, err, 400)
		return
	}
	harness, ok := a.Capabilities.Harnesses[string(x.Harness)]
	if !ok {
		failure(w, fmt.Errorf("harness %q is not configured", x.Harness), http.StatusUnprocessableEntity)
		return
	}
	if x.Model != "" {
		allowed := false
		for _, model := range harness.Models {
			if model == x.Model {
				allowed = true
				break
			}
		}
		if !allowed {
			failure(w, fmt.Errorf("model %q is not configured for harness %q", x.Model, x.Harness), http.StatusUnprocessableEntity)
			return
		}
	}
	if x.Harness == protocol.HarnessPi && x.Model != "" {
		provider, _, ok := strings.Cut(x.Model, "/")
		if !ok || !a.PiProviders[provider] {
			failure(w, fmt.Errorf("pi model %q has no configured provider", x.Model), http.StatusUnprocessableEntity)
			return
		}
	}
	if x.Workspace != nil {
		if x.CWD != "" {
			failure(w, errors.New("dispatch must not specify both workspace and cwd"), http.StatusUnprocessableEntity)
			return
		}
		if a.Workspaces == nil {
			failure(w, errors.New("workspace resolution is unavailable"), http.StatusUnprocessableEntity)
			return
		}
		resolved, err := a.Workspaces.Resolve(r.Context(), *x.Workspace)
		if err != nil {
			failure(w, err, http.StatusUnprocessableEntity)
			return
		}
		x.CWD, x.ResolvedWorkspace = resolved.Path, resolved
	}
	// Host is retained on the wire for compatibility and terminal display, but
	// ownership is always this daemon and cannot be selected by dispatchers.
	x.Host = a.Capabilities.Name
	j, err := a.Store.Create(r.Context(), x)
	if err != nil {
		failure(w, err, 400)
		return
	}
	a.publicJob(&j)
	output(w, 201, j)
}
func (a API) list(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.List(r.Context(), protocol.State(r.URL.Query().Get("state")))
	if err != nil {
		failure(w, err, 400)
		return
	}
	for i := range j {
		a.publicJob(&j[i])
	}
	output(w, 200, j)
}
func (a API) get(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		status := 500
		if errors.Is(err, sql.ErrNoRows) {
			status = 404
		}
		failure(w, err, status)
		return
	}
	a.publicJob(&j)
	output(w, 200, j)
}
func (a API) publicJob(j *protocol.Job) {
	if a.Capabilities.AttachPort == 0 || j.State.Terminal() || j.Activation != nil && j.Activation.Port != a.Capabilities.AttachPort {
		j.Activation = nil
	}
}
func (a API) cancel(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		failure(w, err, 400)
		return
	}
	a.publicJob(&j)
	output(w, 200, j)
}
func (a API) reap(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Reap(r.Context(), r.PathValue("id"))
	if err != nil {
		failure(w, err, 400)
		return
	}
	a.publicJob(&j)
	output(w, 200, j)
}
func (a API) answer(w http.ResponseWriter, r *http.Request) {
	var x protocol.Answer
	if err := decode(w, r, &x); err != nil {
		failure(w, err, 400)
		return
	}
	j, err := a.Store.Answer(r.Context(), r.PathValue("id"), x)
	if err != nil {
		failure(w, err, 400)
		return
	}
	a.publicJob(&j)
	output(w, 200, j)
}
func (a API) steer(w http.ResponseWriter, r *http.Request) {
	var x struct {
		Text string `json:"text"`
	}
	if err := decode(w, r, &x); err != nil {
		failure(w, err, http.StatusBadRequest)
		return
	}
	j, err := a.Store.Steer(r.Context(), r.PathValue("id"), protocol.Steer{Text: x.Text})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		} else if errors.Is(err, ErrSteerConflict) {
			status = http.StatusConflict
		}
		failure(w, err, status)
		return
	}
	a.publicJob(&j)
	output(w, http.StatusOK, j)
}
func (a API) poll(w http.ResponseWriter, r *http.Request) {
	var x protocol.PollRequest
	if r.ContentLength != 0 {
		if err := decode(w, r, &x); err != nil {
			failure(w, err, 400)
			return
		}
	}
	p, err := a.Store.Poll(r.Context())
	if err != nil {
		failure(w, err, 500)
		return
	}
	output(w, 200, p)
}
func (a API) events(w http.ResponseWriter, r *http.Request) {
	var x protocol.EventBatch
	if err := decode(w, r, &x); err != nil {
		failure(w, err, 400)
		return
	}
	if err := a.Store.Record(r.Context(), x); err != nil {
		failure(w, err, 409)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a API) streamEvents(w http.ResponseWriter, r *http.Request) {
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		var err error
		since, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || since < 0 {
			failure(w, errors.New("since must be a non-negative integer"), 400)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		failure(w, errors.New("streaming unsupported"), 500)
		return
	}
	wake, unsubscribe := a.Store.Subscribe()
	defer unsubscribe()
	jobID := r.URL.Query().Get("job")
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		events, err := a.Store.Events(r.Context(), since, jobID)
		if err != nil {
			return
		}
		for _, event := range events {
			body, _ := json.Marshal(event)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Kind, body)
			since = event.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

var errMalformedArtifactPath = errors.New("malformed artifact path")

func (a API) artifactDirectory(r *http.Request) (string, error) {
	job, err := a.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return "", err
	}
	if a.ArtifactRoot == "" || job.Artifacts.ID == "" || filepath.Base(job.Artifacts.ID) != job.Artifacts.ID || job.Artifacts.ID == "." || job.Artifacts.ID == ".." {
		return "", errMalformedArtifactPath
	}
	realRoot, err := filepath.EvalSymlinks(a.ArtifactRoot)
	if err != nil {
		return "", err
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return "", err
	}
	directory, err := filepath.EvalSymlinks(filepath.Join(realRoot, job.Artifacts.ID))
	if err != nil {
		return "", err
	}
	directory, err = filepath.Abs(directory)
	if err != nil || !pathWithin(realRoot, directory) || directory == realRoot {
		return "", errMalformedArtifactPath
	}
	return directory, nil
}

func (a API) listArtifacts(w http.ResponseWriter, r *http.Request) {
	directory, err := a.artifactDirectory(r)
	if err != nil {
		artifactFailure(w, err)
		return
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		artifactFailure(w, err)
		return
	}
	output(w, http.StatusOK, artifacts.List(directory))
}

func (a API) getArtifact(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if malformedArtifactRelative(rel) {
		failure(w, errMalformedArtifactPath, http.StatusBadRequest)
		return
	}
	directory, err := a.artifactDirectory(r)
	if err != nil {
		artifactFailure(w, err)
		return
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(directory, filepath.FromSlash(rel)))
	if err != nil {
		artifactFailure(w, err)
		return
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil || !pathWithin(directory, candidate) || candidate == directory {
		failure(w, errMalformedArtifactPath, http.StatusBadRequest)
		return
	}
	file, err := os.Open(candidate)
	if err != nil {
		artifactFailure(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		artifactFailure(w, err)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func malformedArtifactRelative(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) || strings.Contains(rel, `\`) {
		return true
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return true
		}
	}
	return false
}

// artifactPathGuard runs before ServeMux's path-cleaning redirects so raw dot,
// dot-dot, doubled-slash, and encoded absolute attempts receive JSON 400s
// rather than being canonicalized into a different resource.
func artifactPathGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if r.Method == http.MethodGet && strings.HasPrefix(escaped, "/v1/jobs/") {
			if at := strings.Index(escaped, "/artifacts/"); at >= 0 {
				rel, err := url.PathUnescape(escaped[at+len("/artifacts/"):])
				if err != nil || malformedArtifactRelative(rel) {
					failure(w, errMalformedArtifactPath, http.StatusBadRequest)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func artifactFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, errMalformedArtifactPath) {
		failure(w, errMalformedArtifactPath, http.StatusBadRequest)
		return
	}
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
		failure(w, errors.New("artifact not found"), http.StatusNotFound)
		return
	}
	failure(w, err, http.StatusInternalServerError)
}

// BearerAuth enforces configured TCP credentials. Every configured candidate
// is checked with crypto/subtle; a successful match does not short-circuit the
// remaining candidates.
func BearerAuth(tokens []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Authorization")
		provided := ""
		if strings.HasPrefix(raw, "Bearer ") {
			provided = strings.TrimPrefix(raw, "Bearer ")
		}
		matched := 0
		for _, token := range tokens {
			matched |= subtle.ConstantTimeCompare([]byte(provided), []byte(token))
		}
		if provided == "" || matched != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="golemd"`)
			failure(w, errors.New("bearer token required"), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a API) logging(next http.Handler) http.Handler {
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("request", "component", "agent-service", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
