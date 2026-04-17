package serve

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

var defaultExcludedDirs = []string{".gocache", "node_modules", ".jj", ".git"}

type discoveryOptions struct {
	rootDir     string
	maxDepth    int
	excludedDir []string
}

type projectLoaderFn func(context.Context, string) (composeapi.Stack, error)

func discoverComposeProjects(ctx context.Context, options discoveryOptions, load projectLoaderFn) ([]composeapi.Stack, error) {
	dirs, err := findComposeDirectories(options)
	if err != nil {
		return nil, err
	}

	projects := make([]composeapi.Stack, 0, len(dirs))
	for _, dir := range dirs {
		project, err := load(ctx, dir)
		if err != nil {
			continue
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func findComposeDirectories(options discoveryOptions) ([]string, error) {
	root := options.rootDir
	if root == "" {
		root = "."
	}
	maxDepth := options.maxDepth
	if maxDepth < 0 {
		maxDepth = 0
	}
	excluded := slices.Clone(options.excludedDir)
	if len(excluded) == 0 {
		excluded = slices.Clone(defaultExcludedDirs)
	}

	found := map[string]struct{}{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		depth := 0
		if rel != "." {
			depth = strings.Count(filepath.ToSlash(rel), "/") + 1
		}

		if rel != "." && slices.Contains(excluded, d.Name()) {
			return filepath.SkipDir
		}
		if depth > maxDepth {
			return filepath.SkipDir
		}
		if hasDefaultComposeFile(path) {
			found[path] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	dirs := make([]string, 0, len(found))
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func hasDefaultComposeFile(dir string) bool {
	for _, name := range composecli.DefaultFileNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	for _, name := range composecli.DefaultOverrideFileNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
