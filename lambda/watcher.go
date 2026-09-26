// Package lambda implements the Lambda discovery backend. It polls the AWS
// Resource Groups Tagging API for functions tagged tsserve.enable=true, reads
// the tsserve.* function tags from the same response, and registers each
// function as a function backend of its service.
package lambda

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	taggingtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"mkmba.nz/tsserve/ecs"
	"mkmba.nz/tsserve/labels"
)

// Qualifier is the optional function tag naming the version number or alias
// every request to the function invokes. Unset, requests invoke $LATEST.
const Qualifier = labels.Prefix + "qualifier"

// DefaultPollInterval is the poll cadence of a healthy Watcher when none is
// configured.
const DefaultPollInterval = 30 * time.Second

// TaggingAPI is the subset of the Resource Groups Tagging API client a Watcher
// uses. *resourcegroupstaggingapi.Client satisfies it; tests supply a fake.
type TaggingAPI interface {
	GetResources(ctx context.Context, in *tagging.GetResourcesInput, opts ...func(*tagging.Options)) (*tagging.GetResourcesOutput, error)
}

// Function is one discovered function backend, as its tags describe it.
type Function struct {
	Service string
	Caps    []string
	// ARN is the function's unqualified ARN, which is also its registration key.
	ARN string
	// Qualifier is the version or alias to invoke; empty means $LATEST.
	Qualifier string
}

// Registrar is what a Watcher drives. RegisterFunction has the contract of the
// manager's: idempotent over key, and nil only once the function is in the
// pool of a serving service.
type Registrar interface {
	RegisterFunction(key string, fn *Function) error
	Deregister(key string)
}

// Config configures a Watcher. Every field has a usable zero value.
type Config struct {
	// PollInterval is the cadence while healthy. Defaults to
	// DefaultPollInterval.
	PollInterval time.Duration
	// RetryInterval is the slower cadence while unhealthy. It never polls
	// faster than PollInterval. Defaults to 2 minutes.
	RetryInterval time.Duration
	// Account is the reader's identity label for log lines.
	Account string
	// Region is the region the reader discovers functions in, for log lines.
	Region string
	// Reader, when set, receives per-cycle poll outcomes for the status page.
	Reader *ecs.Reader
	// ResolveAccount, when set, resolves the reader's AWS account ID. It is
	// retried at the start of each cycle until it succeeds, so a reader whose
	// role becomes assumable later recovers without a restart.
	ResolveAccount func(context.Context) (string, error)
	// OnAccountResolved, when set, is invoked once, on the watcher's goroutine
	// and before any RegisterFunction call, when ResolveAccount first succeeds.
	OnAccountResolved func(account string)
}

// registration is what a Watcher last registered for an active key. Function
// tags can change on a live function, so every field of it is compared, not
// just the target. Caps are a set, compared in sorted order, as the manager
// compares them.
type registration struct {
	service   string
	caps      string
	qualifier string
}

func registrationOf(fn *Function) registration {
	return registration{
		service:   fn.Service,
		caps:      strings.Join(slices.Sorted(slices.Values(fn.Caps)), ","),
		qualifier: fn.Qualifier,
	}
}

// Watcher polls the Tagging API for tagged functions and drives a Registrar.
type Watcher struct {
	cfg    Config
	api    TaggingAPI
	reg    Registrar
	logger *slog.Logger
	active map[string]registration // key -> what the last successful RegisterFunction registered
	// failed maps a key to its last failure (invalid tags, or a failed
	// RegisterFunction), so a failure that persists unchanged is logged at
	// Warn once. Entries are dropped when the key succeeds or is not seen.
	failed   map[string]string
	clockNow func() time.Time
	sleepFor func(context.Context, time.Duration) error

	healthy         bool
	accountResolved bool
}

// NewWatcher returns a Watcher ready to Run.
func NewWatcher(cfg Config, api TaggingAPI, reg Registrar, logger *slog.Logger) *Watcher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 2 * time.Minute
	}
	return &Watcher{
		cfg:      cfg,
		api:      api,
		reg:      reg,
		logger:   logger,
		active:   map[string]registration{},
		failed:   map[string]string{},
		clockNow: time.Now,
		sleepFor: ctxSleep,
	}
}

// Run blocks running poll-and-diff cycles until ctx is cancelled. A failed
// cycle is logged and the next one reconciles.
func (w *Watcher) Run(ctx context.Context) error {
	w.logger.Info("lambda watcher starting",
		"account", w.cfg.Account, "region", w.cfg.Region, "poll_interval", w.cfg.PollInterval)

	if err := w.reportCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("lambda initial poll failed", "account", w.cfg.Account, "err", err)
	}
	for {
		if err := w.sleepFor(ctx, w.nextInterval()); err != nil {
			return ctx.Err()
		}
		if err := w.reportCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("lambda poll cycle failed",
				"account", w.cfg.Account, "retry_in", w.nextInterval(), "err", err)
		}
	}
}

// nextInterval is PollInterval while healthy and the slower RetryInterval
// (never faster than PollInterval) while not.
func (w *Watcher) nextInterval() time.Duration {
	if w.healthy {
		return w.cfg.PollInterval
	}
	return max(w.cfg.PollInterval, w.cfg.RetryInterval)
}

