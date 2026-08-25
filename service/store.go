// Package service implements the durable global semantic job registry.
package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/gisikw/golem/protocol"
	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB
	mu       sync.Mutex
	watchers map[chan struct{}]struct{}
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, watchers: make(map[chan struct{}]struct{})}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, idem TEXT UNIQUE NOT NULL, host TEXT NOT NULL,
 state TEXT NOT NULL, body BLOB NOT NULL, created TEXT NOT NULL, updated TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state);
CREATE TABLE IF NOT EXISTS events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, delivery_id TEXT UNIQUE NOT NULL,
 job_id TEXT NOT NULL REFERENCES jobs(id), kind TEXT NOT NULL, body BLOB NOT NULL, created TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_job_seq ON events(job_id,seq);
CREATE TABLE IF NOT EXISTS deliveries (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS settlements (
 id TEXT PRIMARY KEY, job_id TEXT UNIQUE NOT NULL REFERENCES jobs(id), body BLOB NOT NULL, created TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS answers (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), body BLOB NOT NULL, created TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS steers (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL REFERENCES jobs(id), body BLOB NOT NULL, created TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS steers_job_seq ON steers(job_id,seq);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error                    { return s.db.Close() }
func (s *Store) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Create(ctx context.Context, c protocol.CreateJob) (protocol.Job, error) {
	if c.IdempotencyKey == "" || c.Harness == "" || c.Prompt == "" || c.CWD == "" {
		return protocol.Job{}, errors.New("idempotency_key, harness, cwd, and prompt are required")
	}
	// Host remains in the persisted schema to avoid a destructive migration, but
	// it is identity metadata rather than an assignment key.
	if c.Host == "" {
		c.Host = "local"
	}
	now := time.Now().UTC()
	id, err := newID("job")
	if err != nil {
		return protocol.Job{}, err
	}
	// Artifact identity is global semantic metadata; host-local paths are
	// deliberately resolved by the assigned supervisor.
	artifacts := protocol.ArtifactMetadata{ID: id, RetentionDays: c.Artifacts.RetentionDays, Labels: c.Artifacts.Labels}
	j := protocol.Job{ID: id, IdempotencyKey: c.IdempotencyKey, Harness: c.Harness, Model: c.Model, CWD: c.CWD, Workspace: c.ResolvedWorkspace, Prompt: c.Prompt, Artifacts: artifacts, Host: c.Host, State: protocol.Assigned, CreatedAt: now, UpdatedAt: now}
	body, _ := json.Marshal(j)
	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		return protocol.Job{}, txErr
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,idem,host,state,body,created,updated) VALUES(?,?,?,?,?,?,?)`, j.ID, j.IdempotencyKey, j.Host, j.State, body, stamp(now), stamp(now))
	if err == nil {
		err = insertEvent(ctx, tx, j.ID+"-created", protocol.Event{Kind: "job.created", JobID: j.ID, State: j.State, At: now})
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err == nil {
		s.signal()
		return j, nil
	}
	old, getErr := s.getByIdem(ctx, c.IdempotencyKey)
	if getErr != nil {
		return protocol.Job{}, err
	}
	artifactMismatch := c.Artifacts.RetentionDays != old.Artifacts.RetentionDays || !reflect.DeepEqual(c.Artifacts.Labels, old.Artifacts.Labels)
	if old.Harness != c.Harness || old.Model != c.Model || old.CWD != c.CWD || old.Prompt != c.Prompt || old.Host != c.Host || artifactMismatch || !reflect.DeepEqual(old.Workspace, c.ResolvedWorkspace) {
		return protocol.Job{}, errors.New("idempotency key already used for a different request")
	}
	return old, nil
}

func (s *Store) getByIdem(ctx context.Context, key string) (protocol.Job, error) {
	var body []byte
	if err := s.db.QueryRowContext(ctx, "SELECT body FROM jobs WHERE idem=?", key).Scan(&body); err != nil {
		return protocol.Job{}, err
	}
	var j protocol.Job
	return j, json.Unmarshal(body, &j)
}

func (s *Store) Get(ctx context.Context, id string) (protocol.Job, error) {
	var body []byte
	if err := s.db.QueryRowContext(ctx, "SELECT body FROM jobs WHERE id=?", id).Scan(&body); err != nil {
		return protocol.Job{}, err
	}
	var j protocol.Job
	return j, json.Unmarshal(body, &j)
}

func (s *Store) List(ctx context.Context, state protocol.State) ([]protocol.Job, error) {
	if state != "" && !state.Valid() {
		return nil, errors.New("invalid state filter")
	}
	query, args := "SELECT body FROM jobs", []any{}
	if state != "" {
		query += " WHERE state=?"
		args = append(args, state)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY created", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.Job{}
	for rows.Next() {
		var body []byte
		var j protocol.Job
		if err = rows.Scan(&body); err == nil {
			err = json.Unmarshal(body, &j)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) Cancel(ctx context.Context, id string) (protocol.Job, error) {
	return s.update(ctx, id, func(j *protocol.Job) error {
		if j.State.Terminal() || j.State == protocol.Cancelling {
			return nil
		}
		if err := protocol.ValidateTransition(j.State, protocol.Cancelling, false); err != nil {
			return err
		}
		j.CancelRequested, j.State = true, protocol.Cancelling
		return nil
	})
}

func (s *Store) Reap(ctx context.Context, id string) (protocol.Job, error) {
	return s.update(ctx, id, func(j *protocol.Job) error {
		if !j.State.Terminal() {
			return errors.New("only settled jobs can be reaped")
		}
		j.ReapRequested = true
		return nil
	})
}

func (s *Store) Answer(ctx context.Context, id string, a protocol.Answer) (protocol.Job, error) {
	if a.IdempotencyKey == "" || a.Text == "" {
		return protocol.Job{}, errors.New("idempotency_key and text are required")
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	return s.updateTx(ctx, id, func(tx *sql.Tx, j *protocol.Job) error {
		if j.Question == nil {
			return errors.New("job is not awaiting a question")
		}
		if a.QuestionID != "" && a.QuestionID != j.Question.ID {
			return errors.New("question mismatch")
		}
		// Canonicalize an omitted question ID so the durable idempotency record
		// still identifies the exact question answered.
		a.QuestionID = j.Question.ID
		body, _ := json.Marshal(a)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO answers(id,job_id,body,created) VALUES(?,?,?,?)`, a.IdempotencyKey, id, body, stamp(a.At))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			var priorJob string
			var priorBody []byte
			if err = tx.QueryRowContext(ctx, `SELECT job_id,body FROM answers WHERE id=?`, a.IdempotencyKey).Scan(&priorJob, &priorBody); err != nil {
				return err
			}
			var prior protocol.Answer
			if err = json.Unmarshal(priorBody, &prior); err != nil {
				return err
			}
			// At is server-defaulted and Detail is semantic request data too.
			if priorJob != id || prior.QuestionID != a.QuestionID || prior.Text != a.Text || !reflect.DeepEqual(prior.Detail, a.Detail) {
				return errors.New("answer idempotency key already used for a different request")
			}
			return nil
		}
		j.Question.Answer = &a
		if j.State == protocol.Blocked {
			j.State = protocol.Running
		}
		return nil
	})
}

