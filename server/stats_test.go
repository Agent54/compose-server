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
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
	"github.com/docker/compose/v5/server/errdefs"
)

func TestProjectResourcesUsesDockerMetadataWithoutComposeFiles(t *testing.T) {
	for _, path := range []string{"agenda/compose.yaml", "agenda", "agenda/compose.yaml,agenda/override.yaml"} {
		t.Run(path, func(t *testing.T) {
			client := mocks.NewMockAPIClient(gomock.NewController(t))
			filters := mobyclient.Filters{}
			filters.Add("label", composeapi.ProjectLabel+"=agenda")
			client.EXPECT().ContainerList(gomock.Any(), mobyclient.ContainerListOptions{All: true, Filters: filters}).Return(mobyclient.ContainerListResult{
				Items: []containertypes.Summary{
					{ID: "web", Labels: map[string]string{
						composeapi.ServiceLabel:     "web",
						composeapi.WorkingDirLabel:  "/missing/compose-root/agenda",
						composeapi.ConfigFilesLabel: "/missing/compose-root/agenda/compose.yaml,/missing/compose-root/agenda/override.yaml",
					}},
					{ID: "other", Labels: map[string]string{
						composeapi.ServiceLabel:     "web",
						composeapi.ConfigFilesLabel: "/missing/compose-root/other/compose.yaml",
					}},
				},
			}, nil)
			client.EXPECT().ContainerStats(gomock.Any(), "web", mobyclient.ContainerStatsOptions{IncludePreviousSample: true}).Return(mobyclient.ContainerStatsResult{
				Body: io.NopCloser(strings.NewReader(`{"memory_stats":{"usage":512,"limit":1024}}`)),
			}, nil)
			// Size is deliberately false; asking Docker for it walks the container filesystem.
			client.EXPECT().ContainerInspect(gomock.Any(), "web", mobyclient.ContainerInspectOptions{}).Return(mobyclient.ContainerInspectResult{}, nil)
			app := newServerApp(serveConfig{rootDir: "/missing/compose-root"}, func(...composepkg.Option) (composeapi.Compose, error) {
				t.Fatal("resource sampling must not load or discover Compose projects")
				return nil, nil
			}, func() (statsRuntime, error) {
				return statsRuntime{client: client, osType: "linux"}, nil
			})

			resources, err := app.projectResources(t.Context(), "agenda", resourcesRequest{Path: path, All: true})
			assert.NilError(t, err)
			assert.Equal(t, len(resources.Containers), 1)
			assert.Equal(t, resources.Containers[0].ID, "web")
			assert.Equal(t, resources.Containers[0].Usage.MemoryBytes, float64(512))
		})
	}
}

func TestResourceSelectionRejectsInvalidPathsWithoutDockerRequests(t *testing.T) {
	for _, selection := range []struct{ project, path string }{{"", ""}, {"agenda", "../outside"}} {
		client := mocks.NewMockAPIClient(gomock.NewController(t))
		app := newServerApp(serveConfig{rootDir: "/missing/compose-root"}, nil, nil)
		_, err := app.listResourceContainers(t.Context(), client, selection.project, selection.path, true, nil)
		assert.Assert(t, errdefs.IsInvalidParameter(err))
	}
}

func TestStatsStreamDoesNotDiscoverProjects(t *testing.T) {
	client := mocks.NewMockAPIClient(gomock.NewController(t))
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		assert.Equal(t, options.Size, false)
		return mobyclient.ContainerListResult{}, nil
	})
	app := newServerApp(serveConfig{rootDir: "/missing/compose-root"}, nil, func() (statsRuntime, error) {
		return statsRuntime{client: client}, nil
	})
	assert.NilError(t, app.streamStats(t.Context(), "agenda", statsRequest{Path: "agenda/compose.yaml", NoStream: true}, httptest.NewRecorder()))
}
