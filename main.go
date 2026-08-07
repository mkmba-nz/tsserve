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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/aws"
	awsconfigload "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
	// Register tsnet auth-key resolvers. Without these blank imports the
	// TS_CLIENT_ID/TS_AUDIENCE (WIF) and TS_CLIENT_ID/TS_CLIENT_SECRET (OAuth)
	// modes are silent no-ops and tsnet falls through to interactive login.
	_ "tailscale.com/feature/condregister/identityfederation"
	_ "tailscale.com/feature/condregister/oauthkey"
	"tailscale.com/ipn/store"
	// Register the "arn:" state-store prefix (AWS SSM) so TSSERVE_STATE_STORE
	// can point node identity at an SSM parameter. Without this blank import
	// store.New only understands file paths, "mem:", and "tpmseal:".
	_ "tailscale.com/ipn/store/awsstore"
	"tailscale.com/tsnet"

	"mkmba.nz/tsserve/certsync"
	dockerpkg "mkmba.nz/tsserve/docker"
	"mkmba.nz/tsserve/ecs"
	"mkmba.nz/tsserve/labels"
	"mkmba.nz/tsserve/local"
	"mkmba.nz/tsserve/metrics"
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

	// TSSERVE_STATE_STORE, when set, diverts only the node identity
	// (tailscaled.state: machine/node key and prefs) to an external
	// ipn.StateStore — e.g. "arn:aws:ssm:<region>:<acct>:parameter/<name>" for
	// AWS SSM. Dir is still used as tsnet's var root (cert directory, log
	// config), so it pairs with TSSERVE_STATE_DIR + TSSERVE_CERT_S3_BUCKET:
	// identity in SSM, certs on disk mirrored to S3.
	if storeArg := os.Getenv("TSSERVE_STATE_STORE"); storeArg != "" {
		st, err := store.New(tsnetLogf(logger), storeArg)
		if err != nil {
			return fmt.Errorf("state store %q: %w", storeArg, err)
		}
		srv.Store = st
		logger.Info("using external state store", "store", storeArg)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// When an S3 cert cache is configured, restore any previously provisioned
	// certificates into the state dir before tsnet starts so a fresh (e.g.
	// ephemeral ECS) task reuses them instead of re-running the ACME flow.
	certSync, err := setupCertSync(ctx, stateDir, logger)
	if err != nil {
		return err
	}
	if certSync != nil {
		syncDone := make(chan struct{})
		go func() {
			defer close(syncDone)
			if err := certSync.Run(ctx); err != nil {
				logger.Error("cert sync stopped", "error", err)
			}
		}()
		// Guarantee the final sweep runs and completes (bounded) on every
		// return path, not just the graceful awaitTerminal one: cancel ctx so
		// the syncer flushes, then wait for it so a cert issued just before
		// shutdown reaches S3 before the process exits.
		defer func() {
			cancel()
			select {
			case <-syncDone:
			case <-time.After(35 * time.Second):
				logger.Warn("timed out waiting for final cert sync")
			}
		}()
	}

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

	lc, err := srv.LocalClient()
	if err != nil {
		return fmt.Errorf("tsnet local client: %w", err)
	}

	mc := metrics.New()
	mgr := proxy.NewManager(srv, lc, logger, mc)
	defer mgr.Close()

	discoveryMode := strings.ToLower(envDefault("TSSERVE_DISCOVERY", "docker"))

	traefikPort, err := local.ParseTraefikPort(os.Getenv("TSSERVE_TRAEFIK_PORT"))
	if err != nil {
		return err
	}
	metricsAddr := envDefault("TSSERVE_METRICS_ADDR", "127.0.0.1:9090")

	localSrv := &local.Server{
		Logger:        logger,
		Manager:       mgr,
		LocalClient:   lc,
		Metrics:       mc,
		TraefikPort:   traefikPort,
		MetricsAddr:   metricsAddr,
		DiscoveryMode: discoveryMode,
		TSNet:         srv,
		Started:       time.Now(),
	}
	localErr := make(chan error, 1)
	go func() { localErr <- localSrv.Run(ctx) }()

	fatal := make(chan error, 1)
	registrar := &registrarAdapter{mgr: mgr, fatal: fatal}

	watchErr := make(chan error, 1)
	if err := startWatcher(ctx, discoveryMode, registrar, logger, watchErr); err != nil {
		return err
	}

	logger.Info("tsserve ready")
	return awaitTerminal(ctx, fatal, watchErr, localErr, logger)
}

