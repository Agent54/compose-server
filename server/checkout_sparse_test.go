/*
   Copyright 2026 Docker Compose CLI authors

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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/server/errdefs"
)

func sparseTestRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "agents")
	assert.NilError(t, os.Mkdir(repository, 0o755))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repository
		output, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(output))
	}
	git("init", "-b", "main")
	for _, folder := range []string{"alpha", "beta", "gamma"} {
		assert.NilError(t, os.Mkdir(filepath.Join(repository, folder), 0o755))
		assert.NilError(t, os.WriteFile(filepath.Join(repository, folder, "compose.yaml"), []byte("services: {}\n"), 0o644))
	}
	git("add", ".")
	git("-c", "user.name=Checkout test", "-c", "user.email=checkout@example.test", "-c", "commit.gpgsign=false", "commit", "-m", "Apps")
	git("remote", "add", "origin", "https://example.com/team/agents.git")
	return root, repository
}

func TestRepoCheckoutSparseAddsFolders(t *testing.T) {
	root, repository := sparseTestRepository(t)
	directory, err := os.Open(repository)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = directory.Close() })
	_, err = runCheckoutGit(t.Context(), directory, "alpha\n", "sparse-checkout", "set", "--cone", "--stdin")
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(filepath.Join(repository, "alpha", "compose.yaml"), []byte("local changes"), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(repository, "alpha", "notes.txt"), []byte("keep me"), 0o644))
	service := fakeRepoCheckoutService(root, writeFakeCheckout)
	request := repoCheckoutRequest{URL: "https://example.com/team/agents", Branch: "main", SparsePaths: []string{"beta"}}
	for range 2 {
		response, err := service.checkout(t.Context(), request)
		assert.NilError(t, err)
		assert.Equal(t, response.Path, "agents")
	}
	content, err := os.ReadFile(filepath.Join(repository, "alpha", "compose.yaml"))
	assert.NilError(t, err)
	assert.Equal(t, string(content), "local changes")
	content, err = os.ReadFile(filepath.Join(repository, "alpha", "notes.txt"))
	assert.NilError(t, err)
	assert.Equal(t, string(content), "keep me")
	_, err = os.Stat(filepath.Join(repository, "beta", "compose.yaml"))
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(repository, "gamma"))
	assert.Assert(t, os.IsNotExist(err))
	request.SparsePaths = nil
	_, err = service.checkout(t.Context(), request)
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(repository, "gamma", "compose.yaml"))
	assert.NilError(t, err)
}

func TestRepoCheckoutReusesFullRepositoryWithoutShrinking(t *testing.T) {
	root, repository := sparseTestRepository(t)
	service := fakeRepoCheckoutService(root, writeFakeCheckout)
	_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/team/agents.git/", SparsePaths: []string{"beta"}})
	assert.NilError(t, err)
	for _, folder := range []string{"alpha", "beta", "gamma"} {
		_, err := os.Stat(filepath.Join(repository, folder, "compose.yaml"))
		assert.NilError(t, err)
	}
}

func TestRepoCheckoutRejectsDifferentOriginOrBranch(t *testing.T) {
	root, _ := sparseTestRepository(t)
	service := fakeRepoCheckoutService(root, writeFakeCheckout)
	for _, request := range []repoCheckoutRequest{
		{URL: "https://example.com/other/agents.git", SparsePaths: []string{"beta"}},
		{URL: "https://example.com/team/agents.git", Branch: "other", SparsePaths: []string{"beta"}},
	} {
		_, err := service.checkout(t.Context(), request)
		assert.Assert(t, errdefs.IsConflict(err))
	}
}

func TestRepoCheckoutConcurrentSparseAdds(t *testing.T) {
	root, repository := sparseTestRepository(t)
	directory, err := os.Open(repository)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = directory.Close() })
	_, err = runCheckoutGit(t.Context(), directory, "alpha\n", "sparse-checkout", "set", "--cone", "--stdin")
	assert.NilError(t, err)
	service := fakeRepoCheckoutService(root, writeFakeCheckout)
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, folder := range []string{"beta", "gamma"} {
		wait.Go(func() {
			_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/team/agents", SparsePaths: []string{folder}})
			errors <- err
		})
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		assert.NilError(t, err)
	}
	for _, folder := range []string{"alpha", "beta", "gamma"} {
		_, err := os.Stat(filepath.Join(repository, folder, "compose.yaml"))
		assert.NilError(t, err)
	}
}

func TestCheckoutRejectsUnsafeSparseFolders(t *testing.T) {
	for _, path := range []string{"", "../alpha", "/alpha", "alpha/../beta", "alpha//beta", "alpha\\beta", ".git", "alpha/.git", "alpha\nbeta"} {
		assert.Assert(t, errdefs.IsInvalidParameter(validateCheckoutOptions(repoCheckoutRequest{SparsePaths: []string{path}})))
	}
	assert.NilError(t, validateCheckoutOptions(repoCheckoutRequest{SparsePaths: []string{"apps/agents/alpha/shared", "beta"}, Branch: "feature/agents"}))
}

func TestCheckoutSparseClone(t *testing.T) {
	_, source := sparseTestRepository(t)
	// The production API validates HTTPS URLs; this transport is only for a
	// hermetic test of Git's initial sparse checkout with no external server.
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	destination := filepath.Join(t.TempDir(), "clone")
	repositoryURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(source)}).String()
	err := cloneGitRepositoryOptions(t.Context(), repositoryURL, destination, 10, repoCheckoutRequest{Branch: "main", SparsePaths: []string{"alpha"}})
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(destination, "alpha", "compose.yaml"))
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(destination, "beta"))
	assert.Assert(t, os.IsNotExist(err))
}

func TestRepoCheckoutRejectsMissingSparseFolder(t *testing.T) {
	root, _ := sparseTestRepository(t)
	service := fakeRepoCheckoutService(root, writeFakeCheckout)
	_, err := service.checkout(t.Context(), repoCheckoutRequest{URL: "https://example.com/team/agents", SparsePaths: []string{"missing"}})
	assert.Assert(t, errdefs.IsInvalidParameter(err))
}
