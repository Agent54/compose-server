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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/compose/v5/server/errdefs"
)

const (
	maxCheckoutPathDepth = 3
	maxCheckoutGitDepth  = 100
	checkoutTimeout      = 5 * time.Minute
	maxGitOutputBytes    = 64 * 1024
	checkoutStagePrefix  = ".compose-checkout-"
)

type repoCheckoutRequest struct {
	URL   string `json:"url"`
	Path  string `json:"path,omitempty"`
	Depth *int   `json:"depth,omitempty"`
}

type repoCheckoutResponse struct {
	OK    bool   `json:"ok"`
	URL   string `json:"url"`
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

type repoCloneFunc func(context.Context, string, string, int) error

type repoCheckoutService struct {
	root    *os.Root
	rootErr error
	clone   repoCloneFunc
	locks   checkoutPathLocks
}

func newRepoCheckoutService(rootDir string) *repoCheckoutService {
	root, err := os.OpenRoot(rootDir)
	return &repoCheckoutService{
		root:    root,
		rootErr: err,
		clone:   cloneGitRepository,
	}
}

func (r *composeRouter) checkoutRepository(ctx context.Context, w http.ResponseWriter, req *http.Request, vars map[string]string) error {
	var body repoCheckoutRequest
	if err := decodeJSONBody(req, &body); err != nil {
		return err
	}

	response, err := r.checkout.checkout(ctx, body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, response)
}

func (s *repoCheckoutService) checkout(ctx context.Context, request repoCheckoutRequest) (response repoCheckoutResponse, returnErr error) {
	repositoryName, err := checkoutRepositoryName(request.URL)
	if err != nil {
		return repoCheckoutResponse{}, err
	}
	components, err := checkoutPathComponents(request.Path)
	if err != nil {
		return repoCheckoutResponse{}, err
	}
	depth := maxCheckoutGitDepth
	if request.Depth != nil {
		depth = *request.Depth
	}
	if depth < 1 || depth > maxCheckoutGitDepth {
		return repoCheckoutResponse{}, errdefs.InvalidParameter(fmt.Errorf("depth must be between 1 and %d", maxCheckoutGitDepth))
	}

	if s.rootErr != nil {
		return repoCheckoutResponse{}, fmt.Errorf("open serve root: %w", s.rootErr)
	}
	root := s.root

	relativePath := pathpkg.Join(append(slices.Clone(components), repositoryName)...)
	destination := filepath.FromSlash(relativePath)
	unlock := s.locks.lock(destination)
	defer unlock()
	if err := preflightCheckoutPath(root, components, destination); err != nil {
		return repoCheckoutResponse{}, err
	}

	// Git only sees a private directory outside the tree searched by Compose.
	// Its subprocesses never receive a path through the mutable serve root.
	temporary, err := os.MkdirTemp("", "compose-checkout-")
	if err != nil {
		return repoCheckoutResponse{}, fmt.Errorf("create temporary checkout directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(temporary); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary checkout: %w", err))
		}
	}()
	cloned := filepath.Join(temporary, "repository")

	operationCtx, cancel := context.WithTimeout(ctx, checkoutTimeout)
	defer cancel()
	if err := s.clone(operationCtx, request.URL, cloned, depth); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := operationCtx.Err(); err != nil {
		return repoCheckoutResponse{}, err
	}

	parent, err := ensureCheckoutParent(root, components)
	if err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := requireDirectoryOnlyEntries(root, parent); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := requireAbsentCheckoutDestination(root, destination); err != nil {
		return repoCheckoutResponse{}, err
	}

	stage, stageInfo, err := createCheckoutStage(root, parent)
	if err != nil {
		return repoCheckoutResponse{}, err
	}
	published := false
	defer func() {
		if !published {
			returnErr = errors.Join(returnErr, removeOwnedCheckoutStage(root, stage, stageInfo))
		}
	}()
	if err := copyCheckoutTree(operationCtx, root, cloned, stage); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := chmodCheckoutStage(root, stage, 0o755); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := requireAbsentCheckoutDestination(root, destination); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := operationCtx.Err(); err != nil {
		return repoCheckoutResponse{}, err
	}
	if err := root.Rename(stage, destination); err != nil {
		return repoCheckoutResponse{}, fmt.Errorf("install repository checkout: %w", err)
	}
	published = true

	return repoCheckoutResponse{
		OK:    true,
		URL:   request.URL,
		Path:  relativePath,
		Depth: depth,
	}, nil
}

