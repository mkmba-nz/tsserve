// Command tsserve watches Docker for labelled containers and exposes them as
// Tailscale Services using the tsnet ListenService API. See SPEC.md for full
// details.
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

	"tailscale.com/tsnet"

	dockerpkg "mkmba.nz/tsserve/docker"
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

	watcher, err := dockerpkg.NewWatcher(registrar, logger)
	if err != nil {
		return err
	}
	defer watcher.Close()

	watchErr := make(chan error, 1)
	go func() { watchErr <- watcher.Run(ctx) }()

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

func (a *registrarAdapter) Register(containerID string, def *dockerpkg.ServiceDef, backendIP string) error {
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
