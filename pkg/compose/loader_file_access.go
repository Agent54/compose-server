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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"

	"github.com/docker/compose/v5/pkg/api"
)

type composeServiceRef struct {
	file    string
	service string
}

type composeFileVisits struct {
	includes map[string]bool
	extends  map[composeServiceRef]bool
}

func (p *fileAccessPolicy) projectOptions(options api.ProjectLoadOptions) (*cli.ProjectOptions, error) {
	workingDir := options.WorkingDir
	if workingDir == "" {
		workingDir = p.root
		if len(options.ConfigPaths) > 0 {
			workingDir = filepath.Dir(options.ConfigPaths[0])
		}
	}
	workingDir, err := p.path(p.root, workingDir)
	if err != nil {
		return nil, err
	}
	fns := append([]cli.ProjectOptionsFn{}, options.ProjectOptionsFns...)
	fns = append(fns,
		cli.WithWorkingDirectory(workingDir),
		cli.WithOsEnv,
		p.withEnvFiles(options.EnvFiles),
		cli.WithDotEnv,
		cli.WithConfigFileEnv,
		p.withConfigPaths,
		cli.WithDefaultProfiles(options.Profiles...),
		cli.WithName(options.ProjectName),
		// Resolve these only after validating their final, merged paths.
		cli.WithoutEnvironmentResolution,
		cli.WithoutLabelsResolution,
	)
	return cli.NewProjectOptions(options.ConfigPaths, fns...)
}

func (p *fileAccessPolicy) withEnvFiles(files []string) cli.ProjectOptionsFn {
	return func(o *cli.ProjectOptions) error {
		base, err := o.GetWorkingDir()
		if err != nil {
			return err
		}
		if len(files) == 0 {
			file, err := p.path(base, ".env")
			if err != nil {
				return err
			}
			info, err := os.Stat(file)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			files = []string{file}
		}
		for _, file := range files {
			file, err := p.path(base, file)
			if err != nil {
				return err
			}
			o.EnvFiles = append(o.EnvFiles, file)
		}
		return nil
	}
}

func (p *fileAccessPolicy) withConfigPaths(o *cli.ProjectOptions) error {
	base, err := o.GetWorkingDir()
	if err != nil {
		return err
	}
	if len(o.ConfigPaths) == 0 {
		for _, names := range [][]string{cli.DefaultFileNames, cli.DefaultOverrideFileNames} {
			for _, name := range names {
				file, err := p.path(base, name)
				if err != nil {
					return err
				}
				if info, err := os.Stat(file); err == nil && !info.IsDir() {
					o.ConfigPaths = append(o.ConfigPaths, file)
					break
				}
			}
			if len(o.ConfigPaths) == 0 {
				return fmt.Errorf("no Compose configuration in stacks directory %q", base)
			}
		}
	}
	for i, name := range o.ConfigPaths {
		o.ConfigPaths[i], err = p.path(base, name)
		if err != nil {
			return err
		}
	}
	return nil
}

