// Package piintegration ships the lifecycle extension used by pi workers.
package piintegration

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed agent-hooks/index.ts agent-hooks/events.ts
var hooks embed.FS

// WriteHooks materializes the built-in extension in a private worker profile.
func WriteHooks(dir string) (string, error) {
	target := filepath.Join(dir, "golem-agent-hooks")
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", err
	}
	for _, name := range []string{"index.ts", "events.ts"} {
		b, err := hooks.ReadFile("agent-hooks/" + name)
		if err != nil {
			return "", err
		}
		if err = os.WriteFile(filepath.Join(target, name), b, 0o600); err != nil {
			return "", err
		}
	}
	return filepath.Join(target, "index.ts"), nil
}
