package config

import (
	"os"
	"path/filepath"
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