var ErrSteerConflict = errors.New("job cannot receive steering input")

func (s *Store) Steer(ctx context.Context, id string, steer protocol.Steer) (protocol.Job, error) {
	if steer.Text == "" {
		return protocol.Job{}, errors.New("text is required")
	}
	steerID, err := newID("steer")
	if err != nil {
		return protocol.Job{}, err
	}
	steer.ID = steerID
	steer.At = time.Now().UTC()
	j, err := s.updateTx(ctx, id, func(tx *sql.Tx, j *protocol.Job) error {
		if j.State != protocol.Starting && j.State != protocol.Running {
			if j.State == protocol.Blocked {
				return fmt.Errorf("%w: blocked jobs must use answer", ErrSteerConflict)
			}
			return fmt.Errorf("%w in state %s", ErrSteerConflict, j.State)
		}
		body, _ := json.Marshal(steer)
		if _, insertErr := tx.ExecContext(ctx, `INSERT INTO steers(id,job_id,body,created) VALUES(?,?,?,?)`, steer.ID, id, body, stamp(steer.At)); insertErr != nil {
			return insertErr
		}
		j.Steers = append(j.Steers, steer)
		detail, _ := json.Marshal(map[string]string{"interaction": "steer", "status": "queued", "steer_id": steer.ID})
		progress := &protocol.Progress{ID: steer.ID + "-progress", JobID: id, At: steer.At, Message: "steering input queued for delivery", Detail: detail}
		return insertEvent(ctx, tx, steer.ID+"-progress", protocol.Event{Kind: "job.progress", JobID: id, Progress: progress, At: steer.At})
	})
	if err == nil {
		s.signal()
	}
	return j, err
}

