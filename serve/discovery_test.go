package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
)

func TestFindComposeDirectoriesHonorsDepthAndExclusions(t *testing.T) {
	root := t.TempDir()

	assert.NilError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "a", "compose.yaml"), []byte("services: {}\n"), 0o644))

	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".git", "nested"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".git", "nested", "compose.yaml"), []byte("services: {}\n"), 0o644))

	assert.NilError(t, os.MkdirAll(filepath.Join(root, "deep", "b", "c", "d", "e"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "deep", "b", "c", "d", "e", "compose.yaml"), []byte("services: {}\n"), 0o644))

	dirs, err := findComposeDirectories(discoveryOptions{
		rootDir:     root,
		maxDepth:    4,
		excludedDir: []string{".git"},
	})
	assert.NilError(t, err)
	assert.Equal(t, len(dirs), 1)
	assert.Equal(t, dirs[0], filepath.Join(root, "a"))
}

func TestDiscoverComposeProjectsUsesParsedServiceCountForStatus(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "demo")
	assert.NilError(t, os.MkdirAll(projectDir, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte(`
services:
  web:
    image: nginx:latest
  db:
    image: postgres:latest
`), 0o644))

	service, err := composepkg.NewComposeService(nil)
	assert.NilError(t, err)

	projects, err := discoverComposeProjects(t.Context(), discoveryOptions{
		rootDir:     root,
		maxDepth:    2,
		excludedDir: defaultExcludedDirs,
	}, func(ctx context.Context, dir string) (composeapi.Stack, error) {
		project, err := service.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir: dir,
			Offline:    true,
		})
		if err != nil {
			return composeapi.Stack{}, err
		}
		return composepkg.StackForLoadedProject(project, "uncreated"), nil
	})
	assert.NilError(t, err)
	assert.Equal(t, len(projects), 1)
	assert.Equal(t, projects[0].Name, "demo")
	assert.Equal(t, projects[0].Status, "uncreated(2)")
	assert.Equal(t, projects[0].ConfigFiles, filepath.Join(projectDir, "compose.yaml"))
}

func TestParsedProjectContainersUseProjectMetadata(t *testing.T) {
	project := &types.Project{
		Name: "demo",
		Services: types.Services{
			"web": {
				Name:         "web",
				Image:        "nginx:latest",
				ContainerName: "",
				CustomLabels: map[string]string{
					composeapi.ProjectLabel: "demo",
					composeapi.ServiceLabel: "web",
				},
			},
			"db": {
				Name:  "db",
				Image: "postgres:latest",
			},
		},
	}

	containers := parsedProjectContainers(project, []string{"web"})

	assert.Equal(t, len(containers), 1)
	assert.Equal(t, containers[0].Name, "demo-web-1")
	assert.Equal(t, containers[0].Project, "demo")
	assert.Equal(t, containers[0].Service, "web")
	assert.Equal(t, containers[0].Image, "nginx:latest")
	assert.Equal(t, containers[0].Status, "uncreated")
	assert.Equal(t, string(containers[0].State), "uncreated")
	assert.DeepEqual(t, containers[0].Labels, map[string]string{
		composeapi.ProjectLabel: "demo",
		composeapi.ServiceLabel: "web",
	})
}
