package serve

import (
	"os"
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
