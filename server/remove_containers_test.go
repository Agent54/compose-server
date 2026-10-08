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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	engineserver "github.com/docker/docker/api/server"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
	"github.com/docker/compose/v5/server/errdefs"
)

func TestRemoveContainersAfterComposeDefinitionIsDeleted(t *testing.T) {
	for _, change := range []string{"folder deleted", "file deleted", "service removed", "invalid config"} {
		t.Run(change, func(t *testing.T) {
			app, client, directory := realContainerApp(t, "name: demo\nservices:\n  web:\n    image: test-image\n")
			app.config.rootDir = filepath.Dir(directory)
			app.config.guestStacksPath = "/stacks"
			path := filepath.Join(directory, "compose.yaml")
			switch change {
			case "folder deleted":
				assert.NilError(t, os.RemoveAll(directory))
			case "file deleted":
				assert.NilError(t, os.Remove(path))
			case "service removed":
				assert.NilError(t, os.WriteFile(path, []byte("name: demo\nservices:\n  worker:\n    image: test-image\n"), 0o600))
			case "invalid config":
				assert.NilError(t, os.WriteFile(path, []byte("services: ["), 0o600))
			}
			web := serviceSummary(directory, "web", "1")
			web.State = containertypes.StateRunning
			worker := serviceSummary(directory, "worker", "1")
			other := serviceSummary(filepath.Join(app.config.rootDir, "other"), "web", "1")
			other.ID = "other-web"
			oneoff := serviceSummary(directory, "web", "2")
			oneoff.Labels[composeapi.OneoffLabel] = "True"
			filters := mobyclient.Filters{}
			filters.Add("label", composeapi.ProjectLabel+"=demo")
			gomock.InOrder(
				client.EXPECT().ContainerList(gomock.Any(), mobyclient.ContainerListOptions{All: true, Filters: filters}).Return(mobyclient.ContainerListResult{
					Items: []containertypes.Summary{web, worker, other, oneoff},
				}, nil),
				client.EXPECT().ContainerStop(gomock.Any(), web.ID, mobyclient.ContainerStopOptions{}).Return(mobyclient.ContainerStopResult{}, nil),
				// RemoveVolumes remains false: UI removal only deletes containers.
				client.EXPECT().ContainerRemove(gomock.Any(), web.ID, mobyclient.ContainerRemoveOptions{Force: true}).Return(mobyclient.ContainerRemoveResult{}, nil),
			)
			app.backend = func(...composepkg.Option) (composeapi.Compose, error) {
				t.Fatal("removal must not load the deleted service or its Compose files")
				return nil, nil
			}
			response, err := app.rmProject(t.Context(), "demo", rmRequest{Path: path, Services: []string{"web"}, Force: true, Stop: true})
			assert.NilError(t, err)
			assert.Equal(t, response.OK, true)
		})
	}
}

func TestRemoveContainersIsIdempotentDuringConcurrentDeletion(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			app, client, directory := realContainerApp(t, "services: {}")
			ctr := serviceSummary(directory, "web", "1")
			client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{ctr}}, nil)
			if stop {
				client.EXPECT().ContainerStop(gomock.Any(), ctr.ID, gomock.Any()).Return(mobyclient.ContainerStopResult{}, errdefs.NotFound(fmt.Errorf("already removed")))
			}
			client.EXPECT().ContainerRemove(gomock.Any(), ctr.ID, mobyclient.ContainerRemoveOptions{Force: true}).Return(mobyclient.ContainerRemoveResult{}, errdefs.NotFound(fmt.Errorf("already removed")))
			client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{}, nil)
			for range 2 {
				response, err := app.rmProject(t.Context(), "demo", rmRequest{Force: true, Stop: stop})
				assert.NilError(t, err)
				assert.Equal(t, response.OK, true)
			}
		})
	}
}

func TestRemoveContainersPreservesRunningContainersWithoutStop(t *testing.T) {
	app, client, directory := realContainerApp(t, "services: {}")
	var containers []containertypes.Summary
	for _, state := range []containertypes.ContainerState{containertypes.StateRunning, containertypes.StateRestarting, containertypes.StatePaused, containertypes.StateExited} {
		ctr := serviceSummary(directory, "web", string(state))
		ctr.State = state
		containers = append(containers, ctr)
	}
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: containers}, nil)
	client.EXPECT().ContainerRemove(gomock.Any(), "web-exited", mobyclient.ContainerRemoveOptions{Force: true}).Return(mobyclient.ContainerRemoveResult{}, nil)
	_, err := app.rmProject(t.Context(), "demo", rmRequest{Force: true})
	assert.NilError(t, err)
}

