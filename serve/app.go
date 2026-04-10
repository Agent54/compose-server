package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/errdefs"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

type backendFactory func() (composeapi.Compose, error)

type serverApp struct {
	config       serveConfig
	backend      backendFactory
	watches      *watchRegistry
	listOverride func(context.Context, composeapi.ListOptions) ([]composeapi.Stack, error)
}

type upRequest struct {
	Path  string `json:"path,omitempty"`
	Build bool   `json:"build,omitempty"`
	Watch bool   `json:"watch,omitempty"`
}

type watchRequest struct {
	Path string `json:"path,omitempty"`
}

type actionResponse struct {
	OK          bool   `json:"ok"`
	Project     string `json:"project"`
	ConfigFiles string `json:"configFiles,omitempty"`
	Watching    bool   `json:"watching,omitempty"`
	WatchURL    string `json:"watchUrl,omitempty"`
}

func newServerApp(config serveConfig, backend backendFactory) *serverApp {
	return &serverApp{
		config:  config,
		backend: backend,
		watches: newWatchRegistry(),
	}
}

func (a *serverApp) listStacks(ctx context.Context, options composeapi.ListOptions) ([]composeapi.Stack, error) {
	if a.listOverride != nil {
		return a.listOverride(ctx, options)
	}
	backend, err := a.backend()
	if err != nil {
		return nil, err
	}
	existing, err := backend.List(ctx, options)
	if err != nil {
		return nil, err
	}
	discovered, err := discoverComposeProjects(ctx, newDiscoveryOptions(a.config), func(ctx context.Context, dir string) (*discoveredProject, error) {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir: dir,
			Offline:    true,
		})
		if err != nil {
			return nil, err
		}
		return &discoveredProject{
			name:        project.Name,
			configFiles: strings.Join(project.ComposeFiles, ","),
		}, nil
	})
	if err != nil {
		return nil, err
	}
	stacks := mergeStacks(existing, discovered)
	watching := a.watches.snapshot()
	for i := range stacks {
		_, stacks[i].Watching = watching[stacks[i].Name]
	}
	return stacks, nil
}

func (a *serverApp) upProject(ctx context.Context, req upRequest) (actionResponse, error) {
	project, backend, err := a.loadProject(ctx, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	build := a.newBuildOptions(project)
	var buildPtr *composeapi.BuildOptions
	if req.Build || req.Watch {
		buildCopy := build
		buildPtr = &buildCopy
	}

	if err := backend.Up(ctx, project, composeapi.UpOptions{
		Create: composeapi.CreateOptions{
			Build:                buildPtr,
			Services:             project.ServiceNames(),
			Recreate:             composeapi.RecreateDiverged,
			RecreateDependencies: composeapi.RecreateNever,
			Inherit:              true,
		},
		Start: composeapi.StartOptions{
			Project:  project,
			Services: project.ServiceNames(),
		},
	}); err != nil {
		return actionResponse{}, err
	}

	if req.Watch {
		if err := a.startWatch(project, build); err != nil {
			return actionResponse{}, err
		}
	}

	return actionResponse{
		OK:          true,
		Project:     project.Name,
		ConfigFiles: strings.Join(project.ComposeFiles, ","),
		Watching:    req.Watch,
		WatchURL:    watchURL(project.Name, req.Watch),
	}, nil
}

func (a *serverApp) restartWatch(ctx context.Context, projectName string, req watchRequest) (actionResponse, error) {
	project, _, err := a.resolveProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	if err := a.startWatch(project, a.newBuildOptions(project)); err != nil {
		return actionResponse{}, err
	}
	return actionResponse{
		OK:          true,
		Project:     project.Name,
		ConfigFiles: strings.Join(project.ComposeFiles, ","),
		Watching:    true,
		WatchURL:    watchURL(project.Name, true),
	}, nil
}

func (a *serverApp) stopWatch(projectName string) bool {
	return a.watches.stop(projectName)
}

func (a *serverApp) streamWatch(ctx context.Context, projectName string, w http.ResponseWriter) error {
	return a.watches.stream(ctx, projectName, w)
}

func (a *serverApp) startWatch(project *types.Project, build composeapi.BuildOptions) error {
	return a.watches.start(project.Name, func(ctx context.Context, consumer composeapi.LogConsumer) error {
		backend, err := a.backend()
		if err != nil {
			return err
		}
		return backend.Watch(ctx, project, composeapi.WatchOptions{
			Build:    &build,
			LogTo:    consumer,
			Prune:    true,
			Services: project.ServiceNames(),
		})
	})
}

func (a *serverApp) loadProject(ctx context.Context, requestPath string) (*types.Project, composeapi.Compose, error) {
	backend, err := a.backend()
	if err != nil {
		return nil, nil, err
	}
	root, err := a.resolvePath(requestPath)
	if err != nil {
		return nil, nil, err
	}
	project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
		WorkingDir: root,
		Offline:    true,
	})
	if err != nil {
		return nil, nil, err
	}
	return project, backend, nil
}

func (a *serverApp) resolveProject(ctx context.Context, projectName, requestPath string) (*types.Project, composeapi.Compose, error) {
	if requestPath != "" {
		project, backend, err := a.loadProject(ctx, requestPath)
		if err != nil {
			return nil, nil, err
		}
		if project.Name != projectName {
			return nil, nil, errdefs.InvalidParameter(fmt.Errorf("project %q does not match requested watch resource %q", project.Name, projectName))
		}
		return project, backend, nil
	}

	backend, err := a.backend()
	if err != nil {
		return nil, nil, err
	}
	dirs, err := findComposeDirectories(newDiscoveryOptions(a.config))
	if err != nil {
		return nil, nil, err
	}
	for _, dir := range dirs {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir: dir,
			Offline:    true,
		})
		if err == nil && project.Name == projectName {
			return project, backend, nil
		}
	}
	return nil, nil, errdefs.NotFound(fmt.Errorf("watch resource %q not found", projectName))
}

func (a *serverApp) resolvePath(requestPath string) (string, error) {
	root := a.config.rootDir
	if requestPath == "" {
		return root, nil
	}
	if filepath.IsAbs(requestPath) {
		return "", errdefs.InvalidParameter(fmt.Errorf("absolute paths are not allowed: %s", requestPath))
	}
	candidate := filepath.Clean(filepath.Join(root, requestPath))
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errdefs.InvalidParameter(fmt.Errorf("path escapes serve root: %s", requestPath))
	}
	return candidate, nil
}

func (a *serverApp) newBuildOptions(project *types.Project) composeapi.BuildOptions {
	return composeapi.BuildOptions{
		Progress: "plain",
		Services: project.ServiceNames(),
		Deps:     true,
	}
}

func decodeJSONBody(r *http.Request, out any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return httputils.ReadJSON(r, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	return httputils.WriteJSON(w, code, v)
}

func watchURL(project string, enabled bool) string {
	if !enabled {
		return ""
	}
	return "/watch/" + project
}

type sseMessage struct {
	Project string    `json:"project"`
	Stream  string    `json:"stream"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}

func marshalSSE(msg sseMessage) ([]byte, error) {
	return json.Marshal(msg)
}
