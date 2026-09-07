// Package herdr drives a Herdr session as Golem's run substrate over its
// newline-delimited JSON socket API (protocol 20). Golem speaks the socket
// directly rather than shelling out to the CLI: it needs long-lived event
// subscriptions and one process per state read does not scale.
//
// This file is the transport: request/response over one connection, typed
// errors, and a subscription reader. Everything Herdr-specific about Golem's
// job lifecycle lives in backend.go.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Protocol is the only Herdr socket protocol version this client claims to
// understand. Golem checks it at startup and refuses to bind to anything else.
const Protocol = 20

// Error is a Herdr error frame. Code is the machine-readable discriminator
// ("agent_not_found", "workspace_not_found", "agent_blocked", ...).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is a Herdr error frame with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type frame struct {
	ID     string          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// Conn is one nd-JSON connection to a Herdr session socket. Calls are
// serialised: a control connection carries no unsolicited frames, so a
// lock-step write/read is correct and keeps the client small.
type Conn struct {
	mu   sync.Mutex
	c    net.Conn
	r    *bufio.Reader
	next int
}

// Dial opens a connection to the session's Unix socket.
func Dial(ctx context.Context, socket string) (*Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c, r: bufio.NewReaderSize(c, 1<<20)}, nil
}

func (c *Conn) Close() error { return c.c.Close() }

// Call sends one request and decodes its result into out (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	c.next++
	id := fmt.Sprintf("golem-%d", c.next)
	body, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.c.SetDeadline(deadline)
		defer c.c.SetDeadline(time.Time{})
	}
	if _, err = c.c.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	for {
		line, readErr := c.r.ReadBytes('\n')
		if readErr != nil {
			return fmt.Errorf("herdr %s: %w", method, readErr)
		}
		var f frame
		if json.Unmarshal(line, &f) != nil {
			continue // never let an unknown frame shape kill the connection
		}
		if f.Event != "" || f.ID != id {
			continue // out-of-band frame; keep reading for our response
		}
		if f.Error != nil {
			return f.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(f.Result, out)
	}
}

// SubscriptionEvent is one pushed event line. Herdr's envelope carries no
// timestamp and no sequence number, so ReceivedAt (golemd's own clock) is the
// only honest time available; see docs/herdr-rebase.md §3.3.
type SubscriptionEvent struct {
	Event      string
	Data       json.RawMessage
	ReceivedAt time.Time
}

