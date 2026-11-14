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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/compose/v5/server/errdefs"
)

func validateCheckoutOptions(request repoCheckoutRequest) error {
	if request.Branch != "" && (strings.HasPrefix(request.Branch, "-") || strings.ContainsAny(request.Branch, "\x00\r\n \\~^:?*[")) {
		return errdefs.InvalidParameter(errors.New("invalid checkout branch"))
	}
	for _, path := range request.SparsePaths {
		for _, component := range strings.Split(path, "/") {
			if err := validateCheckoutPathComponent(component); err != nil {
				return errdefs.InvalidParameter(fmt.Errorf("invalid sparse checkout folder: %w", err))
			}
		}
	}
	return nil
}

// Git works in the private clone before it is published under the serve root.
func cloneGitRepositoryOptions(ctx context.Context, repositoryURL, destination string, depth int, request repoCheckoutRequest) error {
	args := checkoutGitArguments(repositoryURL, destination, depth)
	options := []string{}
	if request.Branch != "" {
		options = append(options, "--branch="+request.Branch)
	}
	if len(request.SparsePaths) > 0 {
		options = append(options, "--filter=blob:none", "--no-checkout")
	}
	args = append(args[:len(args)-3], append(options, "--", repositoryURL, destination)...)
	if _, err := runCheckoutGit(ctx, nil, "", args...); err != nil {
		return err
	}
	if len(request.SparsePaths) == 0 {
		return nil
	}
	directory, err := os.Open(destination)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := validateSparseCheckoutFolders(ctx, directory, request.SparsePaths); err != nil {
		return err
	}
	if _, err := runCheckoutGit(ctx, directory, strings.Join(request.SparsePaths, "\n")+"\n", "sparse-checkout", "set", "--cone", "--stdin"); err != nil {
		return err
	}
	_, err = runCheckoutGit(ctx, directory, "", "checkout")
	return err
}

func reuseRepositoryCheckout(ctx context.Context, root *os.Root, destination string, request repoCheckoutRequest) error {
	for _, path := range []string{destination, filepath.Join(destination, ".git")} {
		info, err := root.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errdefs.Conflict(fmt.Errorf("checkout destination %q must be an existing repository with its own .git directory", destination))
		}
	}
	directory, err := root.Open(destination)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	git := func(args ...string) (string, error) { return runCheckoutGit(ctx, directory, "", args...) }
	origin, err := git("remote", "get-url", "origin")
	if err != nil || canonicalCheckoutURL(origin) != canonicalCheckoutURL(request.URL) {
		return errdefs.Conflict(errors.New("existing checkout belongs to a different repository"))
	}
	if request.Branch != "" {
		branch, err := git("symbolic-ref", "--short", "HEAD")
		if err != nil || branch != request.Branch {
			return errdefs.Conflict(errors.New("existing checkout uses a different branch; choose another parent folder"))
		}
	}
	if err := validateSparseCheckoutFolders(ctx, directory, request.SparsePaths); err != nil {
		return err
	}
	sparse, err := git("config", "--bool", "--get", "core.sparseCheckout")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return err
		}
	}
	// A full checkout already contains every requested folder. Never shrink it.
	if sparse != "true" {
		return nil
	}
	if len(request.SparsePaths) == 0 {
		_, err = git("sparse-checkout", "disable")
		return err
	}
	_, err = runCheckoutGit(ctx, directory, strings.Join(request.SparsePaths, "\n")+"\n", "sparse-checkout", "add", "--stdin")
	return err
}

func canonicalCheckoutURL(value string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(value), "/"), ".git")
}

func validateSparseCheckoutFolders(ctx context.Context, directory *os.File, paths []string) error {
	for _, path := range paths {
		kind, err := runCheckoutGit(ctx, directory, "", "cat-file", "-t", "HEAD:"+path)
		if err != nil || kind != "tree" {
			return errdefs.InvalidParameter(fmt.Errorf("sparse checkout folder %q does not exist on the checked-out branch", path))
		}
	}
	return nil
}

func runCheckoutGit(ctx context.Context, directory *os.File, input string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "core.hooksPath=", "-c", "core.fsmonitor=false",
	}, args...)...)
	cmd.Env = checkoutGitEnvironment()
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = 5 * time.Second
	if directory != nil {
		setCheckoutCommandDirectory(cmd, directory)
	}
	output := &limitedBuffer{limit: maxGitOutputBytes}
	stderr := &limitedBuffer{limit: maxGitOutputBytes}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := runCheckoutCommand(cmd); err != nil {
		return "", fmt.Errorf("git checkout failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(output.String()), nil
}