// awaitTerminal blocks until the process should shut down, returning the error
// (if any) that should propagate from run.
func awaitTerminal(ctx context.Context, fatal, watchErr, localErr <-chan error, logger *slog.Logger) error {
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
	case err := <-localErr:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("local server: %w", err)
	}
}

// setupCertSync builds the S3-backed cert cache and restores its contents into
// <stateDir>/certs. It returns (nil, nil) when TSSERVE_CERT_S3_BUCKET is unset,
// leaving cert storage entirely on the local disk as before.
func setupCertSync(ctx context.Context, stateDir string, logger *slog.Logger) (*certsync.Syncer, error) {
	bucket := os.Getenv("TSSERVE_CERT_S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	prefix := os.Getenv("TSSERVE_CERT_S3_PREFIX")
	region := envDefault("TSSERVE_CERT_S3_REGION", os.Getenv("AWS_REGION"))

	// The cert-cache S3 client loads its own config (like awsConfigForSpec)
	// rather than sharing tsnet's, so it stays scoped to its configured region
	// and does not perturb tsnet's workload-identity credential resolution.
	opts := []func(*awsconfigload.LoadOptions) error{
		awsconfigload.WithRetryMode(awsconfig.RetryModeAdaptive),
		awsconfigload.WithRetryMaxAttempts(5),
	}
	if region != "" {
		opts = append(opts, awsconfigload.WithRegion(region))
	}
	cfg, err := awsconfigload.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cert s3 config: %w", err)
	}

	syncer, err := certsync.New(certsync.Config{
		Dir:    filepath.Join(stateDir, "certs"),
		Bucket: bucket,
		Prefix: prefix,
	}, awss3.NewFromConfig(cfg), logger)
	if err != nil {
		return nil, err
	}
	logger.Info("cert s3 cache enabled", "bucket", bucket, "prefix", prefix, "region", region)
	// Hydrate is best effort: a transient S3 error or missing object must not
	// block startup — tsnet simply re-provisions whatever was not restored.
	if err := syncer.Hydrate(ctx); err != nil {
		logger.Warn("cert s3 hydrate failed; continuing, certs will be re-provisioned as needed", "error", err)
	}
	// Register the watch before returning (i.e. before srv.Start) so no early
	// cert write is missed, and so a watcher-setup failure surfaces here rather
	// than silently disabling the cache in a background goroutine.
	if err := syncer.Watch(); err != nil {
		return nil, fmt.Errorf("cert s3 watch: %w", err)
	}
	return syncer, nil
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
func startWatcher(ctx context.Context, mode string, registrar *registrarAdapter, logger *slog.Logger, watchErr chan<- error) error {
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
		interval, err := parsePollInterval(os.Getenv("TSSERVE_ECS_POLL_INTERVAL"))
		if err != nil {
			return err
		}

		// A single-account configuration (no TSSERVE_ECS_ACCOUNTS) uses the
		// shared TSSERVE_ECS_* / AWS_REGION env vars directly and behaves
		// exactly as before. When accounts ARE configured, each fills any
		// field it leaves blank from the same shared values.
		accounts, err := ecs.ParseAccounts(os.Getenv("TSSERVE_ECS_ACCOUNTS"))
		if err != nil {
			return err
		}
		specs, err := ecs.Plan(accounts, ecs.Defaults{
			Region:       os.Getenv("AWS_REGION"),
			Profile:      os.Getenv("TSSERVE_ECS_AWS_PROFILE"),
			ConfigFile:   os.Getenv("TSSERVE_ECS_AWS_CONFIG_FILE"),
			Cluster:      os.Getenv("TSSERVE_ECS_CLUSTER"),
			PollInterval: interval,
		})
		if err != nil {
			return err
		}

		// Only resolve account IDs (via STS) when accounts are explicitly
		// configured; the legacy single-account path must not require the
		// extra sts:GetCallerIdentity permission.
		watchers, err := buildECSWatchers(ctx, specs, len(accounts) > 0, registrar, logger)
		if err != nil {
			return err
		}
		logger.Info("ecs discovery configured", "accounts", len(watchers))

		// Run every account's watcher concurrently. Each watcher's Run only
		// returns on ctx cancellation, so a fan-in coordinator forwards the
		// first genuine error immediately (else nil once all have exited),
		// keeping the single watchErr channel main selects on.
		results := make(chan error, len(watchers))
		for _, w := range watchers {
			go func(w *ecs.Watcher) { results <- w.Run(ctx) }(w)
		}
		go func() {
			reported := false
			for range watchers {
				if err := <-results; err != nil && !errors.Is(err, context.Canceled) && !reported {
					watchErr <- err
					reported = true
				}
			}
			if !reported {
				watchErr <- nil
			}
		}()
		return nil

	default:
		return fmt.Errorf("unknown TSSERVE_DISCOVERY=%q; expected 'docker' or 'ecs'", mode)
	}
}

