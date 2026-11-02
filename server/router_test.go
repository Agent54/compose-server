/*
   Copyright 2020 Docker Compose CLI authors

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

package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	engineapi "github.com/docker/docker/api"
	engineserver "github.com/docker/docker/api/server"
	"github.com/docker/docker/api/server/middleware"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/internal"
	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestUpBuildSurvivesClientDisconnect(t *testing.T) {
	requestCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := mocks.NewMockCompose(gomock.NewController(t))
	root := t.TempDir()
	app := newServerApp(serveConfig{rootDir: root}, func(...composepkg.Option) (composeapi.Compose, error) {
		return backend, nil
	}, nil)
	project := &types.Project{Name: "demo", Services: types.Services{
		"web": {Name: "web", Build: &types.BuildConfig{Context: root}},
	}}
	backend.EXPECT().LoadProject(gomock.Any(), gomock.Any()).Return(project, nil).Times(2)
	backend.EXPECT().Up(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ *types.Project, _ composeapi.UpOptions) error {
		cancel()
		return ctx.Err()
	})
	server := &engineserver.Server{}
	mux := server.CreateMux(t.Context(), newRouter(app))
	payload, err := json.Marshal(upRequest{Project: "demo", Path: root, Build: true})
	assert.NilError(t, err)
	request := httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/up", bytes.NewReader(payload))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
	assert.Equal(t, response.Code, http.StatusOK)
	history := app.listBuilds()
	assert.Equal(t, len(history), 1)
	assert.Equal(t, history[0].Status, "succeeded")
}

func TestCommandAndObservationRequestLifetimes(t *testing.T) {
	for _, test := range []struct {
		method   string
		detached bool
	}{
		{method: http.MethodPost, detached: true},
		{method: http.MethodPut, detached: true},
		{method: http.MethodPatch, detached: true},
		{method: http.MethodDelete, detached: true},
		{method: http.MethodGet},
		{method: http.MethodHead},
	} {
		t.Run(test.method, func(t *testing.T) {
			type contextKey struct{}
			requestCtx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "request-value"), time.Minute)
			defer cancel()
			request := httptest.NewRequestWithContext(requestCtx, test.method, "/", http.NoBody)
			called := false
			handler := withDetachedCommands(func(ctx context.Context, _ http.ResponseWriter, req *http.Request, _ map[string]string) error {
				called = true
				cancel()
				assert.Equal(t, ctx.Value(contextKey{}), "request-value")
				assert.Equal(t, req.Context(), ctx)
				_, deadline := ctx.Deadline()
				assert.Equal(t, deadline, !test.detached)
				if test.detached {
					assert.NilError(t, ctx.Err())
					assert.Assert(t, ctx.Done() == nil)
					return nil
				}
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
				return nil
			})

			assert.NilError(t, handler(requestCtx, httptest.NewRecorder(), request, nil))
			assert.Equal(t, called, true)
		})
	}
}

func TestListProjectsRouteSupportsVersionedPath(t *testing.T) {
	versionMiddleware, err := middleware.NewVersionMiddleware(internal.Version, engineapi.DefaultVersion, engineapi.MinSupportedAPIVersion)
	assert.NilError(t, err)

	server := &engineserver.Server{}
	server.UseMiddleware(*versionMiddleware)
	app := newServerApp(serveConfig{rootDir: ".", maxDepth: defaultMaxDepth, excludedDir: defaultExcludedDirs}, nil, nil)
	app.listOverride = func(ctx context.Context, options composeapi.ListOptions) ([]composeapi.Stack, error) {
		assert.Equal(t, options.All, true)
		return []composeapi.Stack{{Name: "demo"}}, nil
	}
	mux := server.CreateMux(t.Context(), newRouter(app))

	req := httptest.NewRequest(http.MethodGet, "/v1.51/ls?all=true", http.NoBody)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusOK)
	assert.Equal(t, resp.Header().Get("Api-Version"), engineapi.DefaultVersion)

	var got []composeapi.Stack
	assert.NilError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Name, "demo")
}

func TestListProjectsRouteRejectsUnsupportedVersion(t *testing.T) {
	versionMiddleware, err := middleware.NewVersionMiddleware(internal.Version, engineapi.DefaultVersion, engineapi.MinSupportedAPIVersion)
	assert.NilError(t, err)

	server := &engineserver.Server{}
	server.UseMiddleware(*versionMiddleware)
	app := newServerApp(serveConfig{rootDir: ".", maxDepth: defaultMaxDepth, excludedDir: defaultExcludedDirs}, nil, nil)
	app.listOverride = func(ctx context.Context, options composeapi.ListOptions) ([]composeapi.Stack, error) {
		t.Fatal("list should not be called for unsupported versions")
		return nil, nil
	}
	mux := server.CreateMux(t.Context(), newRouter(app))

	req := httptest.NewRequest(http.MethodGet, "/v999.0/ls", http.NoBody)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusBadRequest)
	assert.Assert(t, resp.Body.Len() > 0)
}

func TestListProjectsRouteAppliesNameFilter(t *testing.T) {
	versionMiddleware, err := middleware.NewVersionMiddleware(internal.Version, engineapi.DefaultVersion, engineapi.MinSupportedAPIVersion)
	assert.NilError(t, err)

	server := &engineserver.Server{}
	server.UseMiddleware(*versionMiddleware)
	app := newServerApp(serveConfig{rootDir: ".", maxDepth: defaultMaxDepth, excludedDir: defaultExcludedDirs}, nil, nil)
	app.listOverride = func(ctx context.Context, options composeapi.ListOptions) ([]composeapi.Stack, error) {
		return []composeapi.Stack{{Name: "demo"}, {Name: "other"}}, nil
	}
	mux := server.CreateMux(t.Context(), newRouter(app))

	req := httptest.NewRequest(http.MethodGet, "/ls?filter=name=demo", http.NoBody)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusOK)

	var got []composeapi.Stack
	assert.NilError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Name, "demo")
}

func TestRootReturnsSchemaFromRouteDefinitions(t *testing.T) {
	versionMiddleware, err := middleware.NewVersionMiddleware(internal.Version, engineapi.DefaultVersion, engineapi.MinSupportedAPIVersion)
	assert.NilError(t, err)

	server := &engineserver.Server{}
	server.UseMiddleware(*versionMiddleware)
	cfg := serveConfig{
		rootDir:        "/srv/work",
		maxDepth:       3,
		excludedDir:    []string{".git", "node_modules"},
		allowedOrigins: []string{"http://localhost:3000"},
	}
	mux := server.CreateMux(t.Context(), newRouter(newServerApp(cfg, nil, nil)))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusOK)

	var schema apiSchema
	assert.NilError(t, json.Unmarshal(resp.Body.Bytes(), &schema))
	assert.Equal(t, schema.VersionMatcher, "/v{version:[0-9.]+}")
	assert.Equal(t, len(schema.Routes), len(serveRoutes()))
	assert.Equal(t, schema.Config.RootDir, cfg.rootDir)
	assert.Equal(t, schema.Config.MaxDepth, cfg.maxDepth)
	assert.DeepEqual(t, schema.Config.ExcludedDirs, cfg.excludedDir)
	assert.DeepEqual(t, schema.Config.AllowedOrigins, cfg.allowedOrigins)
	assert.Equal(t, schema.Routes[0].Path, "/")
	assert.Equal(t, schema.Routes[0].Method, http.MethodGet)
	assert.Assert(t, hasRoute(schema.Routes, http.MethodPost, "/start/{project}"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodPost, "/start/{project}/container"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodGet, "/events/{project}"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodGet, "/stats/{project}"))
	assert.Assert(t, !hasRoute(schema.Routes, http.MethodGet, "/system/df"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodGet, "/builds"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodGet, "/builds/{build}/stream"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodGet, "/config/{project}"))
	assert.Assert(t, hasRoute(schema.Routes, http.MethodPost, "/pause/{project}"))
}

func hasRoute(routes []routeSchema, method, path string) bool {
	for _, route := range routes {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
