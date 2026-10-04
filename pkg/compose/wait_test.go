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

package compose

import (
	"testing"

	"github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/api"
)

func TestWaitOnExitedContainer(t *testing.T) {
	apiClient, cli := prepareMocks(gomock.NewController(t))
	tested, err := NewComposeService(cli)
	assert.NilError(t, err)

	apiClient.EXPECT().ContainerList(gomock.Any(), client.ContainerListOptions{
		Filters: getDefaultFilters("project", oneOffInclude, "app"),
		All:     true,
	}).Return(client.ContainerListResult{
		Items: Containers{testContainer("app", "app-1", false)},
	}, nil)
	apiClient.EXPECT().ContainerWait(gomock.Any(), "app-1", client.ContainerWaitOptions{}).
		Return(waitResultExit(7))

	code, err := tested.Wait(t.Context(), "project", api.WaitOptions{Services: []string{"app"}})
	assert.NilError(t, err)
	assert.Equal(t, code, int64(7))
}