func TestRemoveContainersRejectsPathsOutsideStacks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	app, _, _ := realContainerApp(t, "services: {}")
	app.config = serveConfig{rootDir: root, guestStacksPath: "/stacks"}
	for _, path := range []string{filepath.Join(outside, "deleted.yaml"), "../deleted.yaml", "escape/deleted.yaml"} {
		_, err := app.rmProject(t.Context(), "demo", rmRequest{Path: path, Force: true, Stop: true})
		assert.Assert(t, errdefs.IsInvalidParameter(err), path)
	}
}

func TestDeletedComposeProjectCanBeListedRemovedAndPolledAgain(t *testing.T) {
	app, client, directory := realContainerApp(t, "name: demo\nservices:\n  web:\n    image: test-image\n")
	app.config.rootDir = filepath.Dir(directory)
	app.config.guestStacksPath = "/stacks"
	path := filepath.Join(directory, "compose.yaml")
	assert.NilError(t, os.RemoveAll(directory))
	web := serviceSummary(directory, "web", "1")
	other := serviceSummary(filepath.Join(app.config.rootDir, "other"), "web", "1")
	other.ID = "other-web"
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{web, other}}, nil).Times(2)
	for _, ctr := range []containertypes.Summary{web, other} {
		client.EXPECT().ContainerInspect(gomock.Any(), ctr.ID, mobyclient.ContainerInspectOptions{}).Return(mobyclient.ContainerInspectResult{}, nil)
	}
	client.EXPECT().ContainerStop(gomock.Any(), web.ID, mobyclient.ContainerStopOptions{}).Return(mobyclient.ContainerStopResult{}, nil)
	client.EXPECT().ContainerRemove(gomock.Any(), web.ID, mobyclient.ContainerRemoveOptions{Force: true}).Return(mobyclient.ContainerRemoveResult{}, nil)
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{other}}, nil)
	client.EXPECT().ContainerInspect(gomock.Any(), other.ID, mobyclient.ContainerInspectOptions{}).Return(mobyclient.ContainerInspectResult{}, nil)
	server := &engineserver.Server{}
	mux := server.CreateMux(t.Context(), newRouter(app))
	psURL := "/ps/demo?all=true&path=" + url.QueryEscape(path)
	before := httptest.NewRecorder()
	mux.ServeHTTP(before, httptest.NewRequest(http.MethodGet, psURL, http.NoBody))
	assert.Equal(t, before.Code, http.StatusOK, before.Body.String())
	assert.Assert(t, strings.Contains(before.Body.String(), "web-1"))
	assert.Assert(t, !strings.Contains(before.Body.String(), "other-web"))
	removed := httptest.NewRecorder()
	body := fmt.Sprintf(`{"path":%q,"services":["web"],"force":true,"stop":true}`, path)
	request := httptest.NewRequest(http.MethodPost, "/rm/demo", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(removed, request)
	assert.Equal(t, removed.Code, http.StatusOK, removed.Body.String())
	after := httptest.NewRecorder()
	mux.ServeHTTP(after, httptest.NewRequest(http.MethodGet, psURL, http.NoBody))
	assert.Equal(t, after.Code, http.StatusOK, after.Body.String())
	assert.Assert(t, !strings.Contains(after.Body.String(), "web-1"))
	ping := httptest.NewRecorder()
	mux.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/_ping", http.NoBody))
	assert.Equal(t, ping.Code, http.StatusOK)
}

func TestRemoveStopsWatchAndWaitsForItsMutation(t *testing.T) {
	client := mocks.NewMockAPIClient(gomock.NewController(t))
	backend := mocks.NewMockCompose(gomock.NewController(t))
	app := newServerApp(serveConfig{}, func(...composepkg.Option) (composeapi.Compose, error) { return backend, nil }, func() (statsRuntime, error) {
		return statsRuntime{client: client}, nil
	})
	entered := make(chan struct{})
	backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ *types.Project, _ composeapi.UpOptions) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	// The removal list must run after watch releases its mutation token.
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		assert.Equal(t, len(app.watches.snapshot()), 0)
		return mobyclient.ContainerListResult{}, nil
	})
	project := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web"}}}
	assert.NilError(t, app.startWatch("demo", []*types.Project{project}, nil, false, false))
	t.Cleanup(func() { app.stopWatch("demo") })
	awaitMutationSignal(t, entered)
	response, err := app.rmProject(t.Context(), "demo", rmRequest{Force: true, Stop: true})
	assert.NilError(t, err)
	assert.Equal(t, response.OK, true)
}
