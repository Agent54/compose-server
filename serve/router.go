package serve

import (
	"context"
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
		routes = append(routes, router.NewRoute(spec.method, spec.path, spec.handler(r)))
	}
	return routes
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
			method:  http.MethodPost,
			path:    "/up",
			summary: "Start a Compose project; optional watch mode starts a per-project watch resource",
			bodyFields: []bodyFieldSpec{
				{Name: "path", Type: "string", Description: "Path relative to the serve root"},
				{Name: "build", Type: "boolean", Description: "Build before starting"},
				{Name: "watch", Type: "boolean", Description: "Start watch mode after up succeeds"},
			},
			handler: func(r *composeRouter) httputils.APIFunc { return r.upProject },
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
				{Name: "path", Type: "string", Description: "Optional project path relative to the serve root"},
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
