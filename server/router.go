package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/server/router"
	"github.com/docker/docker/errdefs"
	"github.com/moby/moby/client"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

type composeRouter struct {
	app *serverApp
}

type queryParamSpec struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Repeated    bool     `json:"repeated,omitempty"`
	Values      []string `json:"values,omitempty"`
}

type bodyFieldSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type routeSchema struct {
	Method      string           `json:"method"`
	Path        string           `json:"path"`
	Versioned   bool             `json:"versioned"`
	Summary     string           `json:"summary,omitempty"`
	QueryParams []queryParamSpec `json:"queryParams,omitempty"`
	BodyFields  []bodyFieldSpec  `json:"bodyFields,omitempty"`
}

type apiSchema struct {
	VersionMatcher string        `json:"versionMatcher"`
	Config         schemaConfig  `json:"config"`
	Routes         []routeSchema `json:"routes"`
}

type schemaConfig struct {
	RootDir        string   `json:"rootDir"`
	MaxDepth       int      `json:"maxDepth"`
	ExcludedDirs   []string `json:"excludedDirs"`
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
}

type routeSpec struct {
	method      string
	path        string
	summary     string
	queryParams []queryParamSpec
	bodyFields  []bodyFieldSpec
	handler     func(*composeRouter) httputils.APIFunc
}

var acceptedListFilters = map[string]bool{
	"name": true,
}

func newRouter(app *serverApp) router.Router {
	return &composeRouter{app: app}
}

func (r *composeRouter) Routes() []router.Route {
	routes := make([]router.Route, 0, len(serveRoutes()))
	for _, spec := range serveRoutes() {
		routes = append(routes, router.NewRoute(spec.method, spec.path, withJSONError(spec.handler(r))))
	}
	return routes
}

type errorResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

func withJSONError(handler httputils.APIFunc) httputils.APIFunc {
	return func(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
		if err := handler(ctx, w, req, vars); err != nil {
			return writeJSON(w, statusCodeForError(err), errorResponse{
				OK:      false,
				Error:   err.Error(),
				Message: err.Error(),
			})
		}
		return nil
	}
}

func statusCodeForError(err error) int {
	switch {
	case errdefs.IsInvalidParameter(err):
		return http.StatusBadRequest
	case errdefs.IsNotFound(err):
		return http.StatusNotFound
	case errors.Is(err, composeapi.ErrNoResources):
		return http.StatusNotFound
	case errdefs.IsConflict(err):
		return http.StatusConflict
	case errdefs.IsUnauthorized(err):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func (r *composeRouter) rootSchema(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	return writeJSON(w, http.StatusOK, apiSchema{
		VersionMatcher: "/v{version:[0-9.]+}",
		Config: schemaConfig{
			RootDir:        r.app.config.rootDir,
			MaxDepth:       r.app.config.maxDepth,
			ExcludedDirs:   slices.Clone(r.app.config.excludedDir),
			AllowedOrigins: slices.Clone(r.app.config.allowedOrigins),
		},
		Routes: schemaFromRouteSpecs(),
	})
}

func (r *composeRouter) ping(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	w.WriteHeader(http.StatusOK)
	return nil
}

func (r *composeRouter) listProjects(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}

	options := composeapi.ListOptions{}
	if value := req.Form.Get("all"); value != "" {
		all, err := strconv.ParseBool(value)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		options.All = all
	}
	filters, err := parseListFilters(req.Form["filter"])
	if err != nil {
		return err
	}

	projects, err := r.app.listStacks(ctx, options)
	if err != nil {
		return err
	}
	if len(filters) > 0 {
		projects = filterProjects(projects, filters)
	}
	return writeJSON(w, http.StatusOK, projects)
}

func (r *composeRouter) systemInfo(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	info, err := r.app.systemInfo(ctx)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, info)
}

func (r *composeRouter) systemDiskUsage(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	usage, err := r.app.systemDiskUsage(ctx)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, usage)
}

func (r *composeRouter) upProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body upRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.upProject(ctx, body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) listBuilds(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	return writeJSON(w, http.StatusOK, r.app.listBuilds())
}

func (r *composeRouter) streamBuild(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	return r.app.streamBuild(ctx, vars["build"], w)
}

func (r *composeRouter) configProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	content, contentType, err := r.app.renderProjectConfig(ctx, vars["project"], req.Form.Get("path"), req.Form.Get("format"))
	if err != nil {
		return err
	}
	return writeContent(w, http.StatusOK, contentType, content)
}