// Inspect each document independently, before Compose can read include env files,
// extends files, service env files or label files. Inspecting only the merged
// project is too late, and misses references hidden by overrides or !reset.
func (p *fileAccessPolicy) checkComposeFiles(ctx context.Context, o *cli.ProjectOptions) error {
	base, err := o.GetWorkingDir()
	if err != nil {
		return err
	}
	// Let Compose determine the project name with its usual precedence, without
	// reading any referenced files. All documents interpolate with this name.
	inspection := *o
	if err := cli.WithLoadOptions(func(opts *loader.Options) {
		opts.SkipInclude = true
		opts.SkipExtends = true
		opts.SkipValidation = true
		opts.SkipConsistencyCheck = true
		opts.ResolvePaths = false
	})(&inspection); err != nil {
		return err
	}
	// LoadModel does not forward ProjectOptions.Environment to compose-go's
	// loader. LoadProject preserves interpolation and COMPOSE_PROJECT_NAME;
	// env/label resolution and include/extends reads remain disabled here.
	if _, err := inspection.LoadProject(ctx); err != nil {
		return err
	}
	for _, file := range o.ConfigPaths {
		visiting := &composeFileVisits{includes: map[string]bool{}, extends: map[composeServiceRef]bool{}}
		if err := p.checkComposeFile(ctx, file, base, base, "", o.Environment.Clone(), visiting); err != nil {
			return err
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkComposeFile(ctx context.Context, file, base, resourceDir, service string, env types.Mapping, visiting *composeFileVisits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, err := p.path(resourceDir, file)
	if err != nil {
		return err
	}
	if service == "" {
		if visiting.includes[resolved] {
			return fmt.Errorf("cyclic Compose include reference: %s", file)
		}
		visiting.includes[resolved] = true
		defer delete(visiting.includes, resolved)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err := p.checkComposeDocument(ctx, file, base, resourceDir, service, raw, env.Clone(), visiting); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
}

func (p *fileAccessPolicy) checkComposeDocument(ctx context.Context, file, base, resourceDir, selectedService string,
	raw map[string]any, env types.Mapping, visiting *composeFileVisits,
) error {
	model, err := loader.LoadModelWithContext(ctx, types.ConfigDetails{
		WorkingDir: base, Environment: env,
		ConfigFiles: []types.ConfigFile{{Filename: file, Config: raw}},
	}, func(o *loader.Options) {
		o.SetProjectName(env["COMPOSE_PROJECT_NAME"], true)
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipValidation = true
		o.SkipNormalization = true
		o.SkipDefaultValues = true
		o.ResolvePaths = false
	})
	if err != nil {
		return err
	}
	var references struct {
		Services map[string]types.ServiceConfig `yaml:"services"`
		Include  []types.IncludeConfig          `yaml:"include"`
	}
	if err := loader.Transform(model, &references); err != nil {
		return err
	}
	// compose-go skips includes in files loaded solely as extends bases.
	if selectedService == "" {
		for _, include := range references.Include {
			if err := p.checkInclude(ctx, base, resourceDir, include, env, visiting); err != nil {
				return err
			}
		}
	}
	for name, service := range references.Services {
		if selectedService != "" && name != selectedService {
			continue
		}
		if err := p.checkServiceExtends(ctx, file, base, resourceDir, name, service.Extends, env, visiting); err != nil {
			return err
		}
	}
	return nil
}

func (p *fileAccessPolicy) checkServiceExtends(ctx context.Context, file, base, resourceDir, name string,
	extends *types.ExtendsConfig, env types.Mapping, visiting *composeFileVisits,
) error {
	if extends == nil {
		return nil
	}
	resolved, err := p.path(resourceDir, file)
	if err != nil {
		return err
	}
	ref := composeServiceRef{file: resolved, service: name}
	if visiting.extends[ref] {
		return fmt.Errorf("cyclic Compose extends reference: %s service %s", file, name)
	}
	visiting.extends[ref] = true
	defer delete(visiting.extends, ref)
	if extends.File == "" {
		return p.checkComposeFile(ctx, file, base, resourceDir, extends.Service, env, visiting)
	}
	extended := localComposeReference(resourceDir, extends.File)
	if _, err := p.path(resourceDir, extended); err != nil {
		return err
	}
	relative, err := filepath.Rel(resourceDir, filepath.Dir(extended))
	if err != nil {
		return err
	}
	return p.checkComposeFile(ctx, extended, relative, filepath.Dir(extended), extends.Service, env, visiting)
}

func (p *fileAccessPolicy) checkInclude(ctx context.Context, base, resourceDir string, include types.IncludeConfig, env types.Mapping, visiting *composeFileVisits) error {
	if len(include.Path) == 0 {
		return fmt.Errorf("include requires a path inside the stacks folder")
	}
	files := make([]string, len(include.Path))
	for i, name := range include.Path {
		file := localComposeReference(resourceDir, name)
		if _, err := p.path(resourceDir, file); err != nil {
			return err
		}
		files[i] = file
	}
	directory := filepath.Dir(files[0])
	relative, err := filepath.Rel(resourceDir, directory)
	if err != nil {
		return err
	}
	if include.ProjectDirectory != "" {
		directory = include.ProjectDirectory
		if filepath.IsAbs(directory) {
			relative = directory
		} else {
			relative = directory
			directory = filepath.Join(base, directory)
		}
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return err
		}
		if _, err := p.path(resourceDir, absolute); err != nil {
			return err
		}
	}
	options := &cli.ProjectOptions{WorkingDir: directory}
	// Explicit include env_file entries are relative to the including project;
	// the implicit .env is relative to the included project's directory.
	if len(include.EnvFile) > 0 {
		// compose-go uses the model working directory here, which can be
		// relative in nested includes. Validate the path it actually opens.
		options.WorkingDir, err = filepath.Abs(base)
		if err != nil {
			return err
		}
		for i, name := range include.EnvFile {
			// dotenv cleans absolute paths before opening them, unlike the
			// loader's direct reads of include and extends files.
			include.EnvFile[i] = filepath.Clean(localComposeReference(options.WorkingDir, name))
		}
	}
	if err := p.withEnvFiles(include.EnvFile)(options); err != nil {
		return err
	}
	values, err := dotenv.GetEnvFromFile(env, options.EnvFiles)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := p.checkComposeFile(ctx, file, relative, directory, "", env.Clone().Merge(values), visiting); err != nil {
			return err
		}
	}
	return nil
}

// Match compose-go's localResourceLoader. Preserve absolute references and the
// spelling of symlinked directories: nested relative references use those names.
func localComposeReference(base, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(base, name)
}
