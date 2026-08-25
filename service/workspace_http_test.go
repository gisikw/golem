package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gisikw/golem/protocol"
)

func TestWorkspaceDispatchHTTPValidation(t *testing.T) {
	repo := gitRepo(t)
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caps := protocol.Capabilities{Name: "local", Harnesses: map[string]protocol.HarnessCapability{"fake": {}}}
	resolver := &WorkspaceResolver{State: t.TempDir(), Projects: map[string]string{"demo": repo}}
	server := httptest.NewServer(API{Store: store, Capabilities: caps, Workspaces: resolver}.Handler())
	defer server.Close()
	post := func(key string, workspace protocol.WorkspaceSelector) (int, protocol.Job) {
		body, _ := json.Marshal(protocol.CreateJob{IdempotencyKey: key, Harness: "fake", Prompt: "go", Workspace: &workspace})
		res, err := http.Post(server.URL+"/v1/jobs", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var job protocol.Job
		_ = json.NewDecoder(res.Body).Decode(&job)
		return res.StatusCode, job
	}
	if status, _ := post("unknown", protocol.WorkspaceSelector{Project: "missing", Worktree: "ok"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown project status %d", status)
	}
	if status, _ := post("clone", protocol.WorkspaceSelector{Repo: "file://" + repo, Worktree: "ok"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("clone gate status %d", status)
	}
	status, job := post("valid", protocol.WorkspaceSelector{Project: "demo", Worktree: "resume"})
	if status != http.StatusCreated || job.Workspace == nil || job.Workspace.Path == "" || job.CWD != job.Workspace.Path {
		t.Fatalf("resolved workspace not stored: status=%d job=%+v", status, job)
	}
}