func checkoutRepositoryName(rawURL string) (string, error) {
	if rawURL == "" {
		return "", errdefs.InvalidParameter(errors.New("url is required"))
	}
	if strings.TrimSpace(rawURL) != rawURL || strings.ContainsAny(rawURL, "\x00\r\n") {
		return "", errdefs.InvalidParameter(errors.New("url contains invalid characters"))
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", errdefs.InvalidParameter(fmt.Errorf("invalid repository URL: %w", err))
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", errdefs.InvalidParameter(errors.New("url must be a credential-free HTTPS URL"))
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errdefs.InvalidParameter(errors.New("url must not contain a query or fragment"))
	}

	name := pathpkg.Base(strings.TrimSuffix(parsed.Path, "/"))
	name = strings.TrimSuffix(name, ".git")
	if err := validateCheckoutPathComponent(name); err != nil {
		return "", errdefs.InvalidParameter(fmt.Errorf("invalid repository name: %w", err))
	}
	return name, nil
}

func checkoutPathComponents(requestPath string) ([]string, error) {
	if requestPath == "" {
		return nil, nil
	}
	if strings.TrimSpace(requestPath) != requestPath || filepath.IsAbs(requestPath) || filepath.VolumeName(requestPath) != "" || strings.Contains(requestPath, "\\") {
		return nil, errdefs.InvalidParameter(errors.New("path must be relative and use forward slashes"))
	}

	components := strings.Split(requestPath, "/")
	if len(components) > maxCheckoutPathDepth {
		return nil, errdefs.InvalidParameter(fmt.Errorf("path must contain at most %d components", maxCheckoutPathDepth))
	}
	for _, component := range components {
		if err := validateCheckoutPathComponent(component); err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid path: %w", err))
		}
	}
	return components, nil
}

func validateCheckoutPathComponent(component string) error {
	if component == "" || component == "." || component == ".." {
		return fmt.Errorf("invalid component %q", component)
	}
	if component[0] == '.' || component[len(component)-1] == '.' || strings.EqualFold(component, "git~1") || strings.EqualFold(component, ".git") {
		return fmt.Errorf("reserved component %q", component)
	}
	for i := range len(component) {
		c := component[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return fmt.Errorf("invalid component %q: use ASCII letters, digits, dots, hyphens, or underscores", component)
	}
	return nil
}

func ensureCheckoutParent(root *os.Root, components []string) (string, error) {
	current := "."
	if err := requireDirectoryOnlyEntries(root, current); err != nil {
		return "", err
	}
	for _, component := range components {
		next := filepath.Join(current, component)
		info, err := root.Lstat(next)
		if os.IsNotExist(err) {
			if mkdirErr := root.Mkdir(next, 0o755); mkdirErr != nil && !os.IsExist(mkdirErr) {
				return "", fmt.Errorf("create checkout parent %q: %w", component, mkdirErr)
			}
			info, err = root.Lstat(next)
		}
		if err != nil {
			return "", fmt.Errorf("inspect checkout parent %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errdefs.InvalidParameter(fmt.Errorf("checkout path contains symlink %q", component))
		}
		if !info.IsDir() {
			return "", errdefs.Conflict(fmt.Errorf("checkout path component %q is not a directory", component))
		}
		if err := requireDirectoryOnlyEntries(root, next); err != nil {
			return "", err
		}
		current = next
	}
	return current, nil
}

func preflightCheckoutPath(root *os.Root, components []string, destination string) error {
	current := "."
	if err := requireDirectoryOnlyEntries(root, current); err != nil {
		return err
	}
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect checkout parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errdefs.InvalidParameter(fmt.Errorf("checkout path contains symlink %q", component))
		}
		if !info.IsDir() {
			return errdefs.Conflict(fmt.Errorf("checkout path component %q is not a directory", component))
		}
		if err := requireDirectoryOnlyEntries(root, current); err != nil {
			return err
		}
	}
	return requireAbsentCheckoutDestination(root, destination)
}

