// Command tsserve watches Docker or AWS ECS for labelled containers and
// exposes them as Tailscale Services using the tsnet ListenService API. See
// SPEC.md for full details.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/aws"
	awsconfigload "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"tailscale.com/tsnet"

	dockerpkg "mkmba.nz/tsserve/docker"
	"mkmba.nz/tsserve/ecs"
	"mkmba.nz/tsserve/labels"
	"mkmba.nz/tsserve/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tsserve:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger(os.Getenv("TSSERVE_LOG_LEVEL"))

	if err := validateAuth(); err != nil {
		return err
	}

	hostname := envDefault("TSSERVE_HOSTNAME", "tsserve")
	stateDir := envDefault("TSSERVE_STATE_DIR", "/var/lib/tsserve")

	srv := &tsnet.Server{
		Hostname:      hostname,
		Dir:           stateDir,
		AuthKey:       os.Getenv("TS_AUTHKEY"),
		ClientID:      os.Getenv("TS_CLIENT_ID"),
		ClientSecret:  os.Getenv("TS_CLIENT_SECRET"),
		IDToken:       os.Getenv("TS_ID_TOKEN"),
		Audience:      os.Getenv("TS_AUDIENCE"),
		AdvertiseTags: splitTags(os.Getenv("TSSERVE_TAGS")),
		Logf:          tsnetLogf(logger),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting tsnet", "hostname", hostname, "state_dir", stateDir, "tags", srv.AdvertiseTags)
	if err := srv.Start(); err != nil {
		return fmt.Errorf("tsnet start: %w", err)
	}
	defer srv.Close()

	// Wait until we are actually up on the tailnet. Up returns when the node
	// is authenticated and reachable.
	if _, err := srv.Up(ctx); err != nil {
		return fmt.Errorf("tsnet up: %w", err)
	}

	mgr := proxy.NewManager(srv, logger)
	defer mgr.Close()

	fatal := make(chan error, 1)
	registrar := &registrarAdapter{mgr: mgr, fatal: fatal}

	watchErr := make(chan error, 1)
	if err := startWatcher(ctx, registrar, logger, watchErr); err != nil {
		return err
	}

	logger.Info("tsserve ready")
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		return nil
	case err := <-fatal:
		return err
	case err := <-watchErr:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

type registrarAdapter struct {
	mgr   *proxy.Manager
	fatal chan<- error
}

func (a *registrarAdapter) Register(containerID string, def *labels.ServiceDef, backendIP string) error {
	err := a.mgr.Register(containerID, &proxy.ServiceDef{
		Service: def.Service,
		Port:    def.Port,
		Network: def.Network,
		Scheme:  def.Scheme,
		Caps:    def.Caps,
	}, backendIP)
	if fatalErr, ok := errors.AsType[*proxy.FatalError](err); ok {
		select {
		case a.fatal <- fatalErr:
		default:
		}
	}
	return err
}

func (a *registrarAdapter) Deregister(containerID string) { a.mgr.Deregister(containerID) }

// startWatcher selects and starts the configured discovery backend, sending
// its terminal error (or nil on clean shutdown) on watchErr.
func startWatcher(ctx context.Context, registrar *registrarAdapter, logger *slog.Logger, watchErr chan<- error) error {
	mode := strings.ToLower(envDefault("TSSERVE_DISCOVERY", "docker"))
	switch mode {
	case "docker":
		watcher, err := dockerpkg.NewWatcher(registrar, logger)
		if err != nil {
			return err
		}
		go func() {
			defer watcher.Close()
			watchErr <- watcher.Run(ctx)
		}()
		return nil

	case "ecs":
		cluster := os.Getenv("TSSERVE_ECS_CLUSTER")
		if cluster == "" {
			return errors.New("TSSERVE_ECS_CLUSTER is required when TSSERVE_DISCOVERY=ecs")
		}
		interval, err := parsePollInterval(os.Getenv("TSSERVE_ECS_POLL_INTERVAL"))
		if err != nil {
			return err
		}

		awsCfg, err := awsconfigload.LoadDefaultConfig(ctx,
			awsconfigload.WithRetryMode(awsconfig.RetryModeAdaptive),
			awsconfigload.WithRetryMaxAttempts(5),
		)
		if err != nil {
			return fmt.Errorf("aws config: %w", err)
		}

		watcher, err := ecs.NewWatcher(
			ecs.Config{Cluster: cluster, PollInterval: interval},
			awsecs.NewFromConfig(awsCfg),
			awsec2.NewFromConfig(awsCfg),
			registrar,
			logger,
		)
		if err != nil {
			return err
		}
		go func() { watchErr <- watcher.Run(ctx) }()
		return nil

	default:
		return fmt.Errorf("unknown TSSERVE_DISCOVERY=%q; expected 'docker' or 'ecs'", mode)
	}
}

func parsePollInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil // 0 lets ecs.NewWatcher pick the default
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("TSSERVE_ECS_POLL_INTERVAL=%q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("TSSERVE_ECS_POLL_INTERVAL must be positive, got %s", s)
	}
	return d, nil
}

func validateAuth() error {
	authKey := os.Getenv("TS_AUTHKEY")
	clientID := os.Getenv("TS_CLIENT_ID")
	clientSecret := os.Getenv("TS_CLIENT_SECRET")
	idToken := os.Getenv("TS_ID_TOKEN")
	audience := os.Getenv("TS_AUDIENCE")
	tags := os.Getenv("TSSERVE_TAGS")

	switch {
	case clientID != "" && (idToken != "" || audience != ""):
		if idToken != "" && audience != "" {
			return errors.New("TS_ID_TOKEN and TS_AUDIENCE are mutually exclusive")
		}
		if tags == "" {
			return errors.New("TSSERVE_TAGS is required when using workload identity federation")
		}
		return nil
	case clientID != "" && clientSecret != "":
		if tags == "" {
			return errors.New("TSSERVE_TAGS is required when using OAuth client auth")
		}
		return nil
	case authKey != "":
		return nil
	default:
		return errors.New(
			"no authentication configured: set TS_AUTHKEY, " +
				"or TS_CLIENT_ID with TS_CLIENT_SECRET (OAuth), " +
				"or TS_CLIENT_ID with TS_AUDIENCE or TS_ID_TOKEN (workload identity federation); " +
				"TSSERVE_TAGS is required for OAuth and workload-identity modes")
	}
}

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// tsnetLogf routes tsnet's verbose logf output through slog at debug level.
func tsnetLogf(logger *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) {
		logger.Debug(strings.TrimRight(fmt.Sprintf(format, args...), "\n"), "source", "tsnet")
	}
}
