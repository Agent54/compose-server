package serve

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestFindComposeDirectoriesHonorsDepthAndExclusions(t *testing.T) {
	root := t.TempDir()

	assert.NilError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "a", "compose.yaml"), []byte("services: {}\n"), 0o644))

	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".git", "nested"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".git", "nested", "compose.yaml"), []byte("services: {}\n"), 0o644))

	assert.NilError(t, os.MkdirAll(filepath.Join(root, "deep", "b", "c", "d", "e"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "deep", "b", "c", "d", "e", "compose.yaml"), []byte("services: {}\n"), 0o644))

	dirs, err := findComposeDirectories(discoveryOptions{
		rootDir:     root,
		maxDepth:    4,
		excludedDir: []string{".git"},
	})
	assert.NilError(t, err)
	assert.Equal(t, len(dirs), 1)
	assert.Equal(t, dirs[0], filepath.Join(root, "a"))
}
