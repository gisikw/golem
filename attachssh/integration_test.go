package attachssh

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gisikw/golem/harnesses"
	"github.com/gisikw/golem/protocol"
	"github.com/gisikw/golem/supervisor"
	gossh "golang.org/x/crypto/ssh"
)

func TestSSHAttachIntegration(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux absent")
	}
	dir := t.TempDir()
	tmux := supervisor.Tmux{Socket: filepath.Join(dir, "tmux.sock")}
	if err := tmux.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionName, target, err := tmux.Start(ctx, "job-live-abcdef", harnesses.Launch{
		Argv: []string{"sh", "-c", "printf 'SSH-ATTACH-MARKER\\n'; sleep 30"},
		Dir:  dir, Transcript: filepath.Join(dir, "transcript"), Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tmux.Kill(context.Background(), sessionName) })
	registry, err := supervisor.OpenRegistry(filepath.Join(dir, "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Put(supervisor.Worker{Job: protocol.Job{ID: "job-live-abcdef"}, Session: sessionName, Target: target}); err != nil {
		t.Fatal(err)
	}
	if err = registry.Put(supervisor.Worker{Job: protocol.Job{ID: "job-dead"}, Session: "missing", Target: "missing:0.0"}); err != nil {
		t.Fatal(err)
	}

	hostSigner, clientSigner, rejectedSigner := newSigner(t), newSigner(t), newSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(registry, tmux, hostSigner, []gossh.PublicKey{clientSigner.PublicKey()})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	dial := func(user string, signer gossh.Signer) (*gossh.Client, error) {
		return gossh.Dial("tcp", listener.Addr().String(), &gossh.ClientConfig{
			User: user, Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
		})
	}
	if rejected, dialErr := dial("job-live-abcdef", rejectedSigner); dialErr == nil {
		rejected.Close()
		t.Fatal("unauthorized key accepted")
	}
	client, err := dial("job-live", clientSigner)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	execSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = execSession.Run("whoami"); err == nil {
		t.Fatal("exec request accepted")
	}
	_ = execSession.Close()
	if forwarded, dialErr := client.Dial("tcp", "127.0.0.1:1"); dialErr == nil {
		forwarded.Close()
		t.Fatal("port forwarding accepted")
	}

	attachSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := attachSession.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = attachSession.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err = attachSession.WindowChange(30, 100); err != nil {
		t.Fatal(err)
	}
	if err = attachSession.Shell(); err != nil {
		t.Fatal(err)
	}
	chunks := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if readErr != nil {
				close(chunks)
				return
			}
		}
	}()
	deadline := time.After(5 * time.Second)
	var output bytes.Buffer
readLoop:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				break readLoop
			}
			output.Write(chunk)
			if strings.Contains(output.String(), "SSH-ATTACH-MARKER") {
				break readLoop
			}
		case <-deadline:
			break readLoop
		}
	}
	_ = attachSession.Close()
	if !strings.Contains(output.String(), "SSH-ATTACH-MARKER") {
		t.Fatalf("pane marker absent: %q", output.String())
	}

	noLive, err := dial("job-dead", clientSigner)
	if err == nil { // liveness is resolved at shell time, not authentication time
		s, sessionErr := noLive.NewSession()
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		out, pipeErr := s.StdoutPipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		_ = s.RequestPty("xterm", 24, 80, gossh.TerminalModes{})
		if shellErr := s.Shell(); shellErr != nil {
			t.Fatal(shellErr)
		}
		message, readErr := io.ReadAll(out)
		waitErr := s.Wait()
		if !strings.Contains(string(message), "no live worker session") {
			t.Fatalf("unclear no-live response: %q (read=%v wait=%v)", message, readErr, waitErr)
		}
		noLive.Close()
	}
}