func (r *composeRouter) startProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.startProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) stopProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.stopProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) restartProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.restartProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) downProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.downProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) rmProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body rmRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.rmProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) pauseProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.pauseProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) unpauseProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.unpauseProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) killProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body projectActionRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.killProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) commitProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body commitRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.commitProject(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) listExecs(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	return writeJSON(w, http.StatusOK, r.app.listExecs(vars["project"]))
}

func (r *composeRouter) execProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body containerExecRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.execInContainer(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, resp)
}

func (r *composeRouter) eventsProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	asJSON, err := parseOptionalBool(req.Form.Get("json"))
	if err != nil {
		return err
	}
	return r.app.streamEvents(
		ctx,
		vars["project"],
		req.Form.Get("path"),
		req.Form["service"],
		req.Form.Get("since"),
		req.Form.Get("until"),
		asJSON,
		w,
	)
}

func (r *composeRouter) statsProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	all, err := parseOptionalBool(req.Form.Get("all"))
	if err != nil {
		return err
	}
	noStream, err := parseOptionalBool(req.Form.Get("no-stream"))
	if err != nil {
		return err
	}
	noTrunc, err := parseOptionalBool(req.Form.Get("no-trunc"))
	if err != nil {
		return err
	}
	options := statsRequest{
		Path:     req.Form.Get("path"),
		Services: req.Form["service"],
		All:      all,
		NoStream: noStream,
		NoTrunc:  noTrunc,
		Format:   req.Form.Get("format"),
	}
	return r.app.streamStats(ctx, vars["project"], options, w)
}

func (r *composeRouter) resourcesProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	all, err := parseOptionalBool(req.Form.Get("all"))
	if err != nil {
		return err
	}
	resources, err := r.app.projectResources(ctx, vars["project"], resourcesRequest{
		Path:        req.Form.Get("path"),
		Services:    req.Form["service"],
		All:         all,
		Granularity: req.Form.Get("granularity"),
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resources)
}

func (r *composeRouter) psProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	all, err := parseOptionalBool(req.Form.Get("all"))
	if err != nil {
		return err
	}
	containers, err := r.app.psProject(ctx, vars["project"], req.Form.Get("path"), req.Form["service"], all, req.Form["status"])
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, containers)
}

func (r *composeRouter) logsProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	follow, err := parseOptionalBool(req.Form.Get("follow"))
	if err != nil {
		return err
	}
	timestamps, err := parseOptionalBool(req.Form.Get("timestamps"))
	if err != nil {
		return err
	}
	index := 0
	if value := req.Form.Get("index"); value != "" {
		index, err = strconv.Atoi(value)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		if index < 0 {
			return errdefs.InvalidParameter(fmt.Errorf("index must be >= 0"))
		}
	}
	return r.app.streamLogs(ctx, vars["project"], logsRequest{
		Path:       req.Form.Get("path"),
		Services:   req.Form["service"],
		Follow:     follow,
		Index:      index,
		Tail:       req.Form.Get("tail"),
		Since:      req.Form.Get("since"),
		Until:      req.Form.Get("until"),
		Timestamps: timestamps,
	}, w)
}

func (r *composeRouter) topProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	containers, err := r.app.topProject(ctx, vars["project"], req.Form.Get("path"), req.Form["service"])
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, containers)
}

func (r *composeRouter) killProcess(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body processKillRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.killContainerProcess(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) volumesProject(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(req); err != nil {
		return err
	}
	volumes, err := r.app.volumesProject(ctx, vars["project"], req.Form.Get("path"), req.Form["service"])
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, volumes)
}

func (r *composeRouter) getWatch(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	return r.app.streamWatch(ctx, vars["project"], w)
}

func (r *composeRouter) postWatch(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body watchRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}
	resp, err := r.app.restartWatch(ctx, vars["project"], body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, resp)
}

func (r *composeRouter) deleteWatch(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	if !r.app.stopWatch(vars["project"]) {
		return errWatchNotFound(vars["project"])
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"project":  vars["project"],
		"watching": false,
	})
}

