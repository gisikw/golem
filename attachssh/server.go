// Package attachssh exposes live Golem worker tmux sessions over a restricted
// public-key-authenticated SSH endpoint.
package attachssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/gisikw/golem/supervisor"
	gliderssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type Server struct {
	Registry *supervisor.Registry
	Tmux     supervisor.Tmux
	server   *gliderssh.Server
}

// LoadAuthorizedKeys parses an OpenSSH authorized_keys file. A malformed
// non-comment line rejects the complete file rather than silently weakening it.
func LoadAuthorizedKeys(path string) ([]gossh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []gossh.PublicKey
	for lineNo, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, _, _, rest, parseErr := gossh.ParseAuthorizedKey(line)
		if parseErr != nil || len(bytes.TrimSpace(rest)) != 0 {
			if parseErr == nil {
				parseErr = errors.New("trailing invalid data")
			}
			return nil, fmt.Errorf("authorized_keys line %d: %w", lineNo+1, parseErr)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("authorized_keys contains no keys")
	}
	return keys, nil
}

// LoadOrCreateHostKey loads an OpenSSH/PEM private key, or atomically creates
// and persists one ed25519 key with mode 0600 when path is absent.
func LoadOrCreateHostKey(path string) (gossh.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return gossh.ParsePrivateKey(data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		// Another daemon may have won creation; load that stable key.
		if current, readErr := os.ReadFile(path); readErr == nil {
			return gossh.ParsePrivateKey(current)
		}
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return gossh.ParsePrivateKey(data)
}

// ResolveUsername accepts an exact job ID or one unambiguous prefix. It does
// not decide liveness; callers must check process reality immediately before use.
func ResolveUsername(user string, workers map[string]supervisor.Worker) (supervisor.Worker, error) {
	if worker, ok := workers[user]; ok {
		return worker, nil
	}
	var matches []supervisor.Worker
	for id, worker := range workers {
		if strings.HasPrefix(id, user) {
			matches = append(matches, worker)
		}
	}
	switch len(matches) {
	case 0:
		return supervisor.Worker{}, fmt.Errorf("unknown job %q", user)
	case 1:
		return matches[0], nil
	default:
		return supervisor.Worker{}, fmt.Errorf("job prefix %q is ambiguous (%d matches)", user, len(matches))
	}
}

func New(registry *supervisor.Registry, tmux supervisor.Tmux, signer gossh.Signer, authorized []gossh.PublicKey) *Server {
	s := &Server{Registry: registry, Tmux: tmux}
	allowed := func(key gossh.PublicKey) bool {
		for _, candidate := range authorized {
			if gliderssh.KeysEqual(key, candidate) {
				return true
			}
		}
		return false
	}
	s.server = &gliderssh.Server{
		HostSigners:      []gliderssh.Signer{signer},
		PublicKeyHandler: func(_ gliderssh.Context, key gliderssh.PublicKey) bool { return allowed(key) },
		// Only session channels exist. No direct-tcpip channels and no global
		// tcpip-forward handlers means local and remote forwarding are rejected.
		ChannelHandlers:   map[string]gliderssh.ChannelHandler{"session": s.sessionChannel},
		RequestHandlers:   map[string]gliderssh.RequestHandler{},
		SubsystemHandlers: map[string]gliderssh.SubsystemHandler{},
	}
	return s
}

func (s *Server) Serve(listener net.Listener) error  { return s.server.Serve(listener) }
func (s *Server) Close() error                       { return s.server.Close() }
func (s *Server) Shutdown(ctx context.Context) error { return s.server.Shutdown(ctx) }

type ptyRequest struct {
	Term                                   string
	Width, Height, PixelWidth, PixelHeight uint32
	Modes                                  string
}
type windowRequest struct{ Width, Height, PixelWidth, PixelHeight uint32 }

// sessionChannel is deliberately stricter than gliderlabs' default handler:
// only pty-req, window-change, and one shell request are accepted. In
// particular exec, env, subsystem/SFTP, auth-agent, X11, signal, and break
// requests receive SSH failure replies.
func (s *Server) sessionChannel(_ *gliderssh.Server, conn *gossh.ServerConn, newChannel gossh.NewChannel, ctx gliderssh.Context) {
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	var requested *ptyRequest
	var windows chan windowRequest
	started := false
	var wg sync.WaitGroup
	for req := range requests {
		switch req.Type {
		case "pty-req":
			var p ptyRequest
			ok := !started && requested == nil && gossh.Unmarshal(req.Payload, &p) == nil
			if ok {
				if p.Width == 0 {
					p.Width = 80
				}
				if p.Height == 0 {
					p.Height = 24
				}
				requested = &p
				windows = make(chan windowRequest, 8)
			}
			_ = req.Reply(ok, nil)
		case "window-change":
			var w windowRequest
			ok := requested != nil && gossh.Unmarshal(req.Payload, &w) == nil && w.Width > 0 && w.Height > 0
			if ok {
				select {
				case windows <- w:
				default:
				}
			}
			_ = req.Reply(ok, nil)
		case "shell":
			if started || len(req.Payload) != 0 {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			_ = req.Reply(true, nil)
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.attach(ctx, conn.User(), channel, requested, windows)
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
			}()
		default:
			_ = req.Reply(false, nil)
		}
	}
	if windows != nil {
		close(windows)
	}
	wg.Wait()
}

func (s *Server) attach(ctx context.Context, user string, channel gossh.Channel, request *ptyRequest, windows <-chan windowRequest) {
	worker, err := ResolveUsername(user, s.Registry.Snapshot())
	if err == nil {
		if !worker.SettledAt.IsZero() || worker.Session == "" || worker.Target == "" || !s.Tmux.Has(ctx, worker.Session) {
			err = errors.New("job has no live worker session")
		} else if alive, _, paneErr := s.Tmux.Pane(ctx, worker.Target); paneErr != nil || !alive {
			err = errors.New("job has no live worker session")
		}
	}
	if err != nil {
		fmt.Fprintf(channel, "golemd: %v\r\n", err)
		return
	}
	if request == nil {
		fmt.Fprint(channel, "golemd: a PTY is required for attach\r\n")
		return
	}
	binary := s.Tmux.Binary
	if binary == "" {
		binary = "tmux"
	}
	cmd := exec.CommandContext(ctx, binary, "-S", s.Tmux.Socket, "attach-session", "-t", worker.Target)
	cmd.Env = []string{"TERM=" + request.Term, "PATH=" + os.Getenv("PATH")}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(request.Width), Rows: uint16(request.Height)})
	if err != nil {
		fmt.Fprintf(channel, "golemd: attach failed: %v\r\n", err)
		return
	}
	defer ptmx.Close()
	go func() {
		for w := range windows {
			_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(w.Width), Rows: uint16(w.Height)})
		}
	}()
	go func() { _, _ = io.Copy(ptmx, channel) }()
	_, _ = io.Copy(channel, ptmx)
	if waitErr := cmd.Wait(); waitErr != nil && ctx.Err() == nil {
		fmt.Fprintf(channel, "golemd: tmux attach ended: %v\r\n", waitErr)
	}
}
