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

package serve

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestPrepareProjectForWatchMarksDevelopBuildServicesForRebuild(t *testing.T) {
	project := &types.Project{
		Services: types.Services{
			"watchable": {
				Name:    "watchable",
				Build:   &types.BuildConfig{Context: "."},
				Develop: &types.DevelopConfig{},
			},
			"build-only": {
				Name:  "build-only",
				Build: &types.BuildConfig{Context: "."},
			},
		},
	}

	prepareProjectForWatch(project)

	assert.Equal(t, project.Services["watchable"].PullPolicy, types.PullPolicyBuild)
	assert.Equal(t, project.Services["build-only"].PullPolicy, "")
}

func TestWatchRequestBuildDefaultsToTrue(t *testing.T) {
	assert.Equal(t, watchRequestBuild(watchRequest{}), true)

	disabled := false
	assert.Equal(t, watchRequestBuild(watchRequest{Build: &disabled}), false)
}

func TestWatchModeUpOptionsIncludesBuildWhenEnabled(t *testing.T) {
	project := &types.Project{
		Name: "demo",
		Services: types.Services{
			"web": {Name: "web"},
		},
	}
	build := composeapi.BuildOptions{Services: []string{"web"}}

	withBuild := watchModeUpOptions(project, build, nil, []string{"web"}, true, true)
	assert.Assert(t, withBuild.Create.Build != nil)
	assert.DeepEqual(t, withBuild.Create.Build.Services, []string{"web"})
	assert.Equal(t, withBuild.Create.RemoveOrphans, true)
	assert.Equal(t, withBuild.Start.Watch, true)

	withoutBuild := watchModeUpOptions(project, build, nil, []string{"web"}, false, false)
	assert.Assert(t, withoutBuild.Create.Build == nil)
	assert.Equal(t, withoutBuild.Start.Watch, true)
}

func TestAnalogUpCommandIncludesWatchBuildAndRemoveOrphans(t *testing.T) {
	project := &types.Project{
		ComposeFiles: []string{"/abs/path/to/docker-compose.yaml"},
	}

	command := analogUpCommand(project, []string{"ui"}, true, true, true)

	assert.Assert(t, strings.Contains(command, "-f /abs/path/to/docker-compose.yaml"))
	assert.Assert(t, strings.Contains(command, "up --watch --build --remove-orphans ui"))
}

func TestStartContainerTargetsOneContainer(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	apiClient := mocks.NewMockAPIClient(mockCtrl)
	apiClient.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{
		Items: []containertypes.Summary{
			{
				ID:    "web-1-id",
				Names: []string{"/demo-web-1"},
				Labels: map[string]string{
					composeapi.ServiceLabel:         "web",
					composeapi.ContainerNumberLabel: "1",
				},
			},
			{
				ID:    "web-2-id",
				Names: []string{"/demo-web-2"},
				Labels: map[string]string{
					composeapi.ServiceLabel:         "web",
					composeapi.ContainerNumberLabel: "2",
				},
			},
		},
	}, nil)
	apiClient.EXPECT().ContainerStart(gomock.Any(), "web-2-id", mobyclient.ContainerStartOptions{}).Return(mobyclient.ContainerStartResult{}, nil)

	app := newServerApp(serveConfig{}, nil, func() (statsRuntime, error) {
		return statsRuntime{client: apiClient}, nil
	})
	resp, err := app.startContainer(t.Context(), "demo", containerActionRequest{Container: "demo-web-2"})

	assert.NilError(t, err)
	assert.Equal(t, resp.OK, true)
	assert.Equal(t, resp.Project, "demo")
	assert.Equal(t, resp.Message, "started container demo-web-2")
}

func TestPSUsesKnownComposePathsWithoutDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups [][]string
	}{
		{"single file", [][]string{{"first/compose.yaml"}}},
		{"base and override", [][]string{{"first/compose.yaml", "first/compose.override.yaml"}}},
		{"multiple roots and overrides", [][]string{{"first/compose.yaml", "first/compose.override.yaml"}, {"second/compose.yaml", "second/compose.override.yaml"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := mocks.NewMockCompose(gomock.NewController(t))
			// A discovery walk would fail: the serve root does not exist.
			root := filepath.Join(t.TempDir(), "missing")
			app := newServerApp(serveConfig{rootDir: root}, func(...composepkg.Option) (composeapi.Compose, error) { return backend, nil }, nil)
			var paths []string
			for index, group := range tc.groups {
				files := make([]string, len(group))
				for i, path := range group {
					files[i] = filepath.Join(root, path)
				}
				paths = append(paths, files...)
				service := "web"
				if index > 0 {
					service = "worker"
				}
				project := &types.Project{
					Name: "demo", ComposeFiles: files,
					Services: types.Services{service: {
						Name: service, Image: "busybox",
						CustomLabels: map[string]string{composeapi.ConfigFilesLabel: strings.Join(files, ",")},
					}},
				}
				backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, options composeapi.ProjectLoadOptions) (*types.Project, error) {
					assert.Equal(t, options.WorkingDir, filepath.Dir(files[0]))
					assert.DeepEqual(t, options.ConfigPaths, files)
					assert.Equal(t, options.Offline, true)
					return project, nil
				})
			}
			backend.EXPECT().Ps(gomock.Any(), "demo", gomock.Any()).DoAndReturn(func(_ context.Context, _ string, options composeapi.PsOptions) ([]composeapi.ContainerSummary, error) {
				assert.Equal(t, options.All, true)
				assert.Equal(t, len(options.Project.Services), len(tc.groups))
				assert.DeepEqual(t, options.Project.ComposeFiles, paths)
				return []composeapi.ContainerSummary{{ID: "running-web", Name: "demo-web-1", Service: "web", Project: "demo", State: containertypes.StateRunning}}, nil
			})

			containers, err := app.psProject(t.Context(), "demo", strings.Join(paths, ","), nil, true, nil)
			assert.NilError(t, err)
			assert.Equal(t, len(containers), len(tc.groups))
			assert.Equal(t, containers[0].ID, "running-web")
			if len(tc.groups) > 1 {
				assert.Equal(t, containers[1].Service, "worker")
				assert.Equal(t, string(containers[1].State), "uncreated")
				assert.Equal(t, containers[1].Labels[composeapi.ConfigFilesLabel], strings.Join(paths[2:], ","))
			}
		})
	}
}
