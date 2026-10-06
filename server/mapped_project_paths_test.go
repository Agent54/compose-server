/*
   Copyright 2026 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package serve

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestMappedProjectPathsStayInsideStacks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stacks")
	outside := filepath.Join(base, "outside")
	assert.NilError(t, os.MkdirAll(root, 0o755))
	assert.NilError(t, os.MkdirAll(outside, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("services: {}"), 0o644))
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	a := &serverApp{config: serveConfig{rootDir: root, guestStacksPath: "/stacks"}}
	for _, path := range []string{filepath.Join(outside, "compose.yaml"), "../outside/compose.yaml", "escape/compose.yaml", "escape/missing/compose.yaml"} {
		_, err := a.resolvePathValue(root, path)
		assert.Assert(t, err != nil, path)
	}
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "demo"), 0o755))
	assert.NilError(t, os.Symlink("demo", filepath.Join(root, "alias")))
	resolved, err := a.resolvePathValue(root, "alias/missing/compose.yaml")
	assert.NilError(t, err)
	canonical, err := filepath.EvalSymlinks(filepath.Join(root, "demo"))
	assert.NilError(t, err)
	assert.Equal(t, resolved, filepath.Join(canonical, "missing", "compose.yaml"))
	resolved, err = a.resolvePathValue(root, filepath.Join(canonical, "missing", "compose.yaml"))
	assert.NilError(t, err)
	assert.Equal(t, resolved, filepath.Join(canonical, "missing", "compose.yaml"))
}

func TestMappedDefaultConfigNeverFallsBackToParentsOrOutsideSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stacks")
	assert.NilError(t, os.MkdirAll(root, 0o755))
	outside := filepath.Join(base, "compose.yaml")
	assert.NilError(t, os.WriteFile(outside, []byte("services: {}"), 0o644))
	a := &serverApp{config: serveConfig{rootDir: root, guestStacksPath: "/stacks"}}
	for _, path := range []string{"", ".", root} {
		_, err := a.resolveProjectLoadRef(path)
		assert.Assert(t, err != nil)
	}
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "compose.yaml")))
	_, err := a.resolveProjectLoadRef(".")
	assert.Assert(t, err != nil)
	assert.NilError(t, os.Remove(filepath.Join(root, "compose.yaml")))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}"), 0o644))
	ref, err := a.resolveProjectLoadRef(".")
	assert.NilError(t, err)
	canonical, err := filepath.EvalSymlinks(root)
	assert.NilError(t, err)
	assert.DeepEqual(t, ref.configPaths, []string{filepath.Join(canonical, "compose.yaml")})
}