// reportCycle runs one cycle and records its outcome. A cancellation is a
// shutdown, not a poll failure.
func (w *Watcher) reportCycle(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.cfg.Reader.PollStarted(w.clockNow())
	err := w.cycle(ctx)
	switch {
	case err == nil:
		w.healthy = true
		w.cfg.Reader.PollSucceeded(w.clockNow())
	case errors.Is(err, context.Canceled):
	default:
		w.healthy = false
		w.cfg.Reader.PollFailed(w.clockNow(), err)
	}
	return err
}

// cycle runs one poll-and-diff iteration: every page of tagged functions is
// read before anything missing from them is deregistered.
func (w *Watcher) cycle(ctx context.Context) error {
	if !w.accountResolved && w.cfg.ResolveAccount != nil {
		account, err := w.cfg.ResolveAccount(ctx)
		if err != nil {
			return fmt.Errorf("resolve account identity: %w", err)
		}
		w.accountResolved = true
		if w.cfg.OnAccountResolved != nil {
			w.cfg.OnAccountResolved(account)
		}
		w.logger.Info("lambda reader identity resolved",
			"was", w.cfg.Account, "account", account, "region", w.cfg.Region)
		w.cfg.Account = account
	}

	mappings, err := w.listTagged(ctx)
	if err != nil {
		return err
	}

	seen := map[string]struct{}{}
	for _, m := range mappings {
		arn := aws.ToString(m.ResourceARN)
		if arn == "" {
			continue
		}
		// A function with invalid tags is still seen: if it was serving under
		// a previous valid tag set, it stays registered.
		seen[arn] = struct{}{}
		fn, err := parseFunction(arn, m.Tags)
		if err != nil {
			w.noteFailure(arn, "invalid function tags; skipping", err)
			continue
		}
		w.apply(fn)
	}

	w.removeMissing(seen)
	return nil
}

// apply registers fn unless it is already registered exactly as tagged. A
// change to its service, caps or qualifier is handled as the backend moving:
// the old registration is dropped before the new one is attempted.
func (w *Watcher) apply(fn *Function) {
	key := fn.ARN
	want := registrationOf(fn)
	if prev, registered := w.active[key]; registered {
		if prev == want {
			// Already registered as tagged. Drop any failure recorded while
			// its tags were invalid, so a later failure warns again.
			delete(w.failed, key)
			return
		}
		w.reg.Deregister(key)
		delete(w.active, key)
	}
	if err := w.reg.RegisterFunction(key, fn); err != nil {
		w.noteFailure(key, "registration failed; will retry next cycle", err)
		return
	}
	delete(w.failed, key)
	w.active[key] = want
}

// noteFailure logs a failure for key at Warn the first time, and at Debug while
// it recurs unchanged, so a persistent problem does not log every cycle.
func (w *Watcher) noteFailure(key, msg string, err error) {
	text := msg + ": " + err.Error()
	if prev, failing := w.failed[key]; failing && prev == text {
		w.logger.Debug(msg, "function", key, "err", err)
	} else {
		w.logger.Warn(msg, "function", key, "err", err)
	}
	w.failed[key] = text
}

// removeMissing deregisters every active key not seen this cycle and forgets
// the failures of keys no longer seen.
func (w *Watcher) removeMissing(seen map[string]struct{}) {
	for key := range w.active {
		if _, ok := seen[key]; !ok {
			w.reg.Deregister(key)
			delete(w.active, key)
		}
	}
	for key := range w.failed {
		if _, ok := seen[key]; !ok {
			delete(w.failed, key)
		}
	}
}

// listTagged pages through GetResources for every function tagged
// tsserve.enable=true. The last page carries an empty, not absent, token.
func (w *Watcher) listTagged(ctx context.Context) ([]taggingtypes.ResourceTagMapping, error) {
	var all []taggingtypes.ResourceTagMapping
	var token *string
	for {
		out, err := w.api.GetResources(ctx, &tagging.GetResourcesInput{
			ResourceTypeFilters: []string{"lambda:function"},
			TagFilters: []taggingtypes.TagFilter{{
				Key:    aws.String(labels.Enable),
				Values: []string{"true"},
			}},
			ResourcesPerPage: aws.Int32(100),
			PaginationToken:  token,
		})
		if err != nil {
			return nil, fmt.Errorf("GetResources: %w", err)
		}
		all = append(all, out.ResourceTagMappingList...)
		if aws.ToString(out.PaginationToken) == "" {
			return all, nil
		}
		token = out.PaginationToken
	}
}

// qualifierPattern is the form Lambda accepts for a qualifier: $LATEST, a
// version number, or an alias name.
var qualifierPattern = regexp.MustCompile(`^(\$LATEST|[A-Za-z0-9_-]{1,128})$`)

// parseFunction validates a function's tags. tsserve.enable is guaranteed by
// the server-side filter; tsserve.port, tsserve.network and tsserve.scheme are
// ignored.
func parseFunction(arn string, tags []taggingtypes.Tag) (*Function, error) {
	values := make(map[string]string, len(tags))
	for _, t := range tags {
		values[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	svc := values[labels.Service]
	if err := labels.ValidateService(svc); err != nil {
		return nil, err
	}
	q := values[Qualifier]
	if q != "" && !qualifierPattern.MatchString(q) {
		return nil, fmt.Errorf("tag %s=%q is not a version number or alias name", Qualifier, q)
	}
	return &Function{
		Service:   svc,
		Caps:      labels.ParseCaps(values[labels.Caps]),
		ARN:       arn,
		Qualifier: q,
	}, nil
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
