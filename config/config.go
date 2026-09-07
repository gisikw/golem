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

// Tiamat enables bounded Router catalogue discovery for Pi. Empty provider and
// model restrictions accept every compatible, non-unavailable record. Models
// are exact advertised IDs (tiamat-{family}-{encoded-provider}/{model}).
type Tiamat struct {
	Providers        []string `toml:"providers"`
	Models           []string `toml:"models"`
	CacheTTL         string   `toml:"cache_ttl"`
	StaleTTL         string   `toml:"stale_ttl"`
	Timeout          string   `toml:"timeout"`
	MaxModels        int      `toml:"max_models"`
	MaxResponseBytes int64    `toml:"max_response_bytes"`
}

func (t Tiamat) Durations() (cache, stale, timeout time.Duration, err error) {
	parse := func(value string) (time.Duration, error) {
		if value == "" {
			return 0, nil
		}
		return time.ParseDuration(value)
	}
	if cache, err = parse(t.CacheTTL); err != nil {
		return
	}
	if stale, err = parse(t.StaleTTL); err != nil {
		return
	}
	timeout, err = parse(t.Timeout)
	return
}

type Provider struct {
	// Kind is reserved for migration diagnostics. Static providers leave it
	// empty; Tiamat providers are discovered through the top-level [tiamat]
	// section rather than duplicated here.
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

// Herdr selects a private, golemd-owned Herdr run substrate. Its absence is
// the default and means tmux. Presence starts a foreground Herdr child in an
// isolated XDG/HOME namespace; startup failure still falls back loudly.
type Herdr struct {
	// Binary is the pinned Herdr executable. Empty uses GOLEM_HERDR, then PATH.
	Binary string `toml:"binary"`
	// Root is the private Herdr namespace. Empty means STATE/herdr. It contains
	// XDG config/state, HOME, session data, logs, and the stable Pi seed.
	Root string `toml:"root"`
	// Session is explicit even inside the private root (default "golem").
	Session string `toml:"session"`
	// Shell is an executable profile-less pane shell. Empty uses
	// GOLEM_HERDR_SHELL, then /bin/sh; shell_mode is always non_login.
	Shell string `toml:"shell"`
	// ServerStartupTimeout is a Go duration bounding child readiness.
	ServerStartupTimeout string `toml:"server_startup_timeout"`
	// StartupTimeoutMS bounds agent.start's readiness wait (3000..300000).
	StartupTimeoutMS int `toml:"startup_timeout_ms"`
	// ReconcileInterval is the safety-net poll (Go duration, default 15s).
	ReconcileInterval string `toml:"reconcile_interval"`
	// PiExtension optionally overrides the stable seed installed by golemd's
	// bundled Herdr. Normally empty; the default is ROOT/pi-seed/extensions/
	// herdr-agent-state.ts. Every worker still receives a private byte copy.
	PiExtension string `toml:"pi_extension"`
	// Kinds maps a Golem harness to a Herdr agent kind. Default {pi = "pi"};
	// every other harness is rejected at dispatch with 400.
	Kinds map[string]string `toml:"kinds"`
}

// ServerStartup is the parsed child readiness bound; zero means the default.
func (h Herdr) ServerStartup() (time.Duration, error) {
	if h.ServerStartupTimeout == "" {
		return 0, nil
	}
	return time.ParseDuration(h.ServerStartupTimeout)
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
	Tiamat          *Tiamat             `toml:"tiamat"`
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
			return Config{}, fmt.Errorf("provider %q: kind = \"tiamat\" was replaced by dynamic [tiamat] discovery", name)
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
	if c.Tiamat != nil {
		if _, ok := c.Harnesses["pi"]; !ok {
			return Config{}, errors.New("[tiamat] requires the pi harness")
		}
		cache, stale, timeout, durationErr := c.Tiamat.Durations()
		if durationErr != nil {
			return Config{}, fmt.Errorf("tiamat duration: %w", durationErr)
		}
		if cache < 0 || stale < 0 || timeout < 0 {
			return Config{}, errors.New("tiamat durations must not be negative")
		}
		if cache > 0 && stale > 0 && stale < cache {
			return Config{}, errors.New("tiamat stale_ttl must be at least cache_ttl")
		}
		if cache > time.Hour || stale > 24*time.Hour || timeout > 30*time.Second {
			return Config{}, errors.New("tiamat cache_ttl must not exceed 1h, stale_ttl 24h, or timeout 30s")
		}
		if c.Tiamat.MaxModels < 0 || c.Tiamat.MaxModels > 5000 {
			return Config{}, errors.New("tiamat max_models must be between 1 and 5000 when set")
		}
		if c.Tiamat.MaxResponseBytes < 0 || c.Tiamat.MaxResponseBytes > 16<<20 {
			return Config{}, errors.New("tiamat max_response_bytes must not exceed 16777216")
		}
		for name := range c.Providers {
			if dynamicTiamatProviderName(name) {
				return Config{}, fmt.Errorf("provider %q uses a namespace reserved by dynamic [tiamat] discovery", name)
			}
		}
		for field, values := range map[string][]string{"providers": c.Tiamat.Providers, "models": c.Tiamat.Models} {
			seen := map[string]bool{}
			for _, value := range values {
				if value == "" || seen[value] {
					return Config{}, fmt.Errorf("tiamat %s must not contain empty or duplicate values", field)
				}
				seen[value] = true
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
		if c.Herdr.Root != "" && !filepath.IsAbs(c.Herdr.Root) {
			return Config{}, errors.New("herdr.root must be an absolute path")
		}
		if c.Herdr.Binary != "" && !filepath.IsAbs(c.Herdr.Binary) {
			return Config{}, errors.New("herdr.binary must be an absolute path")
		}
		if c.Herdr.Shell != "" && !filepath.IsAbs(c.Herdr.Shell) {
			return Config{}, errors.New("herdr.shell must be an absolute path")
		}
		if c.Herdr.Session != "" && !validHerdrSession(c.Herdr.Session) {
			return Config{}, errors.New("herdr.session must contain only letters, digits, dot, underscore, or hyphen")
		}
		if d, parseErr := c.Herdr.ServerStartup(); parseErr != nil {
			return Config{}, fmt.Errorf("herdr server_startup_timeout: %w", parseErr)
		} else if d < 0 {
			return Config{}, errors.New("herdr server_startup_timeout must not be negative")
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
		_, piConfigured := c.Harnesses["pi"]
		_, piMapped := c.Herdr.Kinds["pi"]
		if len(c.Herdr.Kinds) == 0 {
			piMapped = true // backend/herdr's default mapping is pi -> pi.
		}
		if piConfigured && piMapped && c.Herdr.PiExtension != "" {
			if !filepath.IsAbs(c.Herdr.PiExtension) {
				return Config{}, errors.New("herdr.pi_extension must be an absolute path")
			}
			info, statErr := os.Stat(c.Herdr.PiExtension)
			if statErr != nil {
				return Config{}, fmt.Errorf("herdr.pi_extension: %w", statErr)
			}
			if !info.Mode().IsRegular() {
				return Config{}, errors.New("herdr.pi_extension must be a regular file")
			}
			f, openErr := os.Open(c.Herdr.PiExtension)
			if openErr != nil {
				return Config{}, fmt.Errorf("herdr.pi_extension is not readable: %w", openErr)
			}
			if closeErr := f.Close(); closeErr != nil {
				return Config{}, fmt.Errorf("herdr.pi_extension: %w", closeErr)
			}
		}
	}
	return c, nil
}

func validHerdrSession(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return s != "" && s != "default" && s != "." && s != ".."
}

func dynamicTiamatProviderName(value string) bool {
	for _, family := range []string{"anthropic", "openai", "responses"} {
		if strings.HasPrefix(value, "tiamat-"+family+"-") {
			return true
		}
	}
	return false
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
