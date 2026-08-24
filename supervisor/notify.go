package supervisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gisikw/golem/protocol"
)

// Notifier promptly wakes the Golem operator when a job reaches a terminal
// state. Implementations MUST degrade gracefully: a delivery failure is logged
// by the caller and never affects the durable settlement.
type Notifier interface {
	Settled(ctx context.Context, job protocol.Job, s *protocol.Settlement) error
}

// notifySettlement is the best-effort courtesy wake. It swallows and logs all
// errors; the settlement is already durable.
func (s *Supervisor) notifySettlement(ctx context.Context, job protocol.Job, set *protocol.Settlement) {
	if s.Notify == nil || set == nil {
		return
	}
	if err := s.Notify.Settled(ctx, job, set); err != nil {
		s.log().Warn("settlement notification failed", "job", job.ID, "error", err)
	}
}

// settlementNotice is the neutral payload shared by both notification transports.
type settlementNotice struct {
	Priority int    `json:"priority"`
	Type     string `json:"type"`
	Summary  string `json:"summary"`
	Body     string `json:"body"`
	Source   string `json:"source"`
	ID       string `json:"id"`
	JobID    string `json:"job_id"`
	Verdict  string `json:"verdict"`
	Host     string `json:"host"`
}

// priority mirrors worklist/PROTOCOL.md subagent urgency: crash/timeout are more
// urgent (P1) than ordinary completion or cancellation (P2).
func noticeFor(host string, job protocol.Job, set *protocol.Settlement) settlementNotice {
	priority := 2
	switch set.Verdict {
	case protocol.Failed, protocol.Timeout:
		priority = 1
	}
	summary := fmt.Sprintf("agent %s: %s", job.ID, set.Verdict)
	body := set.Summary
	if body == "" {
		body = summary
	}
	return settlementNotice{
		Priority: priority,
		Type:     "notify",
		Summary:  summary,
		Body:     body,
		Source:   "agents",
		ID:       "subagent-" + job.ID + "-settlement",
		JobID:    job.ID,
		Verdict:  string(set.Verdict),
		Host:     host,
	}
}

// WorklistNotifier writes an atomic envelope into the operator worklist drop-box
// (integrations/pi/extensions/worklist incoming/), the designed subagent
// settlement channel. Cross-process, no daemon: the resident worklist extension
// drains it on its timer and surfaces it per attention policy.
type WorklistNotifier struct {
	Host string
	Dir  string // worklist root; incoming/ is created beneath it
}

func (n WorklistNotifier) Settled(_ context.Context, job protocol.Job, set *protocol.Settlement) error {
	notice := noticeFor(n.Host, job, set)
	incoming := filepath.Join(n.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(incoming, 0o700)
	// Stable id keeps drop-box drain idempotent (worklist dedupes on id).
	envelope, err := json.Marshal(map[string]any{
		"id":       notice.ID,
		"priority": notice.Priority,
		"type":     notice.Type,
		"summary":  notice.Summary,
		"body":     notice.Body,
		"source":   notice.Source,
	})
	if err != nil {
		return err
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	tmp := filepath.Join(incoming, "."+notice.ID+".tmp."+hex.EncodeToString(suffix[:]))
	if err := os.WriteFile(tmp, envelope, 0o600); err != nil {
		return err
	}
	// Atomic publish: the extension only ever sees a whole file.
	return os.Rename(tmp, filepath.Join(incoming, notice.ID+".json"))
}

// WebhookNotifier POSTs the settlement notice to a configured URL, for
// cross-host supervisors that cannot write the operator's local worklist dir.
type WebhookNotifier struct {
	Host   string
	URL    string
	Client *http.Client
}

func (n WebhookNotifier) Settled(ctx context.Context, job protocol.Job, set *protocol.Settlement) error {
	body, err := json.Marshal(noticeFor(n.Host, job, set))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := n.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("settlement webhook status %d", resp.StatusCode)
	}
	return nil
}

// Notifiers fans a settlement out to every configured transport. A failure in
// one does not prevent the others.
type Notifiers []Notifier

func (ns Notifiers) Settled(ctx context.Context, job protocol.Job, set *protocol.Settlement) error {
	var firstErr error
	for _, n := range ns {
		if n == nil {
			continue
		}
		if err := n.Settled(ctx, job, set); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
