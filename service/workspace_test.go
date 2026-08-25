package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gisikw/golem/protocol"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, args := range [][]string{{"init", d}, {"-C", d, "config", "user.email", "test@example.invalid"}, {"-C", d, "config", "user.name", "Test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "marker"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", d, "add", "marker").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", d, "commit", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	return d
}

func TestProjectWorktreeCreateAndReuse(t *testing.T) {
	repo := gitRepo(t)
	r := WorkspaceResolver{State: t.TempDir(), Projects: map[string]string{"demo": repo}}
	s := protocol.WorkspaceSelector{Project: "demo", Worktree: "resume"}
	first, err := r.Resolve(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	resumeFile := filepath.Join(first.Path, "continued")
	if err = os.WriteFile(resumeFile, []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := r.Resolve(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != first.Path {
		t.Fatalf("worktree changed: %q != %q", second.Path, first.Path)
	}
	if _, err = os.Stat(resumeFile); err != nil {
		t.Fatalf("worktree was not reused: %v", err)
	}
}

func TestWorkspaceValidationAndGating(t *testing.T) {
	r := WorkspaceResolver{State: t.TempDir(), Projects: map[string]string{}}
	for _, name := range []string{"", "..", "a/b", `a\\b`, "has..dots"} {
		if _, err := r.Resolve(context.Background(), protocol.WorkspaceSelector{Project: "missing", Worktree: name}); err == nil {
			t.Fatalf("accepted worktree %q", name)
		}
	}
	if _, err := r.Resolve(context.Background(), protocol.WorkspaceSelector{Project: "missing", Worktree: "ok"}); err == nil {
		t.Fatal("unknown project accepted")
	}
	if _, err := r.Resolve(context.Background(), protocol.WorkspaceSelector{Repo: "file:///missing", Worktree: "ok"}); err == nil {
		t.Fatal("clone accepted while disabled")
	}
}

func TestCloneFromLocalRemote(t *testing.T) {
	repo := gitRepo(t)
	r := WorkspaceResolver{State: t.TempDir(), CloneEnabled: true}
	s := protocol.WorkspaceSelector{Repo: "file://" + repo, Ref: "HEAD", Worktree: "resume"}
	first, err := r.Resolve(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(first.Path, "marker")); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Resolve(context.Background(), s); err != nil {
		t.Fatalf("clone reuse/fetch failed: %v", err)
	}
}
