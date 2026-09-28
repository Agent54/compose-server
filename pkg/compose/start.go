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
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/docker/compose/v5/pkg/api"
)

func (s *composeService) Start(ctx context.Context, projectName string, options api.StartOptions) error {
	return Run(ctx, func(ctx context.Context) error {
		return s.start(ctx, strings.ToLower(projectName), options, nil)
	}, "start", s.events)
}

func (s *composeService) start(ctx context.Context, projectName string, options api.StartOptions, listener api.ContainerEventListener) error {
	project := options.Project
	if project == nil {
		var containers Containers
		containers, err := s.getContainers(ctx, projectName, oneOffExclude, true)
		if err != nil {
			return err
		}

		project, err = s.projectFromName(containers, projectName, options.AttachTo...)
		if err != nil {
			return err
		}
	}
	// resolve the model once: optional depends_on references left dangling by
	// profiles or service selection are pruned before the dependency graph
	// and the dependency waits read them
	project = project.WithoutUnresolvedOptionalDependencies()

	res, err := s.apiClient().ContainerList(ctx, client.ContainerListOptions{
		Filters: projectFilter(project.Name).Add("label", oneOffFilter(false)),
		All:     true,
	})
	if err != nil {
		return err
	}
	containers := Containers(res.Items)
	if options.ContainerID != "" {
		return s.startContainerByID(ctx, project, containers, options, listener)
	}

	err = InDependencyOrder(ctx, project, func(c context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		return s.startService(ctx, project, service, containers, listener, options.WaitTimeout)
	})
	if err != nil {
		return err
	}

	if options.Wait {
		depends := types.DependsOnConfig{}
		for _, s := range project.Services {
			depends[s.Name] = types.ServiceDependency{
				Condition: getDependencyCondition(s, project),
				Required:  true,
			}
		}
		if options.WaitTimeout > 0 {
			withTimeout, cancel := context.WithTimeout(ctx, options.WaitTimeout)
			ctx = withTimeout
			defer cancel()
		}

		err = s.waitDependencies(ctx, project, project.Name, depends, containers, 0)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("application not healthy after %s", options.WaitTimeout)
			}
			return err
		}
	}

	return nil
}

// startContainerByID preserves the service startup pipeline while limiting its
// mutations and health wait to one container. All replicas remain visible when
// deciding whether the service's pre_start hooks need to run.
func (s *composeService) startContainerByID(ctx context.Context, project *types.Project, containers Containers, options api.StartOptions, listener api.ContainerEventListener) error {
	selected := containers.filter(func(ctr container.Summary) bool { return ctr.ID == options.ContainerID })
	if len(selected) != 1 {
		return errdefs.ErrNotFound.WithMessage(fmt.Sprintf("container %q not found in project %q", options.ContainerID, project.Name))
	}
	ctr := selected[0]
	service, err := project.GetService(ctr.Labels[api.ServiceLabel])
	if err != nil {
		return err
	}
	if err := s.waitDependencies(ctx, project, service.Name, service.DependsOn, containers, options.WaitTimeout); err != nil {
		return err
	}
	if isNotRunning(ctr) {
		replicas := containers.filter(isService(service.Name), isNotOneOff)
		if len(service.PreStart) > 0 && len(replicas.filter(isNotRunning)) == len(replicas) {
			if err := s.runPreStart(ctx, project, service, ctr, listener); err != nil {
				return err
			}
		}
		if err := s.startServiceContainer(ctx, project, service, ctr, listener); err != nil {
			return err
		}
	}
	if options.Wait {
		if options.WaitTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, options.WaitTimeout)
			defer cancel()
		}
		return s.waitDependencies(ctx, project, project.Name, types.DependsOnConfig{
			service.Name: {Condition: getDependencyCondition(service, project), Required: true},
		}, selected, 0)
	}
	return nil
}

// getDependencyCondition checks if service is depended on by other services
// with service_completed_successfully condition, and applies that condition
// instead, or --wait will never finish waiting for one-shot containers
func getDependencyCondition(service types.ServiceConfig, project *types.Project) string {
	for _, services := range project.Services {
		for dependencyService, dependencyConfig := range services.DependsOn {
			if dependencyService == service.Name && dependencyConfig.Condition == types.ServiceConditionCompletedSuccessfully {
				return types.ServiceConditionCompletedSuccessfully
			}
		}
	}
	return ServiceConditionRunningOrHealthy
}
