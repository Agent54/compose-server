package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
)

type backendFactory func(...composepkg.Option) (composeapi.Compose, error)
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
	logs         *logRegistry
	execs        *execRegistry
	builds       *buildRegistry
	listOverride func(context.Context, composeapi.ListOptions) ([]composeapi.Stack, error)
}

type upRequest struct {
	Project  string   `json:"project,omitempty"`
	Path     string   `json:"path,omitempty"`
	Services []string `json:"services,omitempty"`
	Build    bool     `json:"build,omitempty"`
	Watch    bool     `json:"watch,omitempty"`
}

type watchRequest struct {
	Path     string   `json:"path,omitempty"`
	Services []string `json:"services,omitempty"`
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
	BuildID     string `json:"buildId,omitempty"`
	BuildURL    string `json:"buildUrl,omitempty"`
	Message     string `json:"message,omitempty"`
}

type projectLoadRef struct {
	workingDir  string
	configPaths []string
}

func newServerApp(config serveConfig, backend backendFactory, stats statsRuntimeFactory) *serverApp {
	return &serverApp{
		config:  config,
		backend: backend,
		stats:   stats,
		watches: newWatchRegistry(),
		logs:    newLogRegistry(),
		execs:   newExecRegistry(),
		builds:  newBuildRegistry(),
	}
}

func (a *serverApp) setWatchMirror(mirror func(sseMessage)) {
	a.watches.setMirror(mirror)
}

