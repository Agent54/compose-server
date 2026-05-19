package serve

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"gotest.tools/v3/assert"
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
