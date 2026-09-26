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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/server/errdefs"
)

func TestCheckoutRepositoryName(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "git suffix", url: "https://example.com/acme/demo.git", want: "demo"},
		{name: "no suffix", url: "https://example.com/acme/demo", want: "demo"},
		{name: "trailing slash", url: "https://example.com/acme/demo.git/", want: "demo"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := checkoutRepositoryName(test.url)
			assert.NilError(t, err)
			assert.Equal(t, got, test.want)
		})
	}
}

func TestCheckoutRepositoryNameRejectsUnsafeURLs(t *testing.T) {
	urls := []string{
		"",
		"http://example.com/acme/demo.git",
		"https://user:secret@example.com/acme/demo.git",
		"file:///tmp/demo.git",
		"ssh://git@example.com/acme/demo.git",
		"git@example.com:acme/demo.git",
		"https://example.com/",
		"https://example.com/acme/.git.git",
		"https://example.com/acme/.GIT.git",
		"https://example.com/acme/git~1.git",
		"https://example.com/acme/demo..git",
		"https://example.com/acme/demo.git?token=secret",
		"https://example.com/acme/demo.git#main",
		" https://example.com/acme/demo.git",
	}

	for _, repositoryURL := range urls {
		t.Run(repositoryURL, func(t *testing.T) {
			_, err := checkoutRepositoryName(repositoryURL)
			assert.Assert(t, errdefs.IsInvalidParameter(err))
		})
	}
}

func TestCheckoutPathComponents(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{path: "", want: nil},
		{path: "team", want: []string{"team"}},
		{path: "team/product/dev", want: []string{"team", "product", "dev"}},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			got, err := checkoutPathComponents(test.path)
			assert.NilError(t, err)
			assert.DeepEqual(t, got, test.want)
		})
	}
}

func TestCheckoutPathComponentsRejectsTraversal(t *testing.T) {
	paths := []string{
		"/team",
		"../team",
		"team/../project",
		"team/./project",
		"team//project",
		"team/project/",
		`team\project`,
		"one/two/three/four",
		"team/.git",
		"team/.GIT",
		"team/git~1",
		"team./dev",
		"team/dev ",
		" team",
	}

	for _, requestPath := range paths {
		t.Run(requestPath, func(t *testing.T) {
			_, err := checkoutPathComponents(requestPath)
			assert.Assert(t, errdefs.IsInvalidParameter(err))
		})
	}
}

func TestRepoCheckoutClonesWithoutPathPrefix(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.Mkdir(filepath.Join(root, "another-stack"), 0o755))
	service := fakeRepoCheckoutService(root, func(ctx context.Context, repositoryURL, destination string, depth int) error {
		assert.Equal(t, repositoryURL, "https://example.com/acme/demo.git")
		assert.Equal(t, depth, maxCheckoutGitDepth)
		assert.Assert(t, filepath.Dir(destination) != root)
		assert.NilError(t, os.Mkdir(destination, 0o755))
		return os.WriteFile(filepath.Join(destination, "compose.yaml"), []byte("services: {}\n"), 0o644)
	})

	response, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/acme/demo.git"})

	assert.NilError(t, err)
	assert.Equal(t, response.Path, "demo")
	assert.Equal(t, response.Depth, maxCheckoutGitDepth)
	content, err := os.ReadFile(filepath.Join(root, "demo", "compose.yaml"))
	assert.NilError(t, err)
	assert.Equal(t, string(content), "services: {}\n")
	info, err := os.Stat(filepath.Join(root, "demo"))
	assert.NilError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, info.Mode().Perm(), os.FileMode(0o755))
	}
	assertNoTemporaryCheckouts(t, root)
}

func TestRepoCheckoutClonesUnderThreeComponentPath(t *testing.T) {
	root := t.TempDir()
	service := fakeRepoCheckoutService(root, writeFakeCheckout)

	response, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:  "https://example.com/acme/demo.git",
		Path: "team/product/dev",
	})

	assert.NilError(t, err)
	assert.Equal(t, response.Path, "team/product/dev/demo")
	_, err = os.Stat(filepath.Join(root, "team", "product", "dev", "demo", "compose.yaml"))
	assert.NilError(t, err)
}

func TestRepoCheckoutUsesRequestedDepth(t *testing.T) {
	root := t.TempDir()
	service := fakeRepoCheckoutService(root, func(ctx context.Context, repositoryURL, destination string, depth int) error {
		assert.Equal(t, depth, 25)
		return writeFakeCheckout(ctx, repositoryURL, destination, depth)
	})
	depth := 25

	response, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:   "https://example.com/acme/demo.git",
		Depth: &depth,
	})

	assert.NilError(t, err)
	assert.Equal(t, response.Depth, depth)
}

