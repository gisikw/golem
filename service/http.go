package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gisikw/golem/protocol"
)

type API struct {
	Store  *Store
	Logger *slog.Logger
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
	m.HandleFunc("POST /v1/jobs", a.create)
	m.HandleFunc("GET /v1/jobs", a.list)
	m.HandleFunc("GET /v1/jobs/{id}", a.get)
	m.HandleFunc("POST /v1/jobs/{id}/cancel", a.cancel)
	m.HandleFunc("POST /v1/jobs/{id}/reap", a.reap)
	m.HandleFunc("POST /v1/jobs/{id}/answer", a.answer)
	m.HandleFunc("POST /v1/hosts/{host}/poll", a.poll)
	m.HandleFunc("POST /v1/events", a.events)
	return a.logging(m)
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
	j, err := a.Store.Create(r.Context(), x)
	if err != nil {
		failure(w, err, 400)
		return
	}
	output(w, 201, j)
}
func (a API) list(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.List(r.Context(), protocol.State(r.URL.Query().Get("state")))
	if err != nil {
		failure(w, err, 400)
		return
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
	output(w, 200, j)
}
func (a API) cancel(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		failure(w, err, 400)
		return
	}
	output(w, 200, j)
}
func (a API) reap(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Reap(r.Context(), r.PathValue("id"))
	if err != nil {
		failure(w, err, 400)
		return
	}
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
	output(w, 200, j)
}
func (a API) poll(w http.ResponseWriter, r *http.Request) {
	var x protocol.PollRequest
	if r.ContentLength != 0 {
		if err := decode(w, r, &x); err != nil {
			failure(w, err, 400)
			return
		}
	}
	p, err := a.Store.Poll(r.Context(), r.PathValue("host"))
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