// buildECSWatchers turns resolved WatcherSpecs into ready-to-run watchers, one
// per account. When deriveNames is set, an account without an explicit name has
// its AWS account ID resolved via STS GetCallerIdentity and used as its log
// identity; resolved identities must be unique across accounts.
func buildECSWatchers(
	ctx context.Context,
	specs []ecs.WatcherSpec,
	deriveNames bool,
	registrar *registrarAdapter,
	logger *slog.Logger,
) ([]*ecs.Watcher, error) {
	watchers := make([]*ecs.Watcher, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))

	for i, spec := range specs {
		awsCfg, err := awsConfigForSpec(ctx, spec)
		if err != nil {
			return nil, err
		}

		name := spec.Name
		if name == "" && deriveNames {
			ident, err := awssts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &awssts.GetCallerIdentityInput{})
			if err != nil {
				return nil, fmt.Errorf("ecs accounts[%d]: resolve account identity: %w", i, err)
			}
			name = awsconfig.ToString(ident.Account)
			if name == "" {
				return nil, fmt.Errorf("ecs accounts[%d]: STS returned an empty account id", i)
			}
		}
		if name != "" {
			if _, dup := seen[name]; dup {
				return nil, fmt.Errorf("ecs account identity %q resolves more than once; set a unique name", name)
			}
			seen[name] = struct{}{}
			logger.Info("ecs account ready", "account", name, "cluster", spec.Cluster, "region", spec.Region)
		}

		watcher, err := ecs.NewWatcher(
			ecs.Config{Cluster: spec.Cluster, PollInterval: spec.PollInterval, Account: name},
			awsecs.NewFromConfig(awsCfg),
			awsec2.NewFromConfig(awsCfg),
			registrar,
			logger,
		)
		if err != nil {
			return nil, err
		}
		watchers = append(watchers, watcher)
	}
	return watchers, nil
}

// awsConfigForSpec loads the AWS SDK config for one account's credential
// context. It is deliberately scoped to tsserve's own settings (region,
// profile, config file, or static credentials) rather than the process-wide
// AWS_PROFILE / AWS_CONFIG_FILE, so tsnet's WIF flow — which calls
// LoadDefaultConfig itself and must resolve to the EC2 instance role identity
// Tailscale trusts — is never pulled onto a cross-account role assumed for ECS
// reads.
func awsConfigForSpec(ctx context.Context, spec ecs.WatcherSpec) (awsconfig.Config, error) {
	opts := []func(*awsconfigload.LoadOptions) error{
		awsconfigload.WithRetryMode(awsconfig.RetryModeAdaptive),
		awsconfigload.WithRetryMaxAttempts(5),
	}
	if spec.Region != "" {
		opts = append(opts, awsconfigload.WithRegion(spec.Region))
	}
	if spec.Profile != "" {
		opts = append(opts, awsconfigload.WithSharedConfigProfile(spec.Profile))
	}
	if spec.ConfigFile != "" {
		opts = append(opts, awsconfigload.WithSharedConfigFiles([]string{spec.ConfigFile}))
	}
	if spec.AccessKeyID != "" && spec.SecretAccessKey != "" {
		opts = append(opts, awsconfigload.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(spec.AccessKeyID, spec.SecretAccessKey, ""),
		))
	}
	cfg, err := awsconfigload.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return awsconfig.Config{}, fmt.Errorf("aws config: %w", err)
	}
	return cfg, nil
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
