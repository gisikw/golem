package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNestedHarnessModelsAndProjects(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(t.TempDir(), "golemd.toml")
	data := "name = \"laptop\"\nclone_enabled = false\n[providers.openai]\nbase_url = \"https://api.openai.com/v1\"\napi_key_env = \"\"\n[attach_ssh]\nport = 2222\nhost_key_path = \"/tmp/host-key\"\nauthorized_keys_path = \"/tmp/authorized-keys\"\n[harnesses.pi]\nmodels = [\"openai/gpt-5.6\"]\n[harnesses.fake]\nmodels = []\n[projects.demo]\npath = \"" + project + "\"\ndescription = \"Demo\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	caps := cfg.Capabilities("test")
	if caps.Name != "laptop" || caps.Version != "test" || caps.AttachPort != 2222 || len(caps.Harnesses["pi"].Models) != 1 || len(caps.Projects) != 1 || caps.Projects[0].Name != "demo" {
		t.Fatalf("unexpected capabilities: %#v", caps)
	}
}

func TestLoadTiamatProviderWithoutStaticEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golemd.toml")
	data := "name = \"test\"\n[providers.tiamat-responses-codex-personal]\nkind = \"tiamat\"\n[harnesses.pi]\nmodels = [\"tiamat-responses-codex-personal/gpt-5.6-sol\"]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["tiamat-responses-codex-personal"].Kind != "tiamat" {
		t.Fatalf("tiamat provider lost: %#v", cfg.Providers)
	}
}

func TestLoadRejectsMissingAndRelativeProjectPaths(t *testing.T) {
	for name, project := range map[string]string{"relative": "somewhere", "missing": filepath.Join(t.TempDir(), "missing")} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "golemd.toml")
			data := "name = \"test\"\n[harnesses.fake]\nmodels = []\n[projects.bad]\npath = \"" + project + "\"\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid project accepted")
			}
		})
	}
}

// [herdr] is optional: its absence must leave the daemon on tmux, and its
// presence must resolve a socket without inventing one.
func TestHerdrSectionIsOptionalAndResolvesASocket(t *testing.T) {
	base := "name = \"test\"\n[harnesses.fake]\nmodels = []\n"
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "golemd.toml")
		if err := os.WriteFile(path, []byte(base+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := Load(write(""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Herdr != nil {
		t.Fatal("absent [herdr] must not select the herdr backend")
	}
	cfg, err = Load(write("[herdr]\nsocket = \"/run/herdr/golem.sock\"\nreconcile_interval = \"15s\"\nstartup_timeout_ms = 60000\n[herdr.kinds]\npi = \"pi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	socket, err := cfg.Herdr.SocketPath()
	if err != nil || socket != "/run/herdr/golem.sock" {
		t.Fatalf("socket %q %v", socket, err)
	}
	if interval, intervalErr := cfg.Herdr.Reconcile(); intervalErr != nil || interval.String() != "15s" {
		t.Fatalf("reconcile %v %v", interval, intervalErr)
	}
	if cfg.Herdr.Kinds["pi"] != "pi" {
		t.Fatalf("kinds %v", cfg.Herdr.Kinds)
	}
	cfg, err = Load(write("[herdr]\nsession = \"golem\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if socket, err = cfg.Herdr.SocketPath(); err != nil || !strings.HasSuffix(socket, "/.config/herdr/sessions/golem/herdr.sock") {
		t.Fatalf("session socket %q %v", socket, err)
	}
	if _, err = Load(write("[herdr]\nstartup_timeout_ms = 10\n")); err == nil {
		t.Fatal("out-of-range startup_timeout_ms accepted")
	}
	if _, err = Load(write("[herdr]\nreconcile_interval = \"soon\"\n")); err == nil {
		t.Fatal("unparseable reconcile_interval accepted")
	}
}

func TestHerdrPiRequiresAReadableExtensionSource(t *testing.T) {
	write := func(extension string) string {
		path := filepath.Join(t.TempDir(), "golemd.toml")
		data := "name = \"test\"\n[harnesses.pi]\nmodels = []\n[herdr]\n" + extension
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, extension := range map[string]string{
		"missing setting": "",
		"relative path":   "pi_extension = \"seed/extensions/herdr-agent-state.ts\"\n",
		"missing source":  "pi_extension = \"/definitely/missing/herdr-agent-state.ts\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(extension)); err == nil || !strings.Contains(err.Error(), "herdr.pi_extension") {
				t.Fatalf("invalid Herdr Pi extension accepted or unclear error: %v", err)
			}
		})
	}

	source := filepath.Join(t.TempDir(), "herdr-agent-state.ts")
	if err := os.WriteFile(source, []byte("export default true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write("pi_extension = \"" + source + "\"\n"))
	if err != nil {
		t.Fatalf("valid Herdr Pi extension rejected: %v", err)
	}
	if cfg.Herdr.PiExtension != source {
		t.Fatalf("pi_extension changed: %q", cfg.Herdr.PiExtension)
	}
}
