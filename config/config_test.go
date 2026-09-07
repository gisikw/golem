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

func TestLoadDynamicTiamatWithoutExhaustiveInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golemd.toml")
	data := "name = \"test\"\n[harnesses.pi]\nmodels = []\n[tiamat]\nproviders = [\"astra/personal\"]\ncache_ttl = \"30s\"\nstale_ttl = \"10m\"\ntimeout = \"5s\"\nmax_models = 500\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tiamat == nil || cfg.Tiamat.Providers[0] != "astra/personal" || len(cfg.Harnesses["pi"].Models) != 0 {
		t.Fatalf("dynamic config lost: %#v", cfg)
	}
}

func TestLoadRejectsReplacedStaticTiamatInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golemd.toml")
	data := "name = \"test\"\n[providers.tiamat-responses-codex-personal]\nkind = \"tiamat\"\n[harnesses.pi]\nmodels = [\"tiamat-responses-codex-personal/gpt-5.6-sol\"]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "dynamic [tiamat] discovery") {
		t.Fatalf("old duplicated inventory accepted or unclear migration: %v", err)
	}
}

func TestLoadRejectsDynamicTiamatNamespaceCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golemd.toml")
	data := "name = \"test\"\n[providers.tiamat-responses-attacker]\nbase_url = \"https://wrong.example/v1\"\n[harnesses.pi]\nmodels = [\"tiamat-responses-attacker/model\"]\n[tiamat]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "namespace reserved") {
		t.Fatalf("dynamic provider collision accepted or unclear error: %v", err)
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

// [herdr] is optional: absence leaves the daemon on tmux, while presence
// validates only private-daemon controls (socket selection is not exposed).
func TestHerdrSectionIsOptionalAndValidatesPrivateControls(t *testing.T) {
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
	cfg, err = Load(write("[herdr]\nroot = \"/var/lib/golem/herdr\"\nsession = \"golem-test\"\nshell = \"/bin/sh\"\nserver_startup_timeout = \"10s\"\nreconcile_interval = \"15s\"\nstartup_timeout_ms = 60000\n[herdr.kinds]\npi = \"pi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Herdr.Root != "/var/lib/golem/herdr" || cfg.Herdr.Session != "golem-test" {
		t.Fatalf("private controls lost: %#v", cfg.Herdr)
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
	if _, err = Load(write("[herdr]\nstartup_timeout_ms = 10\n")); err == nil {
		t.Fatal("out-of-range startup_timeout_ms accepted")
	}
	if _, err = Load(write("[herdr]\nreconcile_interval = \"soon\"\n")); err == nil {
		t.Fatal("unparseable reconcile_interval accepted")
	}
	for _, bad := range []string{"root = \"relative\"", "binary = \"herdr\"", "shell = \"bash\"", "session = \"bad/session\"", "session = \"..\"", "server_startup_timeout = \"soon\"", "server_startup_timeout = \"-1s\""} {
		if _, err = Load(write("[herdr]\n" + bad + "\n")); err == nil {
			t.Fatalf("invalid private Herdr control accepted: %s", bad)
		}
	}
}

func TestHerdrPiOptionalOverrideMustBeReadable(t *testing.T) {
	write := func(extension string) string {
		path := filepath.Join(t.TempDir(), "golemd.toml")
		data := "name = \"test\"\n[harnesses.pi]\nmodels = []\n[herdr]\n" + extension
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Empty selects the seed that golemd installs with its bundled binary.
	if _, err := Load(write("")); err != nil {
		t.Fatalf("default bundled seed rejected: %v", err)
	}
	for name, extension := range map[string]string{
		"relative path":  "pi_extension = \"seed/extensions/herdr-agent-state.ts\"\n",
		"missing source": "pi_extension = \"/definitely/missing/herdr-agent-state.ts\"\n",
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
