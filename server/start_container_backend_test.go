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
	"archive/tar"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/streams"
	engineserver "github.com/docker/docker/api/server"
	containertypes "github.com/moby/moby/api/types/container"
	imagetypes "github.com/moby/moby/api/types/image"
	networktypes "github.com/moby/moby/api/types/network"
	volumetypes "github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
	"github.com/docker/compose/v5/server/errdefs"
)

// realContainerApp exercises the real loader, reconciler and startup pipeline.
// Only Docker calls are mocked; unexpected container/resource mutations fail.
func realContainerApp(t *testing.T, model string) (*serverApp, *mocks.MockAPIClient, string) {
	t.Helper()
	directory := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(model), 0o600))
	ctrl := gomock.NewController(t)
	client := mocks.NewMockAPIClient(ctrl)
	cli := mocks.NewMockCli(ctrl)
	cli.EXPECT().Client().Return(client).AnyTimes()
	cli.EXPECT().ConfigFile().Return(configfile.New("")).AnyTimes()
	cli.EXPECT().ServerInfo().Return(command.ServerInfo{OSType: "linux"}).AnyTimes()
	cli.EXPECT().Out().Return(streams.NewOut(io.Discard)).AnyTimes()
	cli.EXPECT().Err().Return(streams.NewOut(io.Discard)).AnyTimes()
	client.EXPECT().DaemonHost().Return("unix:///docker.sock").AnyTimes()
	client.EXPECT().Ping(gomock.Any(), gomock.Any()).Return(mobyclient.PingResult{APIVersion: "1.44"}, nil).AnyTimes()
	client.EXPECT().ClientVersion().Return("1.44").AnyTimes()
	client.EXPECT().ImageInspect(gomock.Any(), "test-image").Return(mobyclient.ImageInspectResult{
		InspectResponse: imagetypes.InspectResponse{ID: "sha256:test"},
	}, nil).AnyTimes()
	app := newServerApp(serveConfig{rootDir: directory}, func(options ...composepkg.Option) (composeapi.Compose, error) {
		return composepkg.NewComposeService(cli, options...)
	}, func() (statsRuntime, error) { return statsRuntime{client: client}, nil })
	return app, client, directory
}

// serviceSummary supplies the project identity labels used by both API layers.
func serviceSummary(directory, service, number string) containertypes.Summary {
	return containertypes.Summary{
		ID: service + "-" + number, Names: []string{"/demo-" + service + "-" + number}, State: containertypes.StateExited,
		Labels: map[string]string{
			composeapi.ProjectLabel: "demo", composeapi.ServiceLabel: service,
			composeapi.ContainerNumberLabel: number, composeapi.ConfigHashLabel: "hash",
			composeapi.ConfigFilesLabel: filepath.Join(directory, "compose.yaml"), composeapi.OneoffLabel: "False",
		},
	}
}

