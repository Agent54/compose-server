package serve

import (
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestBuildHistoryRetainsServicesWithoutSharingMutableSlices(t *testing.T) {
	registry := newBuildRegistry()
	services := []string{"vscode", "darc", "vscode"}
	id := registry.start("darc", services...)
	services[0] = "other"
	registry.finish(id, false)
	history := registry.list()
	assert.DeepEqual(t, history[0].Services, []string{"darc", "vscode"})
	assert.Equal(t, history[0].Status, "failed")
	history[0].Services[0] = "other"
	assert.DeepEqual(t, registry.list()[0].Services, []string{"darc", "vscode"})
}

func TestUpBuildHistoryIdentifiesServiceAndRecordsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			backend := mocks.NewMockCompose(gomock.NewController(t))
			app := newServerApp(serveConfig{rootDir: t.TempDir()}, func(...composepkg.Option) (composeapi.Compose, error) {
				return backend, nil
			}, nil)
			project := &types.Project{Name: "darc", Services: types.Services{"vscode": {Name: "vscode", Image: "editor"}}}
			backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil).Times(2)
			var upErr error
			if fail {
				upErr = errors.New("Rust download failed")
			}
			backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).Return(upErr)
			_, err := app.upProject(t.Context(), upRequest{Project: "darc", Path: app.config.rootDir, Build: true})
			assert.Assert(t, errors.Is(err, upErr))
			history := app.listBuilds()
			assert.DeepEqual(t, history[0].Services, []string{"vscode"})
			assert.Equal(t, *history[0].Success, !fail)
			if fail {
				messages := app.builds.builds[history[0].ID].messages
				assert.Equal(t, messages[len(messages)-2].Message, "Rust download failed")
			}
		})
	}
}
