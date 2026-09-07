package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

// fakeHerdr is a canned protocol-20 server: one nd-JSON frame in, one out. It
// records every method it saw so tests can assert the exact call sequence.
type fakeHerdr struct {
	t        *testing.T
	socket   string
	listener net.Listener

	mu      sync.Mutex
	calls   []call
	handler func(method string, params map[string]any) (any, *Error)
	pushes  chan map[string]any
}

type call struct {
	Method string
	Params map[string]any
}

func newFakeHerdr(t *testing.T, handler func(string, map[string]any) (any, *Error)) *fakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdrfake")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{t: t, socket: socket, listener: listener, handler: handler, pushes: make(chan map[string]any, 8)}
	go f.serve()
	t.Cleanup(func() { _ = listener.Close(); _ = os.RemoveAll(dir) })
	return f
}

func (f *fakeHerdr) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeHerdr) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, call{Method: req.Method, Params: req.Params})
		handler := f.handler
		f.mu.Unlock()

		result, herdrErr := handler(req.Method, req.Params)
		var body []byte
		if herdrErr != nil {
			body, _ = json.Marshal(map[string]any{"id": req.ID, "error": herdrErr})
		} else {
			body, _ = json.Marshal(map[string]any{"id": req.ID, "result": result})
		}
		if _, err = conn.Write(append(body, '\n')); err != nil {
			return
		}
		if req.Method == "events.subscribe" {
			for push := range f.pushes {
				body, _ = json.Marshal(push)
				if _, err = conn.Write(append(body, '\n')); err != nil {
					return
				}
			}
			return
		}
	}
}

func (f *fakeHerdr) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeHerdr) call(method string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == method {
			return c, true
		}
	}
	return call{}, false
}

func pongFrame(proto int) any {
	return map[string]any{"type": "pong", "version": "0.8.1", "protocol": proto}
}

func agentInfo(name, pane, status string) any {
	return map[string]any{"type": "agent_info", "agent": map[string]any{
		"terminal_id": "term_1", "name": name, "agent": "pi", "pane_id": pane,
		"workspace_id": "w1", "tab_id": "w1:t1", "agent_status": status,
		"focused": false, "revision": 1, "interactive_ready": true,
	}}
}

func TestPrepareRequiresProtocol20(t *testing.T) {
	good := newFakeHerdr(t, func(string, map[string]any) (any, *Error) { return pongFrame(20), nil })
	if err := (&Backend{Socket: good.socket}).Prepare(); err != nil {
		t.Fatalf("protocol 20 rejected: %v", err)
	}
	bad := newFakeHerdr(t, func(string, map[string]any) (any, *Error) { return pongFrame(19), nil })
	err := (&Backend{Socket: bad.socket}).Prepare()
	if err == nil {
		t.Fatal("protocol 19 accepted: golemd would speak a protocol it does not know")
	}
	if (&Backend{Socket: filepath.Join(t.TempDir(), "absent.sock")}).Prepare() == nil {
		t.Fatal("unreachable socket accepted")
	}
}