func TestRepoCheckoutRejectsInvalidDepth(t *testing.T) {
	for _, depth := range []int{-1, 0, maxCheckoutGitDepth + 1} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			root := t.TempDir()
			service := fakeRepoCheckoutService(root, func(context.Context, string, string, int) error {
				t.Fatal("invalid depth reached Git clone")
				return nil
			})
			_, err := service.checkout(t.Context(), repoCheckoutRequest{
				URL:   "https://example.com/acme/demo.git",
				Depth: &depth,
			})
			assert.Assert(t, errdefs.IsInvalidParameter(err))
		})
	}
}

func TestRepoCheckoutPreservesExistingEmptyDestination(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "demo")
	assert.NilError(t, os.Mkdir(destination, 0o700))
	service := fakeRepoCheckoutService(root, writeFakeCheckout)

	_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/acme/demo.git"})

	assert.Assert(t, errdefs.IsConflict(err))
	info, err := os.Stat(destination)
	assert.NilError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o700))
}

func TestRepoCheckoutRejectsFilesInParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "team")
	assert.NilError(t, os.Mkdir(parent, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(parent, "notes.txt"), []byte("keep"), 0o644))
	called := false
	service := fakeRepoCheckoutService(root, func(context.Context, string, string, int) error {
		called = true
		return nil
	})

	_, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:  "https://example.com/acme/demo.git",
		Path: "team",
	})

	assert.Assert(t, errdefs.IsConflict(err))
	assert.Assert(t, !called)
}

func TestRepoCheckoutRejectsNonEmptyDestination(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "demo")
	assert.NilError(t, os.Mkdir(destination, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(destination, "compose.yaml"), []byte("keep"), 0o644))
	service := fakeRepoCheckoutService(root, writeFakeCheckout)

	_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/acme/demo.git"})

	assert.Assert(t, errdefs.IsConflict(err))
	content, readErr := os.ReadFile(filepath.Join(destination, "compose.yaml"))
	assert.NilError(t, readErr)
	assert.Equal(t, string(content), "keep")
}

func TestRepoCheckoutRejectsSymlinkInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional privileges on Windows")
	}

	root := t.TempDir()
	outside := t.TempDir()
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "team")))
	service := fakeRepoCheckoutService(root, writeFakeCheckout)

	_, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:  "https://example.com/acme/demo.git",
		Path: "team",
	})

	assert.Assert(t, errdefs.IsInvalidParameter(err))
	_, statErr := os.Stat(filepath.Join(outside, "demo"))
	assert.Assert(t, os.IsNotExist(statErr))
}

func TestRepoCheckoutCleansUpAfterCloneFailure(t *testing.T) {
	root := t.TempDir()
	service := fakeRepoCheckoutService(root, func(context.Context, string, string, int) error {
		return errors.New("clone failed")
	})

	_, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:  "https://example.com/acme/demo.git",
		Path: "new/parent",
	})

	assert.ErrorContains(t, err, "clone failed")
	_, statErr := os.Stat(filepath.Join(root, "new"))
	assert.Assert(t, os.IsNotExist(statErr))
}

func TestRemoveOwnedCheckoutStageRemovesPartialCheckout(t *testing.T) {
	serveDir := t.TempDir()
	root, err := os.OpenRoot(serveDir)
	assert.NilError(t, err)
	defer func() { _ = root.Close() }()
	stage, stageInfo, err := createCheckoutStage(root, ".")
	assert.NilError(t, err)

	source := t.TempDir()
	assert.NilError(t, os.Mkdir(filepath.Join(source, "nested"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(source, "nested", "compose.yaml"), []byte("services: {}\n"), 0o644))
	assert.NilError(t, copyCheckoutTree(t.Context(), root, source, stage))
	// Staging content is disposable even if it differs from the cloned tree.
	assert.NilError(t, os.WriteFile(filepath.Join(serveDir, stage, "unexpected.txt"), []byte("discard"), 0o600))
	assert.NilError(t, removeOwnedCheckoutStage(root, stage, stageInfo))
	_, err = root.Lstat(stage)
	assert.Assert(t, os.IsNotExist(err))
}

func TestRemoveOwnedCheckoutStagePreservesReplacementDirectory(t *testing.T) {
	serveDir := t.TempDir()
	root, err := os.OpenRoot(serveDir)
	assert.NilError(t, err)
	defer func() { _ = root.Close() }()
	stage, stageInfo, err := createCheckoutStage(root, ".")
	assert.NilError(t, err)
	original, err := root.Open(stage)
	assert.NilError(t, err)
	defer func() { _ = original.Close() }()
	assert.NilError(t, root.Rename(stage, "moved-stage"))
	assert.NilError(t, root.Mkdir(stage, 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(serveDir, stage, "keep.txt"), []byte("keep"), 0o600))

	err = removeOwnedCheckoutStage(root, stage, stageInfo)

	assert.Assert(t, errdefs.IsConflict(err))
	content, err := os.ReadFile(filepath.Join(serveDir, stage, "keep.txt"))
	assert.NilError(t, err)
	assert.Equal(t, string(content), "keep")
}

func TestRepoCheckoutRejectsParentSwappedToOutsideSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	assert.NilError(t, os.Mkdir(filepath.Join(root, "team"), 0o755))
	service := fakeRepoCheckoutService(root, func(ctx context.Context, repositoryURL, destination string, depth int) error {
		assert.NilError(t, os.Rename(filepath.Join(root, "team"), filepath.Join(root, "old-team")))
		assert.NilError(t, os.Symlink(outside, filepath.Join(root, "team")))
		return writeFakeCheckout(ctx, repositoryURL, destination, depth)
	})

	_, err := service.checkout(t.Context(), repoCheckoutRequest{
		URL:  "https://example.com/acme/demo.git",
		Path: "team",
	})

	assert.Assert(t, err != nil)
	_, err = os.Stat(filepath.Join(outside, "demo"))
	assert.Assert(t, os.IsNotExist(err))
}

func TestRepoCheckoutStaysWithOpenedServeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional privileges on Windows")
	}
	base := t.TempDir()
	stacks := filepath.Join(base, "stacks")
	moved := filepath.Join(base, "moved-stacks")
	outside := filepath.Join(base, "outside")
	assert.NilError(t, os.Mkdir(stacks, 0o755))
	assert.NilError(t, os.Mkdir(outside, 0o755))
	service := fakeRepoCheckoutService(stacks, writeFakeCheckout)
	assert.NilError(t, os.Rename(stacks, moved))
	assert.NilError(t, os.Symlink(outside, stacks))

	_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/acme/demo.git"})

	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(moved, "demo", "compose.yaml"))
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(outside, "demo"))
	assert.Assert(t, os.IsNotExist(err))
}

func TestCheckoutGitEnvironmentDisablesAskpass(t *testing.T) {
	t.Setenv("SSH_ASKPASS", "/tmp/should-not-run")
	t.Setenv("GIT_ASKPASS", "/tmp/should-not-run")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GH_DEBUG", "api")
	t.Setenv("GH_PROMPT_DISABLED", "0")
	t.Setenv("GH_NO_UPDATE_NOTIFIER", "0")
	environment := checkoutGitEnvironment()
	for _, value := range environment {
		assert.Assert(t, !strings.HasPrefix(value, "SSH_ASKPASS="), value)
		assert.Assert(t, !strings.HasPrefix(value, "GIT_CONFIG_COUNT="), value)
		assert.Assert(t, !strings.HasPrefix(value, "GH_DEBUG="), value)
	}
	assert.Assert(t, containsEnvironmentValue(environment, "GIT_ASKPASS="))
	assert.Assert(t, containsEnvironmentValue(environment, "GIT_TERMINAL_PROMPT=0"))
	assert.Assert(t, containsEnvironmentValue(environment, "GH_PROMPT_DISABLED=1"))
	assert.Assert(t, containsEnvironmentValue(environment, "GH_NO_UPDATE_NOTIFIER=1"))
}

func TestComposeDiscoverySkipsCheckoutStage(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, checkoutStagePrefix+"in-progress")
	assert.NilError(t, os.Mkdir(stage, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(stage, "compose.yaml"), []byte("services: {}\n"), 0o644))
	directories, err := findComposeDirectories(discoveryOptions{rootDir: root, maxDepth: 3})
	assert.NilError(t, err)
	assert.Equal(t, len(directories), 0)
}

func containsEnvironmentValue(environment []string, expected string) bool {
	for _, value := range environment {
		if value == expected {
			return true
		}
	}
	return false
}

func TestCheckoutRepositoryRoute(t *testing.T) {
	root := t.TempDir()
	router := &composeRouter{
		app:      newServerApp(serveConfig{rootDir: root}, nil, nil),
		checkout: fakeRepoCheckoutService(root, writeFakeCheckout),
	}
	req := httptest.NewRequest(http.MethodPost, "/repos/checkout", strings.NewReader(`{"url":"https://example.com/acme/demo.git","depth":25}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	err := withJSONError(router.checkoutRepository)(t.Context(), resp, req, nil)

	assert.NilError(t, err)
	assert.Equal(t, resp.Code, http.StatusCreated)
	var response repoCheckoutResponse
	assert.NilError(t, json.Unmarshal(resp.Body.Bytes(), &response))
	assert.Equal(t, response.Path, "demo")
	assert.Equal(t, response.Depth, 25)
	assert.Assert(t, hasRoute(schemaFromRouteSpecs(), http.MethodPost, "/repos/checkout"))
}

func fakeRepoCheckoutService(root string, clone repoCloneFunc) *repoCheckoutService {
	service := newRepoCheckoutService(root)
	service.clone = clone
	return service
}

func writeFakeCheckout(ctx context.Context, repositoryURL, destination string, depth int) error {
	if err := os.Mkdir(destination, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "compose.yaml"), []byte("services: {}\n"), 0o644)
}

func assertNoTemporaryCheckouts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	assert.NilError(t, err)
	for _, entry := range entries {
		assert.Assert(t, !strings.Contains(entry.Name(), ".checkout-"), entry.Name())
	}
}
