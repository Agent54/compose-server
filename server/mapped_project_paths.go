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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"

	"github.com/docker/compose/v5/server/errdefs"
)

// Resolve existing prefixes before returning a host path to the loader. The
// launcher maps only the selected stacks tree into its Docker VM.
func resolveMappedProjectPath(root, candidate string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := candidate
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			current, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = filepath.Dir(current)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		current = filepath.Join(current, missing[i])
	}
	relative, err := filepath.Rel(resolvedRoot, current)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errdefs.InvalidParameter(fmt.Errorf("project path is outside the stacks folder: %s", candidate))
	}
	return current, nil
}

// Pin default files to the requested directory. Compose's ordinary CLI fallback
// searches parent directories, which can otherwise leave the selected tree.
func (a *serverApp) mappedDefaultConfigFiles(directory string) ([]string, error) {
	var files []string
	for _, names := range [][]string{composecli.DefaultFileNames, composecli.DefaultOverrideFileNames} {
		for _, name := range names {
			candidate, err := a.resolvePathValue(a.config.rootDir, filepath.Join(directory, name))
			if err != nil {
				return nil, err
			}
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				files = append(files, candidate)
				break
			}
		}
		if len(files) == 0 {
			return nil, errdefs.NotFound(fmt.Errorf("no Compose configuration in requested directory: %s", directory))
		}
	}
	return files, nil
}
