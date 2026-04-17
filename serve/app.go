package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
)

type backendFactory func() (composeapi.Compose, error)
type statsRuntimeFactory func() (statsRuntime, error)

type statsRuntime struct {
	client mobyclient.APIClient
	osType string
}

type serverApp struct {
	config       serveConfig
	backend      backendFactory
	stats        statsRuntimeFactory
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

type projectActionRequest struct {
	Path            string   `json:"path,omitempty"`
	Services        []string `json:"services,omitempty"`
	TimeoutSeconds  *int     `json:"timeoutSeconds,omitempty"`
	Wait            bool     `json:"wait,omitempty"`
	WaitTimeoutSecs *int     `json:"waitTimeoutSeconds,omitempty"`
	NoDeps          bool     `json:"noDeps,omitempty"`
	RemoveOrphans   bool     `json:"removeOrphans,omitempty"`
	Images          string   `json:"images,omitempty"`
	Volumes         bool     `json:"volumes,omitempty"`
	Signal          string   `json:"signal,omitempty"`
}

type commitRequest struct {
	Path      string   `json:"path,omitempty"`
	Service   string   `json:"service"`
	Reference string   `json:"reference,omitempty"`
	Pause     *bool    `json:"pause,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	Author    string   `json:"author,omitempty"`
	Changes   []string `json:"changes,omitempty"`
	Index     int      `json:"index,omitempty"`
}

type actionResponse struct {
	OK          bool   `json:"ok"`
	Project     string `json:"project"`
	ConfigFiles string `json:"configFiles,omitempty"`
	Watching    bool   `json:"watching,omitempty"`
	WatchURL    string `json:"watchUrl,omitempty"`
	Message     string `json:"message,omitempty"`
}

func newServerApp(config serveConfig, backend backendFactory, stats statsRuntimeFactory) *serverApp {
	return &serverApp{
		config:  config,
		backend: backend,
		stats:   stats,
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
	discoveredProjects, err := a.discoverMergedProjects(ctx, backend)
	if err != nil {
		return nil, err
	}
	discovered := make([]composeapi.Stack, 0, len(discoveredProjects))
	for _, project := range discoveredProjects {
		discovered = append(discovered, composepkg.StackForLoadedProject(project, "uncreated"))
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

func (a *serverApp) startProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	waitTimeout, err := durationFromSeconds(req.WaitTimeoutSecs)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Start(ctx, name, composeapi.StartOptions{
		Project:     project,
		AttachTo:    req.Services,
		Services:    req.Services,
		Wait:        req.Wait,
		WaitTimeout: waitTimeout,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) stopProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Stop(ctx, name, composeapi.StopOptions{
		Project:  project,
		Services: req.Services,
		Timeout:  timeout,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) restartProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Restart(ctx, name, composeapi.RestartOptions{
		Project:  project,
		Services: req.Services,
		Timeout:  timeout,
		NoDeps:   req.NoDeps,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) downProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Down(ctx, name, composeapi.DownOptions{
		RemoveOrphans: req.RemoveOrphans,
		Project:       project,
		Timeout:       timeout,
		Images:        req.Images,
		Volumes:       req.Volumes,
		Services:      req.Services,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) pauseProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Pause(ctx, name, composeapi.PauseOptions{
		Project:  project,
		Services: req.Services,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, "pause freezes container processes in place; stop asks them to exit and leaves the containers stopped"), nil
}

func (a *serverApp) unpauseProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.UnPause(ctx, name, composeapi.PauseOptions{
		Project:  project,
		Services: req.Services,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) killProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	project, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	if err := backend.Kill(ctx, name, composeapi.KillOptions{
		RemoveOrphans: req.RemoveOrphans,
		Project:       project,
		Services:      req.Services,
		Signal:        req.Signal,
	}); err != nil {
		return actionResponse{}, err
	}
	return a.actionResult(project, name, false, ""), nil
}

func (a *serverApp) commitProject(ctx context.Context, projectName string, req commitRequest) (actionResponse, error) {
	if req.Service == "" {
		return actionResponse{}, errdefs.InvalidParameter(fmt.Errorf("service is required"))
	}
	_, backend, name, err := a.resolveActionProject(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	pause := true
	if req.Pause != nil {
		pause = *req.Pause
	}
	changes := opts.NewListOpts(nil)
	for _, change := range req.Changes {
		if err := changes.Set(change); err != nil {
			return actionResponse{}, errdefs.InvalidParameter(err)
		}
	}
	if err := backend.Commit(ctx, name, composeapi.CommitOptions{
		Service:   req.Service,
		Reference: req.Reference,
		Pause:     pause,
		Comment:   req.Comment,
		Author:    req.Author,
		Changes:   changes,
		Index:     req.Index,
	}); err != nil {
		return actionResponse{}, err
	}
	return actionResponse{
		OK:      true,
		Project: name,
		Message: "commit created an image from the selected service container",
	}, nil
}

func (a *serverApp) psProject(ctx context.Context, projectName string, path string, services []string, all bool, statuses []string) ([]composeapi.ContainerSummary, error) {
	project, backend, name, err := a.resolvePSProject(ctx, projectName, path)
	if err != nil {
		return nil, err
	}
	containers, err := backend.Ps(ctx, name, composeapi.PsOptions{
		Project:  project,
		All:      all || len(statuses) > 0,
		Services: services,
	})
	if err != nil {
		if project == nil || !errdefs.IsNotFound(err) {
			return nil, err
		}
		containers = nil
	}
	if len(containers) == 0 && project != nil {
		containers = parsedProjectContainers(project, services)
	}
	if len(statuses) > 0 {
		containers = filterByStatus(containers, statuses)
	}
	slices.SortFunc(containers, func(a, b composeapi.ContainerSummary) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return containers, nil
}

func (a *serverApp) resolvePSProject(ctx context.Context, projectName, requestPath string) (*types.Project, composeapi.Compose, string, error) {
	if requestPath != "" {
		return a.resolveActionProject(ctx, projectName, requestPath)
	}

	backend, err := a.backend()
	if err != nil {
		return nil, nil, "", err
	}
	if projectName == "" {
		return nil, nil, "", errdefs.InvalidParameter(fmt.Errorf("project is required"))
	}

	project, err := a.findMergedProjectByName(ctx, backend, projectName)
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, nil, "", err
	}
	if err != nil {
		project = nil
	}
	return project, backend, projectName, nil
}

func (a *serverApp) topProject(ctx context.Context, projectName, path string, services []string) ([]composeapi.ContainerProcSummary, error) {
	_, backend, name, err := a.resolveActionProject(ctx, projectName, path)
	if err != nil {
		return nil, err
	}
	containers, err := backend.Top(ctx, name, services)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(containers, func(a, b composeapi.ContainerProcSummary) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return containers, nil
}

func (a *serverApp) volumesProject(ctx context.Context, projectName, path string, services []string) ([]composeapi.VolumesSummary, error) {
	_, backend, name, err := a.resolveActionProject(ctx, projectName, path)
	if err != nil {
		return nil, err
	}
	return backend.Volumes(ctx, name, composeapi.VolumesOptions{Services: services})
}

func (a *serverApp) streamEvents(ctx context.Context, projectName, path string, services []string, since, until string, asJSON bool, w http.ResponseWriter) error {
	_, backend, name, err := a.resolveActionProject(ctx, projectName, path)
	if err != nil {
		return err
	}
	stream, err := newTextSSEStream(w)
	if err != nil {
		return err
	}
	return backend.Events(ctx, name, composeapi.EventsOptions{
		Services: services,
		Since:    since,
		Until:    until,
		Consumer: func(event composeapi.Event) error {
			return stream.Send("message", renderComposeEvent(event, asJSON))
		},
	})
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

func (a *serverApp) resolveActionProject(ctx context.Context, projectName, requestPath string) (*types.Project, composeapi.Compose, string, error) {
	if requestPath != "" {
		project, backend, err := a.loadProject(ctx, requestPath)
		if err != nil {
			return nil, nil, "", err
		}
		if projectName != "" && project.Name != projectName {
			return nil, nil, "", errdefs.InvalidParameter(fmt.Errorf("project %q does not match requested project %q", project.Name, projectName))
		}
		return project, backend, project.Name, nil
	}
	backend, err := a.backend()
	if err != nil {
		return nil, nil, "", err
	}
	if projectName == "" {
		return nil, nil, "", errdefs.InvalidParameter(fmt.Errorf("project is required"))
	}
	return nil, backend, projectName, nil
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
	project, err := a.findFirstProjectByName(ctx, backend, projectName)
	if err != nil {
		return nil, nil, err
	}
	return project, backend, nil
}

func (a *serverApp) findFirstProjectByName(ctx context.Context, backend composeapi.Compose, projectName string) (*types.Project, error) {
	dirs, err := findComposeDirectories(newDiscoveryOptions(a.config))
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir: dir,
			Offline:    true,
		})
		if err == nil && project.Name == projectName {
			return project, nil
		}
	}
	return nil, errdefs.NotFound(fmt.Errorf("project %q not found", projectName))
}

func (a *serverApp) findMergedProjectByName(ctx context.Context, backend composeapi.Compose, projectName string) (*types.Project, error) {
	projects, err := a.discoverMergedProjects(ctx, backend)
	if err != nil {
		return nil, err
	}
	project, ok := projects[projectName]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf("project %q not found", projectName))
	}
	return project, nil
}

func (a *serverApp) discoverMergedProjects(ctx context.Context, backend composeapi.Compose) (map[string]*types.Project, error) {
	dirs, err := findComposeDirectories(newDiscoveryOptions(a.config))
	if err != nil {
		return nil, err
	}

	projects := map[string]*types.Project{}
	for _, dir := range dirs {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir: dir,
			Offline:    true,
		})
		if err != nil {
			continue
		}
		projects[project.Name] = mergeLoadedProjects(projects[project.Name], project)
	}
	return projects, nil
}

func mergeLoadedProjects(existing, next *types.Project) *types.Project {
	if existing == nil {
		return next
	}

	for _, file := range next.ComposeFiles {
		if !slices.Contains(existing.ComposeFiles, file) {
			existing.ComposeFiles = append(existing.ComposeFiles, file)
		}
	}
	for name, service := range next.Services {
		existing.Services[name] = service
	}
	return existing
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

func parsedProjectContainers(project *types.Project, selectedServices []string) []composeapi.ContainerSummary {
	summaries := []composeapi.ContainerSummary{}
	for _, serviceName := range project.ServiceNames() {
		if len(selectedServices) > 0 && !slices.Contains(selectedServices, serviceName) {
			continue
		}

		service := project.Services[serviceName]
		name := parsedProjectContainerName(project.Name, service, 1)
		summaries = append(summaries, composeapi.ContainerSummary{
			Name:       name,
			Names:      []string{name},
			Image:      composeapi.GetImageNameOrDefault(service, project.Name),
			Command:    parsedProjectCommand(service),
			Project:    project.Name,
			Service:    service.Name,
			State:      containertypes.ContainerState("uncreated"),
			Status:     "uncreated",
			Publishers: parsedProjectPublishers(service),
			Labels:     service.CustomLabels,
		})
	}
	return summaries
}

func parsedProjectContainerName(projectName string, service types.ServiceConfig, index int) string {
	if service.ContainerName != "" {
		return service.ContainerName
	}
	return strings.Join([]string{projectName, service.Name, strconv.Itoa(index)}, composeapi.Separator)
}

func parsedProjectCommand(service types.ServiceConfig) string {
	if len(service.Command) == 0 {
		return ""
	}
	return strings.Join([]string(service.Command), " ")
}

func parsedProjectPublishers(service types.ServiceConfig) composeapi.PortPublishers {
	publishers := composeapi.PortPublishers{}
	for _, port := range service.Ports {
		published := 0
		if port.Published != "" {
			if value, err := strconv.Atoi(port.Published); err == nil {
				published = value
			}
		}

		publishers = append(publishers, composeapi.PortPublisher{
			URL:           port.HostIP,
			TargetPort:    int(port.Target),
			PublishedPort: published,
			Protocol:      port.Protocol,
		})
	}
	sort.Sort(publishers)
	return publishers
}

func (a *serverApp) actionResult(project *types.Project, name string, watching bool, message string) actionResponse {
	resp := actionResponse{
		OK:       true,
		Project:  name,
		Watching: watching,
		WatchURL: watchURL(name, watching),
		Message:  message,
	}
	if project != nil {
		resp.ConfigFiles = strings.Join(project.ComposeFiles, ",")
	}
	return resp
}

func durationPointerFromSeconds(seconds *int) (*time.Duration, error) {
	if seconds == nil {
		return nil, nil
	}
	if *seconds < 0 {
		return nil, errdefs.InvalidParameter(fmt.Errorf("timeoutSeconds must be >= 0"))
	}
	value := time.Duration(*seconds) * time.Second
	return &value, nil
}

func durationFromSeconds(seconds *int) (time.Duration, error) {
	if seconds == nil {
		return 0, nil
	}
	if *seconds < 0 {
		return 0, errdefs.InvalidParameter(fmt.Errorf("waitTimeoutSeconds must be >= 0"))
	}
	return time.Duration(*seconds) * time.Second, nil
}

func renderComposeEvent(event composeapi.Event, asJSON bool) string {
	if asJSON {
		payload, err := json.Marshal(map[string]any{
			"time":       event.Timestamp,
			"type":       "container",
			"service":    event.Service,
			"id":         event.Container,
			"action":     event.Status,
			"attributes": event.Attributes,
		})
		if err == nil {
			return string(payload)
		}
	}
	return fmt.Sprint(event)
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
