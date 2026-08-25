package attachssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/gisikw/golem/protocol"
	"github.com/gisikw/golem/supervisor"
	gossh "golang.org/x/crypto/ssh"
)

func worker(id string) supervisor.Worker { return supervisor.Worker{Job: protocol.Job{ID: id}} }

func TestResolveUsername(t *testing.T) {
	workers := map[string]supervisor.Worker{
		"job-abcdef": worker("job-abcdef"),
		"job-abc123": worker("job-abc123"),
		"job-xyz789": worker("job-xyz789"),
	}
	for _, tc := range []struct {
		name, user, want string
		fail             bool
	}{
		{"exact", "job-abcdef", "job-abcdef", false},
		{"prefix", "job-x", "job-xyz789", false},
		{"ambiguous", "job-abc", "", true},
		{"unknown", "missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveUsername(tc.user, workers)
			if tc.fail != (err != nil) {
				t.Fatalf("error=%v", err)
			}
			if !tc.fail && got.Job.ID != tc.want {
				t.Fatalf("got %q want %q", got.Job.ID, tc.want)
			}
		})
	}
}

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestAuthorizedKeysParsingAndRejection(t *testing.T) {
	signer := newSigner(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	line := gossh.MarshalAuthorizedKey(signer.PublicKey())
	if err := os.WriteFile(path, append([]byte("# operator\n\n"), line...), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadAuthorizedKeys(path)
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0].Marshal(), signer.PublicKey().Marshal()) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	if err = os.WriteFile(path, append(line, []byte("not-a-key\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadAuthorizedKeys(path); err == nil {
		t.Fatal("malformed key file accepted")
	}
}

func TestHostKeyGenerationPersistenceIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "host")
	first, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	before, _ := os.ReadFile(path)
	second, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("host key changed")
	}
}

func TestNoLiveResolutionErrorIsClear(t *testing.T) {
	_, err := ResolveUsername("none", map[string]supervisor.Worker{})
	if err == nil || err.Error() != `unknown job "none"` {
		t.Fatalf("%v", err)
	}
}