func TestStartCreatesWorkspaceThenAgentThenPrompt(t *testing.T) {
	started := false
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "ping":
			return pongFrame(20), nil
		case "agent.get":
			if !started {
				return nil, &Error{Code: "agent_not_found", Message: "agent target not found"}
			}
			return agentInfo("job-abc", "w1:p1", "idle"), nil
		case "workspace.create":
			return map[string]any{"type": "workspace_created",
				"workspace": map[string]any{"workspace_id": "w1", "label": params["label"]},
				"tab":       map[string]any{"tab_id": "w1:t1", "workspace_id": "w1"},
				"root_pane": map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "agent_status": "unknown"}}, nil
		case "agent.start":
			started = true
			return agentInfo("job-abc", "w1:p1", "working"), nil
		case "agent.prompt":
			return agentInfo("job-abc", "w1:p1", "working"), nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket, StartupTimeout: 5 * time.Second}
	session, target, err := b.Start(context.Background(), "abc", harnesses.Launch{
		Argv:   []string{"/opt/pi/bin/pi", "--session", "/tmp/s.jsonl", "do the thing"},
		Dir:    "/tmp",
		Env:    map[string]string{"GOLEM_ARTIFACT_DIR": "/tmp/art"},
		Prompt: "do the thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session != "job-abc" || target != "w1:p1" {
		t.Fatalf("unexpected binding %q/%q", session, target)
	}
	want := []string{"agent.get", "workspace.create", "agent.start", "agent.get", "agent.prompt"}
	got := fake.methods()
	if len(got) != len(want) {
		t.Fatalf("call sequence %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call sequence %v, want %v", got, want)
		}
	}
	create, _ := fake.call("workspace.create")
	if create.Params["cwd"] != "/tmp" || create.Params["label"] != "golem/abc" {
		t.Fatalf("workspace not created for the job's workspace: %v", create.Params)
	}
	env, _ := create.Params["env"].(map[string]any)
	if env["GOLEM_ARTIFACT_DIR"] != "/tmp/art" {
		t.Fatalf("harness env not delivered to the pane: %v", env)
	}
	if env["PATH"] == nil || env["PATH"] == "" {
		t.Fatal("PATH not set: herdr resolves the agent kind's executable through it")
	}
	start, _ := fake.call("agent.start")
	if start.Params["kind"] != "pi" || start.Params["name"] != "job-abc" || start.Params["pane_id"] != "w1:p1" {
		t.Fatalf("agent.start params wrong: %v", start.Params)
	}
	args, _ := start.Params["args"].([]any)
	if len(args) != 2 || args[0] != "--session" || args[1] != "/tmp/s.jsonl" {
		t.Fatalf("prompt not split out of argv: %v", args)
	}
	prompt, _ := fake.call("agent.prompt")
	if prompt.Params["text"] != "do the thing" {
		t.Fatalf("prompt not submitted through the agent surface: %v", prompt.Params)
	}
}