func (a *serverApp) shutdown(statusf func(string, ...any)) {
	if statusf != nil {
		count := len(a.watches.snapshot())
		if count == 0 {
			statusf("No active watch resources to stop.\n")
		} else {
			statusf("Stopping %d active watch resource(s)...\n", count)
		}
	}
	_ = a.watches.stopAll()
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
	if req.Watch {
		projectName, projects, configFiles, err := a.resolveUpWatchProjects(ctx, req.Project, req.Path, req.Services)
		if err != nil {
			return actionResponse{}, err
		}
		for _, project := range projects {
			prepareProjectForWatch(project)
		}
		if err := a.startWatch(projectName, projects, req.Services); err != nil {
			return actionResponse{}, err
		}
		return actionResponse{
			OK:          true,
			Project:     projectName,
			ConfigFiles: configFiles,
			Watching:    true,
			WatchURL:    watchURL(projectName, true),
		}, nil
	}

	projects, backend, projectName, err := a.resolveUpProjects(ctx, req.Project, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	projects, err = filterProjectVariantsByServices(projects, req.Services)
	if err != nil {
		return actionResponse{}, err
	}

	var buildID string
	if req.Build {
		buildID = a.builds.start(projectName)
		backend, err = a.instrumentedBuildBackend(buildID, projectName)
		if err != nil {
			a.builds.finish(buildID, false)
			return actionResponse{}, err
		}
		projects, _, err = a.resolveProjectVariantsWithBackend(ctx, backend, req.Project, req.Path)
		if err != nil {
			a.builds.finish(buildID, false)
			return actionResponse{}, err
		}
		projects, err = filterProjectVariantsByServices(projects, req.Services)
		if err != nil {
			a.builds.finish(buildID, false)
			return actionResponse{}, err
		}
	}

	var upErr error
	for _, project := range projects {
		services, ok := servicesForProject(project, req.Services)
		if !ok {
			continue
		}
		var buildPtr *composeapi.BuildOptions
		if req.Build {
			build := a.newBuildOptionsForServices(project, services)
			buildPtr = &build
		}
		err = backend.Up(ctx, project, composeapi.UpOptions{
			Create: composeapi.CreateOptions{
				Build:                buildPtr,
				Services:             services,
				Recreate:             composeapi.RecreateDiverged,
				RecreateDependencies: composeapi.RecreateDiverged,
				Inherit:              true,
			},
			Start: composeapi.StartOptions{
				Project:  project,
				Services: services,
			},
		})
		if err != nil {
			upErr = err
			break
		}
	}
	if buildID != "" {
		a.builds.finish(buildID, upErr == nil)
	}
	if upErr != nil {
		return actionResponse{}, upErr
	}

	return actionResponse{
		OK:          true,
		Project:     projectName,
		ConfigFiles: strings.Join(uniqueProjectConfigFiles(projects), ","),
		Watching:    req.Watch,
		WatchURL:    watchURL(projectName, req.Watch),
		BuildID:     buildID,
		BuildURL:    buildURL(buildID),
	}, nil
}

func (a *serverApp) instrumentedBuildBackend(buildID, projectName string) (composeapi.Compose, error) {
	return a.backend(
		composepkg.WithEventProcessor(a.builds.processor(buildID, projectName)),
		composepkg.WithOutputStream(a.builds.writer(buildID, projectName, "stdout")),
		composepkg.WithErrorStream(a.builds.writer(buildID, projectName, "stderr")),
	)
}

func (a *serverApp) listBuilds() []buildSummary {
	return a.builds.list()
}

func (a *serverApp) streamBuild(ctx context.Context, buildID string, w http.ResponseWriter) error {
	return a.builds.stream(ctx, buildID, w)
}

func (a *serverApp) renderProjectConfig(ctx context.Context, projectName, requestPath, format string) ([]byte, string, error) {
	project, _, err := a.resolveProject(ctx, projectName, requestPath)
	if err != nil {
		return nil, "", err
	}

	switch format {
	case "", "yaml":
		content, err := project.MarshalYAML()
		if err != nil {
			return nil, "", err
		}
		return content, "application/yaml", nil
	case "json":
		content, err := project.MarshalJSON()
		if err != nil {
			return nil, "", err
		}
		return content, "application/json", nil
	default:
		return nil, "", errdefs.InvalidParameter(fmt.Errorf("unsupported format %q", format))
	}
}

func (a *serverApp) restartWatch(ctx context.Context, projectName string, req watchRequest) (actionResponse, error) {
	projects, configFiles, err := a.resolveWatchProjects(ctx, projectName, req.Path, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	for _, project := range projects {
		prepareProjectForWatch(project)
	}
	if err := a.startWatch(projectName, projects, req.Services); err != nil {
		return actionResponse{}, err
	}
	return actionResponse{
		OK:          true,
		Project:     projectName,
		ConfigFiles: configFiles,
		Watching:    true,
		WatchURL:    watchURL(projectName, true),
	}, nil
}

func (a *serverApp) startProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	projects, backend, name, err := a.resolveActionProjects(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	projects, err = filterProjectVariantsByServices(projects, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	waitTimeout, err := durationFromSeconds(req.WaitTimeoutSecs)
	if err != nil {
		return actionResponse{}, err
	}
	for _, project := range projects {
		services, _ := servicesForProject(project, req.Services)
		if err := backend.Start(ctx, name, composeapi.StartOptions{
			Project:     project,
			AttachTo:    services,
			Services:    services,
			Wait:        req.Wait,
			WaitTimeout: waitTimeout,
		}); err != nil {
			return actionResponse{}, err
		}
	}
	return a.actionResult(projects[0], name, false, ""), nil
}

func (a *serverApp) stopProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	projects, backend, name, err := a.resolveActionProjects(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	projects, err = filterProjectVariantsByServices(projects, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	for _, project := range projects {
		services, _ := servicesForProject(project, req.Services)
		if err := backend.Stop(ctx, name, composeapi.StopOptions{
			Project:  project,
			Services: services,
			Timeout:  timeout,
		}); err != nil {
			return actionResponse{}, err
		}
	}
	return a.actionResult(projects[0], name, false, ""), nil
}

func (a *serverApp) restartProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	projects, backend, name, err := a.resolveActionProjects(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	projects, err = filterProjectVariantsByServices(projects, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	for _, project := range projects {
		services, _ := servicesForProject(project, req.Services)
		if err := backend.Restart(ctx, name, composeapi.RestartOptions{
			Project:  project,
			Services: services,
			Timeout:  timeout,
			NoDeps:   req.NoDeps,
		}); err != nil {
			return actionResponse{}, err
		}
	}
	return a.actionResult(projects[0], name, false, ""), nil
}

func (a *serverApp) downProject(ctx context.Context, projectName string, req projectActionRequest) (actionResponse, error) {
	projects, backend, name, err := a.resolveActionProjects(ctx, projectName, req.Path)
	if err != nil {
		return actionResponse{}, err
	}
	projects, err = filterProjectVariantsByServices(projects, req.Services)
	if err != nil {
		return actionResponse{}, err
	}
	timeout, err := durationPointerFromSeconds(req.TimeoutSeconds)
	if err != nil {
		return actionResponse{}, err
	}
	for _, project := range projects {
		services, _ := servicesForProject(project, req.Services)
		if err := backend.Down(ctx, name, composeapi.DownOptions{
			RemoveOrphans: req.RemoveOrphans,
			Project:       project,
			Timeout:       timeout,
			Images:        req.Images,
			Volumes:       req.Volumes,
			Services:      services,
		}); err != nil {
			return actionResponse{}, err
		}
	}
	return a.actionResult(projects[0], name, false, ""), nil
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

type logsRequest struct {
	Path       string
	Services   []string
	Follow     bool
	Index      int
	Tail       string
	Since      string
	Until      string
	Timestamps bool
}

func (a *serverApp) streamLogs(ctx context.Context, projectName string, req logsRequest, w http.ResponseWriter) error {
	if req.Index > 0 && len(req.Services) != 1 {
		return errdefs.InvalidParameter(fmt.Errorf("index requires exactly one service"))
	}
	project, backend, name, err := a.resolvePSProject(ctx, projectName, req.Path)
	if err != nil {
		return err
	}

	services := slices.Clone(req.Services)
	if project != nil && len(services) == 0 {
		for serviceName, service := range project.Services {
			if service.Attach == nil || *service.Attach {
				services = append(services, serviceName)
			}
		}
	}

	stream, err := newTextSSEStream(w)
	if err != nil {
		return err
	}
	consumer := &sseLogConsumer{
		project: projectName,
		stream:  stream,
	}
	ch, unsubscribe := a.logs.subscribe(projectName)
	defer unsubscribe()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				consumer.sendMessage(msg)
			}
		}
	}()
	tail := req.Tail
	if tail == "" {
		tail = "all"
	}
	return backend.Logs(ctx, name, consumer, composeapi.LogOptions{
		Project:    project,
		Services:   services,
		Follow:     req.Follow,
		Index:      req.Index,
		Tail:       tail,
		Since:      req.Since,
		Until:      req.Until,
		Timestamps: req.Timestamps,
	})
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

func (a *serverApp) startWatch(projectName string, projects []*types.Project, requestedServices []string) error {
	return a.watches.start(projectName, func(ctx context.Context, consumer composeapi.LogConsumer) error {
		eg, ctx := errgroup.WithContext(ctx)
		for _, project := range projects {
			project := project
			eg.Go(func() error {
				services, ok := servicesForProject(project, requestedServices)
				if !ok {
					return nil
				}
				backend, err := a.backend(
					composepkg.WithOutputStream(newLogConsumerWriter(consumer, "stdout")),
					composepkg.WithErrorStream(newLogConsumerWriter(consumer, "stderr")),
				)
				if err != nil {
					return err
				}
				return backend.Up(ctx, project, watchModeUpOptions(project, a.newBuildOptionsForServices(project, services), consumer, services))
			})
		}
		return eg.Wait()
	})
}

func (a *serverApp) loadProject(ctx context.Context, requestPath string) (*types.Project, composeapi.Compose, error) {
	backend, err := a.backend()
	if err != nil {
		return nil, nil, err
	}
	project, err := a.loadProjectWithBackend(ctx, backend, requestPath)
	if err != nil {
		return nil, nil, err
	}
	return project, backend, nil
}

func (a *serverApp) loadProjectWithBackend(ctx context.Context, backend composeapi.Compose, requestPath string) (*types.Project, error) {
	ref, err := a.resolveProjectLoadRef(requestPath)
	if err != nil {
		return nil, err
	}
	project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
		WorkingDir:        ref.workingDir,
		ConfigPaths:       ref.configPaths,
		Offline:           true,
		ProjectOptionsFns: runtimeProjectLoadOptions(),
	})
	if err != nil {
		return nil, err
	}
	project, err = runtimeProject(project)
	if err != nil {
		return nil, err
	}
	return project, nil
}

func (a *serverApp) loadProjectVariants(ctx context.Context, requestPath string) ([]*types.Project, composeapi.Compose, error) {
	backend, err := a.backend()
	if err != nil {
		return nil, nil, err
	}
	projects, err := a.loadProjectVariantsWithBackend(ctx, backend, requestPath)
	if err != nil {
		return nil, nil, err
	}
	return projects, backend, nil
}

func (a *serverApp) loadProjectVariantsWithBackend(ctx context.Context, backend composeapi.Compose, requestPath string) ([]*types.Project, error) {
	parts := splitPathList(requestPath)
	if len(parts) <= 1 || !a.pathListHasMultipleRoots(parts) {
		project, err := a.loadProjectWithBackend(ctx, backend, requestPath)
		if err != nil {
			return nil, err
		}
		return []*types.Project{project}, nil
	}

	projects := make([]*types.Project, 0, len(parts))
	for _, part := range parts {
		project, err := a.loadProjectWithBackend(ctx, backend, part)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if _, err := commonProjectName(projects); err != nil {
		return nil, err
	}
	return projects, nil
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

func (a *serverApp) resolveActionProjects(ctx context.Context, projectName, requestPath string) ([]*types.Project, composeapi.Compose, string, error) {
	backend, err := a.backend()
	if err != nil {
		return nil, nil, "", err
	}
	projects, name, err := a.resolveProjectVariantsWithBackend(ctx, backend, projectName, requestPath)
	if err != nil {
		if requestPath == "" && projectName != "" && errdefs.IsNotFound(err) {
			return []*types.Project{nil}, backend, projectName, nil
		}
		return nil, nil, "", err
	}
	return projects, backend, name, nil
}

func (a *serverApp) resolveUpProjects(ctx context.Context, projectName, requestPath string) ([]*types.Project, composeapi.Compose, string, error) {
	backend, err := a.backend()
	if err != nil {
		return nil, nil, "", err
	}
	projects, name, err := a.resolveProjectVariantsWithBackend(ctx, backend, projectName, requestPath)
	if err != nil {
		return nil, nil, "", err
	}
	return projects, backend, name, nil
}

func (a *serverApp) resolveProjectVariantsWithBackend(ctx context.Context, backend composeapi.Compose, projectName, requestPath string) ([]*types.Project, string, error) {
	if requestPath != "" {
		projects, err := a.loadProjectVariantsWithBackend(ctx, backend, requestPath)
		if err != nil {
			return nil, "", err
		}
		name, err := commonProjectName(projects)
		if err != nil {
			return nil, "", err
		}
		if projectName != "" && name != projectName {
			return nil, "", errdefs.InvalidParameter(fmt.Errorf("project %q does not match requested project %q", name, projectName))
		}
		return projects, name, nil
	}

	if projectName != "" {
		projects, err := a.findProjectVariantsByName(ctx, backend, projectName)
		if err != nil {
			return nil, "", err
		}
		return projects, projectName, nil
	}

	return nil, "", errdefs.InvalidParameter(fmt.Errorf("project or path is required"))
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
	project, err := a.findMergedProjectByName(ctx, backend, projectName)
	if err != nil {
		return nil, nil, err
	}
	project, err = runtimeProject(project)
	if err != nil {
		return nil, nil, err
	}
	return project, backend, nil
}

func (a *serverApp) resolveUpWatchProjects(ctx context.Context, projectName, requestPath string, services []string) (string, []*types.Project, string, error) {
	projects, _, name, err := a.resolveUpProjects(ctx, projectName, requestPath)
	if err != nil {
		return "", nil, "", err
	}
	projects, err = filterProjectVariantsByServices(projects, services)
	if err != nil {
		return "", nil, "", err
	}
	return name, projects, strings.Join(uniqueProjectConfigFiles(projects), ","), nil
}

func (a *serverApp) resolveWatchProjects(ctx context.Context, projectName, requestPath string, services []string) ([]*types.Project, string, error) {
	projects, _, _, err := a.resolveUpProjects(ctx, projectName, requestPath)
	if err != nil {
		return nil, "", err
	}
	projects, err = filterProjectVariantsByServices(projects, services)
	if err != nil {
		return nil, "", err
	}
	return projects, strings.Join(uniqueProjectConfigFiles(projects), ","), nil
}

func (a *serverApp) findFirstProjectByName(ctx context.Context, backend composeapi.Compose, projectName string) (*types.Project, error) {
	dirs, err := findComposeDirectories(newDiscoveryOptions(a.config))
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir:        dir,
			Offline:           true,
			ProjectOptionsFns: runtimeProjectLoadOptions(),
		})
		if err == nil && project.Name == projectName {
			project, err = runtimeProject(project)
			if err != nil {
				return nil, err
			}
			return project, nil
		}
	}
	return nil, errdefs.NotFound(fmt.Errorf("project %q not found", projectName))
}

func (a *serverApp) findProjectVariantsByName(ctx context.Context, backend composeapi.Compose, projectName string) ([]*types.Project, error) {
	dirs, err := findComposeDirectories(newDiscoveryOptions(a.config))
	if err != nil {
		return nil, err
	}

	projects := make([]*types.Project, 0)
	seen := map[string]struct{}{}
	for _, dir := range dirs {
		project, err := backend.LoadProject(ctx, composeapi.ProjectLoadOptions{
			WorkingDir:        dir,
			Offline:           true,
			ProjectOptionsFns: runtimeProjectLoadOptions(),
		})
		if err != nil || project.Name != projectName {
			continue
		}
		project, err = runtimeProject(project)
		if err != nil {
			return nil, err
		}
		key := strings.Join(project.ComposeFiles, ",")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		projects = append(projects, project)
	}
	if len(projects) == 0 {
		return nil, errdefs.NotFound(fmt.Errorf("project %q not found", projectName))
	}
	return projects, nil
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
			WorkingDir:        dir,
			Offline:           true,
			ProjectOptionsFns: runtimeProjectLoadOptions(),
		})
		if err != nil {
			continue
		}
		project, err = runtimeProject(project)
		if err != nil {
			return nil, err
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

func commonProjectName(projects []*types.Project) (string, error) {
	if len(projects) == 0 {
		return "", errdefs.NotFound(fmt.Errorf("project not found"))
	}
	name := projects[0].Name
	for _, project := range projects[1:] {
		if project.Name != name {
			return "", errdefs.InvalidParameter(fmt.Errorf("project %q does not match requested project %q", project.Name, name))
		}
	}
	return name, nil
}

func uniqueProjectConfigFiles(projects []*types.Project) []string {
	configFiles := make([]string, 0)
	for _, project := range projects {
		if project == nil {
			continue
		}
		for _, file := range project.ComposeFiles {
			if !slices.Contains(configFiles, file) {
				configFiles = append(configFiles, file)
			}
		}
	}
	return configFiles
}

func servicesForProject(project *types.Project, requested []string) ([]string, bool) {
	if project == nil {
		return requested, true
	}
	if len(requested) == 0 {
		return nil, true
	}
	services := make([]string, 0, len(requested))
	for _, service := range requested {
		if _, err := project.GetService(service); err == nil {
			services = append(services, service)
		}
	}
	return services, len(services) > 0
}

func filterProjectVariantsByServices(projects []*types.Project, requested []string) ([]*types.Project, error) {
	if len(requested) == 0 {
		return projects, nil
	}
	filtered := make([]*types.Project, 0, len(projects))
	for _, project := range projects {
		if _, ok := servicesForProject(project, requested); ok {
			filtered = append(filtered, project)
		}
	}
	if len(filtered) == 0 {
		return nil, errdefs.InvalidParameter(fmt.Errorf("service not found: %s", strings.Join(requested, ",")))
	}
	return filtered, nil
}

func splitPathList(requestPath string) []string {
	raw := strings.Split(requestPath, ",")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func (a *serverApp) pathListHasMultipleRoots(parts []string) bool {
	roots := map[string]struct{}{}
	for _, part := range parts {
		resolved, err := a.resolvePathValue(a.config.rootDir, part)
		if err != nil {
			continue
		}
		root := resolved
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			root = filepath.Dir(resolved)
		}
		roots[root] = struct{}{}
	}
	return len(roots) > 1
}

func (a *serverApp) resolveProjectLoadRef(requestPath string) (projectLoadRef, error) {
	root := a.config.rootDir
	if requestPath == "" {
		return projectLoadRef{workingDir: root}, nil
	}

	parts := strings.Split(requestPath, ",")
	resolved := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		path, err := a.resolvePathValue(root, part)
		if err != nil {
			return projectLoadRef{}, err
		}
		resolved = append(resolved, path)
	}

	if len(resolved) == 0 {
		return projectLoadRef{}, errdefs.InvalidParameter(fmt.Errorf("path is required"))
	}

	if len(resolved) > 1 {
		return projectLoadRef{
			workingDir:  filepath.Dir(resolved[0]),
			configPaths: resolved,
		}, nil
	}

	info, err := os.Stat(resolved[0])
	if err == nil && info.IsDir() {
		return projectLoadRef{workingDir: resolved[0]}, nil
	}

	return projectLoadRef{
		workingDir:  filepath.Dir(resolved[0]),
		configPaths: resolved,
	}, nil
}

