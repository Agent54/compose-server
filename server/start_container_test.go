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
	"fmt"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
	"github.com/docker/compose/v5/server/errdefs"
)

func TestStartUncreatedServiceCreatesAndStartsOnlyOneContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	backend := mocks.NewMockCompose(ctrl)
	client := mocks.NewMockAPIClient(ctrl)
	directory := t.TempDir()
	file := filepath.Join(directory, "compose.yaml")
	replicas := 3
	project := &types.Project{
		Name: "demo", WorkingDir: directory, ComposeFiles: []string{file},
		Services: types.Services{
			"web": {
				Name:      "web",
				Image:     "web",
				Scale:     &replicas,
				Deploy:    &types.DeployConfig{Replicas: &replicas},
				DependsOn: types.DependsOnConfig{"db": {Condition: "service_started", Required: true}},
			},
			"db": {Name: "db", Image: "db"},
		},
	}
	container := containertypes.Summary{ID: "web-1", Labels: map[string]string{
		composeapi.ServiceLabel: "web", composeapi.ContainerNumberLabel: "1", composeapi.ConfigFilesLabel: file,
	}}
	backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil)
	gomock.InOrder(
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{}, nil),
		backend.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, selected *types.Project, options composeapi.CreateOptions) error {
				assert.Equal(t, len(selected.Services), 1)
				web := selected.Services["web"]
				assert.Equal(t, web.GetScale(), 1)
				assert.Equal(t, len(selected.Services["web"].DependsOn), 0)
				assert.DeepEqual(t, options.Services, []string{"web"})
				assert.Equal(t, options.RemoveOrphans, false)
				assert.Equal(t, options.Recreate, composeapi.RecreateNever)
				assert.Equal(t, options.Additive, true)
				return nil
			}),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{container}}, nil),
		backend.EXPECT().Start(gomock.Any(), "demo", gomock.Any()).DoAndReturn(func(_ context.Context, _ string, options composeapi.StartOptions) error {
			assert.Equal(t, options.ContainerID, "web-1")
			assert.Equal(t, len(options.Project.Services), 1)
			assert.Equal(t, len(options.Project.Services["web"].DependsOn), 0)
			return nil
		}),
	)
	app := newServerApp(serveConfig{rootDir: directory}, func(...composepkg.Option) (composeapi.Compose, error) {
		return backend, nil
	}, func() (statsRuntime, error) { return statsRuntime{client: client}, nil })
	response, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
	assert.NilError(t, err)
	assert.Equal(t, response.OK, true)
	assert.Equal(t, len(project.Services), 2)
	web := project.Services["web"]
	assert.Equal(t, web.GetScale(), 3)
}

func TestEnsureSingleServiceContainerPreservesExistingReplicas(t *testing.T) {
	for _, firstExists := range []bool{true, false} {
		t.Run(fmt.Sprintf("first-exists-%t", firstExists), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			backend := mocks.NewMockCompose(ctrl)
			client := mocks.NewMockAPIClient(ctrl)
			project := &types.Project{Name: "demo", ComposeFiles: []string{"/demo/compose.yaml"}, Services: types.Services{"web": {Name: "web"}}}
			containers := []containertypes.Summary{{ID: "web-2", Labels: map[string]string{
				composeapi.ServiceLabel: "web", composeapi.ContainerNumberLabel: "2", composeapi.ConfigFilesLabel: "/demo/compose.yaml",
			}}}
			if firstExists {
				containers = append(containers, containertypes.Summary{ID: "web-1", Labels: map[string]string{
					composeapi.ServiceLabel: "web", composeapi.ContainerNumberLabel: "1", composeapi.ConfigFilesLabel: "/demo/compose.yaml",
				}})
			}
			client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: containers}, nil)
			selected, err := ensureSingleServiceContainer(t.Context(), backend, client, project, "web")
			if firstExists {
				assert.NilError(t, err)
				assert.Equal(t, selected.ID, "web-1")
			} else {
				assert.Assert(t, errdefs.IsNotFound(err))
			}
		})
	}
}

func TestStartServiceContainerRejectsUnscopedRequests(t *testing.T) {
	app := newServerApp(serveConfig{}, nil, nil)
	for _, request := range []containerActionRequest{
		{Service: "web"},
		{Path: "/demo", Service: "web", Container: "web-1"},
	} {
		_, err := app.startContainer(t.Context(), "demo", request)
		assert.Assert(t, errdefs.IsInvalidParameter(err))
	}
}
