package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

func TestStartLaunchesInteractiveTUIWithSideChannel(t *testing.T) {
	dir := t.TempDir()
	adapter := Adapter{Binary: "fake-pi", HookExtension: "/extensions/agent-hooks", WebExtension: "/extensions/web", DefaultProvider: "llama.cpp", DefaultModel: "gemma", Env: map[string]string{
		"EXAMPLE_ROUTER_URL": "http://router",
	}}
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{Directory: dir}}
	launch, err := adapter.Start(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(launch.Argv, " ")
	if strings.Contains(joined, "--mode json") || strings.Contains(joined, "--print") {
		t.Fatalf("interactive launch still uses print mode: %v", launch.Argv)
	}
	if !launch.Interactive {
		t.Fatal("launch not marked interactive")
	}
	// Extensions load via the per-job settings.json, NOT the command line: pi
	// errors if a tool (agents_block) is registered twice, so the extension set
	// must live in exactly one place.
	if strings.Contains(joined, "--extension") {
		t.Fatalf("extensions must not be passed on the command line: %v", launch.Argv)
	}
	if !strings.HasPrefix(launch.Argv[len(launch.Argv)-1], "p") {
		t.Fatalf("initial prompt not delivered as positional message: %v", launch.Argv)
	}
	if !strings.Contains(launch.Argv[len(launch.Argv)-1], "agents_block") {
		t.Fatalf("block-tool suffix not appended to prompt: %v", launch.Argv)
	}
	if launch.Events == "" || launch.Env[EventsEnv] != launch.Events {
		t.Fatalf("side-channel path not wired into env: %#v", launch)
	}
	// Worker profile isolation: PI_CODING_AGENT_DIR is set explicitly to the
	// per-job dir so the ambient (operator) profile can never leak in.
	wantDir := filepath.Join(dir, "pi")
	if launch.Env[CodingDirEnv] != wantDir {
		t.Fatalf("worker coding dir not isolated: %q want %q", launch.Env[CodingDirEnv], wantDir)
	}
	// Copy discipline: launch env must not alias adapter env.
	launch.Env["EXAMPLE_ROUTER_URL"] = "mutated"
	if adapter.Env["EXAMPLE_ROUTER_URL"] != "http://router" {
		t.Fatal("launch mutated adapter environment")
	}
}