func requireDirectoryOnlyEntries(root *os.Root, dir string) error {
	opened, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("inspect checkout parent: %w", err)
	}
	defer func() { _ = opened.Close() }()
	entries, err := opened.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("inspect checkout parent: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errdefs.InvalidParameter(fmt.Errorf("checkout parent contains symlink %q", entry.Name()))
		}
		if dir == "." && entry.Name() == "compose.sock" {
			info, infoErr := entry.Info()
			if infoErr == nil && info.Mode()&os.ModeSocket != 0 {
				continue
			}
		}
		return errdefs.Conflict(fmt.Errorf("checkout parent contains non-directory entry %q", entry.Name()))
	}
	return nil
}

func requireAbsentCheckoutDestination(root *os.Root, destination string) error {
	_, err := root.Lstat(destination)
	if err == nil {
		return errdefs.Conflict(fmt.Errorf("checkout destination %q already exists", destination))
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect checkout destination: %w", err)
	}
	return nil
}

func createCheckoutStage(root *os.Root, parent string) (string, os.FileInfo, error) {
	for range 3 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, fmt.Errorf("generate checkout stage name: %w", err)
		}
		stage := filepath.Join(parent, checkoutStagePrefix+hex.EncodeToString(nonce[:]))
		if err := root.Mkdir(stage, 0o700); os.IsExist(err) {
			continue
		} else if err != nil {
			return "", nil, fmt.Errorf("create checkout stage: %w", err)
		}
		info, err := root.Lstat(stage)
		if err != nil {
			return "", nil, fmt.Errorf("inspect checkout stage: %w", err)
		}
		return stage, info, nil
	}
	return "", nil, errors.New("unable to allocate a unique checkout stage")
}

func removeOwnedCheckoutStage(root *os.Root, stage string, original os.FileInfo) error {
	info, err := root.Lstat(stage)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect checkout stage during cleanup: %w", err)
	}
	if !info.IsDir() || !os.SameFile(info, original) {
		return errdefs.Conflict(errors.New("checkout stage changed before cleanup"))
	}
	// Everything inside this private staging directory is disposable. Root
	// confines recursive cleanup to the serve tree even if a symlink appears.
	if err := root.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove checkout stage: %w", err)
	}
	return nil
}

