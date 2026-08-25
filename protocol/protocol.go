// Package protocol is the canonical wire contract shared by the agent service,
// supervisors, harness adapters, and clients. JSON names are API-stable.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type State string

const (
	Pending    State = "pending"
	Assigned   State = "assigned"
	Starting   State = "starting"
	Running    State = "running"
	Blocked    State = "blocked"
	Cancelling State = "cancelling"
	Done       State = "done"
	Failed     State = "failed"
	Cancelled  State = "cancelled"
	Timeout    State = "timeout"
)

var terminal = map[State]bool{Done: true, Failed: true, Cancelled: true, Timeout: true}

func (s State) Terminal() bool { return terminal[s] }
func (s State) Valid() bool {
	switch s {
	case Pending, Assigned, Starting, Running, Blocked, Cancelling, Done, Failed, Cancelled, Timeout:
		return true
	default:
		return false
	}
}

// CanTransition validates lifecycle ordering only. A terminal transition must
// additionally carry a durable settlement, enforced by ValidateTransition and
// by the service transaction which stores the event and settlement together.
func CanTransition(from, to State, hasSettlement bool) bool {
	if from == to && !to.Terminal() {
		return true // repeated observations are harmless
	}
	if from.Terminal() {
		return from == to && hasSettlement
	}
	if to.Terminal() {
		return hasSettlement
	}
	switch from {
	case Pending:
		return to == Assigned || to == Cancelling
	case Assigned:
		return to == Starting || to == Cancelling
	case Starting:
		return to == Running || to == Blocked || to == Cancelling
	case Running:
		return to == Blocked || to == Cancelling
	case Blocked:
		return to == Running || to == Cancelling
	case Cancelling:
		return false
	default:
		return false
	}
}

func ValidateTransition(from, to State, hasSettlement bool) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("unknown lifecycle transition %q -> %q", from, to)
	}
	if to.Terminal() && !hasSettlement {
		return errors.New("terminal state requires durable settlement")
	}
	if !CanTransition(from, to, hasSettlement) {
		return fmt.Errorf("illegal lifecycle transition %s -> %s", from, to)
	}
	return nil
}

type HarnessKind string

type HarnessCapability struct {
	Models []string `json:"models"`
}

type ProjectCapability struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type Capabilities struct {
	Name         string                       `json:"name"`
	Version      string                       `json:"version"`
	Harnesses    map[string]HarnessCapability `json:"harnesses"`
	Projects     []ProjectCapability          `json:"projects"`
	CloneEnabled bool                         `json:"clone_enabled"`
	AttachPort   int                          `json:"attach_port"`
}

const (
	HarnessPi     HarnessKind = "pi"
	HarnessClaude HarnessKind = "claude"
	HarnessCodex  HarnessKind = "codex"
	HarnessFake   HarnessKind = "fake"
)

type ArtifactRequest struct {
	RetentionDays int               `json:"retention_days,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

type ArtifactMetadata struct {
	// ID is a service-assigned logical identifier. Supervisors resolve it below
	// their host-local artifact root; filesystem paths never cross the wire.
	ID            string            `json:"id"`
	RetentionDays int               `json:"retention_days,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Directory     string            `json:"-"` // host-local, supervisor-owned
}

// Job identity and request fields are immutable after creation. Host, State,
// Question, Settlement, and timestamps are registry-owned reconciliation data.
type TerminalEndpoint struct {
	Host   string `json:"host"`
	Socket string `json:"socket"`
	Target string `json:"target"`
}