func serveRoutes() []routeSpec {
	return []routeSpec{
		{
			method:  http.MethodGet,
			path:    "/",
			summary: "Return the API schema generated from the registered routes",
			handler: func(r *composeRouter) httputils.APIFunc { return r.rootSchema },
		},
		{
			method:  http.MethodGet,
			path:    "/_ping",
			summary: "Health check endpoint",
			handler: func(r *composeRouter) httputils.APIFunc { return r.ping },
		},
		{
			method:  http.MethodHead,
			path:    "/_ping",
			summary: "Health check endpoint",
			handler: func(r *composeRouter) httputils.APIFunc { return r.ping },
		},
		{
			method:  http.MethodGet,
			path:    "/ls",
			summary: "List Compose projects and whether a watch resource is active",
			queryParams: []queryParamSpec{
				{Name: "all", Type: "boolean", Description: "Include stopped Compose projects"},
				{Name: "filter", Type: "string", Repeated: true, Values: []string{"name=<regex>"}, Description: "Repeatable filter expression"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.listProjects },
		},
		{
			method:  http.MethodGet,
			path:    "/system",
			summary: "Return Docker daemon host info from the socket this server uses",
			handler: func(r *composeRouter) httputils.APIFunc { return r.systemInfo },
		},
		{
			method:  http.MethodGet,
			path:    "/system/df",
			summary: "Return Docker daemon disk usage from the socket this server uses",
			handler: func(r *composeRouter) httputils.APIFunc { return r.systemDiskUsage },
		},
		{
			method:  http.MethodPost,
			path:    "/up",
			summary: "Start a Compose project; optional watch mode starts a per-project watch resource",
			bodyFields: []bodyFieldSpec{
				{Name: "project", Type: "string", Description: "Optional project name; when path is omitted, all discovered compose-file variants with this name are started independently"},
				{Name: "path", Type: "string", Description: "Project directory, compose file path, or comma-separated compose file list; relative paths are resolved from the serve root"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "build", Type: "boolean", Description: "Build before starting"},
				{Name: "watch", Type: "boolean", Description: "Start watch mode after up succeeds"},
				{Name: "removeOrphans", Type: "boolean", Description: "Remove orphan containers"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.upProject },
		},
		{
			method:  http.MethodGet,
			path:    "/builds",
			summary: "List builds seen by this compose serve process, newest first",
			handler: func(r *composeRouter) httputils.APIFunc { return r.listBuilds },
		},
		{
			method:  http.MethodGet,
			path:    "/builds/{build}/stream",
			summary: "Replay and follow a build's in-memory output as SSE",
			handler: func(r *composeRouter) httputils.APIFunc { return r.streamBuild },
		},
		{
			method:  http.MethodGet,
			path:    "/config/{project}",
			summary: "Render the parsed Compose project config as YAML or JSON",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "format", Type: "string", Values: []string{"yaml", "json"}, Description: "Output format; defaults to yaml"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.configProject },
		},
		{
			method:  http.MethodPost,
			path:    "/start/{project}",
			summary: "Start services for a project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "wait", Type: "boolean", Description: "Wait for services to become running or healthy"},
				{Name: "waitTimeoutSeconds", Type: "integer", Description: "Maximum wait duration in seconds"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.startProject },
		},
		{
			method:  http.MethodPost,
			path:    "/stop/{project}",
			summary: "Stop services for a project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "timeoutSeconds", Type: "integer", Description: "Graceful shutdown timeout in seconds"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.stopProject },
		},
		{
			method:  http.MethodPost,
			path:    "/restart/{project}",
			summary: "Restart services for a project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "timeoutSeconds", Type: "integer", Description: "Graceful shutdown timeout in seconds"},
				{Name: "noDeps", Type: "boolean", Description: "Skip dependent services"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.restartProject },
		},
		{
			method:  http.MethodPost,
			path:    "/down/{project}",
			summary: "Stop and remove a project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "timeoutSeconds", Type: "integer", Description: "Graceful shutdown timeout in seconds"},
				{Name: "removeOrphans", Type: "boolean", Description: "Remove orphan containers"},
				{Name: "volumes", Type: "boolean", Description: "Remove named and anonymous volumes"},
				{Name: "images", Type: "string", Description: `Image removal mode: "local" or "all"`},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.downProject },
		},
		{
			method:  http.MethodPost,
			path:    "/rm/{project}",
			summary: "Remove stopped service containers for a project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "force", Type: "boolean", Description: "Do not ask for confirmation before removal"},
				{Name: "stop", Type: "boolean", Description: "Stop running containers before removal"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.rmProject },
		},
		{
			method:  http.MethodPost,
			path:    "/pause/{project}",
			summary: "Pause services by freezing their processes in place; unlike stop, containers remain running but suspended",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.pauseProject },
		},
		{
			method:  http.MethodPost,
			path:    "/unpause/{project}",
			summary: "Resume previously paused services",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.unpauseProject },
		},
		{
			method:  http.MethodPost,
			path:    "/kill/{project}",
			summary: "Force-stop services by sending a signal",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "removeOrphans", Type: "boolean", Description: "Remove orphan containers"},
				{Name: "signal", Type: "string", Description: "Signal to send, for example SIGKILL"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.killProject },
		},
		{
			method:  http.MethodPost,
			path:    "/commit/{project}",
			summary: "Create an image from a service container",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Description: "Service name to commit"},
				{Name: "reference", Type: "string", Description: "Target image reference"},
				{Name: "pause", Type: "boolean", Description: "Pause the container during commit"},
				{Name: "comment", Type: "string", Description: "Commit message"},
				{Name: "author", Type: "string", Description: "Author string"},
				{Name: "changes", Type: "string[]", Description: "Dockerfile-style changes to apply"},
				{Name: "index", Type: "integer", Description: "Replica index for scaled services"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.commitProject },
		},
		{
			method:  http.MethodGet,
			path:    "/exec/{project}",
			summary: "List exec processes started through this compose server instance",
			handler: func(r *composeRouter) httputils.APIFunc { return r.listExecs },
		},
		{
			method:  http.MethodPost,
			path:    "/exec/{project}",
			summary: "Execute a command in a project container and mix stdout/stderr into the logs SSE stream",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "container", Type: "string", Description: "Optional container id or name; otherwise service is required"},
				{Name: "service", Type: "string", Description: "Service name when container is not specified"},
				{Name: "index", Type: "integer", Description: "Replica index; defaults to 1"},
				{Name: "command", Type: "string[]", Description: "Command argv to execute"},
				{Name: "shell", Type: "string", Description: "Shell command; converted to shellExecutable -lc"},
				{Name: "shellExecutable", Type: "string", Description: "Shell executable for shell; defaults to /bin/sh"},
				{Name: "workingDir", Type: "string", Description: "Working directory inside the container"},
				{Name: "user", Type: "string", Description: "User inside the container"},
				{Name: "env", Type: "string[]", Description: "Environment variables KEY=VALUE"},
				{Name: "privileged", Type: "boolean", Description: "Run exec privileged"},
				{Name: "tty", Type: "boolean", Description: "Allocate a TTY"},
				{Name: "startStopped", Type: "boolean", Description: "Start a stopped container before exec; defaults to true"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.execProject },
		},
		{
			method:  http.MethodGet,
			path:    "/ps/{project}",
			summary: "List project containers as JSON",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
				{Name: "all", Type: "boolean", Description: "Include stopped containers"},
				{Name: "status", Type: "string", Repeated: true, Description: "Filter by container state"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.psProject },
		},
		{
			method:  http.MethodGet,
			path:    "/logs/{project}",
			summary: "Stream project container logs over SSE using Compose's internal logs implementation",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
				{Name: "follow", Type: "boolean", Description: "Follow log output"},
				{Name: "index", Type: "integer", Description: "Replica index; requires exactly one service"},
				{Name: "tail", Type: "string", Description: "Number of lines from the end; defaults to all"},
				{Name: "since", Type: "string", Description: "Show logs since timestamp or relative duration"},
				{Name: "until", Type: "string", Description: "Show logs before timestamp or relative duration"},
				{Name: "timestamps", Type: "boolean", Description: "Include timestamps"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.logsProject },
		},
		{
			method:  http.MethodGet,
			path:    "/top/{project}",
			summary: "Return running process information for project containers",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.topProject },
		},
		{
			method:  http.MethodPost,
			path:    "/top/{project}/kill",
			summary: "Send a signal to a process inside a project container; hard=true uses SIGKILL",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "container", Type: "string", Description: "Optional container id or name; otherwise service is required"},
				{Name: "service", Type: "string", Description: "Service name when container is not specified"},
				{Name: "index", Type: "integer", Description: "Replica index; defaults to 1"},
				{Name: "pid", Type: "integer", Description: "PID from /top/{project} output"},
				{Name: "signal", Type: "string", Description: "Signal to send; defaults to SIGTERM"},
				{Name: "hard", Type: "boolean", Description: "Send SIGKILL regardless of signal"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.killProcess },
		},
		{
			method:  http.MethodGet,
			path:    "/volumes/{project}",
			summary: "List project volumes as JSON",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.volumesProject },
		},
		{
			method:  http.MethodGet,
			path:    "/events/{project}",
			summary: "Stream project events over SSE using the same plain or JSON rendering as the CLI events command",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
				{Name: "since", Type: "string", Description: "Show events since this timestamp"},
				{Name: "until", Type: "string", Description: "Stop streaming at this timestamp"},
				{Name: "json", Type: "boolean", Description: "Render each event like `compose events --json`"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.eventsProject },
		},
		{
			method:  http.MethodGet,
			path:    "/stats/{project}",
			summary: "Stream project container statistics over SSE",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
				{Name: "all", Type: "boolean", Description: "Include stopped containers"},
				{Name: "no-stream", Type: "boolean", Description: "Send one frame and close"},
				{Name: "no-trunc", Type: "boolean", Description: "Do not truncate container IDs"},
				{Name: "format", Type: "string", Description: "Docker stats formatter template or `json`"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.statsProject },
		},
		{
			method:  http.MethodGet,
			path:    "/resources/{project}",
			summary: "Return project resource usage, configured limits, and utilization as JSON",
			queryParams: []queryParamSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "service", Type: "string", Repeated: true, Description: "Optional service names"},
				{Name: "all", Type: "boolean", Description: "Include stopped containers; stats may be unavailable for stopped containers"},
				{Name: "granularity", Type: "string", Values: []string{"all", "container", "service", "project"}, Description: "Response grouping; defaults to all"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.resourcesProject },
		},
		{
			method:  http.MethodGet,
			path:    "/watch/{project}",
			summary: "Stream watch logs for a project as SSE",
			handler: func(r *composeRouter) httputils.APIFunc { return r.getWatch },
		},
		{
			method:  http.MethodPost,
			path:    "/watch/{project}",
			summary: "Start or restart a watch resource for an already upped project",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Optional project directory, compose file path, or comma-separated compose file list"},
				{Name: "services", Type: "string[]", Description: "Optional service names"},
				{Name: "build", Type: "boolean", Description: "Build before restarting watch; defaults to true"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.postWatch },
		},
		{
			method:  http.MethodDelete,
			path:    "/watch/{project}",
			summary: "Stop a project's watch resource without stopping the running containers",
			handler: func(r *composeRouter) httputils.APIFunc { return r.deleteWatch },
		},
	}
}

