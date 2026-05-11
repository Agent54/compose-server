package serve

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/docker/cli/cli/command"
	dopts "github.com/docker/cli/opts"
	"github.com/docker/docker/api"
	engineserver "github.com/docker/docker/api/server"
	"github.com/docker/docker/api/server/middleware"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/docker/compose/v5/internal"
	composeapi "github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
)

const defaultMaxDepth = 4

type serveConfig struct {
	rootDir        string
	maxDepth       int
	excludedDir    []string
	allowedOrigins []string
}

// NewCommand creates the compose daemon command.
func NewCommand(dockerCli command.Cli, backendOpts []composepkg.Option) *cobra.Command {
	var (
		port           int
		maxDepth       int
		exclusions     []string
		allowedOrigins []string
	)

	cmd := &cobra.Command{
		Use:   "serve [DIR]",
		Short: "Serve a Compose HTTP API over a unix socket or TCP port",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg := serveConfig{
				rootDir:        ".",
				maxDepth:       maxDepth,
				excludedDir:    slices.Clone(exclusions),
				allowedOrigins: slices.Clone(allowedOrigins),
			}
			if len(args) > 0 {
				cfg.rootDir = args[0]
			}

			target, err := serveTargetForConfig(cfg, port)
			if err != nil {
				return err
			}

			srv, err := newHTTPServer(target, cfg, func(extraOpts ...composepkg.Option) (composeapi.Compose, error) {
				opts := append(slices.Clone(backendOpts), composepkg.WithEventProcessor(noopEventProcessor{}))
				opts = append(opts, extraOpts...)
				return composepkg.NewComposeService(dockerCli, opts...)
			}, func() (statsRuntime, error) {
				return statsRuntime{
					client: dockerCli.Client(),
					osType: dockerCli.ServerInfo().OSType,
				}, nil
			}, func(format string, args ...any) {
				_, _ = fmt.Fprintf(dockerCli.Out(), format, args...)
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(dockerCli.Out(), "Compose API listening on %s\n", target.display())
			_, _ = fmt.Fprintf(dockerCli.Out(), "Serve root: %s\n", cfg.rootDir)
			_, _ = fmt.Fprintf(dockerCli.Out(), "Max depth: %d\n", cfg.maxDepth)
			_, _ = fmt.Fprintf(dockerCli.Out(), "Excluded dirs: %s\n", strings.Join(cfg.excludedDir, ", "))
			_, _ = fmt.Fprintf(dockerCli.Out(), "Allowed origins: %s\n", formatAllowedOrigins(cfg.allowedOrigins))
			return srv.run(ctx)
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 0, "Listen on the given TCP port instead of a unix socket")
	cmd.Flags().IntVar(&maxDepth, "max-depth", defaultMaxDepth, "Maximum directory depth to crawl for Compose files")
	cmd.Flags().StringArrayVar(&exclusions, "exclude", slices.Clone(defaultExcludedDirs), "Directory names to exclude while crawling")
	cmd.Flags().StringArrayVar(&allowedOrigins, "allowed-origins", nil, "Allowed CORS origins; repeat the flag to allow multiple origins")
	return cmd
}

func newHTTPServer(target serveTarget, cfg serveConfig, backend backendFactory, stats statsRuntimeFactory, statusf func(string, ...any)) (*httpServer, error) {
	versionMiddleware, err := middleware.NewVersionMiddleware(internal.Version, api.DefaultVersion, api.MinSupportedAPIVersion)
	if err != nil {
		return nil, err
	}

	server := &engineserver.Server{}
	server.UseMiddleware(*versionMiddleware)

	app := newServerApp(cfg, backend, stats)
	if statusf != nil {
		app.setWatchMirror(func(msg sseMessage) {
			statusf("%s\n", formatWatchMirrorMessage(msg))
		})
	}
	mux := server.CreateMux(context.Background(), newRouter(app))
	return &httpServer{
		target: target,
		server: &http.Server{Handler: wrapCORS(mux, cfg.allowedOrigins)},
		app:    app,
		statusf: statusf,
	}, nil
}

func formatWatchMirrorMessage(msg sseMessage) string {
	if msg.Source != "" && msg.Source != composeapi.WatchLogger && msg.Source != composeapi.ResourceCompose {
		return fmt.Sprintf("watch[%s][%s][%s] %s", msg.Project, msg.Stream, msg.Source, msg.Message)
	}
	return fmt.Sprintf("watch[%s][%s] %s", msg.Project, msg.Stream, msg.Message)
}

func formatAllowedOrigins(origins []string) string {
	if len(origins) == 0 {
		return "(disabled)"
	}
	return strings.Join(origins, ", ")
}

type serveTarget struct {
	network string
	address string
}

func (t serveTarget) display() string {
	if t.network == "unix" {
		return "unix://" + t.address
	}
	return "http://" + t.address
}

func serveTargetForConfig(cfg serveConfig, port int) (serveTarget, error) {
	if port < 0 || port > 65535 {
		return serveTarget{}, fmt.Errorf("invalid port %d", port)
	}
	if port != 0 {
		return serveTarget{network: "tcp", address: fmt.Sprintf("127.0.0.1:%d", port)}, nil
	}

	socketPath, err := socketPathForDir(cfg.rootDir)
	if err != nil {
		return serveTarget{}, err
	}
	return serveTarget{network: "unix", address: socketPath}, nil
}

func socketPathForDir(dir string) (string, error) {
	if dir != "" {
		return filepath.Join(dir, "compose.sock"), nil
	}

	host, err := dopts.ParseHost(false, os.Getenv(client.EnvOverrideHost))
	if err != nil {
		return "", err
	}
	if host == "" {
		host = client.DefaultDockerHost
	}
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		return filepath.Join(filepath.Dir(path), "compose.sock"), nil
	}
	return filepath.Join(filepath.Dir("/var/run/docker.sock"), "compose.sock"), nil
}

func ensureSocketDir(socketPath string) error {
	return os.MkdirAll(filepath.Dir(socketPath), 0o755)
}

func mergeStacks(existing []composeapi.Stack, discovered []composeapi.Stack) []composeapi.Stack {
	seen := map[string]struct{}{}
	merged := make([]composeapi.Stack, 0, len(existing)+len(discovered))
	for _, stack := range existing {
		merged = append(merged, stack)
		seen[stack.Name] = struct{}{}
	}
	for _, stack := range discovered {
		if _, ok := seen[stack.Name]; ok {
			continue
		}
		merged = append(merged, stack)
		seen[stack.Name] = struct{}{}
	}
	slices.SortFunc(merged, func(a, b composeapi.Stack) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return merged
}

func newDiscoveryOptions(cfg serveConfig) discoveryOptions {
	return discoveryOptions{
		rootDir:     cfg.rootDir,
		maxDepth:    cfg.maxDepth,
		excludedDir: slices.Clone(cfg.excludedDir),
	}
}