type Activation struct {
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

// WorkspaceSelector identifies a provisioned project or managed clone.
// Worktree is the durable resume key within that workspace.
type WorkspaceSelector struct {
	Project  string `json:"project,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Worktree string `json:"worktree"`
}

// ResolvedWorkspace records the accepted selector and its daemon-resolved path.
type ResolvedWorkspace struct {
	Project  string `json:"project,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Worktree string `json:"worktree"`
	Path     string `json:"path"`
}

type Job struct {
	ID              string             `json:"id"`
	IdempotencyKey  string             `json:"idempotency_key"`
	Harness         HarnessKind        `json:"harness"`
	Model           string             `json:"model,omitempty"`
	CWD             string             `json:"cwd"`
	Workspace       *ResolvedWorkspace `json:"workspace,omitempty"`
	Prompt          string             `json:"prompt"`
	Artifacts       ArtifactMetadata   `json:"artifacts"`
	Host            string             `json:"host"`
	State           State              `json:"state"`
	CancelRequested bool               `json:"cancel_requested,omitempty"`
	ReapRequested   bool               `json:"reap_requested,omitempty"`
	Question        *BlockedQuestion   `json:"question,omitempty"`
	LastProgress    *Progress          `json:"last_progress,omitempty"`
	Settlement      *Settlement        `json:"settlement,omitempty"`
	Terminal        *TerminalEndpoint  `json:"terminal,omitempty"`
	Activation      *Activation        `json:"activation,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type CreateJob struct {
	IdempotencyKey    string             `json:"idempotency_key"`
	Harness           HarnessKind        `json:"harness"`
	Model             string             `json:"model,omitempty"`
	CWD               string             `json:"cwd,omitempty"`
	Workspace         *WorkspaceSelector `json:"workspace,omitempty"`
	ResolvedWorkspace *ResolvedWorkspace `json:"-"`
	Prompt            string             `json:"prompt"`
	Artifacts         ArtifactRequest    `json:"artifacts,omitempty"`
	Host              string             `json:"host,omitempty"`
}

type Assignment struct {
	Job          Job       `json:"job"`
	DesiredState State     `json:"desired_state"`
	LeaseID      string    `json:"lease_id,omitempty"`
	AssignedAt   time.Time `json:"assigned_at,omitempty"`
}

type Progress struct {
	ID      string          `json:"id"`
	JobID   string          `json:"job_id"`
	At      time.Time       `json:"at"`
	Message string          `json:"message,omitempty"`
	Percent *float64        `json:"percent,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"` // harness-owned opaque JSON
}

type BlockedQuestion struct {
	ID      string          `json:"id"`
	Prompt  string          `json:"prompt"`
	Options []string        `json:"options,omitempty"` // suggested answers, operator-facing
	At      time.Time       `json:"at"`
	Detail  json.RawMessage `json:"detail,omitempty"`
	Answer  *Answer         `json:"answer,omitempty"`
}

type Answer struct {
	IdempotencyKey string          `json:"idempotency_key"`
	QuestionID     string          `json:"question_id"`
	Text           string          `json:"text"`
	At             time.Time       `json:"at,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros,omitempty"`
}

type ArtifactRef struct {
	Name       string    `json:"-"` // legacy adapter label; Path is the wire identity
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
	Digest     string    `json:"-"`
}

type ArtifactListing struct {
	Artifacts          []ArtifactRef `json:"artifacts"`
	ArtifactsTruncated bool          `json:"artifacts_truncated,omitempty"`
}

type WorktreeSettlement struct {
	Name  string `json:"name"`
	Head  string `json:"head,omitempty"`
	Dirty bool   `json:"dirty"`
}

type Settlement struct {
	ID    string `json:"id,omitempty"`
	JobID string `json:"job_id,omitempty"`
	State State  `json:"state"`
	// Verdict and Summary retain the pre-Phase-3 Go names. State and Summary
	// are the public settlement state and bounded harness verdict respectively.
	Verdict            State               `json:"-"`
	Summary            string              `json:"verdict,omitempty"`
	ExitStatus         *int                `json:"exit_status,omitempty"`
	Usage              Usage               `json:"usage"`
	Artifacts          []ArtifactRef       `json:"artifacts,omitempty"`
	ArtifactsTruncated bool                `json:"artifacts_truncated,omitempty"`
	Worktree           *WorktreeSettlement `json:"worktree,omitempty"`
	Detail             json.RawMessage     `json:"detail,omitempty"`
	At                 time.Time           `json:"at"`
}

func (s Settlement) MarshalJSON() ([]byte, error) {
	state := s.State
	if state == "" && s.Verdict.Terminal() {
		state = s.Verdict
	}
	verdict := s.Summary
	if verdict == "" && s.Verdict != "" && s.Verdict != state {
		verdict = string(s.Verdict)
	}
	type wire struct {
		ID                 string              `json:"id,omitempty"`
		JobID              string              `json:"job_id,omitempty"`
		State              State               `json:"state"`
		Verdict            string              `json:"verdict,omitempty"`
		ExitStatus         *int                `json:"exit_status,omitempty"`
		Usage              Usage               `json:"usage"`
		Artifacts          []ArtifactRef       `json:"artifacts,omitempty"`
		ArtifactsTruncated bool                `json:"artifacts_truncated,omitempty"`
		Worktree           *WorktreeSettlement `json:"worktree,omitempty"`
		Detail             json.RawMessage     `json:"detail,omitempty"`
		At                 time.Time           `json:"at"`
	}
	return json.Marshal(wire{s.ID, s.JobID, state, verdict, s.ExitStatus, s.Usage, s.Artifacts, s.ArtifactsTruncated, s.Worktree, s.Detail, s.At})
}

func (s *Settlement) UnmarshalJSON(data []byte) error {
	type wire struct {
		ID                 string              `json:"id"`
		JobID              string              `json:"job_id"`
		State              State               `json:"state"`
		Verdict            string              `json:"verdict"`
		ExitStatus         *int                `json:"exit_status"`
		Usage              Usage               `json:"usage"`
		Artifacts          []ArtifactRef       `json:"artifacts"`
		ArtifactsTruncated bool                `json:"artifacts_truncated"`
		Worktree           *WorktreeSettlement `json:"worktree"`
		Detail             json.RawMessage     `json:"detail"`
		At                 time.Time           `json:"at"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = Settlement{ID: w.ID, JobID: w.JobID, State: w.State, Verdict: w.State, Summary: w.Verdict, ExitStatus: w.ExitStatus, Usage: w.Usage, Artifacts: w.Artifacts, ArtifactsTruncated: w.ArtifactsTruncated, Worktree: w.Worktree, Detail: w.Detail, At: w.At}
	return nil
}

type Event struct {
	Seq        int64            `json:"seq"`
	Kind       string           `json:"kind"`
	JobID      string           `json:"job_id"`
	State      State            `json:"state,omitempty"`
	Question   *BlockedQuestion `json:"question,omitempty"`
	Progress   *Progress        `json:"progress,omitempty"`
	Settlement *Settlement      `json:"settlement,omitempty"`
	At         time.Time        `json:"at"`
}

type ObservedEvent struct {
	ID         string            `json:"id"` // delivery idempotency key
	JobID      string            `json:"job_id"`
	State      State             `json:"state,omitempty"`
	Progress   *Progress         `json:"progress,omitempty"`
	Question   *BlockedQuestion  `json:"question,omitempty"`
	Settlement *Settlement       `json:"settlement,omitempty"`
	Terminal   *TerminalEndpoint `json:"terminal,omitempty"`
	Activation *Activation       `json:"activation,omitempty"`
	ObservedAt time.Time         `json:"observed_at,omitempty"`
}

type EventBatch struct {
	Host   string          `json:"host"`
	Events []ObservedEvent `json:"events"`
}

type PollRequest struct {
	Known map[string]State `json:"known,omitempty"`
}

type PollResponse struct {
	Assignments []Assignment `json:"assignments"`
	ServerTime  time.Time    `json:"server_time"`
}
