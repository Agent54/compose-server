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
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/watch"
)

func stackFile(t *testing.T, root, name, content string) string {
	t.Helper()
	name = filepath.Join(root, name)
	assert.NilError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	assert.NilError(t, os.WriteFile(name, []byte(content), 0o600))
	return name
}

func restrictedStacks(t *testing.T) (api.Compose, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	assert.NilError(t, err)
	root, outside := filepath.Join(base, "stacks"), filepath.Join(base, "stacks-outside")
	assert.NilError(t, os.MkdirAll(root, 0o755))
	assert.NilError(t, os.MkdirAll(outside, 0o755))
	service, err := NewComposeService(nil, WithFileAccessRoot(root))
	assert.NilError(t, err)
	return service, root, outside
}

func TestRestrictedProjectRejectsExternalReferences(t *testing.T) {
	cases := map[string]string{
		"include":            "include: [%q]\nservices: {}",
		"extends":            "services: {app: {extends: {file: %q, service: base}}}",
		"env file":           "services: {app: {image: busybox, env_file: [%q]}}",
		"optional env file":  "services: {app: {image: busybox, env_file: [{path: %q, required: false}]}}",
		"label file":         "services: {app: {image: busybox, label_file: [%q]}}",
		"context":            "services: {app: {build: {context: %q}}}",
		"dockerfile":         "services: {app: {build: {context: ., dockerfile: %q}}}",
		"additional context": "services: {app: {build: {context: ., additional_contexts: {extra: %q}}}}",
		"build secret":       "services: {app: {build: {context: ., secrets: [key]}}}\nsecrets: {key: {file: %q}}",
		"config":             "services: {app: {image: busybox, configs: [key]}}\nconfigs: {key: {file: %q}}",
		"watch":              "services: {app: {image: busybox, develop: {watch: [{path: %q, action: rebuild}]}}}",
		"ssh key":            "services: {app: {build: {context: ., ssh: [\"key=%s\"]}}}",
		"seccomp":            "services: {app: {image: busybox, security_opt: [\"seccomp=%s\"]}}",
		"bind mount":         "services: {app: {image: busybox, volumes: [{type: bind, source: %q, target: /data}]}}",
		"volume device":      "services: {app: {image: busybox, volumes: [data:/data]}}\nvolumes: {data: {driver: local, driver_opts: {type: none, o: bind, device: %q}}}",
		"cache source":       "services: {app: {build: {context: ., cache_from: [\"type=local,src=%s\"]}}}",
		"cache destination":  "services: {app: {build: {context: ., cache_to: [\"type=local,dest=%s\"]}}}",
	}
	for name, template := range cases {
		t.Run(name, func(t *testing.T) {
			service, root, outside := restrictedStacks(t)
			file := stackFile(t, outside, "payload", "SECRET=${UNSET_VALUE:?external file was read}\n")
			for _, reference := range []string{file, "../stacks-outside/payload", "escape"} {
				t.Run(reference, func(t *testing.T) {
					if reference == "escape" {
						assert.NilError(t, os.Symlink(file, filepath.Join(root, "escape")))
					}
					config := stackFile(t, root, "compose.yaml", fmt.Sprintf(template, reference))
					_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
					assert.ErrorContains(t, err, "outside the stacks folder")
				})
			}
		})
	}
}

func TestRestrictedProjectGuardsInitialEnvironmentAndConfig(t *testing.T) {
	for _, input := range []string{"default env", "explicit env", "compose env", "inherited compose env", "config", "working directory"} {
		t.Run(input, func(t *testing.T) {
			service, root, outside := restrictedStacks(t)
			file := stackFile(t, outside, "payload", "SECRET=${UNSET_VALUE:?external file was read}\n")
			config := stackFile(t, root, "compose.yaml", "services: {app: {image: busybox}}")
			options := api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}}
			switch input {
			case "default env":
				assert.NilError(t, os.Symlink(file, filepath.Join(root, ".env")))
			case "explicit env":
				options.EnvFiles = []string{file}
			case "compose env":
				stackFile(t, root, ".env", "COMPOSE_FILE="+file)
				options.ConfigPaths = nil
			case "inherited compose env":
				t.Setenv("COMPOSE_FILE", file)
				options.ConfigPaths = nil
			case "config":
				options.ConfigPaths = []string{file}
			case "working directory":
				options.WorkingDir = outside
			}
			_, err := service.LoadProject(t.Context(), options)
			assert.ErrorContains(t, err, "outside the stacks folder")
		})
	}
}