// The worker settings.json is a security boundary: it must enumerate ONLY the
// worker extension set (agent-hooks + optional web) and never the operator's
// worklist/identity/attention/zip/handoff/agents-dispatch/subscriber/telemetry
// suite. This guards against future profile leakage.
func TestWorkerSettingsContainsOnlyExpectedExtensions(t *testing.T) {
	dir := t.TempDir()
	// A source profile carrying the operator's full (dangerous) extension list
	// plus model defaults. The adapter must read ONLY the model defaults from it.
	source := filepath.Join(dir, "source-pi")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceSettings := `{"defaultProvider":"anthropic","defaultModel":"claude-fable-5","extensions":["/p/worklist","/p/identity","/p/attention","/p/zip","/p/agents","/p/subscriber","/p/telemetry"]}`
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(sourceSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "models-store.json"), []byte(`{"anthropic":{"models":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := Adapter{Binary: "fake-pi", HookExtension: "/extensions/agent-hooks", WebExtension: "/extensions/web", SourceProfile: source}
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{Directory: dir}}
	if _, err := adapter.Start(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pi", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Extensions      []string `json:"extensions"`
		DefaultProvider string   `json:"defaultProvider"`
		DefaultModel    string   `json:"defaultModel"`
		Compaction      *struct {
			Enabled bool `json:"enabled"`
		} `json:"compaction"`
	}
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatalf("worker settings not valid JSON: %v\n%s", err, b)
	}
	want := map[string]bool{"/extensions/agent-hooks": true, "/extensions/web": true}
	if len(s.Extensions) != len(want) {
		t.Fatalf("unexpected worker extension set: %v", s.Extensions)
	}
	for _, e := range s.Extensions {
		if !want[e] {
			t.Fatalf("forbidden extension leaked into worker profile: %q (full: %v)", e, s.Extensions)
		}
	}
	// The operator's model defaults are seeded (model access parity) but its
	// extension suite is not.
	if s.DefaultProvider != "anthropic" || s.DefaultModel != "claude-fable-5" {
		t.Fatalf("model defaults not seeded from source profile: %+v", s)
	}
	// Native compaction: the key is omitted so pi's default (enabled) applies.
	// It must never be present-and-disabled.
	if s.Compaction != nil && !s.Compaction.Enabled {
		t.Fatalf("worker must not disable pi native compaction: %s", b)
	}
	// The model catalog is copied so job.Model resolves the same providers.
	if _, err = os.Stat(filepath.Join(dir, "pi", "models-store.json")); err != nil {
		t.Fatalf("model catalog not seeded into worker dir: %v", err)
	}
	// Auth is NOT copied by default (secrets stay out of per-job artifact dirs).
	if _, err = os.Stat(filepath.Join(dir, "pi", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth.json copied without opt-in: %v", err)
	}
}

func TestConfiguredProviderProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEST_LLAMA_KEY", "daemon-secret")
	a := Adapter{Binary: "fake-pi", Providers: map[string]Provider{"llama": {BaseURL: "https://llama.example/v1", APIKeyEnv: "TEST_LLAMA_KEY"}}}
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Model: "llama/Qwen", Artifacts: protocol.ArtifactMetadata{Directory: dir}}
	launch, err := a.Start(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pi", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Providers["llama"]
	if p.BaseURL != "https://llama.example/v1" || p.APIKey != "daemon-secret" || len(p.Models) != 1 || p.Models[0].ID != "Qwen" {
		t.Fatalf("bad profile: %s", b)
	}
	if !strings.Contains(strings.Join(launch.Argv, " "), "--model llama/Qwen") {
		t.Fatalf("model not selected: %v", launch.Argv)
	}
}

func TestBuiltInHookIsMaterializedByDefault(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", Prompt: "go", CWD: dir, Artifacts: protocol.ArtifactMetadata{Directory: dir}}
	launch, err := (Adapter{Binary: "pi"}).Start(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(dir, "pi", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Extensions []string `json:"extensions"`
	}
	if err = json.Unmarshal(settings, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Extensions) != 1 || !strings.HasSuffix(got.Extensions[0], "golem-agent-hooks/index.ts") {
		t.Fatalf("built-in hook missing: %#v", got.Extensions)
	}
	if _, err = os.Stat(got.Extensions[0]); err != nil {
		t.Fatal(err)
	}
	if launch.Events == "" || launch.Env[EventsEnv] != launch.Events {
		t.Fatalf("side-channel not provisioned: %#v", launch)
	}
}

func TestObserveProjectsSideChannelWithCursor(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{ID: "j", Directory: dir}}
	l, err := (Adapter{Binary: "fake-pi"}).Start(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	initial := "{\"type\":\"running\",\"ts\":1}\n{\"type\":\"progress\",\"ts\":2,\"turn\":0}\n"
	if err = os.WriteFile(l.Events, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	alive := func(context.Context) (bool, *int, error) { return true, nil, nil }
	r := &harnesses.Runtime{Launch: l, Alive: alive}
	o, err := (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || len(o.Progresses) != 2 || o.Cursor != int64(len(initial)) || o.Settled {
		t.Fatalf("first observation %#v, %v", o, err)
	}
	r.ObservationCursor = o.Cursor
	o, err = (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || len(o.Progresses) != 0 {
		t.Fatalf("cursor replayed events: %#v, %v", o, err)
	}
	// A partial record must not advance the durable cursor.
	f, _ := os.OpenFile(l.Events, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("{\"type\":\"settled\",\"ts\":3,\"verdict\":\"done\"")
	_ = f.Close()
	o, _ = (Adapter{}).Observe(context.Background(), j, r)
	if o.Settled || o.Cursor != r.ObservationCursor {
		t.Fatalf("advanced over partial line: %#v", o)
	}
	f, _ = os.OpenFile(l.Events, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(",\"summary\":\"all done\",\"usage\":{\"input\":10,\"output\":4,\"cost\":0.002}}\n")
	_ = f.Close()
	o, err = (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || !o.Settled || o.Verdict != protocol.Done || o.Summary != "all done" {
		t.Fatalf("settlement not projected: %#v, %v", o, err)
	}
	if o.Usage == nil || o.Usage.InputTokens != 10 || o.Usage.OutputTokens != 4 || o.Usage.CostMicros != 2000 {
		t.Fatalf("settlement usage not projected: %#v", o.Usage)
	}
}

func TestObserveProjectsBlockedQuestion(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{ID: "j", Directory: dir}}
	l, _ := (Adapter{}).Start(context.Background(), j)
	if err := os.WriteFile(l.Events, []byte("{\"type\":\"blocked\",\"ts\":1,\"id\":\"q1\",\"prompt\":\"which db?\",\"options\":[\"postgres\",\"sqlite\"]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &harnesses.Runtime{Launch: l, Alive: func(context.Context) (bool, *int, error) { return true, nil, nil }}
	o, err := (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || o.Question == nil || o.Question.ID != "q1" || o.Question.Prompt != "which db?" {
		t.Fatalf("blocked question not projected: %#v, %v", o, err)
	}
	if len(o.Question.Options) != 2 || o.Question.Options[0] != "postgres" || o.Question.Options[1] != "sqlite" {
		t.Fatalf("blocked options not projected: %#v", o.Question)
	}
}

// After a blocked event the answer is delivered as the next TUI message; the
// worker resumes and the side channel emits fresh progress. The next Observe
// sees no new blocked record, so o.Question is nil and the supervisor returns
// the worker to Running (edge-triggered: blocked is set only on the tick that
// reads the blocked line). This test documents that projection contract.
func TestObserveResumesAfterBlockedAnswer(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{ID: "j", Directory: dir}}
	l, _ := (Adapter{}).Start(context.Background(), j)
	if err := os.WriteFile(l.Events, []byte("{\"type\":\"blocked\",\"ts\":1,\"id\":\"q1\",\"prompt\":\"which db?\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &harnesses.Runtime{Launch: l, Alive: func(context.Context) (bool, *int, error) { return true, nil, nil }}
	o, err := (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || o.Question == nil {
		t.Fatalf("blocked question not projected: %#v, %v", o, err)
	}
	r.ObservationCursor = o.Cursor
	// Operator's answer resumes the worker; it emits progress on the next turn.
	f, _ := os.OpenFile(l.Events, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("{\"type\":\"progress\",\"ts\":2,\"turn\":1}\n")
	_ = f.Close()
	o, err = (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || o.Question != nil {
		t.Fatalf("stale blocked question after answer: %#v, %v", o, err)
	}
	if len(o.Progresses) != 1 || o.State != protocol.Running {
		t.Fatalf("worker did not resume to running with progress: %#v", o)
	}
}

func TestCollectSettlementFromSideChannelReconcilesUsage(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{ID: "j", Directory: dir}}
	l := harnesses.Launch{Session: filepath.Join("testdata", "session.jsonl")}
	// Side channel reported the verdict + usage directly: session is NOT summed.
	usage := &protocol.Usage{InputTokens: 3, OutputTokens: 1, CostMicros: 500}
	s, err := (Adapter{}).CollectSettlement(context.Background(), j, l, harnesses.Observation{Settled: true, Verdict: protocol.Done, Summary: "ok", Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	if s.Verdict != protocol.Done || s.Summary != "ok" || s.Usage.InputTokens != 3 {
		t.Fatalf("side-channel settlement not used verbatim: %#v", s)
	}
}

func TestCollectSettlementFallsBackToSessionUsage(t *testing.T) {
	dir := t.TempDir()
	j := protocol.Job{ID: "j", CWD: dir, Prompt: "p", Artifacts: protocol.ArtifactMetadata{ID: "j", Directory: dir}}
	l := harnesses.Launch{Session: filepath.Join("testdata", "session.jsonl")}
	zero := 0
	// No usage on the observation (e.g. crash boundary): fall back to the
	// cumulative session-JSONL sum, preserving the original accounting.
	s, err := (Adapter{}).CollectSettlement(context.Background(), j, l, harnesses.Observation{State: protocol.Failed, ExitCode: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if s.Verdict != protocol.Done || s.Usage.InputTokens != 18 || s.Usage.OutputTokens != 7 || s.Usage.CostMicros != 6000 {
		t.Fatalf("session usage double-counted or incomplete: %#v", s)
	}
}