func TestStartAdoptsLiveAgentByName(t *testing.T) {
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		if method == "agent.get" {
			return agentInfo("job-abc", "w2:p3", "idle"), nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket}
	session, target, err := b.Start(context.Background(), "abc", harnesses.Launch{Argv: []string{"pi", "hi"}, Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if session != "job-abc" || target != "w2:p3" {
		t.Fatalf("adoption did not reuse the live agent: %q/%q", session, target)
	}
	if got := fake.methods(); len(got) != 1 {
		t.Fatalf("adoption created a second workspace: %v", got)
	}
	status, ok := b.Status("w2:p3")
	if !ok || status.State != protocol.Running {
		t.Fatalf("adopted agent state %+v", status)
	}
}

func TestStartRejectsUnsupportedHarness(t *testing.T) {
	fake := newFakeHerdr(t, func(string, map[string]any) (any, *Error) { return pongFrame(20), nil })
	b := &Backend{Socket: fake.socket}
	_, _, err := b.Start(context.Background(), "abc", harnesses.Launch{Argv: []string{"claude", "hi"}})
	if err == nil {
		t.Fatal("claude accepted on the herdr backend")
	}
}

func TestTeardownVerifiesAbsence(t *testing.T) {
	present := true
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "workspace.close":
			return map[string]any{"type": "ok"}, nil
		case "agent.get":
			if present {
				return agentInfo("job-abc", "w1:p1", "idle"), nil
			}
			return nil, &Error{Code: "agent_not_found", Message: "gone"}
		case "workspace.get":
			if present {
				return map[string]any{"type": "workspace_info", "workspace": map[string]any{"workspace_id": "w1"}}, nil
			}
			return nil, &Error{Code: "workspace_not_found", Message: "gone"}
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket}
	if err := b.Teardown(context.Background(), "job-abc", "w1:p1"); err == nil {
		t.Fatal("teardown claimed success while the agent and workspace were still present")
	}
	present = false
	if err := b.Teardown(context.Background(), "job-abc", "w1:p1"); err != nil {
		t.Fatalf("verified teardown reported failure: %v", err)
	}
}

func TestCancelInterruptsThenClosesAndVerifies(t *testing.T) {
	closed := false
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "agent.send_keys":
			return agentInfo("job-abc", "w1:p1", "idle"), nil
		case "workspace.close":
			closed = true
			return map[string]any{"type": "ok"}, nil
		case "agent.get":
			if closed {
				return nil, &Error{Code: "agent_not_found", Message: "gone"}
			}
			return agentInfo("job-abc", "w1:p1", "working"), nil
		case "workspace.get":
			return nil, &Error{Code: "workspace_not_found", Message: "gone"}
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket}
	if err := b.Cancel(context.Background(), "job-abc", "w1:p1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	keys := []string{}
	fake.mu.Lock()
	for _, c := range fake.calls {
		if c.Method == "agent.send_keys" {
			if list, ok := c.Params["keys"].([]any); ok && len(list) == 1 {
				keys = append(keys, list[0].(string))
			}
		}
	}
	fake.mu.Unlock()
	if len(keys) != 2 || keys[0] != "esc" || keys[1] != "ctrl+c" {
		t.Fatalf("cancel keys %v, want esc then ctrl+c", keys)
	}
	if !closed {
		t.Fatal("cancel never closed the workspace")
	}
}

func TestSendUsesAgentPromptForWorkingSteerAndNeverTypesIntoBlockedDialog(t *testing.T) {
	blocked := false
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		if method != "agent.prompt" {
			return nil, &Error{Code: "unexpected", Message: method}
		}
		if blocked {
			return nil, &Error{Code: "agent_blocked", Message: "agent is at an approval dialog"}
		}
		return agentInfo("job-abc", "w1:p1", "working"), nil
	})
	b := &Backend{Socket: fake.socket}
	if err := b.Send(context.Background(), "w1:p1", "change direction"); err != nil {
		t.Fatalf("working steer rejected: %v", err)
	}
	blocked = true
	if err := b.Send(context.Background(), "w1:p1", "unsafe late steer"); !IsCode(err, "agent_blocked") {
		t.Fatalf("blocked steer should be rejected without fallback: %v", err)
	}
	if got := fake.methods(); len(got) != 2 || got[0] != "agent.prompt" || got[1] != "agent.prompt" {
		t.Fatalf("steer used raw pane interaction: %v", got)
	}
}

func TestAnswerRoutesStructuredPiAndUnstructuredScreenDialogsDifferently(t *testing.T) {
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "agent.prompt":
			return nil, &Error{Code: "agent_blocked", Message: "blocked"}
		case "pane.send_text", "pane.send_keys", "agent.send_keys":
			return map[string]any{"type": "ok"}, nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket}
	if err := b.Answer(context.Background(), "w1:p1", protocol.HarnessPi, "the database is postgres"); err != nil {
		t.Fatalf("structured Pi answer: %v", err)
	}
	if err := b.Answer(context.Background(), "w2:p1", protocol.HarnessClaude, "2 enter"); err != nil {
		t.Fatalf("screen key answer: %v", err)
	}
	if err := b.Answer(context.Background(), "w2:p1", protocol.HarnessClaude, "paste this prose"); err == nil {
		t.Fatal("unsafe prose was pasted into a screen dialog")
	}
	keys, ok := fake.call("agent.send_keys")
	if !ok {
		t.Fatalf("screen answer did not use agent.send_keys: %v", fake.methods())
	}
	got := keys.Params["keys"].([]any)
	if len(got) != 2 || got[0] != "2" || got[1] != "enter" {
		t.Fatalf("wrong deliberate key sequence: %v", got)
	}
}

