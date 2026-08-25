package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisikw/golem/protocol"
)

func artifactTestAPI(t *testing.T) (http.Handler, string, protocol.Job) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "artifacts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(base, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	job, err := store.Create(context.Background(), protocol.CreateJob{IdempotencyKey: "artifact-test", Harness: protocol.HarnessFake, CWD: base, Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, job.Artifacts.ID)
	if err = os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = store.Record(context.Background(), protocol.EventBatch{Events: []protocol.ObservedEvent{
		{ID: "artifact-starting", JobID: job.ID, State: protocol.Starting},
		{ID: "artifact-running", JobID: job.ID, State: protocol.Running},
	}}); err != nil {
		t.Fatal(err)
	}
	job.State = protocol.Running
	return API{Store: store, ArtifactRoot: root}.Handler(), dir, job
}

func TestArtifactListingAndStreamingWhileRunning(t *testing.T) {
	handler, dir, job := artifactTestAPI(t)
	body := []byte("hello artifact\n")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	listReq := httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts", nil)
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, listReq)
	if list.Code != http.StatusOK {
		t.Fatalf("listing: %d %s", list.Code, list.Body.String())
	}
	var got protocol.ArtifactListing
	if err := json.Unmarshal(list.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Path != "hello.txt" || got.Artifacts[0].Size != int64(len(body)) || got.Artifacts[0].ModifiedAt.IsZero() {
		t.Fatalf("unexpected listing: %#v", got)
	}

	fetch := httptest.NewRecorder()
	handler.ServeHTTP(fetch, httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts/hello.txt", nil))
	if fetch.Code != http.StatusOK || !bytes.Equal(fetch.Body.Bytes(), body) {
		t.Fatalf("fetch: %d %q", fetch.Code, fetch.Body.Bytes())
	}
	if fetch.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("content type: %q", fetch.Header().Get("Content-Type"))
	}
	if fetch.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Fatalf("content length: %q", fetch.Header().Get("Content-Length"))
	}

	rangeReq := httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts/hello.txt", nil)
	rangeReq.Header.Set("Range", "bytes=6-13")
	partial := httptest.NewRecorder()
	handler.ServeHTTP(partial, rangeReq)
	if partial.Code != http.StatusPartialContent || partial.Body.String() != "artifact" || partial.Header().Get("Content-Range") != "bytes 6-13/15" {
		t.Fatalf("range: %d %q %#v", partial.Code, partial.Body.String(), partial.Header())
	}
}

func TestArtifactPathDefenseAndMissing(t *testing.T) {
	handler, dir, job := artifactTestAPI(t)
	outside := filepath.Join(filepath.Dir(filepath.Dir(dir)), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	for name, escaped := range map[string]string{
		"encoded dotdot":   "%2e%2e/secret.txt",
		"raw dotdot":       "../secret.txt",
		"absolute":         "%2Fetc%2Fpasswd",
		"double slash":     "/etc/passwd",
		"backslash":        "..%5Csecret.txt",
		"escaping symlink": "escape",
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRecorder()
			handler.ServeHTTP(r, httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts/"+escaped, nil))
			if r.Code != http.StatusBadRequest {
				t.Fatalf("got %d: %s", r.Code, r.Body.String())
			}
		})
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", missing.Code, missing.Body.String())
	}
}

func TestArtifactListingCap(t *testing.T) {
	handler, dir, job := artifactTestAPI(t)
	for i := 0; i < 101; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest("GET", "/v1/jobs/"+job.ID+"/artifacts", nil))
	var got protocol.ArtifactListing
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 100 || !got.ArtifactsTruncated {
		t.Fatalf("unexpected cap: %d truncated=%v", len(got.Artifacts), got.ArtifactsTruncated)
	}
}

func TestBearerAuth(t *testing.T) {
	plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { output(w, 200, map[string]bool{"ok": true}) })
	authed := BearerAuth([]string{"short", strings.Repeat("x", 64)}, plain)
	for _, header := range []string{"", "Bearer wrong", "Basic short"} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/events", nil)
		req.Header.Set("Authorization", header)
		authed.ServeHTTP(r, req)
		if r.Code != http.StatusUnauthorized || !strings.Contains(r.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("%q: %d", header, r.Code)
		}
		data, _ := io.ReadAll(r.Body)
		if !json.Valid(data) {
			t.Fatalf("non-JSON 401: %s", data)
		}
	}
	for _, token := range []string{"short", strings.Repeat("x", 64)} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		authed.ServeHTTP(r, req)
		if r.Code != http.StatusOK {
			t.Fatalf("valid token rejected: %d", r.Code)
		}
	}
	// The same handler is intentionally unwrapped on a Unix listener.
	r := httptest.NewRecorder()
	plain.ServeHTTP(r, httptest.NewRequest("GET", "/", nil))
	if r.Code != http.StatusOK {
		t.Fatal("unix-exempt handler rejected request")
	}
}
