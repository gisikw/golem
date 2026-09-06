// Package config loads and validates golemd's operator-owned configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gisikw/golem/protocol"
	"github.com/pelletier/go-toml/v2"
)

type Harness struct {
	Models []string `toml:"models"`
}

type Provider struct {
	// Kind is empty for a statically configured OpenAI-compatible provider.
	// "tiamat" delegates provider/model registration to Golem's isolated pi
	// extension and therefore requires no base URL or provider credential here.
	Kind      string `toml:"kind"`
	BaseURL   string `toml:"base_url"`
	APIKeyEnv string `toml:"api_key_env"`
}

type Project struct {
	Path        string `toml:"path"`
	Description string `toml:"description"`
}

type AttachSSH struct {
	Port               int    `toml:"port"`
	HostKeyPath        string `toml:"host_key_path"`
	AuthorizedKeysPath string `toml:"authorized_keys_path"`
}

// Herdr selects the Herdr run substrate. Its absence is the default and means
// the private tmux server, byte-for-byte as before. Its presence makes golemd
// check the socket at startup and fall back to tmux, loudly, if the check
// fails: golemd must come up either way.
type Herdr struct {
	// Socket is an explicit session socket path. When empty it is derived from
	// Session as ~/.config/herdr/sessions/<session>/herdr.sock.
	Socket string `toml:"socket"`
	// Session is the named Herdr session this daemon owns (default "golem").
	Session string `toml:"session"`
	// StartupTimeoutMS bounds agent.start's readiness wait (3000..300000).
	StartupTimeoutMS int `toml:"startup_timeout_ms"`
	// ReconcileInterval is the safety-net poll (Go duration, default 15s).
	ReconcileInterval string `toml:"reconcile_interval"`
	// Kinds maps a Golem harness to a Herdr agent kind. Default {pi = "pi"};
	// every other harness is rejected at dispatch with 400.
	Kinds map[string]string `toml:"kinds"`
}

// SocketPath resolves the session socket this daemon must talk to.
func (h Herdr) SocketPath() (string, error) {
	if h.Socket != "" {
		return h.Socket, nil
	}
	session := h.Session
	if session == "" {
		session = "golem"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "herdr", "sessions", session, "herdr.sock"), nil
}

// Reconcile is the parsed poll interval; zero means the client default.
func (h Herdr) Reconcile() (time.Duration, error) {
	if h.ReconcileInterval == "" {
		return 0, nil
	}
	return time.ParseDuration(h.ReconcileInterval)
}

type Config struct {
	Name            string              `toml:"name"`
	Harnesses       map[string]Harness  `toml:"harnesses"`
	Projects        map[string]Project  `toml:"projects"`
	Providers       map[string]Provider `toml:"providers"`
	CloneEnabled    bool                `toml:"clone_enabled"`
	APIBearerTokens []string            `toml:"api_bearer_tokens"`
	AttachSSH       AttachSSH           `toml:"attach_ssh"`
	Herdr           *Herdr              `toml:"herdr"`
}

func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if c.Name == "" {
		return Config{}, errors.New("config name is required")
	}
	if len(c.Harnesses) == 0 {
		return Config{}, errors.New("at least one harness is required")
	}
	for name, h := range c.Harnesses {
		if name == "" {
			return Config{}, errors.New("harness name cannot be empty")
		}
		seen := map[string]bool{}
		for _, model := range h.Models {
			if model == "" || seen[model] {
				return Config{}, fmt.Errorf("harness %q has an empty or duplicate model", name)
			}
			seen[model] = true
		}
	}
	for name, p := range c.Providers {
		if name == "" {
			return Config{}, errors.New("provider name cannot be empty")
		}
		switch p.Kind {
		case "":
			if p.BaseURL == "" {
				return Config{}, fmt.Errorf("provider %q requires base_url", name)
			}
		case "tiamat":
			if p.BaseURL != "" || p.APIKeyEnv != "" {
				return Config{}, fmt.Errorf("tiamat provider %q must not set base_url or api_key_env", name)
			}
		default:
			return Config{}, fmt.Errorf("provider %q has unsupported kind %q", name, p.Kind)
		}
		if p.APIKeyEnv != "" && !validEnvName(p.APIKeyEnv) {
			return Config{}, fmt.Errorf("provider %q has invalid api_key_env", name)
		}
	}
	if pi, ok := c.Harnesses["pi"]; ok {
		for _, model := range pi.Models {
			provider, _, found := strings.Cut(model, "/")
			if !found || provider == "" {
				return Config{}, fmt.Errorf("pi model %q must be provider/model", model)
			}
			if _, found = c.Providers[provider]; !found {
				return Config{}, fmt.Errorf("pi model %q references missing provider %q", model, provider)
			}
		}
	}
	seenTokens := map[string]bool{}
	for _, token := range c.APIBearerTokens {
		if token == "" || seenTokens[token] {
			return Config{}, errors.New("api_bearer_tokens must not contain empty or duplicate tokens")
		}
		seenTokens[token] = true
	}
	for name, p := range c.Projects {
		if name == "" || !filepath.IsAbs(p.Path) {
			return Config{}, fmt.Errorf("project %q path must be absolute", name)
		}
		info, statErr := os.Stat(p.Path)
		if statErr != nil {
			return Config{}, fmt.Errorf("project %q: %w", name, statErr)
		}
		if !info.IsDir() {
			return Config{}, fmt.Errorf("project %q path is not a directory", name)
		}
	}
	if c.AttachSSH.Port < 0 || c.AttachSSH.Port > 65535 {
		return Config{}, errors.New("attach_ssh.port must be between 0 and 65535")
	}
	if c.AttachSSH.Port != 0 && (c.AttachSSH.HostKeyPath == "" || c.AttachSSH.AuthorizedKeysPath == "") {
		return Config{}, errors.New("attach_ssh.host_key_path and authorized_keys_path are required when enabled")
	}
	if c.Herdr != nil {
		if _, err = c.Herdr.SocketPath(); err != nil {
			return Config{}, fmt.Errorf("herdr socket: %w", err)
		}
		if _, err = c.Herdr.Reconcile(); err != nil {
			return Config{}, fmt.Errorf("herdr reconcile_interval: %w", err)
		}
		if t := c.Herdr.StartupTimeoutMS; t != 0 && (t < 3000 || t > 300000) {
			return Config{}, errors.New("herdr startup_timeout_ms must be between 3000 and 300000")
		}
		for golem, kind := range c.Herdr.Kinds {
			if golem == "" || kind == "" {
				return Config{}, errors.New("herdr.kinds entries must be non-empty")
			}
		}
	}
	return c, nil
}

func validEnvName(s string) bool {
	for i, r := range s {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// Capabilities returns a stable, path-free public view of operator config.
func (c Config) Capabilities(version string) protocol.Capabilities {
	harnesses := make(map[string]protocol.HarnessCapability, len(c.Harnesses))
	for name, h := range c.Harnesses {
		models := append([]string{}, h.Models...)
		harnesses[name] = protocol.HarnessCapability{Models: models}
	}
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	projects := make([]protocol.ProjectCapability, 0, len(names))
	for _, name := range names {
		projects = append(projects, protocol.ProjectCapability{Name: name, Description: c.Projects[name].Description})
	}
	return protocol.Capabilities{Name: c.Name, Version: version, Harnesses: harnesses, Projects: projects, CloneEnabled: c.CloneEnabled, AttachPort: c.AttachSSH.Port}
}
