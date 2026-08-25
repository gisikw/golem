// Package artifacts implements the shared, bounded host-local artifact listing.
package artifacts

import (
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/gisikw/golem/protocol"
)

const ListingLimit = 100

// List enumerates regular files without following directory symlinks. Paths are
// slash-separated and relative to root. Per-entry errors are ignored so a
// transiently changing running-job directory can still be listed.
func List(root string) protocol.ArtifactListing {
	out := protocol.ArtifactListing{Artifacts: []protocol.ArtifactRef{}}
	if root == "" {
		return out
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		if len(out.Artifacts) >= ListingLimit {
			out.ArtifactsTruncated = true
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			out.Artifacts = append(out.Artifacts, protocol.ArtifactRef{Path: filepath.ToSlash(rel), Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		}
		return nil
	})
	sort.Slice(out.Artifacts, func(i, j int) bool { return out.Artifacts[i].Path < out.Artifacts[j].Path })
	return out
}
