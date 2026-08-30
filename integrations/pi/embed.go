// Package piintegration ships the lifecycle extension used by pi workers.
package piintegration

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed agent-hooks/index.ts agent-hooks/events.ts
var hooks embed.FS

//go:embed tiamat/index.ts tiamat/catalog.ts
var tiamat embed.FS

// WriteHooks materializes the built-in extension in a private worker profile.
func WriteHooks(dir string) (string, error) {
	return writeExtension(hooks, "agent-hooks", filepath.Join(dir, "golem-agent-hooks"), []string{"index.ts", "events.ts"})
}

// WriteTiamat materializes the Router provider extension in a worker profile.
func WriteTiamat(dir string) (string, error) {
	return writeExtension(tiamat, "tiamat", filepath.Join(dir, "golem-tiamat"), []string{"index.ts", "catalog.ts"})
}

func writeExtension(source embed.FS, sourceDir, target string, names []string) (string, error) {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", err
	}
	for _, name := range names {
		b, err := source.ReadFile(sourceDir + "/" + name)
		if err != nil {
			return "", err
		}
		if err = os.WriteFile(filepath.Join(target, name), b, 0o600); err != nil {
			return "", err
		}
	}
	return filepath.Join(target, "index.ts"), nil
}