// Subscribe registers subscriptions on this connection and returns a channel
// of pushed events. The connection belongs to the subscription afterwards and
// must not be used for further calls. The channel closes when the connection
// drops or ctx ends; the caller then re-snapshots (there is no resumable
// cursor in protocol 20).
func (c *Conn) Subscribe(ctx context.Context, subs []map[string]any) (<-chan SubscriptionEvent, error) {
	if err := c.Call(ctx, "events.subscribe", map[string]any{"subscriptions": subs}, nil); err != nil {
		return nil, err
	}
	out := make(chan SubscriptionEvent, 32)
	go func() {
		defer close(out)
		defer c.Close()
		go func() {
			<-ctx.Done()
			_ = c.c.Close()
		}()
		for {
			line, err := c.r.ReadBytes('\n')
			if err != nil {
				return
			}
			received := time.Now().UTC()
			var f frame
			if json.Unmarshal(line, &f) != nil || f.Event == "" {
				continue
			}
			select {
			case out <- SubscriptionEvent{Event: f.Event, Data: f.Data, ReceivedAt: received}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- typed results -------------------------------------------------------

type PaneInfo struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	AgentStatus string `json:"agent_status"`
}

type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

type AgentInfo struct {
	Name             *string `json:"name"`
	Agent            *string `json:"agent"`
	PaneID           string  `json:"pane_id"`
	WorkspaceID      string  `json:"workspace_id"`
	TabID            string  `json:"tab_id"`
	AgentStatus      string  `json:"agent_status"`
	InteractiveReady bool    `json:"interactive_ready"`
	LaunchPending    bool    `json:"launch_pending"`
	CWD              *string `json:"cwd"`
}

type pong struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Ping returns the server version and protocol version.
func (c *Conn) Ping(ctx context.Context) (string, int, error) {
	var p pong
	if err := c.Call(ctx, "ping", map[string]any{}, &p); err != nil {
		return "", 0, err
	}
	return p.Version, p.Protocol, nil
}

// WorkspaceCreate makes a workspace whose root pane runs in cwd with env. Env
// applies to the launched process only; secrets belong here, never in argv
// (pane argv is visible in layout.export and pane.process_info).
func (c *Conn) WorkspaceCreate(ctx context.Context, cwd, label string, env map[string]string) (WorkspaceInfo, PaneInfo, error) {
	var out struct {
		Workspace WorkspaceInfo `json:"workspace"`
		RootPane  PaneInfo      `json:"root_pane"`
	}
	params := map[string]any{"cwd": cwd, "label": label, "focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	err := c.Call(ctx, "workspace.create", params, &out)
	return out.Workspace, out.RootPane, err
}

// AgentStart launches a supported agent kind in an existing pane. It returns
// only once Herdr has detected that agent and considers it interactive-ready.
func (c *Conn) AgentStart(ctx context.Context, name, kind, paneID string, args []string, timeoutMS int) (AgentInfo, error) {
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	params := map[string]any{"name": name, "kind": kind, "pane_id": paneID}
	if len(args) > 0 {
		params["args"] = args
	}
	if timeoutMS > 0 {
		params["timeout_ms"] = timeoutMS
	}
	err := c.Call(ctx, "agent.start", params, &out)
	return out.Agent, err
}

// AgentPrompt submits text plus Enter atomically, honouring the pane's live
// bracketed-paste mode. It fails with agent_blocked at an approval dialog.
func (c *Conn) AgentPrompt(ctx context.Context, target, text string) error {
	return c.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
}

type ReadResult struct {
	PaneID    string `json:"pane_id"`
	Source    string `json:"source"`
	Text      string `json:"text"`
	Revision  uint64 `json:"revision"`
	Truncated bool   `json:"truncated"`
}

// AgentRead passively reads a bounded terminal snapshot through the live agent
// identity. Detection is plain text and is the same screen source Herdr uses
// for manifest classification.
func (c *Conn) AgentRead(ctx context.Context, target, source string, lines int) (ReadResult, error) {
	var out struct {
		Read ReadResult `json:"read"`
	}
	params := map[string]any{"target": target, "source": source, "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	err := c.Call(ctx, "agent.read", params, &out)
	return out.Read, err
}

// AgentSendKeys sends validated logical keys ("esc", "ctrl+c").
func (c *Conn) AgentSendKeys(ctx context.Context, target string, keys ...string) error {
	return c.Call(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, nil)
}

// AgentGet resolves a live agent by name or by the pane hosting it. A missing
// agent is (zero, false, nil): absence is a fact, not a transport failure.
func (c *Conn) AgentGet(ctx context.Context, target string) (AgentInfo, bool, error) {
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	err := c.Call(ctx, "agent.get", map[string]any{"target": target}, &out)
	if IsCode(err, "agent_not_found") {
		return AgentInfo{}, false, nil
	}
	return out.Agent, err == nil, err
}

// AgentList is the reconcile and restart-adoption read.
func (c *Conn) AgentList(ctx context.Context) ([]AgentInfo, error) {
	var out struct {
		Agents []AgentInfo `json:"agents"`
	}
	err := c.Call(ctx, "agent.list", map[string]any{}, &out)
	return out.Agents, err
}

// WorkspaceGet reports whether a workspace still exists.
func (c *Conn) WorkspaceGet(ctx context.Context, id string) (WorkspaceInfo, bool, error) {
	var out struct {
		Workspace WorkspaceInfo `json:"workspace"`
	}
	err := c.Call(ctx, "workspace.get", map[string]any{"workspace_id": id}, &out)
	if IsCode(err, "workspace_not_found") {
		return WorkspaceInfo{}, false, nil
	}
	return out.Workspace, err == nil, err
}

// WorkspaceClose closes a workspace and every pane in it. A successful
// response is not by itself evidence that descendants died: callers must
// verify absence (see Backend.Cancel).
func (c *Conn) WorkspaceClose(ctx context.Context, id string) error {
	err := c.Call(ctx, "workspace.close", map[string]any{"workspace_id": id}, nil)
	if IsCode(err, "workspace_not_found") {
		return nil
	}
	return err
}

// WorkspaceList is the operator-visible inventory Golem's smoke test asserts.
func (c *Conn) WorkspaceList(ctx context.Context) ([]WorkspaceInfo, error) {
	var out struct {
		Workspaces []WorkspaceInfo `json:"workspaces"`
	}
	err := c.Call(ctx, "workspace.list", map[string]any{}, &out)
	return out.Workspaces, err
}