func chmodCheckoutStage(root *os.Root, stage string, mode os.FileMode) error {
	opened, err := root.Open(stage)
	if err != nil {
		return fmt.Errorf("open checkout stage: %w", err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.Chmod(mode); err != nil {
		return fmt.Errorf("set checkout permissions: %w", err)
	}
	return nil
}

func copyCheckoutTree(ctx context.Context, root *os.Root, source, stage string) error {
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return fmt.Errorf("open cloned repository: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()
	directories := []string{}
	err = fs.WalkDir(sourceRoot.FS(), ".", func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if sourcePath == "." {
			return nil
		}
		destination := filepath.Join(stage, filepath.FromSlash(sourcePath))
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := root.Mkdir(destination, 0o700); err != nil {
				return err
			}
			directories = append(directories, destination)
			return nil
		case entry.Type().IsRegular():
			return copyCheckoutFile(root, sourceRoot, sourcePath, destination, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			target, err := sourceRoot.Readlink(sourcePath)
			if err != nil {
				return err
			}
			return root.Symlink(target, destination)
		default:
			return fmt.Errorf("unsupported file type in cloned repository: %s", sourcePath)
		}
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := chmodCheckoutStage(root, directories[i], 0o755); err != nil {
			return err
		}
	}
	return nil
}

func copyCheckoutFile(root, sourceRoot *os.Root, source, destination string, mode os.FileMode) error {
	input, err := sourceRoot.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Chmod(mode)
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func cloneGitRepository(ctx context.Context, repositoryURL, destination string, depth int) error {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git executable is required for repository checkout: %w", err)
	}

	ghPath, _ := exec.LookPath("gh")
	cmd := exec.CommandContext(ctx, gitPath, checkoutGitArguments(repositoryURL, destination, depth, ghPath)...)
	cmd.Env = checkoutGitEnvironment()
	cmd.WaitDelay = 5 * time.Second
	output := &limitedBuffer{limit: maxGitOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := runCheckoutCommand(cmd); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git clone canceled or timed out: %w", ctx.Err())
		}
		if details := strings.TrimSpace(output.String()); details != "" {
			if isGitHubCheckout(repositoryURL) && (strings.Contains(details, "could not read Username") ||
				strings.Contains(details, "could not read Password") || strings.Contains(details, "Authentication failed") || strings.Contains(details, "Repository not found")) {
				details += "\nFor private GitHub repositories, install GitHub CLI and run 'gh auth login --hostname github.com --git-protocol https' as the user running compose-server. " +
					"The account must have access to the repository."
			}
			return fmt.Errorf("git clone failed: %w: %s", err, details)
		}
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

func checkoutGitArguments(repositoryURL, destination string, depth int, ghPath string) []string {
	args := []string{
		"-c", "credential.helper=",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if ghPath != "" && isGitHubCheckout(repositoryURL) {
		// gh looks up credentials by host string, so normalize casing and the
		// explicit default port before Git passes the host to the helper.
		parsed, _ := url.Parse(repositoryURL)
		parsed.Host = "github.com"
		repositoryURL = parsed.String()
		// Git executes credential helpers with a shell. Quote the resolved binary
		// path, never interpolate request data, and scope the helper to GitHub.
		// Command-line configuration is not saved in the cloned repository.
		helper := "!'" + strings.ReplaceAll(filepath.ToSlash(ghPath), "'", "'\\''") + "' auth git-credential"
		args = append(args, "-c", "credential.https://github.com.helper="+helper)
	}
	return append(args, "clone", "--depth="+strconv.Itoa(depth), "--", repositoryURL, destination)
}

func isGitHubCheckout(repositoryURL string) bool {
	parsed, err := url.Parse(repositoryURL)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && strings.EqualFold(parsed.Hostname(), "github.com") &&
		(parsed.Port() == "" || parsed.Port() == "443")
}

func checkoutGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+7)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") || strings.EqualFold(key, "GCM_INTERACTIVE") || strings.EqualFold(key, "SSH_ASKPASS") ||
			strings.EqualFold(key, "GH_DEBUG") || strings.EqualFold(key, "GH_PROMPT_DISABLED") || strings.EqualFold(key, "GH_NO_UPDATE_NOTIFIER") {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=Never",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	)
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:min(remaining, len(data))])
	}
	if len(data) > remaining {
		b.truncated = true
	}
	return len(data), nil
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buffer.String() + "\n[output truncated]"
	}
	return b.buffer.String()
}

type checkoutPathLocks struct {
	mu    sync.Mutex
	paths map[string]*checkoutPathLock
}

type checkoutPathLock struct {
	mu    sync.Mutex
	users int
}

func (l *checkoutPathLocks) lock(path string) func() {
	l.mu.Lock()
	if l.paths == nil {
		l.paths = map[string]*checkoutPathLock{}
	}
	pathLock := l.paths[path]
	if pathLock == nil {
		pathLock = &checkoutPathLock{}
		l.paths[path] = pathLock
	}
	pathLock.users++
	l.mu.Unlock()

	pathLock.mu.Lock()
	return func() {
		pathLock.mu.Unlock()
		l.mu.Lock()
		pathLock.users--
		if pathLock.users == 0 {
			delete(l.paths, path)
		}
		l.mu.Unlock()
	}
}
