package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

func TestServeTargetForArgsUsesPortWhenSet(t *testing.T) {
	target, err := serveTargetForConfig(serveConfig{rootDir: "/ignored"}, 8080)
	assert.NilError(t, err)
	assert.Equal(t, target.network, "tcp")
	assert.Equal(t, target.address, "127.0.0.1:8080")
}

func TestSocketPathForDirUsesDockerDefaultResolution(t *testing.T) {
	prev, hadPrev := os.LookupEnv(client.EnvOverrideHost)
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv(client.EnvOverrideHost, prev)
		} else {
			_ = os.Unsetenv(client.EnvOverrideHost)
		}
	})
	_ = os.Unsetenv(client.EnvOverrideHost)

	path, err := socketPathForDir("")
	assert.NilError(t, err)
	assert.Equal(t, path, "/var/run/compose.sock")
}

func TestSocketPathForDirUsesProvidedDirectoryVerbatim(t *testing.T) {
	path, err := socketPathForDir("./tmp")
	assert.NilError(t, err)
	assert.Equal(t, path, "tmp/compose.sock")
}

func TestMergeStacksKeepsExistingAndAddsUncreated(t *testing.T) {
	merged := mergeStacks(
		[]composeapi.Stack{{Name: "running", Status: "running(1)"}},
		[]composeapi.Stack{{Name: "running", Status: "uncreated"}, {Name: "fresh", Status: "uncreated"}},
	)
	assert.Equal(t, len(merged), 2)
	assert.Equal(t, merged[0].Name, "fresh")
	assert.Equal(t, merged[0].Status, "uncreated")
	assert.Equal(t, merged[1].Name, "running")
	assert.Equal(t, merged[1].Status, "running(1)")
}

func TestWrapCORSAllowsConfiguredOrigin(t *testing.T) {
	handler := wrapCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodGet, "/ls", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusOK)
	assert.Equal(t, resp.Header().Get("Access-Control-Allow-Origin"), "http://localhost:3000")
	assert.Assert(t, resp.Header().Get("Access-Control-Allow-Methods") != "")
}

func TestWrapCORSRejectsDisallowedPreflight(t *testing.T) {
	handler := wrapCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached for rejected preflight")
	}), []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodOptions, "/up", http.NoBody)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(t, resp.Code, http.StatusForbidden)
}

func TestFormatAllowedOriginsDisabled(t *testing.T) {
	assert.Equal(t, formatAllowedOrigins(nil), "(disabled)")
}

func TestResolveProjectLoadRefAllowsAbsoluteComposeFile(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "demo", "compose.yaml")
	assert.NilError(t, os.MkdirAll(filepath.Dir(configFile), 0o755))
	assert.NilError(t, os.WriteFile(configFile, []byte("services: {}\n"), 0o644))

	app := newServerApp(serveConfig{rootDir: root}, nil, nil)
	ref, err := app.resolveProjectLoadRef(configFile)
	assert.NilError(t, err)
	assert.Equal(t, ref.workingDir, filepath.Dir(configFile))
	assert.DeepEqual(t, ref.configPaths, []string{configFile})
}

func TestResolveProjectLoadRefAllowsAbsoluteComposeFileList(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "one", "compose.yaml")
	second := filepath.Join(root, "two", "compose.yaml")
	assert.NilError(t, os.MkdirAll(filepath.Dir(first), 0o755))
	assert.NilError(t, os.MkdirAll(filepath.Dir(second), 0o755))
	assert.NilError(t, os.WriteFile(first, []byte("services: {}\n"), 0o644))
	assert.NilError(t, os.WriteFile(second, []byte("services: {}\n"), 0o644))

	app := newServerApp(serveConfig{rootDir: root}, nil, nil)
	ref, err := app.resolveProjectLoadRef(first + "," + second)
	assert.NilError(t, err)
	assert.Equal(t, ref.workingDir, filepath.Dir(first))
	assert.DeepEqual(t, ref.configPaths, []string{first, second})
}

func TestResolveProjectLoadRefRejectsRelativeEscape(t *testing.T) {
	root := t.TempDir()
	app := newServerApp(serveConfig{rootDir: root}, nil, nil)

	_, err := app.resolveProjectLoadRef("../outside")
	assert.ErrorContains(t, err, "path escapes serve root")
}
