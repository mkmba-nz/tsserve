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
	"sync"
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

	// readers tracks per-reader ECS discovery status for the status page. It
	// stays empty in Docker mode.
	readers := ecs.NewRegistry()

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
		Readers:       readerSource{readers},
	}
	localErr := make(chan error, 1)
	go func() { localErr <- localSrv.Run(ctx) }()

	fatal := make(chan error, 1)
	registrar := &registrarAdapter{mgr: mgr, fatal: fatal}

	watchErr := make(chan error, 1)
	if err := startWatcher(ctx, discoveryMode, registrar, readers, logger, watchErr); err != nil {
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

// readerSource adapts the ecs reader Registry to the local package's
// ReaderSnapshotter, converting between the two mirrored status types so local
// need not import ecs.
type readerSource struct{ reg *ecs.Registry }

func (s readerSource) Readers() []local.ReaderStatus {
	src := s.reg.Snapshot()
	out := make([]local.ReaderStatus, len(src))
	for i, r := range src {
		out[i] = local.ReaderStatus{
			Name:          r.Name,
			Account:       r.Account,
			Cluster:       r.Cluster,
			Region:        r.Region,
			PollInterval:  r.PollInterval,
			LastPollStart: r.LastPollStart,
			LastPollOK:    r.LastPollOK,
			LastError:     r.LastError,
			Polls:         r.Polls,
			Healthy:       r.Healthy,
		}
	}
	return out
}

type registrarAdapter struct {
	mgr   *proxy.Manager
	fatal chan<- error
	// origin tags every service this adapter registers with its discovery
	// source. It is zero for Docker discovery and set per-reader for ECS.
	origin proxy.Origin
}

func (a *registrarAdapter) Register(containerID string, def *labels.ServiceDef, backendIP string) error {
	err := a.mgr.Register(containerID, &proxy.ServiceDef{
		Service: def.Service,
		Port:    def.Port,
		Network: def.Network,
		Scheme:  def.Scheme,
		Caps:    def.Caps,
		Origin:  a.origin,
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
func startWatcher(ctx context.Context, mode string, registrar *registrarAdapter, readers *ecs.Registry, logger *slog.Logger, watchErr chan<- error) error {
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
		retryInterval, err := parseRetryInterval(os.Getenv("TSSERVE_ECS_RETRY_INTERVAL"))
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
		watchers, err := buildECSWatchers(ctx, specs, len(accounts) > 0, retryInterval, registrar, readers, logger)
		if err != nil {
			return err
		}
		if len(watchers) == 0 {
			// Every configured reader hit a hard local misconfiguration (e.g. an
			// AWS config file that will not load). Unassumable-role readers are
			// NOT counted here — they keep retrying. Don't crash-loop: the
			// readers table surfaces why, and a config fix + restart recovers.
			//
			// Deliberately do NOT send on watchErr: a nil there would be received
			// by awaitTerminal and exit the process. With no watcher running, the
			// daemon stays up on ctx.Done()/localErr, serving the status page so
			// the failed readers remain visible until shutdown.
			logger.Error("ecs discovery: no readers could be initialised; " +
				"the daemon will run but discover nothing until the configuration is fixed")
			return nil
		}
		logger.Info("ecs discovery configured", "readers", len(watchers), "configured", len(specs))

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
// per account, registering each reader's status in readers. When deriveNames is
// set, an account without an explicit name has its AWS account ID resolved via
// STS GetCallerIdentity and used as its log identity.
//
// Resilience is the whole point here: a reader whose role is not assumable yet
// (STS/ECS returns 403) must never abort the daemon or be dropped. Its account
// identity is resolved lazily by the watcher, which keeps retrying at the slow
// retryInterval until the role becomes assumable — recovering with no restart.
// Such a reader stays in the readers registry marked unhealthy so it surfaces,
// highlighted, on the status page. Only a hard local misconfiguration (AWS
// config that will not load, or a duplicate identity) disables a reader
// outright; the daemon still starts with whatever readers remain.
func buildECSWatchers(
	ctx context.Context,
	specs []ecs.WatcherSpec,
	deriveNames bool,
	retryInterval time.Duration,
	base *registrarAdapter,
	readers *ecs.Registry,
	logger *slog.Logger,
) ([]*ecs.Watcher, error) {
	watchers := make([]*ecs.Watcher, 0, len(specs))

	// identities dedupes resolved account IDs across readers (including those
	// resolved lazily, on watcher goroutines) so two readers pointing at the
	// same account do not silently double-register.
	var idMu sync.Mutex
	identities := make(map[string]struct{}, len(specs))
	claim := func(id string) bool {
		idMu.Lock()
		defer idMu.Unlock()
		if _, dup := identities[id]; dup {
			return false
		}
		identities[id] = struct{}{}
		return true
	}

	for i, spec := range specs {
		// A stable display identity for the readers table, even before (or
		// without) an STS-resolved account id.
		display := spec.Name
		if display == "" {
			if deriveNames {
				display = fmt.Sprintf("accounts[%d]", i)
			} else {
				display = "default"
			}
		}
		reader := readers.Add(ecs.ReaderStatus{
			Name:         display,
			Cluster:      spec.Cluster,
			Region:       spec.Region,
			PollInterval: pollIntervalOrDefault(spec.PollInterval),
		})

		awsCfg, err := awsConfigForSpec(ctx, spec)
		if err != nil {
			reader.NoteError(err)
			logger.Error("ecs reader disabled: aws config failed",
				"reader", display, "cluster", spec.Cluster, "err", err)
			continue
		}

		// Each reader gets its own registrar so the services it advertises are
		// tagged with its origin, sharing the manager and fatal channel. origin
		// is only ever read/written on this reader's single watcher goroutine
		// (Register and OnAccountResolved both run there), so no lock is needed.
		reg := *base
		reg.origin = proxy.Origin{Account: display, Cluster: spec.Cluster, Region: spec.Region}

		cfg := ecs.Config{
			Cluster:       spec.Cluster,
			PollInterval:  spec.PollInterval,
			RetryInterval: retryInterval,
			Account:       display,
			Reader:        reader,
		}

		// A derived reader (no explicit name) needs its account ID from STS. Try
		// once up front for a clean startup log and immediate table data; on
		// failure, hand the watcher a resolver so it keeps retrying rather than
		// giving up. An explicit name skips STS entirely.
		if spec.Name == "" && deriveNames {
			stsClient := awssts.NewFromConfig(awsCfg)
			resolve := func(ctx context.Context) (string, error) {
				out, err := stsClient.GetCallerIdentity(ctx, &awssts.GetCallerIdentityInput{})
				if err != nil {
					return "", err
				}
				account := awsconfig.ToString(out.Account)
				if account == "" {
					return "", errors.New("STS returned an empty account id")
				}
				return account, nil
			}
			onResolved := func(account string) {
				if !claim(account) {
					logger.Warn("ecs reader identity is a duplicate; its services may be ignored",
						"reader", display, "identity", account)
				}
				reg.origin.Account = account
				// cfg.Account only takes effect in the eager path below, where
				// this runs before NewWatcher copies cfg. On lazy resolution the
				// watcher updates its own copy's label (see watcher.go), so this
				// write is a harmless no-op there.
				cfg.Account = account
				reader.SetAccount(account)
			}

			if account, err := resolve(ctx); err != nil {
				reader.NoteError(fmt.Errorf("resolve account identity: %w", err))
				logger.Warn("ecs reader identity unresolved (role not assumable yet?); will keep retrying",
					"reader", display, "cluster", spec.Cluster, "retry_interval", retryInterval, "err", err)
				cfg.ResolveAccount = resolve
				cfg.OnAccountResolved = onResolved
			} else {
				onResolved(account)
			}
		} else if spec.Name != "" {
			// Explicit names are unique by construction (ecs.Plan enforces it),
			// so recording it just keeps a lazily-resolved account from colliding.
			claim(spec.Name)
		}

		logger.Info("ecs reader ready", "reader", display, "cluster", spec.Cluster, "region", spec.Region)

		watcher, err := ecs.NewWatcher(
			cfg,
			awsecs.NewFromConfig(awsCfg),
			awsec2.NewFromConfig(awsCfg),
			&reg,
			logger,
		)
		if err != nil {
			reader.NoteError(err)
			logger.Error("ecs reader disabled: watcher construction failed", "reader", display, "err", err)
			continue
		}
		watchers = append(watchers, watcher)
	}
	return watchers, nil
}

// pollIntervalOrDefault mirrors the default ecs.NewWatcher applies to a
// non-positive interval, so the readers table shows the interval actually used.
func pollIntervalOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
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

// parseRetryInterval reads the slow cadence used to retry unhealthy readers. An
// empty value yields 0, letting ecs.NewWatcher apply its 2-minute default.
func parseRetryInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("TSSERVE_ECS_RETRY_INTERVAL=%q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("TSSERVE_ECS_RETRY_INTERVAL must be positive, got %s", s)
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
