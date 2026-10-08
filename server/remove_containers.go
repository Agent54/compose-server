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

	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/server/errdefs"
)

// Remove existing containers by their recorded identity. Their Compose files
// and service definitions may already have been deleted from the stacks tree.
func (a *serverApp) removeProjectContainers(ctx context.Context, projectName string, req rmRequest) (actionResponse, error) {
	if projectName == "" {
		return actionResponse{}, errdefs.InvalidParameter(fmt.Errorf("project is required"))
	}
	if _, err := a.resolveContainerPaths(req.Path); err != nil {
		return actionResponse{}, err
	}
	runtime, err := a.dockerRuntime()
	if err != nil {
		return actionResponse{}, err
	}
	name := strings.ToLower(projectName)
	// A watch owns the mutation token and could recreate the removed service.
	a.stopWatch(name)
	unlock, err := a.lockProjectMutation(ctx, name)
	if err != nil {
		return actionResponse{}, err
	}
	defer unlock()
	containers, err := a.listResourceContainers(ctx, runtime.client, name, req.Path, true, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	for _, ctr := range containers {
		if strings.EqualFold(ctr.Labels[composeapi.OneoffLabel], "true") {
			continue
		}
		if !req.Stop && (ctr.State == containertypes.StateRunning || ctr.State == containertypes.StateRestarting || ctr.State == containertypes.StatePaused) {
			continue
		}
		if req.Stop {
			if _, err := runtime.client.ContainerStop(ctx, ctr.ID, mobyclient.ContainerStopOptions{}); err != nil && !errdefs.IsNotFound(err) {
				return actionResponse{}, err
			}
		}
		if _, err := runtime.client.ContainerRemove(ctx, ctr.ID, mobyclient.ContainerRemoveOptions{Force: req.Force}); err != nil && !errdefs.IsNotFound(err) {
			return actionResponse{}, err
		}
	}
	return actionResponse{OK: true, Project: name}, nil
}
