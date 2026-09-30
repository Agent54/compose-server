package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type bindPathMapping struct {
	hostRoot  string
	guestRoot string
}

// WithBindPathMapping translates host-side stack files into paths visible to a
// Docker daemon running in a VM. Project loading and builds still use host paths.
func WithBindPathMapping(hostRoot, guestRoot string) Option {
	return func(service *composeService) error {
		if !filepath.IsAbs(hostRoot) || !filepath.IsAbs(guestRoot) {
			return fmt.Errorf("stack bind mapping requires absolute host and guest roots")
		}
		resolved, err := filepath.EvalSymlinks(hostRoot)
		if err != nil {
			return fmt.Errorf("resolve stacks folder %q: %w", hostRoot, err)
		}
		service.bindPathMapping = &bindPathMapping{hostRoot: resolved, guestRoot: filepath.Clean(guestRoot)}
		return nil
	}
}

func (m *bindPathMapping) guestSource(source string) (string, error) {
	if m == nil {
		return source, nil
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("bind source %q is not an absolute host path", source)
	}
	source = filepath.Clean(source)
	// Docker control/storage and timezone files are supplied by the Linux guest.
	if source == "/etc/localtime" || source == "/etc/timezone" || source == "/var/run/docker.sock" || source == "/run/docker.sock" ||
		source == "/var/lib/docker" || strings.HasPrefix(source, "/var/lib/docker/") {
		return source, nil
	}
	resolved, err := resolveBindSource(source)
	if err != nil {
		return "", fmt.Errorf("resolve bind source %q: %w", source, err)
	}
	relative, err := filepath.Rel(m.hostRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bind source %q is outside the stacks folder %q; move it under the stacks folder before starting this service", source, m.hostRoot)
	}
	return filepath.Join(m.guestRoot, relative), nil
}

// Resolve the existing prefix so a not-yet-created bind path cannot escape
// through a symlink in one of its parent directories.
func resolveBindSource(source string) (string, error) {
	path := filepath.Clean(source)
	var missing []string
	for {
		_, err := os.Lstat(path)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return "", resolveErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}
