package harnesses_test

import (
	"context"
	"errors"
	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/harnesses/claude"
	"github.com/gisikw/golem/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeShellAdapterContract(t *testing.T) {
	dir := t.TempDir()
	a := claude.Adapter{ArgvTemplate: []string{"sh", "-c", "printf ok", "{prompt}"}}
	j := protocol.Job{ID: "j", Harness: "claude", CWD: dir, Prompt: "hello", Artifacts: protocol.ArtifactMetadata{Directory: dir}}
	l, e := a.Start(context.Background(), j)
	if e != nil {
		t.Fatal(e)
	}
	if l.Argv[3] != "hello" {
		t.Fatalf("prompt not expanded: %#v", l.Argv)
	}
	if e = os.WriteFile(l.Transcript, []byte("ok"), 0600); e != nil {
		t.Fatal(e)
	}
	zero := 0
	s, e := a.CollectSettlement(context.Background(), j, l, harnesses.Observation{ExitCode: &zero})
	if e != nil || s.Verdict != protocol.Done {
		t.Fatalf("settlement %#v %v", s, e)
	}
	if filepath.Base(s.Artifacts[0].Path) != "claude-transcript.log" {
		t.Fatal("transcript not collected")
	}
	if !errors.Is(a.Prompt(context.Background(), &harnesses.Runtime{}, "x"), harnesses.ErrUnsupported) {
		t.Fatal("minimal adapter faked steering")
	}
}
