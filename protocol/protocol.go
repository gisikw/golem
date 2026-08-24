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
type IsolationPolicy string

const (
	HarnessPi         HarnessKind     = "pi"
	HarnessClaude     HarnessKind     = "claude"
	HarnessCodex      HarnessKind     = "codex"
	HarnessFake       HarnessKind     = "fake"
	IsolationNone     IsolationPolicy = "none"
	IsolationWorktree IsolationPolicy = "worktree"
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

// ProviderConfig is the opaque, single-provider connection descriptor the
// dispatching client resolves at dispatch time so the worker's pi
// boots with EXACTLY the dispatched model+provider available and selected —
// nothing else. It is written by the adapter into the worker's isolated per-job
// pi dir (models.json + settings defaults), never logged or emitted in
// events/settlements.
//
// SECURITY: this rides the job payload, which transits the service DB. The
// extension forwards ApiKey ONLY as an unresolved reference (pi's config-value
// form: "!cmd" runs a host-local command, "$ENV"/"${ENV}" interpolate env), so
// the DB stores a reference string resolved on the worker host at runtime, not a
// plaintext secret. Built-in/login providers carry no key here at all: they set
// Builtin+CopyAuth and the adapter copies auth.json into the private per-job dir
// (0700/0600). See DECISIONS.md #20 for the credential-exposure note and
// the remote-host caveat.
type ProviderConfig struct {
	// Provider and Model are the canonical ids used for defaults and for scoping
	// the worker to a single model (enabledModels "provider/model").
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Builtin marks a provider pi knows natively (credentials in auth.json). No
	// models.json is written; the adapter relies on the copied auth.json instead.
	Builtin bool `json:"builtin,omitempty"`
	// CopyAuth requests the adapter copy the source profile's auth.json into the
	// worker dir (needed for oauth/stored-credential built-in providers).
	CopyAuth bool `json:"copy_auth,omitempty"`
	// ModelsJSON is the complete single-provider pi models.json object written
	// verbatim (keyed by provider id, carrying baseUrl/apiKey-ref/api/models).
	// Empty for Builtin providers.
	ModelsJSON json.RawMessage `json:"models_json,omitempty"`
}

type Job struct {
	ID              string            `json:"id"`
	IdempotencyKey  string            `json:"idempotency_key"`
	Harness         HarnessKind       `json:"harness"`
	Model           string            `json:"model,omitempty"`
	ProviderConfig  *ProviderConfig   `json:"provider_config,omitempty"`
	CWD             string            `json:"cwd"`
	Isolation       IsolationPolicy   `json:"isolation"`
	Prompt          string            `json:"prompt"`
	Artifacts       ArtifactMetadata  `json:"artifacts"`
	Host            string            `json:"host"`
	State           State             `json:"state"`
	CancelRequested bool              `json:"cancel_requested,omitempty"`
	ReapRequested   bool              `json:"reap_requested,omitempty"`
	Question        *BlockedQuestion  `json:"question,omitempty"`
	LastProgress    *Progress         `json:"last_progress,omitempty"`
	Settlement      *Settlement       `json:"settlement,omitempty"`
	Terminal        *TerminalEndpoint `json:"terminal,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type CreateJob struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Harness        HarnessKind     `json:"harness"`
	Model          string          `json:"model,omitempty"`
	ProviderConfig *ProviderConfig `json:"provider_config,omitempty"`
	CWD            string          `json:"cwd"`
	Isolation      IsolationPolicy `json:"isolation,omitempty"`
	Prompt         string          `json:"prompt"`
	Artifacts      ArtifactRequest `json:"artifacts,omitempty"`
	Host           string          `json:"host"`
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
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	CostMicros   int64 `json:"cost_micros,omitempty"`
}

type ArtifactRef struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}

type Settlement struct {
	ID        string          `json:"id"`
	JobID     string          `json:"job_id"`
	Verdict   State           `json:"verdict"`
	Summary   string          `json:"summary,omitempty"`
	Usage     Usage           `json:"usage,omitempty"`
	Artifacts []ArtifactRef   `json:"artifacts,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	At        time.Time       `json:"at"`
}

type ObservedEvent struct {
	ID         string            `json:"id"` // delivery idempotency key
	JobID      string            `json:"job_id"`
	State      State             `json:"state,omitempty"`
	Progress   *Progress         `json:"progress,omitempty"`
	Question   *BlockedQuestion  `json:"question,omitempty"`
	Settlement *Settlement       `json:"settlement,omitempty"`
	Terminal   *TerminalEndpoint `json:"terminal,omitempty"`
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
