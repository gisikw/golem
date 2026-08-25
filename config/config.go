// Package config loads and validates golemd's operator-owned configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gisikw/golem/protocol"
	"github.com/pelletier/go-toml/v2"
)

type Harness struct {
	Models []string `toml:"models"`
}

type Project struct {
	Path        string `toml:"path"`
	Description string `toml:"description"`
}

type AttachSSH struct {
	Port int `toml:"port"`
}

type Config struct {
	Name            string             `toml:"name"`
	Harnesses       map[string]Harness `toml:"harnesses"`
	Projects        map[string]Project `toml:"projects"`
	CloneEnabled    bool               `toml:"clone_enabled"`
	APIBearerTokens []string           `toml:"api_bearer_tokens"`
	AttachSSH       AttachSSH          `toml:"attach_ssh"`
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
	return c, nil
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