func TestRestrictedProjectGuardsRecursiveReferences(t *testing.T) {
	for _, input := range []string{"nested include", "nested extends", "include env", "include dot env", "project directory", "multiple documents", "reset include"} {
		t.Run(input, func(t *testing.T) {
			service, root, outside := restrictedStacks(t)
			file := stackFile(t, outside, "payload", "SECRET=${UNSET_VALUE:?external file was read}\n")
			content := "include: [nested.yaml]\nservices: {}"
			switch input {
			case "nested include":
				stackFile(t, root, "nested.yaml", fmt.Sprintf("include: [%q]\nservices: {}", file))
			case "nested extends":
				content = "services: {app: {extends: {file: nested.yaml, service: base}}}"
				stackFile(t, root, "nested.yaml", fmt.Sprintf("services: {base: {extends: {file: %q, service: base}}}", file))
			case "include env":
				content = fmt.Sprintf("include: [{path: nested.yaml, env_file: %q}]\nservices: {}", file)
				stackFile(t, root, "nested.yaml", "services: {app: {image: busybox}}")
			case "include dot env":
				content = "include: [sub/nested.yaml]\nservices: {}"
				stackFile(t, root, "sub/nested.yaml", "services: {app: {image: busybox}}")
				assert.NilError(t, os.Symlink(file, filepath.Join(root, "sub", ".env")))
			case "project directory":
				content = fmt.Sprintf("include: [{path: nested.yaml, project_directory: %q}]\nservices: {}", outside)
				stackFile(t, root, "nested.yaml", "services: {app: {image: busybox}}")
			case "multiple documents":
				content = fmt.Sprintf("include: [%q]\nservices: {}\n---\ninclude: !override []\nservices: {app: {image: busybox}}", file)
			case "reset include":
				content = fmt.Sprintf("include: !reset [%q]\nservices: {app: {image: busybox}}", file)
			}
			config := stackFile(t, root, "compose.yaml", content)
			_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
			assert.ErrorContains(t, err, "outside the stacks folder")
		})
	}
}

func TestRestrictedProjectLoadsSafeIncludesAndSymlinks(t *testing.T) {
	service, root, _ := restrictedStacks(t)
	stackFile(t, root, ".env", "LOCAL=from-project\n")
	stackFile(t, root, "sub/.env", "INCLUDED=from-include\n")
	stackFile(t, root, "sub/service.env", "VALUE=from-service\n")
	stackFile(t, root, "sub/labels", "label=from-label-file\n")
	stackFile(t, root, "sub/base.yaml", "services: {base: {image: busybox}}")
	stackFile(t, root, "sub/nested.yaml", `services:
  app:
    extends: {file: base.yaml, service: base}
    env_file: service.env
    label_file: labels
    environment: {LOCAL: "${LOCAL}", INCLUDED: "${INCLUDED}", NAME: "${COMPOSE_PROJECT_NAME}"}
`)
	assert.NilError(t, os.Symlink("sub", filepath.Join(root, "alias")))
	config := stackFile(t, root, "compose.yaml", "name: safe-project\ninclude: [alias/nested.yaml]\nservices: {}")
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.NilError(t, err)
	app := project.Services["app"]
	assert.Equal(t, app.Image, "busybox")
	assert.Equal(t, *app.Environment["VALUE"], "from-service")
	assert.Equal(t, *app.Environment["LOCAL"], "from-project")
	assert.Equal(t, *app.Environment["INCLUDED"], "from-include")
	assert.Equal(t, *app.Environment["NAME"], "safe-project")
	assert.Equal(t, app.Labels["label"], "from-label-file")
}

func TestRestrictedProjectInheritsProcessEnvironment(t *testing.T) {
	service, root, _ := restrictedStacks(t)
	t.Setenv("CONTEXT_TOKEN", "from-process")
	t.Setenv("HOST_UID", "1234")
	t.Setenv("PWD", root)
	stackFile(t, root, ".env", "CONTEXT_TOKEN=from-project\n")
	stackFile(t, root, "service.env", "SERVICE_TOKEN=${CONTEXT_TOKEN}\n")
	stackFile(t, root, "compose.yaml", `services:
  app:
    image: busybox
    env_file: service.env
    environment:
      - TOKEN=${CONTEXT_TOKEN}
      - HOST_UID=${HOST_UID}
      - CONTEXT_TOKEN
      - PWD=${PWD}
`)
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root})
	assert.NilError(t, err)
	env := project.Services["app"].Environment
	assert.Equal(t, *env["TOKEN"], "from-process")
	assert.Equal(t, *env["CONTEXT_TOKEN"], "from-process")
	assert.Equal(t, *env["SERVICE_TOKEN"], "from-process")
	assert.Equal(t, *env["HOST_UID"], "1234")
	assert.Equal(t, *env["PWD"], root)
}

