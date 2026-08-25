package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gisikw/golem/protocol"
)

// WorkspaceResolver maps wire selectors to daemon-owned, persistent worktrees.
type WorkspaceResolver struct {
	State        string
	Projects     map[string]string
	CloneEnabled bool
	mu           sync.Mutex
}

func validWorktreeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "..") && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func (r *WorkspaceResolver) Resolve(ctx context.Context, s protocol.WorkspaceSelector) (*protocol.ResolvedWorkspace, error) {
	if !validWorktreeName(s.Worktree) {
		return nil, errors.New("workspace worktree must be a simple name without separators or '..'")
	}
	if (s.Project == "") == (s.Repo == "") {
		return nil, errors.New("workspace must specify exactly one of project or repo")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var root, target string
	resolved := &protocol.ResolvedWorkspace{Project: s.Project, Repo: s.Repo, Ref: s.Ref, Worktree: s.Worktree}
	if s.Project != "" {
		var ok bool
		root, ok = r.Projects[s.Project]
		if !ok {
			return nil, fmt.Errorf("project %q is not configured", s.Project)
		}
		target = "HEAD"
		if s.Ref != "" {
			return nil, errors.New("ref is only valid for repo workspaces")
		}
	} else {
		if !r.CloneEnabled {
			return nil, errors.New("repository cloning is disabled")
		}
		root = filepath.Join(r.State, "clones", cloneName(s.Repo))
		if _, err := os.Stat(root); os.IsNotExist(err) {
			if err = os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
				return nil, err
			}
			if out, err := exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--", s.Repo, root).CombinedOutput(); err != nil {
				_ = os.RemoveAll(root)
				return nil, fmt.Errorf("clone repository: %s: %w", strings.TrimSpace(string(out)), err)
			}
		}
		ref := s.Ref
		if ref == "" {
			ref = "HEAD"
		}
		out, err := exec.CommandContext(ctx, "git", "-C", root, "fetch", "--force", "origin", ref).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("fetch ref %q: %s: %w", ref, strings.TrimSpace(string(out)), err)
		}
		target = "FETCH_HEAD"
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	worktree := filepath.Join(root, ".golem", "worktrees", s.Worktree)
	if info, err := os.Stat(worktree); err == nil {
		if !info.IsDir() {
			return nil, errors.New("workspace worktree path is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		if err = os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
			return nil, err
		}
		out, addErr := exec.CommandContext(ctx, "git", "-C", root, "worktree", "add", "--detach", worktree, target).CombinedOutput()
		if addErr != nil {
			return nil, fmt.Errorf("create worktree: %s: %w", strings.TrimSpace(string(out)), addErr)
		}
	}
	resolved.Path = worktree
	return resolved, nil
}

func cloneName(repo string) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(repo, "/")), ".git")
	var clean strings.Builder
	for _, c := range base {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			clean.WriteRune(c)
		} else {
			clean.WriteByte('-')
		}
	}
	if clean.Len() == 0 {
		clean.WriteString("repo")
	}
	h := sha256.Sum256([]byte(repo))
	return clean.String() + "-" + hex.EncodeToString(h[:6])
}
