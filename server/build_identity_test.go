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
	"context"
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestUpExplicitBuildUsesCLIPolicy(t *testing.T) {
	for _, build := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse images", true: "rebuild images"}[build], func(t *testing.T) {
			backend := mocks.NewMockCompose(gomock.NewController(t))
			app := newServerApp(serveConfig{rootDir: t.TempDir()}, func(...composepkg.Option) (composeapi.Compose, error) {
				return backend, nil
			}, nil)
			version := "changed-version"
			project := &types.Project{Name: "darc", Services: types.Services{
				"vscode": {
					Name: "vscode", PullPolicy: types.PullPolicyMissing,
					Build:     &types.BuildConfig{Context: "./darc-code", Args: types.MappingWithEquals{"CODE_SERVER_VERSION": &version}},
					DependsOn: types.DependsOnConfig{"builder": {Condition: types.ServiceConditionStarted}},
				},
				"builder": {Name: "builder", Build: &types.BuildConfig{Context: "./builder"}, PullPolicy: types.PullPolicyNever},
				"db":      {Name: "db", Image: "postgres", PullPolicy: types.PullPolicyAlways},
			}}
			loads := 1
			if build {
				loads++
			}
			backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil).Times(loads)
			backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, loaded *types.Project, options composeapi.UpOptions) error {
				webPolicy, dependencyPolicy := types.PullPolicyMissing, types.PullPolicyNever
				if build {
					webPolicy, dependencyPolicy = types.PullPolicyBuild, types.PullPolicyBuild
					assert.Assert(t, options.Create.Build != nil)
					assert.DeepEqual(t, options.Create.Build.Services, []string{"vscode"})
					assert.Equal(t, options.Create.Build.Deps, true)
				} else {
					assert.Assert(t, options.Create.Build == nil)
				}
				assert.Equal(t, loaded.Services["vscode"].PullPolicy, webPolicy)
				assert.Equal(t, loaded.Services["builder"].PullPolicy, dependencyPolicy)
				assert.Equal(t, loaded.Services["db"].PullPolicy, types.PullPolicyAlways)
				assert.Equal(t, loaded.Services["vscode"].Build.Context, "./darc-code")
				assert.Equal(t, *loaded.Services["vscode"].Build.Args["CODE_SERVER_VERSION"], version)
				assert.DeepEqual(t, options.Create.Services, []string{"vscode"})
				assert.Equal(t, options.Start.Watch, false)
				return nil
			})
			_, err := app.upProject(t.Context(), upRequest{Project: "darc", Path: app.config.rootDir, Services: []string{"vscode"}, Build: build})
			assert.NilError(t, err)
		})
	}
}

func TestBuildHistoryRetainsServicesWithoutSharingMutableSlices(t *testing.T) {
	registry := newBuildRegistry()
	services := []string{"vscode", "darc", "vscode"}
	id := registry.start("darc", services...)
	services[0] = "other"
	registry.finish(id, false)
	history := registry.list()
	assert.DeepEqual(t, history[0].Services, []string{"darc", "vscode"})
	assert.Equal(t, history[0].Status, "failed")
	history[0].Services[0] = "other"
	assert.DeepEqual(t, registry.list()[0].Services, []string{"darc", "vscode"})
}

func TestUpBuildHistoryIdentifiesServiceAndRecordsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			backend := mocks.NewMockCompose(gomock.NewController(t))
			app := newServerApp(serveConfig{rootDir: t.TempDir()}, func(...composepkg.Option) (composeapi.Compose, error) {
				return backend, nil
			}, nil)
			project := &types.Project{Name: "darc", Services: types.Services{"vscode": {Name: "vscode", Image: "editor"}}}
			backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil).Times(2)
			var upErr error
			if fail {
				upErr = errors.New("Rust download failed")
			}
			backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).Return(upErr)
			_, err := app.upProject(t.Context(), upRequest{Project: "darc", Path: app.config.rootDir, Build: true})
			assert.Assert(t, errors.Is(err, upErr))
			history := app.listBuilds()
			assert.DeepEqual(t, history[0].Services, []string{"vscode"})
			assert.Equal(t, *history[0].Success, !fail)
			if fail {
				messages := app.builds.builds[history[0].ID].messages
				assert.Equal(t, messages[len(messages)-2].Message, "Rust download failed")
			}
		})
	}
}
