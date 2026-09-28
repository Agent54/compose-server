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
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/server/errdefs"
)

// startServiceContainer creates at most one service replica, then starts it through Compose.
// Dependencies and other replicas are never started by this operation.
func (a *serverApp) startServiceContainer(ctx context.Context, projectName string, req containerActionRequest) (actionResponse, error) {
	if req.Container != "" || req.Path == "" {
		return actionResponse{}, errdefs.InvalidParameter(fmt.Errorf("service start requires path and cannot include container"))
	}
	unlock, err := a.lockProjectMutation(ctx, projectName)
	if err != nil {
		return actionResponse{}, err
	}
	defer unlock()
	project, backend, err := a.loadServiceProject(ctx, projectName, req.Path, req.Service)
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
	if err := backend.Start(ctx, project.Name, composeapi.StartOptions{Project: project, ContainerID: ctr.ID}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, project.Name, fmt.Sprintf("started container %s", containerDisplayName(ctr))), nil
}

// loadServiceProject activates the named service's profiles during loading, then
// excludes dependencies before resolving env files or other runtime resources.
func (a *serverApp) loadServiceProject(ctx context.Context, name, path, service string) (*types.Project, composeapi.Compose, error) {
	ref, err := a.resolveProjectLoadRef(path)
	if err != nil {
		return nil, nil, err
	}
	backend, err := a.backend()
	if err != nil {
		return nil, nil, err
	}
	project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
		WorkingDir: ref.workingDir, ConfigPaths: ref.configPaths, Services: []string{service},
		Offline: true, ProjectOptionsFns: runtimeProjectLoadOptions(),
	})
	if err != nil {
		return nil, nil, err
	}
	if project.Name != name {
		return nil, nil, errdefs.InvalidParameter(fmt.Errorf("project %q does not match requested project %q", project.Name, name))
	}
	project, err = project.WithSelectedServices([]string{service}, types.IgnoreDependencies)
	if err != nil {
		return nil, nil, errdefs.InvalidParameter(err)
	}
	project, err = runtimeProject(project.WithoutUnnecessaryResources())
	return project, backend, err
}

// ensureSingleServiceContainer reuses replica one or creates it additively when
// the entire project/service identity is absent. Other replicas are preserved.
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
		Additive: true,
	}); err != nil {
		return containertypes.Summary{}, err
	}
	containers, err = singleServiceContainers(ctx, client, project, service)
	if err != nil {
		return containertypes.Summary{}, err
	}
	return firstServiceContainer(containers)
}

// singleServiceContainers observes the same project/service identity as Create.
// A config-file mismatch is an ownership conflict, not an absent container.
func singleServiceContainers(ctx context.Context, client mobyclient.APIClient, project *types.Project, service string) ([]containertypes.Summary, error) {
	containers, err := listProjectContainers(ctx, client, project.Name, true, []string{service})
	if err != nil {
		return nil, err
	}
	regular := make([]containertypes.Summary, 0, len(containers))
	for _, ctr := range containers {
		if ctr.Labels[composeapi.HookLabel] != "" {
			continue
		}
		if !strings.EqualFold(ctr.Labels[composeapi.OneoffLabel], "true") {
			if !slices.Equal(splitPathList(ctr.Labels[composeapi.ConfigFilesLabel]), project.ComposeFiles) {
				return nil, errdefs.Conflict(fmt.Errorf("container %s belongs to project %q service %q with different config files", containerDisplayName(ctr), project.Name, service))
			}
			regular = append(regular, ctr)
		}
	}
	return regular, nil
}

// firstServiceContainer selects replica one without converging the service scale.
func firstServiceContainer(containers []containertypes.Summary) (containertypes.Summary, error) {
	for _, ctr := range containers {
		if ctr.Labels[composeapi.ContainerNumberLabel] == "1" {
			return ctr, nil
		}
	}
	// Do not scale down an existing service just because its first replica is missing.
	return containertypes.Summary{}, errdefs.NotFound(fmt.Errorf("first service container is missing; select an existing container by ID or name"))
}
