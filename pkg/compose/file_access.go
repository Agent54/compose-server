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

package compose

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/paths"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/tonistiigi/go-csvvalue"
)

type fileAccessPolicy struct {
	root string
}

// WithFileAccessRoot confines Compose file references to root, including their
// symlink targets. The ordinary CLI does not enable this server policy.
// This is input validation, not an OS sandbox: callers must prevent concurrent
// untrusted filesystem changes while Compose or its builder consumes the files.
func WithFileAccessRoot(root string) Option {
	// Pin the selected root when the server constructs its backend options,
	// rather than following a replacement symlink on every HTTP request.
	root, err := filepath.Abs(root)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	return func(s *composeService) error {
		if err != nil {
			return fmt.Errorf("resolve stacks folder: %w", err)
		}
		s.fileAccess = &fileAccessPolicy{root: root}
		return nil
	}
}

func (p *fileAccessPolicy) path(base, name string) (string, error) {
	if name == "-" || strings.Contains(name, "://") {
		return "", fmt.Errorf("only files inside the stacks folder are supported: %q", name)
	}
	if !filepath.IsAbs(name) {
		name = base + string(filepath.Separator) + name
	}
	var resolved string
	// Consumers differ in whether they clean .. before opening a path. Both
	// interpretations must stay inside stacks, including their symlink targets.
	for i, candidate := range []string{name, filepath.Clean(name)} {
		if i > 0 && candidate == name {
			continue
		}
		current, err := resolveBindSource(candidate)
		if err != nil {
			return "", fmt.Errorf("resolve stack file %q: %w", name, err)
		}
		rel, err := filepath.Rel(p.root, current)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("file %q is outside the stacks folder %q", name, p.root)
		}
		if resolved == "" {
			resolved = current
		}
	}
	return resolved, nil
}

func (p *fileAccessPolicy) checkPaths(base string, names ...string) error {
	if p == nil {
		return nil
	}
	if base == "" {
		base = p.root
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := p.path(base, name); err != nil {
			return err
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkProject(project *types.Project) error {
	if p == nil {
		return nil
	}
	if err := p.checkPaths(project.WorkingDir, append(project.ComposeFiles, project.WorkingDir)...); err != nil {
		return err
	}
	for _, resource := range project.Configs {
		if err := p.checkPaths(project.WorkingDir, resource.File); err != nil {
			return err
		}
	}
	for _, resource := range project.Secrets {
		if err := p.checkPaths(project.WorkingDir, resource.File); err != nil {
			return err
		}
	}
	for _, service := range project.AllServices() {
		if err := p.checkService(project.WorkingDir, service); err != nil {
			return fmt.Errorf("service %s: %w", service.Name, err)
		}
	}
	for _, volume := range project.Volumes {
		if volume.Driver == "local" || volume.Driver == "" {
			if err := p.checkBindSource(project.WorkingDir, volume.DriverOpts["device"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkBindSource(base, source string) error {
	if guestBindSource(filepath.Clean(source)) {
		return nil
	}
	return p.checkPaths(base, source)
}

func (p *fileAccessPolicy) checkService(base string, service types.ServiceConfig) error {
	if service.Provider != nil {
		return fmt.Errorf("host service providers are unavailable in the Compose server")
	}
	if err := p.checkServiceFiles(base, service); err != nil {
		return err
	}
	for _, volume := range service.Volumes {
		if volume.Type == types.VolumeTypeBind {
			if err := p.checkBindSource(base, volume.Source); err != nil {
				return err
			}
		}
	}
	if service.Build != nil {
		if err := p.checkBuild(base, service.Build); err != nil {
			return err
		}
	}
	if service.Develop != nil {
		for _, trigger := range service.Develop.Watch {
			if err := p.checkPaths(base, trigger.Path); err != nil {
				return err
			}
		}
	}
	for _, opt := range service.SecurityOpt {
		key, value, ok := strings.Cut(opt, "=")
		if !ok {
			key, value, _ = strings.Cut(opt, ":")
		}
		if key == "seccomp" && value != "unconfined" && value != "builtin" {
			if err := p.checkPaths(base, paths.ExpandUser(value)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkServiceFiles(base string, service types.ServiceConfig) error {
	for _, file := range service.EnvFiles {
		if err := p.checkPaths(base, file.Path); err != nil {
			return err
		}
	}
	return p.checkPaths(base, service.LabelFiles...)
}

func remoteBuildContext(name string) bool {
	for _, prefix := range []string{"https://", "http://", "git://", "ssh://", "git@", "github.com/", "docker-image://", types.ServicePrefix} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (p *fileAccessPolicy) checkBuild(base string, build *types.BuildConfig) error {
	if err := p.checkContext(base, build.Context); err != nil {
		return err
	}
	if remoteBuildContext(build.Context) {
		if build.Dockerfile != "" && !filepath.IsLocal(build.Dockerfile) {
			return fmt.Errorf("remote build Dockerfile must be relative to its context")
		}
	} else {
		context := localComposeReference(base, build.Context)
		if _, err := p.path(base, context); err != nil {
			return err
		}
		dockerfile := build.Dockerfile
		if dockerfile == "" && build.DockerfileInline == "" {
			dockerfile = "Dockerfile"
		}
		if err := p.checkPaths(context, dockerfile, "dockerfile", ".dockerignore", dockerfile+".dockerignore"); err != nil {
			return err
		}
	}
	for _, context := range build.AdditionalContexts {
		if err := p.checkContext(base, context); err != nil {
			return err
		}
	}
	if err := p.checkCaches(base, build.CacheFrom); err != nil {
		return err
	}
	if err := p.checkCaches(base, build.CacheTo); err != nil {
		return err
	}
	return p.checkSSH(base, build.SSH)
}

func (p *fileAccessPolicy) checkContext(base, name string) error {
	if strings.HasPrefix(name, "docker-image://") || strings.HasPrefix(name, types.ServicePrefix) {
		return nil
	}
	if remoteBuildContext(name) {
		// The classic builder checks out Git contexts on the host outside the
		// selected stacks folder. Require a checkout under stacks instead.
		return fmt.Errorf("remote build contexts are unavailable in the Compose server; check out the context inside the stacks folder")
	}
	return p.checkPaths(base, name)
}

func (p *fileAccessPolicy) checkCaches(base string, caches []string) error {
	for _, entry := range caches {
		// Buildx treats entries without '=' as registry references and uses
		// csvvalue plus case-insensitive option keys for the long syntax.
		if !strings.Contains(entry, "=") {
			continue
		}
		fields, err := csvvalue.Fields(entry, nil)
		if err != nil {
			return err
		}
		attributes := map[string]string{}
		for _, field := range fields {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return fmt.Errorf("invalid cache option %q", field)
			}
			attributes[strings.ToLower(key)] = value
		}
		if attributes["type"] == "" {
			return fmt.Errorf("cache type required in %q", entry)
		}
		if attributes["type"] == "local" {
			for _, name := range []string{attributes["src"], attributes["dest"]} {
				if name != "" {
					if err := p.checkPaths(base, name); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkSSH(base string, keys types.SSHConfig) error {
	for _, key := range keys {
		if strings.ContainsAny(key.ID, "=,\r\n") {
			return fmt.Errorf("invalid build SSH key ID %q", key.ID)
		}
		if key.Path == "" {
			return fmt.Errorf("ambient SSH agents are unavailable in the Compose server; select a key inside the stacks folder")
		}
		for _, name := range strings.Split(key.Path, ",") {
			if err := p.checkPaths(base, name); err != nil {
				return err
			}
		}
	}
	return nil
}