func schemaFromRouteSpecs() []routeSchema {
	specs := serveRoutes()
	routes := make([]routeSchema, 0, len(specs))
	for _, spec := range specs {
		routes = append(routes, routeSchema{
			Method:      spec.method,
			Path:        spec.path,
			Versioned:   true,
			Summary:     spec.summary,
			QueryParams: spec.queryParams,
			BodyFields:  spec.bodyFields,
		})
	}
	return routes
}

func parseListFilters(values []string) (client.Filters, error) {
	filters := make(client.Filters)
	for _, value := range values {
		key, match, ok := splitFilter(value)
		if !ok {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid filter %q", value))
		}
		if !acceptedListFilters[key] {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid filter %q", key))
		}
		filters.Add(key, match)
	}
	return filters, nil
}

func splitFilter(value string) (string, string, bool) {
	if key, match, ok := strings.Cut(value, "="); ok && key != "" && match != "" {
		return key, match, true
	}
	return "", "", false
}

func filterProjects(projects []composeapi.Stack, filters client.Filters) []composeapi.Stack {
	var filtered []composeapi.Stack
	for _, project := range projects {
		if matchFilter(filters, "name", project.Name) {
			filtered = append(filtered, project)
		}
	}
	return filtered
}

func filterByStatus(containers []composeapi.ContainerSummary, statuses []string) []composeapi.ContainerSummary {
	var filtered []composeapi.ContainerSummary
	for _, container := range containers {
		if slices.Contains(statuses, string(container.State)) {
			filtered = append(filtered, container)
		}
	}
	return filtered
}

func matchFilter(filters client.Filters, field, source string) bool {
	values, ok := filters[field]
	if !ok {
		return false
	}
	if values[source] {
		return true
	}
	for pattern := range values {
		isMatch, err := regexp.MatchString(pattern, source)
		if err != nil {
			continue
		}
		if isMatch {
			return true
		}
	}
	return false
}

func errWatchNotFound(project string) error {
	return errdefs.NotFound(fmt.Errorf("watch resource %q not found", project))
}

func parseOptionalBool(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, errdefs.InvalidParameter(err)
	}
	return parsed, nil
}
