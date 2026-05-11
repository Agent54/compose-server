package serve

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
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