func TestServiceContainerHTTPPreservesReplicasAndLifecycle(t *testing.T) {
	app, client, directory := realContainerApp(t, `name: demo
services:
  web:
    image: test-image
    network_mode: none
    profiles: [debug]
    deploy:
      replicas: 3
    depends_on:
      db:
        condition: service_healthy
    secrets: [token]
    configs: [settings]
    pre_start:
      - command: [init]
    post_start:
      - command: [notify]
  db:
    image: test-image
    env_file: missing-dependency.env
  unused:
    image: test-image
    env_file: missing-unused.env
secrets:
  token:
    environment: SERVICE_START_TOKEN
configs:
  settings:
    content: configured
`)
	t.Setenv("SERVICE_START_TOKEN", "secret")
	first := serviceSummary(directory, "web", "1")
	second := serviceSummary(directory, "web", "2")
	db := serviceSummary(directory, "db", "1")
	// Replica two is running: service-level pre_start must not run, and neither
	// it nor the stopped dependency may be started or recreated by this request.
	second.State = containertypes.StateRunning
	containers := mobyclient.ContainerListResult{Items: []containertypes.Summary{second, db, first}}
	gomock.InOrder(
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(containers, nil),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(containers, nil),
		client.EXPECT().CopyToContainer(gomock.Any(), first.ID, gomock.Any()).DoAndReturn(checkCopiedFile(t, "run/secrets/token", "secret")),
		client.EXPECT().CopyToContainer(gomock.Any(), first.ID, gomock.Any()).DoAndReturn(checkCopiedFile(t, "settings", "configured")),
		client.EXPECT().ContainerStart(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ContainerStartResult{}, nil),
		client.EXPECT().ExecCreate(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ExecCreateResult{ID: "post-start"}, nil),
		client.EXPECT().ExecAttach(gomock.Any(), "post-start", gomock.Any()).Return(emptyExecAttach(t), nil),
		client.EXPECT().ExecInspect(gomock.Any(), "post-start", gomock.Any()).Return(mobyclient.ExecInspectResult{}, nil),
	)
	server := &engineserver.Server{}
	mux := server.CreateMux(t.Context(), newRouter(app))
	request := httptest.NewRequest(http.MethodPost, "/start/demo/container", strings.NewReader(`{"path":"`+directory+`","service":"web"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	assert.Equal(t, response.Code, http.StatusOK, response.Body.String())
	activity := app.listBuilds()
	assert.Equal(t, len(activity), 1)
	assert.Equal(t, activity[0].Status, "succeeded")
	stream := httptest.NewRecorder()
	assert.NilError(t, app.streamBuild(t.Context(), activity[0].ID, stream))
	assert.Assert(t, strings.Contains(stream.Body.String(), "Starting"), stream.Body.String())
	assert.Assert(t, strings.Contains(stream.Body.String(), "Started"), stream.Body.String())
}

func TestServiceContainerCreatesOnlyFirstAndRunsPreStart(t *testing.T) {
	app, client, directory := realContainerApp(t, `name: demo
services:
  web:
    image: test-image
    network_mode: none
    profiles: [debug]
    scale: 3
    depends_on: [db]
    pre_start:
      - command: [init]
  db:
    image: test-image
    env_file: missing.env
`)
	first := serviceSummary(directory, "web", "1")
	db := serviceSummary(directory, "db", "1")
	gomock.InOrder(
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{db}}, nil),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{db}}, nil),
		client.EXPECT().ContainerCreate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, options mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
			assert.Equal(t, options.Name, "demo-web-1")
			assert.Equal(t, options.Config.Labels[composeapi.ServiceLabel], "web")
			assert.Equal(t, options.Config.Labels[composeapi.ContainerNumberLabel], "1")
			assert.Equal(t, options.Config.Labels[composeapi.DependenciesLabel], "")
			first.Labels = options.Config.Labels
			return mobyclient.ContainerCreateResult{ID: first.ID}, nil
		}),
		client.EXPECT().ContainerInspect(gomock.Any(), first.ID, gomock.Any()).DoAndReturn(func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
			return mobyclient.ContainerInspectResult{Container: containertypes.InspectResponse{
				ID: first.ID, Name: first.Names[0], Config: &containertypes.Config{Labels: first.Labels}, NetworkSettings: &containertypes.NetworkSettings{},
			}}, nil
		}),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
			return mobyclient.ContainerListResult{Items: []containertypes.Summary{first, db}}, nil
		}),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
			return mobyclient.ContainerListResult{Items: []containertypes.Summary{first, db}}, nil
		}),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{}, nil),
		client.EXPECT().ContainerCreate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, options mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
			assert.Equal(t, options.Config.Labels[composeapi.HookLabel], "pre_start")
			assert.DeepEqual(t, options.HostConfig.VolumesFrom, []string{first.ID})
			return mobyclient.ContainerCreateResult{ID: "pre-start"}, nil
		}),
		client.EXPECT().ContainerWait(gomock.Any(), "pre-start", gomock.Any()).Return(completedContainerWait(0)),
		client.EXPECT().ContainerLogs(gomock.Any(), "pre-start", gomock.Any()).Return(io.NopCloser(strings.NewReader("")), nil),
		client.EXPECT().ContainerStart(gomock.Any(), "pre-start", gomock.Any()).Return(mobyclient.ContainerStartResult{}, nil),
		client.EXPECT().ContainerRemove(gomock.Any(), "pre-start", mobyclient.ContainerRemoveOptions{RemoveVolumes: true}).Return(mobyclient.ContainerRemoveResult{}, nil),
		client.EXPECT().ContainerStart(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ContainerStartResult{}, nil),
	)
	client.EXPECT().NetworkList(gomock.Any(), gomock.Any()).Return(mobyclient.NetworkListResult{}, nil)
	client.EXPECT().VolumeList(gomock.Any(), gomock.Any()).Return(mobyclient.VolumeListResult{}, nil)
	response, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
	assert.NilError(t, err)
	assert.Equal(t, response.OK, true)
}

// completedContainerWait supplies a buffered hook exit result.
func completedContainerWait(code int64) mobyclient.ContainerWaitResult {
	result := make(chan containertypes.WaitResponse, 1)
	result <- containertypes.WaitResponse{StatusCode: code}
	return mobyclient.ContainerWaitResult{Result: result, Error: make(chan error)}
}

// checkCopiedFile verifies the injected archive contents, not just the API call.
func checkCopiedFile(t *testing.T, path, content string) func(context.Context, string, mobyclient.CopyToContainerOptions) (mobyclient.CopyToContainerResult, error) {
	t.Helper()
	return func(_ context.Context, _ string, options mobyclient.CopyToContainerOptions) (mobyclient.CopyToContainerResult, error) {
		reader := tar.NewReader(options.Content)
		header, err := reader.Next()
		assert.NilError(t, err)
		assert.Equal(t, strings.TrimPrefix(header.Name, "/"), path)
		data, err := io.ReadAll(reader)
		assert.NilError(t, err)
		assert.Equal(t, string(data), content)
		return mobyclient.CopyToContainerResult{}, nil
	}
}

// emptyExecAttach provides a completed hook stream without a Docker daemon.
func emptyExecAttach(t *testing.T) mobyclient.ExecAttachResult {
	t.Helper()
	writer, reader := net.Pipe()
	assert.NilError(t, writer.Close())
	t.Cleanup(func() { _ = reader.Close() })
	return mobyclient.ExecAttachResult{HijackedResponse: mobyclient.NewHijackedResponse(reader, "")}
}

func TestServiceContainerRejectsConfigOwnershipConflict(t *testing.T) {
	for _, ownership := range []string{"different-file", "extra-override"} {
		t.Run(ownership, func(t *testing.T) {
			app, client, directory := realContainerApp(t, "name: demo\nservices:\n  web:\n    image: test-image\n    network_mode: none\n")
			first := serviceSummary(directory, "web", "1")
			second := serviceSummary(directory, "web", "2")
			if ownership == "different-file" {
				second.Labels[composeapi.ConfigFilesLabel] = "/another/compose.yaml"
			} else {
				second.Labels[composeapi.ConfigFilesLabel] += "," + filepath.Join(directory, "override.yaml")
			}
			client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{first, second}}, nil)
			_, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
			assert.Assert(t, errdefs.IsConflict(err))
		})
	}
}

func TestServiceContainerIgnoresOneOffsAndRetainedHooks(t *testing.T) {
	app, client, directory := realContainerApp(t, "name: demo\nservices:\n  web:\n    image: test-image\n    network_mode: none\n")
	first := serviceSummary(directory, "web", "1")
	oneoff := containertypes.Summary{ID: "oneoff", Labels: map[string]string{composeapi.ServiceLabel: "web", composeapi.OneoffLabel: "True"}}
	hook := containertypes.Summary{ID: "hook", Labels: map[string]string{composeapi.ServiceLabel: "web", composeapi.HookLabel: "pre_start"}}
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{hook, oneoff, first}}, nil)
	client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{first}}, nil)
	client.EXPECT().ContainerStart(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ContainerStartResult{}, nil)
	_, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
	assert.NilError(t, err)
}

func TestServiceContainerAdditiveCreateRejectsConcurrentReplicas(t *testing.T) {
	app, client, directory := realContainerApp(t, "name: demo\nservices:\n  web:\n    image: test-image\n    network_mode: none\n    scale: 3\n")
	gomock.InOrder(
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{}, nil),
		client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{
			serviceSummary(directory, "web", "1"), serviceSummary(directory, "web", "2"),
		}}, nil),
	)
	client.EXPECT().NetworkList(gomock.Any(), gomock.Any()).Return(mobyclient.NetworkListResult{}, nil)
	client.EXPECT().VolumeList(gomock.Any(), gomock.Any()).Return(mobyclient.VolumeListResult{}, nil)
	_, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
	assert.Assert(t, errdefs.IsConflict(err))
}

func TestServiceContainerAdditiveCreatePreservesSharedResources(t *testing.T) {
	for _, resource := range []string{"network", "volume", "unchanged"} {
		t.Run(resource, func(t *testing.T) {
			app, client, directory := realContainerApp(t, `name: demo
services:
  web:
    image: test-image
    volumes: [data:/data]
volumes:
  data: {}
`)
			initial := client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{}, nil).Times(2)
			networkHash, volumeHash := "", ""
			switch resource {
			case "network":
				networkHash = "diverged"
			case "volume":
				volumeHash = "diverged"
			}
			client.EXPECT().NetworkList(gomock.Any(), gomock.Any()).Return(mobyclient.NetworkListResult{Items: []networktypes.Summary{{
				Network: networktypes.Network{ID: "shared-network", Name: "demo_default", Labels: map[string]string{
					composeapi.NetworkLabel: "default", composeapi.ProjectLabel: "demo", composeapi.ConfigHashLabel: networkHash,
				}},
			}}}, nil)
			client.EXPECT().VolumeList(gomock.Any(), gomock.Any()).Return(mobyclient.VolumeListResult{Items: []volumetypes.Volume{{
				Name: "demo_data", Labels: map[string]string{
					composeapi.VolumeLabel: "data", composeapi.ProjectLabel: "demo", composeapi.ConfigHashLabel: volumeHash,
				},
			}}}, nil)
			if resource == "unchanged" {
				first := serviceSummary(directory, "web", "1")
				client.EXPECT().ContainerCreate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, options mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
					assert.Equal(t, options.Name, "demo-web-1")
					assert.Equal(t, len(options.HostConfig.Binds), 1)
					assert.Assert(t, strings.HasPrefix(options.HostConfig.Binds[0], "demo_data:/data:"))
					return mobyclient.ContainerCreateResult{ID: first.ID}, nil
				})
				client.EXPECT().ContainerInspect(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ContainerInspectResult{
					Container: containertypes.InspectResponse{ID: first.ID, Name: first.Names[0], Config: &containertypes.Config{Labels: first.Labels}, NetworkSettings: &containertypes.NetworkSettings{}},
				}, nil)
				client.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(mobyclient.ContainerListResult{Items: []containertypes.Summary{first}}, nil).Times(2).After(initial)
				client.EXPECT().ContainerStart(gomock.Any(), first.ID, gomock.Any()).Return(mobyclient.ContainerStartResult{}, nil)
			}
			_, err := app.startContainer(t.Context(), "demo", containerActionRequest{Path: directory, Service: "web"})
			if resource == "unchanged" {
				assert.NilError(t, err)
			} else {
				assert.Assert(t, errdefs.IsConflict(err))
			}
		})
	}
}