func TestRestrictedProjectGuardsInterpolatedFileReferences(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	file := stackFile(t, outside, "service.env", "TOKEN=external\n")
	t.Setenv("CONTEXT_ENV_FILE", file)
	config := stackFile(t, root, "compose.yaml", `services:
  app:
    image: busybox
    env_file: "${CONTEXT_ENV_FILE}"
`)
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedBuildRechecksContextSymlinks(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	config := stackFile(t, root, "compose.yaml", "services: {app: {build: .}}")
	stackFile(t, root, "Dockerfile", "FROM scratch\n")
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.NilError(t, err)
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "new-link")))
	err = service.Build(t.Context(), project, api.BuildOptions{})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedProjectDoesNotSearchParentDirectories(t *testing.T) {
	service, root, _ := restrictedStacks(t)
	stackFile(t, filepath.Dir(root), "compose.yaml", "services: {app: {image: busybox}}")
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root})
	assert.ErrorContains(t, err, "no Compose configuration")
}

func TestRestrictedTreesFollowInternalSymlinkTargets(t *testing.T) {
	_, root, outside := restrictedStacks(t)
	stackFile(t, root, "context/Dockerfile", "FROM scratch\n")
	assert.NilError(t, os.Mkdir(filepath.Join(root, "other"), 0o755))
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "other", "escape")))
	assert.NilError(t, os.Symlink("../other", filepath.Join(root, "context", "link")))
	p := &fileAccessPolicy{root: root}
	assert.ErrorContains(t, p.checkTree(root, "context"), "outside the stacks folder")
	project := &types.Project{WorkingDir: root, Services: types.Services{"app": {Build: &types.BuildConfig{Context: "context"}}}}
	assert.ErrorContains(t, p.checkBuildContexts(project), "outside the stacks folder")
}