func (s *Store) Poll(ctx context.Context) (protocol.PollResponse, error) {
	// Every non-terminal job belongs to this daemon. An explicit reap request is
	// retained just long enough to destroy its lingering tmux session.
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM jobs WHERE state NOT IN ('done','failed','cancelled','timeout') OR json_extract(body, '$.reap_requested')=1 ORDER BY created`)
	if err != nil {
		return protocol.PollResponse{}, err
	}
	defer rows.Close()
	r := protocol.PollResponse{Assignments: []protocol.Assignment{}, ServerTime: time.Now().UTC()}
	for rows.Next() {
		var body []byte
		var j protocol.Job
		if err = rows.Scan(&body); err == nil {
			err = json.Unmarshal(body, &j)
		}
		if err != nil {
			return r, err
		}
		r.Assignments = append(r.Assignments, protocol.Assignment{Job: j, DesiredState: j.State, AssignedAt: j.CreatedAt})
	}
	return r, rows.Err()
}

// Record atomically deduplicates each observed event and stores both settlement
// ownership and the resulting terminal job state. Partial batches roll back.
func (s *Store) Record(ctx context.Context, batch protocol.EventBatch) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ev := range batch.Events {
		if ev.ID == "" || ev.JobID == "" {
			return errors.New("event id and job id are required")
		}
		res, deliveryErr := tx.ExecContext(ctx, "INSERT OR IGNORE INTO deliveries(id) VALUES(?)", ev.ID)
		if deliveryErr != nil {
			return deliveryErr
		}
		inserted, _ := res.RowsAffected()
		if inserted == 0 {
			continue
		}
		var raw []byte
		if err = tx.QueryRowContext(ctx, "SELECT body FROM jobs WHERE id=?", ev.JobID).Scan(&raw); err != nil {
			return err
		}
		var j protocol.Job
		if err = json.Unmarshal(raw, &j); err != nil {
			return err
		}
		priorState := j.State
		next := ev.State
		settlementInserted := false
		if ev.Settlement != nil {
			if ev.Settlement.State == "" {
				ev.Settlement.State = ev.Settlement.Verdict
			}
			if ev.Settlement.Verdict == "" {
				ev.Settlement.Verdict = ev.Settlement.State
			}
			if ev.Settlement.ID == "" || ev.Settlement.JobID != j.ID || !ev.Settlement.State.Terminal() {
				return errors.New("invalid settlement")
			}
			var prior []byte
			err = tx.QueryRowContext(ctx, "SELECT body FROM settlements WHERE id=? OR job_id=?", ev.Settlement.ID, j.ID).Scan(&prior)
			if err == nil {
				var old protocol.Settlement
				_ = json.Unmarshal(prior, &old)
				if old.State == "" {
					old.State = old.Verdict
				}
				next, j.Settlement = old.State, &old // first durable settlement wins
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			} else {
				settleBody, _ := json.Marshal(ev.Settlement)
				if _, err = tx.ExecContext(ctx, `INSERT INTO settlements(id,job_id,body,created) VALUES(?,?,?,?)`, ev.Settlement.ID, j.ID, settleBody, stamp(ev.Settlement.At)); err != nil {
					return err
				}
				next, j.Settlement = ev.Settlement.State, ev.Settlement
				settlementInserted = true
			}
		}
		if ev.Settlement != nil && !settlementInserted && ev.State == "" && ev.Progress == nil && ev.Question == nil && ev.Terminal == nil && ev.Activation == nil {
			continue // first settlement is immutable; later attempts are true no-ops
		}
		if next != "" {
			if err = protocol.ValidateTransition(j.State, next, ev.Settlement != nil || j.Settlement != nil); err != nil {
				return err
			}
			j.State = next
		}
		if ev.Progress != nil {
			j.LastProgress = ev.Progress
		}
		if ev.Terminal != nil {
			if ev.Terminal.Host == "" || !filepath.IsAbs(ev.Terminal.Socket) || ev.Terminal.Target == "" {
				return errors.New("invalid terminal endpoint")
			}
			j.Terminal = ev.Terminal
		}
		if ev.Activation != nil {
			if ev.Activation.Type != "ssh" || ev.Activation.Host == "" || ev.Activation.Port < 1 || ev.Activation.Port > 65535 || ev.Activation.User != j.ID {
				return errors.New("invalid activation endpoint")
			}
			j.Activation = ev.Activation
		}
		if ev.Settlement != nil {
			j.Activation = nil // settled/dead panes are never remotely attachable
		}
		if ev.Question != nil {
			if j.State != protocol.Blocked {
				if err = protocol.ValidateTransition(j.State, protocol.Blocked, false); err != nil {
					return err
				}
			}
			j.Question, j.State = ev.Question, protocol.Blocked
		}
		j.UpdatedAt = time.Now().UTC()
		raw, _ = json.Marshal(j)
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state=?,body=?,updated=? WHERE id=?", j.State, raw, stamp(j.UpdatedAt), j.ID); err != nil {
			return err
		}
		public := []protocol.Event{}
		if ev.Progress != nil {
			public = append(public, protocol.Event{Kind: "job.progress", JobID: j.ID, Progress: ev.Progress, At: j.UpdatedAt})
		}
		if j.State != priorState {
			public = append(public, protocol.Event{Kind: "job.state", JobID: j.ID, State: j.State, At: j.UpdatedAt, Question: func() *protocol.BlockedQuestion {
				if j.State == protocol.Blocked {
					return j.Question
				}
				return nil
			}()})
		}
		if settlementInserted {
			public = append(public, protocol.Event{Kind: "job.settled", JobID: j.ID, Settlement: j.Settlement, At: j.UpdatedAt})
		}
		for n, event := range public {
			if err = insertEvent(ctx, tx, fmt.Sprintf("%s/%d", ev.ID, n), event); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err == nil {
		s.signal()
	}
	return err
}

// Events replays durable public events strictly after since.
func (s *Store) Events(ctx context.Context, since int64, jobID string) ([]protocol.Event, error) {
	query := "SELECT seq,body FROM events WHERE seq>?"
	args := []any{since}
	if jobID != "" {
		query += " AND job_id=?"
		args = append(args, jobID)
	}
	query += " ORDER BY seq"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.Event{}
	for rows.Next() {
		var seq int64
		var body []byte
		var event protocol.Event
		if err = rows.Scan(&seq, &body); err == nil {
			err = json.Unmarshal(body, &event)
		}
		if err != nil {
			return nil, err
		}
		event.Seq = seq
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.watchers, ch); s.mu.Unlock() }
}
func (s *Store) signal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func insertEvent(ctx context.Context, tx *sql.Tx, deliveryID string, event protocol.Event) error {
	body, _ := json.Marshal(event)
	_, err := tx.ExecContext(ctx, `INSERT INTO events(delivery_id,job_id,kind,body,created) VALUES(?,?,?,?,?)`, deliveryID, event.JobID, event.Kind, body, stamp(event.At))
	return err
}

func (s *Store) update(ctx context.Context, id string, f func(*protocol.Job) error) (protocol.Job, error) {
	return s.updateTx(ctx, id, func(_ *sql.Tx, j *protocol.Job) error { return f(j) })
}
func (s *Store) updateTx(ctx context.Context, id string, f func(*sql.Tx, *protocol.Job) error) (protocol.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Job{}, err
	}
	defer tx.Rollback()
	var body []byte
	if err = tx.QueryRowContext(ctx, "SELECT body FROM jobs WHERE id=?", id).Scan(&body); err != nil {
		return protocol.Job{}, err
	}
	var j protocol.Job
	if err = json.Unmarshal(body, &j); err != nil {
		return j, err
	}
	priorState := j.State
	if err = f(tx, &j); err != nil {
		return j, err
	}
	j.UpdatedAt = time.Now().UTC()
	if j.State != priorState {
		eventID, idErr := newID("event")
		if idErr != nil {
			return j, idErr
		}
		e := protocol.Event{Kind: "job.state", JobID: j.ID, State: j.State, At: j.UpdatedAt}
		if j.State == protocol.Blocked {
			e.Question = j.Question
		}
		if err = insertEvent(ctx, tx, eventID, e); err != nil {
			return j, err
		}
	}
	body, _ = json.Marshal(j)
	if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state=?,body=?,updated=? WHERE id=?", j.State, body, stamp(j.UpdatedAt), id); err == nil {
		err = tx.Commit()
	}
	if err == nil && j.State != priorState {
		s.signal()
	}
	return j, err
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b[:])), nil
}
