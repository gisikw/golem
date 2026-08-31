package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
)

func TestStartIsInteractiveAndIsolated(t *testing.T) {
	d := t.TempDir()
	j := protocol.Job{ID: "j", CWD: d, Model: "claude-sonnet", Prompt: "do work", Artifacts: protocol.ArtifactMetadata{Directory: d}}
	l, err := (Adapter{Binary: "claude-pinned"}).Start(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Interactive || l.Env["ANTHROPIC_BASE_URL"] != RouterBaseURL {
		t.Fatalf("bad launch: %#v", l)
	}
	if l.Env["CLAUDE_CONFIG_DIR"] != filepath.Join(d, "claude") {
		t.Fatalf("config dir not isolated: %#v", l.Env)
	}
	a := strings.Join(l.Argv, " ")
	for _, want := range []string{"--model claude-sonnet", "--settings", "--permission-mode default"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv lacks %q: %s", want, a)
		}
	}
	if !strings.Contains(a, "do work") || !strings.Contains(a, "permission/confirmation") {
		t.Errorf("prompt/block guidance missing: %s", a)
	}
	if _, err = os.Stat(filepath.Join(d, "claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
}
func TestObserveSideChannelAndCursor(t *testing.T) {
	d := t.TempDir()
	j := protocol.Job{ID: "j", Artifacts: protocol.ArtifactMetadata{Directory: d}}
	l := harnesses.Launch{Events: filepath.Join(d, "events.jsonl")}
	os.WriteFile(l.Events, []byte("{\"type\":\"running\",\"ts\":1}\n{\"type\":\"settled\",\"ts\":2,\"verdict\":\"done\",\"summary\":\"ok\"}\n"), 0600)
	r := &harnesses.Runtime{Launch: l, Alive: func(context.Context) (bool, *int, error) { return true, nil, nil }}
	o, err := (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || !o.Settled || o.Verdict != protocol.Done || o.Cursor == 0 {
		t.Fatalf("observation: %#v %v", o, err)
	}
	r.ObservationCursor = o.Cursor
	o, err = (Adapter{}).Observe(context.Background(), j, r)
	if err != nil || o.Settled || len(o.Progresses) != 0 {
		t.Fatalf("cursor replay: %#v %v", o, err)
	}
}