func TestRestrictedIncludeUsesActualWorkingDirectory(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	t.Chdir(outside)
	stackFile(t, outside, "sub/secret.env", "SECRET=${UNSET_VALUE:?external file was read}\n")
	stackFile(t, root, "sub/child.yaml", "services: {app: {image: busybox}}")
	stackFile(t, root, "sub/nested.yaml", "include: [{path: child.yaml, env_file: secret.env}]\nservices: {}")
	config := stackFile(t, root, "compose.yaml", "include: [sub/nested.yaml]\nservices: {}")
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedIncludePreservesSymlinkDirectoryNames(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	t.Chdir(outside)
	stackFile(t, root, "sub/safe.env", "CHILD=child.yaml\n")
	stackFile(t, root, "sub/child.yaml", "services: {app: {image: busybox}}")
	stackFile(t, root, "sub/nested.yaml", "include: [{path: '${CHILD:-child.yaml}', env_file: safe.env}]\nservices: {}")
	assert.NilError(t, os.Symlink("sub", filepath.Join(root, "alias")))
	// A preflight that canonicalizes alias to sub would validate this internal
	// file, while the actual loader reads the external alias/safe.env instead.
	assert.NilError(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(outside, "sub")))
	stackFile(t, outside, "alias/safe.env", "SECRET=${UNSET_VALUE:?external file was read}\n")
	config := stackFile(t, root, "compose.yaml", "include: [alias/nested.yaml]\nservices: {}")
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedPathsCheckSymlinksBeforeCleaningParents(t *testing.T) {
	_, root, outside := restrictedStacks(t)
	assert.NilError(t, os.Mkdir(filepath.Join(outside, "directory"), 0o755))
	assert.NilError(t, os.Symlink(filepath.Join(outside, "directory"), filepath.Join(root, "link")))
	stackFile(t, outside, "payload", "external")
	p := &fileAccessPolicy{root: root}
	for _, name := range []string{"link/../payload", "link/../missing/payload"} {
		// Concatenation deliberately preserves the symlink/.. component.
		_, err := p.path(root, root+"/"+name)
		assert.ErrorContains(t, err, "outside the stacks folder")
	}
}

func TestRestrictedBackendPinsRootBeforeRequests(t *testing.T) {
	_, root, outside := restrictedStacks(t)
	option := WithFileAccessRoot(root)
	assert.NilError(t, os.Rename(root, root+"-moved"))
	assert.NilError(t, os.Symlink(outside, root))
	config := stackFile(t, outside, "compose.yaml", "services: {app: {image: busybox}}")
	service, err := NewComposeService(nil, option)
	assert.NilError(t, err)
	_, err = service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedBuildKeepsSecretFieldsSeparate(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	stackFile(t, root, "key", "secret")
	config := stackFile(t, root, "compose.yaml", fmt.Sprintf(`services:
  app:
    build:
      context: .
      secrets: [{source: key, target: 'key,src=%s'}]
secrets: {key: {file: key}}
`, filepath.Join(outside, "secret")))
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.NilError(t, err)
	secrets, _ := toBakeSecrets(project, project.Services["app"].Build.Secrets)
	assert.Equal(t, secrets[0].ID, "key,src="+filepath.Join(outside, "secret"))
	assert.Equal(t, secrets[0].FilePath, filepath.Join(root, "key"))
}

func TestRestrictedBuildRejectsIgnoreFileSymlinks(t *testing.T) {
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore", "Dockerfile", "dockerfile"} {
		t.Run(name, func(t *testing.T) {
			service, root, outside := restrictedStacks(t)
			file := stackFile(t, outside, "payload", "external")
			assert.NilError(t, os.Symlink(file, filepath.Join(root, name)))
			config := stackFile(t, root, "compose.yaml", "services: {app: {build: .}}")
			_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
			assert.ErrorContains(t, err, "outside the stacks folder")
		})
	}
}

func TestRestrictedBuildAllowsDockerfileElsewhereInsideStacks(t *testing.T) {
	service, root, _ := restrictedStacks(t)
	stackFile(t, root, "context/file", "contents")
	stackFile(t, root, "definitions/Dockerfile", "FROM scratch\n")
	config := stackFile(t, root, "compose.yaml", `services:
  app:
    build: {context: context, dockerfile: ../definitions/Dockerfile}
`)
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.NilError(t, err)
}

func TestRestrictedWatchRejectsNewExternalSymlink(t *testing.T) {
	_, root, outside := restrictedStacks(t)
	service := &composeService{fileAccess: &fileAccessPolicy{root: root}}
	config := &types.DevelopConfig{Watch: []types.Trigger{{Path: root, Target: "/app", Action: types.WatchActionSync}}}
	rules, err := getWatchRules(config, types.ServiceConfig{Name: "app"})
	assert.NilError(t, err)
	file := stackFile(t, outside, "payload", "external")
	link := filepath.Join(root, "new-link")
	assert.NilError(t, os.Symlink(file, link))
	err = service.handleWatchBatch(t.Context(), &types.Project{WorkingDir: root}, api.WatchOptions{LogTo: stdLogger{}}, []watch.FileEvent{watch.FileEvent(link)}, rules, nil)
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedBuildChecksBothContextPathInterpretations(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	stackFile(t, root, "other/deep/file", "contents")
	stackFile(t, root, "other/context/Dockerfile", "FROM scratch\n")
	assert.NilError(t, os.Symlink("other/deep", filepath.Join(root, "link")))
	file := stackFile(t, outside, "Dockerfile", "external")
	assert.NilError(t, os.Mkdir(filepath.Join(root, "context"), 0o755))
	assert.NilError(t, os.Symlink(file, filepath.Join(root, "context", "Dockerfile")))
	context := root + "/link/../context"
	config := stackFile(t, root, "compose.yaml", fmt.Sprintf("services: {app: {build: {context: %q}}}", context))
	_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.ErrorContains(t, err, "outside the stacks folder")
	p := &fileAccessPolicy{root: root}
	assert.ErrorContains(t, p.checkTree(root, context), "outside the stacks folder")
}

func TestRestrictedBuildRequiresLocalCheckoutOfRemoteContexts(t *testing.T) {
	for _, field := range []string{"context", "additional context"} {
		t.Run(field, func(t *testing.T) {
			service, root, _ := restrictedStacks(t)
			build := "{context: 'https://example.com/repo.git'}"
			if field != "context" {
				build = "{context: ., additional_contexts: {extra: 'https://example.com/repo.git'}}"
			}
			config := stackFile(t, root, "compose.yaml", "services: {app: {build: "+build+"}}")
			_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
			assert.ErrorContains(t, err, "check out the context inside the stacks folder")
		})
	}
}

func TestRestrictedCachesMatchBuildxOptionParsing(t *testing.T) {
	_, root, outside := restrictedStacks(t)
	p := &fileAccessPolicy{root: root}
	for _, pattern := range []string{
		"TYPE=local,SRC=%s", "type=local,DEST=%s", "TyPe=local,sRc=%s",
		"type=local,src=inside,SRC=%s", `type=local,"SRC=%s,branch"`,
	} {
		t.Run(pattern, func(t *testing.T) {
			assert.ErrorContains(t, p.checkCaches(root, []string{fmt.Sprintf(pattern, outside)}), "outside the stacks folder")
		})
	}
	assert.NilError(t, p.checkCaches(root, []string{"type=local,SRC=" + outside + ",src=inside"}))
	assert.NilError(t, p.checkCaches(root, []string{"TYPE=local,SRC=inside,DEST=inside"}))
	assert.NilError(t, p.checkCaches(root, []string{"registry.example.com/cache", ""}))
	assert.ErrorContains(t, p.checkCaches(root, []string{"type=local,src"}), "invalid cache option")
}

func TestRestrictedBuildScansEnabledAdditionalContextDependencies(t *testing.T) {
	service, root, outside := restrictedStacks(t)
	stackFile(t, root, "app/Dockerfile", "FROM scratch\n")
	stackFile(t, root, "base/Dockerfile", "FROM scratch\n")
	stackFile(t, root, "leaf/Dockerfile", "FROM scratch\n")
	project := &types.Project{
		Name: "test", WorkingDir: root,
		Services: types.Services{"app": {Name: "app", Build: &types.BuildConfig{
			Context: filepath.Join(root, "app"), AdditionalContexts: types.Mapping{"base": "service:base"},
		}}},
		DisabledServices: types.Services{
			"base": {Name: "base", Profiles: []string{"hidden"}, Build: &types.BuildConfig{
				Context: filepath.Join(root, "base"), AdditionalContexts: types.Mapping{"leaf": "service:leaf"},
			}},
			"leaf": {Name: "leaf", Profiles: []string{"hidden"}, Build: &types.BuildConfig{Context: filepath.Join(root, "leaf")}},
		},
	}
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "leaf", "escape")))
	err := service.Build(t.Context(), project, api.BuildOptions{Services: []string{"app"}})
	assert.ErrorContains(t, err, "outside the stacks folder")
}