func (a *serverApp) resolvePathValue(root, requestPath string) (string, error) {
	if filepath.IsAbs(requestPath) {
		return filepath.Clean(requestPath), nil
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
	return a.newBuildOptionsForServices(project, project.ServiceNames())
}

func (a *serverApp) newBuildOptionsForServices(project *types.Project, services []string) composeapi.BuildOptions {
	return composeapi.BuildOptions{
		Progress: "plain",
		Services: serviceSelection(project, services),
		Deps:     true,
	}
}

func serviceSelection(project *types.Project, services []string) []string {
	if len(services) == 0 {
		return project.ServiceNames()
	}
	return services
}

func prepareProjectForWatch(project *types.Project) {
	for index, service := range project.Services {
		if service.Build != nil && service.Develop != nil {
			service.PullPolicy = types.PullPolicyBuild
		}
		project.Services[index] = service
	}
}

func runtimeProject(project *types.Project) (*types.Project, error) {
	return project.WithServicesEnvironmentResolved(true)
}

func runtimeProjectLoadOptions() []composecli.ProjectOptionsFn {
	return []composecli.ProjectOptionsFn{composecli.WithoutEnvironmentResolution}
}

func watchModeUpOptions(project *types.Project, build composeapi.BuildOptions, consumer composeapi.LogConsumer, services []string) composeapi.UpOptions {
	buildCopy := build
	selectedServices := serviceSelection(project, services)
	return composeapi.UpOptions{
		Create: composeapi.CreateOptions{
			Build:                &buildCopy,
			Services:             selectedServices,
			Recreate:             composeapi.RecreateDiverged,
			RecreateDependencies: composeapi.RecreateDiverged,
			Inherit:              true,
		},
		Start: composeapi.StartOptions{
			Project:  project,
			Attach:   consumer,
			AttachTo: selectedServices,
			Watch:    true,
			Services: selectedServices,
		},
	}
}

type logConsumerWriter struct {
	consumer composeapi.LogConsumer
	stream   string
	mu       sync.Mutex
	pending  string
}

type sseLogConsumer struct {
	project string
	stream  *textSSEStream
	mu      sync.Mutex
}

func (c *sseLogConsumer) Log(containerName, message string) {
	c.send("stdout", containerName, message)
}

func (c *sseLogConsumer) Err(containerName, message string) {
	c.send("stderr", containerName, message)
}

func (c *sseLogConsumer) Status(containerName, message string) {
	c.send("status", containerName, message)
}

func (c *sseLogConsumer) send(stream, source, message string) {
	c.sendMessage(sseMessage{
		Project: c.project,
		Stream:  stream,
		Source:  source,
		Message: message,
		Time:    time.Now().UTC(),
	})
}

func (c *sseLogConsumer) sendMessage(msg sseMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.stream.Send("message", string(mustMarshalSSE(msg)))
}

func newLogConsumerWriter(consumer composeapi.LogConsumer, stream string) *logConsumerWriter {
	return &logConsumerWriter{
		consumer: consumer,
		stream:   stream,
	}
}

func (w *logConsumerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending += strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\r", "\n")
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.TrimSpace(line) == "" {
			continue
		}
		if w.stream == "stderr" {
			w.consumer.Err(composeapi.ResourceCompose, line)
		} else {
			w.consumer.Log(composeapi.ResourceCompose, line)
		}
	}
	return len(p), nil
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

func writeContent(w http.ResponseWriter, code int, contentType string, content []byte) error {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(code)
	_, err := w.Write(content)
	return err
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

func buildURL(buildID string) string {
	if buildID == "" {
		return ""
	}
	return "/builds/" + buildID + "/stream"
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
