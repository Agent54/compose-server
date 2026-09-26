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
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/server/errdefs"
)

// startServiceContainer creates at most one container, then starts it by ID.
// Dependencies and other replicas are never started by this operation.
func (a *serverApp) startServiceContainer(ctx context.Context, projectName string, req containerActionRequest) (actionResponse, error) {
	if req.Container != "" || req.Path == "" {
		return actionResponse{}, errdefs.InvalidParameter(fmt.Errorf("service start requires path and cannot include container"))
	}
	// Serialize create-if-missing requests so each observes the previous create.
	a.containerStartMu.Lock()
	defer a.containerStartMu.Unlock()
	if err := ctx.Err(); err != nil {
		return actionResponse{}, err
	}
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	runtime, err := a.dockerRuntime()
	if err != nil {
		return actionResponse{}, err
	}
	ctr, err := ensureSingleServiceContainer(ctx, backend, runtime.client, project, req.Service)
	if err != nil {
		return actionResponse{}, err
	}
	if _, err := runtime.client.ContainerStart(ctx, ctr.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, fmt.Sprintf("started container %s", containerDisplayName(ctr))), nil
}

func ensureSingleServiceContainer(ctx context.Context, backend composeapi.Compose, client mobyclient.APIClient, project *types.Project, service string) (containertypes.Summary, error) {
	selected, err := project.WithSelectedServices([]string{service}, types.IgnoreDependencies)
	if err != nil {
		return containertypes.Summary{}, errdefs.InvalidParameter(err)
	}
	containers, err := singleServiceContainers(ctx, client, project, service)
	if err != nil {
		return containertypes.Summary{}, err
	}
	if len(containers) > 0 {
		return firstServiceContainer(containers)
	}

	config := selected.Services[service]
	config.SetScale(1)
	selected.Services[service] = config
	selected = selected.WithoutUnnecessaryResources()
	if err := backend.Create(ctx, selected, composeapi.CreateOptions{
		Services: []string{service}, Recreate: composeapi.RecreateNever,
		RecreateDependencies: composeapi.RecreateNever, IgnoreOrphans: true,
	}); err != nil {
		return containertypes.Summary{}, err
	}
	containers, err = singleServiceContainers(ctx, client, project, service)
	if err != nil {
		return containertypes.Summary{}, err
	}
	return firstServiceContainer(containers)
}

func singleServiceContainers(ctx context.Context, client mobyclient.APIClient, project *types.Project, service string) ([]containertypes.Summary, error) {
	containers, err := listProjectContainers(ctx, client, project.Name, true, []string{service})
	if err != nil {
		return nil, err
	}
	containers = filterContainersByConfigFiles(containers, project.ComposeFiles)
	regular := make([]containertypes.Summary, 0, len(containers))
	for _, ctr := range containers {
		if !strings.EqualFold(ctr.Labels[composeapi.OneoffLabel], "true") {
			regular = append(regular, ctr)
		}
	}
	return regular, nil
}

func firstServiceContainer(containers []containertypes.Summary) (containertypes.Summary, error) {
	for _, ctr := range containers {
		if ctr.Labels[composeapi.ContainerNumberLabel] == "1" {
			return ctr, nil
		}
	}
	// Do not scale down an existing service just because its first replica is missing.
	return containertypes.Summary{}, errdefs.NotFound(fmt.Errorf("first service container is missing; create it in Compose"))
}
