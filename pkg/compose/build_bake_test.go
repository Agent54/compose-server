/*
   Copyright 2020 Docker Compose CLI authors

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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/streams"
	"golang.org/x/crypto/ssh"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/api"
)

func TestBakeTargetNames(t *testing.T) {
	project := &types.Project{
		Services: types.Services{
			"web":   {},
			"a.b":   {},
			"a_b":   {},
			"a.b.c": {},
			"a_b.c": {},
		},
	}

	names := bakeTargetNames(project)

	// dots are replaced, and services whose names only differ by `.` vs `_`
	// still get distinct bake targets, allocated in sorted service order
	assert.DeepEqual(t, names, map[string]string{
		"web":   "web",
		"a.b":   "a_b",
		"a_b":   "a_b_",
		"a.b.c": "a_b_c",
		"a_b.c": "a_b_c_",
	})
}

func TestToBakeAttest(t *testing.T) {
	tests := []struct {
		name     string
		config   types.BuildConfig
		expected []string
	}{
		{
			name:     "empty — no attest entries",
			config:   types.BuildConfig{},
			expected: nil,
		},
		{
			name:     "provenance true",
			config:   types.BuildConfig{Provenance: "true"},
			expected: []string{"type=provenance"},
		},
		{
			name:     "provenance false — must disable, not omit",
			config:   types.BuildConfig{Provenance: "false"},
			expected: []string{"type=provenance,disabled=true"},
		},
		{
			name:     "provenance mode=max",
			config:   types.BuildConfig{Provenance: "mode=max"},
			expected: []string{"type=provenance,mode=max"},
		},
		{
			name:     "sbom true",
			config:   types.BuildConfig{SBOM: "true"},
			expected: []string{"type=sbom"},
		},
		{
			name:     "sbom false — must disable, not omit",
			config:   types.BuildConfig{SBOM: "false"},
			expected: []string{"type=sbom,disabled=true"},
		},
		{
			name:     "provenance false + sbom false",
			config:   types.BuildConfig{Provenance: "false", SBOM: "false"},
			expected: []string{"type=provenance,disabled=true", "type=sbom,disabled=true"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.DeepEqual(t, toBakeAttest(tc.config), tc.expected)
		})
	}
}

// makeConsole must hand the genuine *os.File over when the stream wraps one:
// on Windows, containerd/console rejects anything but the exact
// os.Stdin/Stdout/Stderr values, so a wrapper would disable the TTY progress
// rendering entirely (#14086).
func TestMakeConsole(t *testing.T) {
	t.Run("stream wrapping a real file yields the file itself", func(t *testing.T) {
		out := makeConsole(streams.NewOut(os.Stdout))
		assert.Equal(t, out, os.Stdout)
	})

	t.Run("file-less stream keeps the console.File wrapper", func(t *testing.T) {
		out := makeConsole(streams.NewOut(&bytes.Buffer{}))
		_, ok := out.(*_console)
		assert.Check(t, ok, "expected a *_console, got %T", out)
	})

	t.Run("plain writer is left untouched", func(t *testing.T) {
		buf := &bytes.Buffer{}
		assert.Equal(t, makeConsole(buf), io.Writer(buf))
	})
}

func TestBakeSerializationEscapesAllTemplateForms(t *testing.T) {
	literal := `${base64decode("Li4v")}/%{ if true }file%{ endif }/$${already_escaped}`
	config := bakeConfig{Targets: map[string]bakeTarget{"app": {
		Context: literal, Contexts: map[string]string{"extra": literal}, Dockerfile: literal,
		DockerfileInline: "RUN echo " + literal,
		Args:             map[string]string{"VALUE": literal}, Labels: map[string]string{"value": literal},
		Secrets: []bakeSecret{{ID: "key", FilePath: literal}}, SSH: []string{"key=" + literal},
		CacheFrom: []string{literal}, CacheTo: []string{literal},
		Tags: []string{literal}, Entitlements: []string{literal},
	}}}
	data, err := marshalBakeConfig(config)
	assert.NilError(t, err)
	var decoded bakeConfig
	assert.NilError(t, json.Unmarshal(data, &decoded))
	target := decoded.Targets["app"]
	want := `$${base64decode("Li4v")}/%%{ if true }file%%{ endif }/$$${already_escaped}`
	assert.Equal(t, target.Context, want)
	assert.Equal(t, target.Contexts["extra"], want)
	assert.Equal(t, target.Dockerfile, want)
	assert.Equal(t, target.DockerfileInline, "RUN echo "+want)
	assert.Equal(t, target.Args["VALUE"], want)
	assert.Equal(t, target.Labels["value"], want)
	assert.Equal(t, target.Secrets[0].FilePath, want)
	assert.Equal(t, target.SSH[0], "key="+want)
	assert.Equal(t, target.CacheFrom[0], want)
	assert.Equal(t, target.CacheTo[0], want)
	assert.Equal(t, target.Tags[0], want)
	assert.Equal(t, target.Entitlements[0], want)
}

func TestBakePrintKeepsValidatedPathsLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Literal HCL expressions contain quotes, which Windows filenames cannot contain")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker CLI is unavailable; Bake parser regression requires buildx")
	}
	if err := exec.CommandContext(t.Context(), docker, "buildx", "version").Run(); err != nil {
		t.Skip("Buildx is unavailable")
	}
	service, root, outside := restrictedStacks(t)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NilError(t, err)
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	assert.NilError(t, err)
	key := string(pem.EncodeToMemory(block))
	stackFile(t, outside, "key", key)
	expression := fmt.Sprintf(`${base64decode(%q)}`, base64.StdEncoding.EncodeToString([]byte("../stacks-outside/key")))
	literal := stackFile(t, root, expression, key)
	compose := fmt.Sprintf(`services:
  app:
    build:
      context: .
      dockerfile: %q
      ssh: [%q]
      secrets: [{source: key, target: 'key,src=../stacks-outside/key'}]
      args: {VALUE: '$${HOME} %%{ if true }literal%%{ endif }'}
      labels: {value: '$${HOME}', '${HOME}': 'key-value'}
secrets:
  key: {file: %q}
`, strings.ReplaceAll(literal, "$", "$$"), "key="+strings.ReplaceAll(literal, "$", "$$"), strings.ReplaceAll(literal, "$", "$$"))
	configFile := stackFile(t, root, "compose.yaml", compose)
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{configFile}})
	assert.NilError(t, err)
	s := &composeService{proxyConfig: map[string]string{}, events: &ignore{}}
	bake := s.prepareBakeBuild(project, project.Services, api.BuildOptions{})
	data, err := marshalBakeConfig(bake.cfg)
	assert.NilError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "buildx", "bake", "--print", "--file", "-")
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(data)
	// --print must succeed without a Docker daemon or network connection.
	env := types.NewMapping(os.Environ())
	delete(env, "DOCKER_CONTEXT")
	delete(env, "BUILDX_BUILDER")
	env["DOCKER_HOST"] = "unix://" + filepath.Join(root, "missing.sock")
	env["BUILDX_CONFIG"] = filepath.Join(root, "buildx")
	cmd.Env = env.Values()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	assert.NilError(t, err, stderr.String())
	var printed struct {
		Targets map[string]struct {
			Dockerfile string
			Args       map[string]string
			Labels     map[string]string
			Secrets    []bakeSecret `json:"secret"`
			SSH        []struct{ Paths []string }
		} `json:"target"`
	}
	assert.NilError(t, json.Unmarshal(output, &printed))
	target := printed.Targets["app"]
	assert.Equal(t, target.Dockerfile, literal)
	assert.Equal(t, target.SSH[0].Paths[0], literal)
	assert.Equal(t, target.Secrets[0].FilePath, literal)
	assert.Equal(t, target.Secrets[0].ID, "key,src=../stacks-outside/key")
	// Bake re-escapes argument and label values in --print output so that the
	// output can itself be parsed again. File paths above are printed directly.
	assert.Equal(t, target.Args["VALUE"], `$${HOME} %%{ if true }literal%%{ endif }`)
	assert.Equal(t, target.Labels["value"], `$${HOME}`)
	assert.Equal(t, target.Labels[`${HOME}`], "key-value")
}

func TestBakeSecretsCannotOverrideProcessSettings(t *testing.T) {
	for _, source := range []string{"BUILDX_BAKE_ENTITLEMENTS_FS", "DOCKER_HOST", "PATH", "LD_PRELOAD"} {
		t.Run(source, func(t *testing.T) {
			project := &types.Project{
				Environment: types.Mapping{source: "project-controlled-value"},
				Secrets:     types.Secrets{"key": {Environment: source}},
			}
			secrets, env := toBakeSecrets(project, []types.ServiceSecretConfig{{Source: "key"}})
			assert.Equal(t, len(secrets), 1)
			assert.Equal(t, len(env), 1)
			name, value, found := strings.Cut(env[0], "=")
			assert.Assert(t, found)
			assert.Assert(t, strings.HasPrefix(name, "COMPOSE_BAKE_SECRET_"))
			assert.Equal(t, secrets[0].Env, name)
			assert.Equal(t, value, "project-controlled-value")
			child := types.NewMapping(append([]string{source + "=trusted-process-value"}, env...))
			assert.Equal(t, child[source], "trusted-process-value")
		})
	}
}

func TestBakeProjectEnvironmentSecretKeepsEntitlementsEnabled(t *testing.T) {
	service, root, _ := restrictedStacks(t)
	stackFile(t, root, ".env", "BUILDX_BAKE_ENTITLEMENTS_FS=0\n")
	config := stackFile(t, root, "compose.yaml", `services:
  app:
    build: {context: ., secrets: [key]}
secrets:
  key: {environment: BUILDX_BAKE_ENTITLEMENTS_FS}
`)
	project, err := service.LoadProject(t.Context(), api.ProjectLoadOptions{WorkingDir: root, ConfigPaths: []string{config}})
	assert.NilError(t, err)
	s := &composeService{proxyConfig: map[string]string{}}
	bake := s.prepareBakeBuild(project, nil, api.BuildOptions{})
	child := types.NewMapping(append([]string{"BUILDX_BAKE_ENTITLEMENTS_FS=1"}, bake.secretsEnv...))
	assert.Equal(t, child["BUILDX_BAKE_ENTITLEMENTS_FS"], "1")
	assert.Equal(t, child[bake.cfg.Targets["app"].Secrets[0].Env], "0")
}