func TestBlockedQuestionIsBoundedUnstructuredScreenSnapshot(t *testing.T) {
	long := strings.Repeat("approval details ", 800)
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		if method == "agent.read" {
			if params["source"] != "detection" || params["lines"] != float64(80) {
				t.Fatalf("unbounded/wrong read params: %v", params)
			}
			return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": "w1:p1", "source": "detection", "text": long, "revision": 42, "truncated": false}}, nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket}
	if q, err := b.BlockedQuestion(context.Background(), "w1:p1", protocol.HarnessPi); err != nil || q != nil {
		t.Fatalf("Pi must stay on agents_block side channel: %#v %v", q, err)
	}
	q, err := b.BlockedQuestion(context.Background(), "w1:p1", protocol.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(q.Prompt)) != 8192 || len(q.Options) != 0 || !strings.HasPrefix(q.ID, "screen-") {
		t.Fatalf("bad bounded question: id=%q runes=%d options=%v", q.ID, len([]rune(q.Prompt)), q.Options)
	}
	var detail map[string]any
	if json.Unmarshal(q.Detail, &detail) != nil || detail["source"] != "screen" || detail["structured"] != false || detail["truncated"] != true {
		t.Fatalf("dishonest screen detail: %s", q.Detail)
	}
}

func TestSubscriptionStampsReceiptTimeAndMapsState(t *testing.T) {
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "events.subscribe":
			return map[string]any{"type": "ok"}, nil
		case "agent.list":
			return map[string]any{"type": "agent_list", "agents": []any{}}, nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Backend{Socket: fake.socket, Reconcile: time.Hour}
	go b.Run(ctx)
	for i := 0; i < 100; i++ {
		b.mu.Lock()
		ready := b.base != nil
		b.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := time.Now().UTC()
	b.watch("w1:p1")
	fake.pushes <- map[string]any{"event": "pane.agent_status_changed", "data": map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "blocked"}}

	var status = func() bool {
		for i := 0; i < 200; i++ {
			if s, ok := b.Status("w1:p1"); ok && s.State == protocol.Blocked {
				if s.AsOf.Before(before) {
					t.Fatalf("as_of %s predates the observation (%s): golem must not backdate", s.AsOf, before)
				}
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}()
	if !status {
		t.Fatal("blocked event never reached the state cache")
	}
}

func TestAgentNameIsAValidUniqueHerdrName(t *testing.T) {
	name := AgentName("job-9F3C1A/../evil")
	if len(name) > 32 {
		t.Fatalf("name %q exceeds herdr's 32-character cap", name)
	}
	for i, r := range name {
		valid := r == '-' || r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9'
		if !valid {
			t.Fatalf("name %q has an invalid character %q", name, r)
		}
	}
	if AgentName("abc") != "job-abc" {
		t.Fatalf("unexpected name %q", AgentName("abc"))
	}
}

// agent.start returns as soon as Herdr has detected the agent, but the named
// binding can lag: agent.prompt then answers agent_not_ready. Golem must wait
// for the binding instead of failing a worker that is about to be fine.
func TestPromptWaitsForTheNamedAgentBinding(t *testing.T) {
	gets := 0
	fake := newFakeHerdr(t, func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "agent.get":
			gets++
			info := agentInfo("job-abc", "w1:p1", "idle").(map[string]any)
			if gets < 3 {
				info["agent"].(map[string]any)["launch_pending"] = true
				info["agent"].(map[string]any)["interactive_ready"] = false
			}
			return info, nil
		case "agent.prompt":
			return agentInfo("job-abc", "w1:p1", "working"), nil
		}
		return nil, &Error{Code: "unexpected", Message: method}
	})
	b := &Backend{Socket: fake.socket, StartupTimeout: 10 * time.Second}
	if err := b.promptWhenReady(context.Background(), "job-abc", "go"); err != nil {
		t.Fatalf("prompt never landed: %v", err)
	}
	if _, ok := fake.call("agent.prompt"); !ok {
		t.Fatalf("no prompt was submitted: %v", fake.methods())
	}
}