func TestRestrictedExtendsTracksServicesSeparately(t *testing.T) {
	for _, content := range []string{
		"services: {base: {image: busybox}, app: {extends: {file: compose.yaml, service: base}}}",
		"services: {base: {image: busybox}, middle: {extends: base}, app: {extends: {file: compose.yaml, service: middle}}}",
		"services: {base: {image: busybox}, middle: {extends: {file: other.yaml, service: other}}, app: {extends: middle}}",
	} {
		t.Run(content, func(t *testing.T) {
			service, root, _ := restrictedStacks(t)
			config := stackFile(t, root, "compose.yaml", content)
			stackFile(t, root, "other.yaml", "services: {other: {extends: {file: compose.yaml, service: base}}}")
			project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
			assert.NilError(t, err)
			assert.Equal(t, project.Services["app"].Image, "busybox")
		})
	}
}

func TestRestrictedComposeStillRejectsRealCycles(t *testing.T) {
	for _, test := range []struct{ name, main, other, message string }{
		{"include", "include: [other.yaml]\nservices: {}", "include: [compose.yaml]\nservices: {}", "cyclic Compose include"},
		{"same-file extends", "services: {app: {extends: base}, base: {extends: app}}", "services: {}", "cyclic Compose extends"},
		{
			"cross-file extends", "services: {app: {extends: {file: other.yaml, service: base}}}",
			"services: {base: {extends: {file: compose.yaml, service: app}}}", "cyclic Compose extends",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, root, _ := restrictedStacks(t)
			config := stackFile(t, root, "compose.yaml", test.main)
			stackFile(t, root, "other.yaml", test.other)
			_, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
			assert.ErrorContains(t, err, test.message)
		})
	}
}
