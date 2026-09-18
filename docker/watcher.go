// Package docker contains the Docker integration: event subscription and
// container IP resolution. Label parsing lives in the shared labels/ package.
package docker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"mkmba.nz/tsserve/labels"
)

// Registrar is implemented by the proxy package. The watcher calls Register
// when a labelled container becomes ready and Deregister when one goes away.
type Registrar interface {
	Register(containerID string, def *labels.ServiceDef, backendIP string) error
	Deregister(containerID string)
}

// dockerAPI is the subset of *client.Client the watcher uses. Tests substitute
// a fake; production wires in the real client.
type dockerAPI interface {
	Ping(ctx context.Context) (types.Ping, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	Close() error
}

// defaultSweepInterval is how often the watcher re-lists containers and
// reconciles them against what is registered. It is the cadence at which a
// running, labelled container whose registration failed rejoins its service, so
// it is short enough to be unremarkable to a user and long enough that listing
// containers is not a load on the daemon.
const defaultSweepInterval = 30 * time.Second

// Watcher subscribes to Docker events and drives the [Registrar] in response
// to container lifecycle changes. Between events it re-lists containers on a
// fixed cadence and reconciles them against what is registered, so a container
// whose registration failed does not stay unserved until it is restarted.
type Watcher struct {
	cli      dockerAPI
	reg      Registrar
	logger   *slog.Logger
	settleFn func() // injectable for tests; real impl sleeps 1s before resolving IP

	// active maps a container ID to the "ip:port" of its last successful
	// Register, so a sweep can tell a container that is already serving — and so
	// needs nothing done to it — from one that still needs registering. A start
	// event for a container already in here is a backend that moved.
	active map[string]string
	// failed maps a container ID whose last Register attempt did not put a
	// backend into service to what that attempt said, so one that keeps failing
	// the same way is logged once rather than on every sweep. Entries are
	// dropped as soon as the container registers, is deregistered, or stops
	// being listed.
	failed map[string]string

	// sweepInterval is the reconciliation cadence; tests shorten it.
	sweepInterval time.Duration
}

// NewWatcher builds a Watcher using Docker environment variables
// (DOCKER_HOST, etc.) for client configuration.
func NewWatcher(reg Registrar, logger *slog.Logger) (*Watcher, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return newWatcher(cli, reg, logger), nil
}

// newWatcher builds a Watcher around an already-constructed Docker client. It
// is the single place the watcher's defaults live, so tests start from the same
// state production does.
func newWatcher(cli dockerAPI, reg Registrar, logger *slog.Logger) *Watcher {
	return &Watcher{
		cli:           cli,
		reg:           reg,
		logger:        logger,
		settleFn:      func() { time.Sleep(time.Second) },
		active:        map[string]string{},
		failed:        map[string]string{},
		sweepInterval: defaultSweepInterval,
	}
}

// Run lists existing containers, then blocks reading the Docker event stream —
// and sweeping for containers the event stream left unregistered — until ctx is
// cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if _, err := w.cli.Ping(ctx); err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}

	if err := w.reconcile(ctx); err != nil {
		return fmt.Errorf("initial container scan: %w", err)
	}

	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))
	msgs, errs := w.cli.Events(ctx, events.ListOptions{Filters: f})
	interval := w.sweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	sweeps := time.NewTicker(interval)
	defer sweeps.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if err == nil || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("docker events: %w", err)
		case msg := <-msgs:
			w.handleEvent(ctx, msg)
		case <-sweeps.C:
			if err := w.reconcile(ctx); err != nil && ctx.Err() == nil {
				w.logger.Warn("container sweep failed; will retry",
					"retry_in", interval, "err", err)
			}
		}
	}
}

// reconcile lists the containers Docker currently has and brings the registered
// set into line with them: every labelled container that is not already serving
// is registered (or retried, if its last attempt failed), and every container
// that has gone is deregistered. It runs once at start-up and on every sweep,
// so a registration that failed for a transient reason recovers without the
// container being restarted.
func (w *Watcher) reconcile(ctx context.Context) error {
	cs, err := w.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(cs))
	for _, c := range cs {
		// A container is "seen" because Docker listed it as running, which is
		// all removeMissing asks. Whether its labels make it eligible to serve
		// is a separate question, decided below: were the two conflated, a
		// container whose listed labels failed to parse would be treated as
		// gone and torn out of a pool it is happily serving.
		seen[c.ID] = struct{}{}
		def, err := labels.Parse(c.Labels)
		if err != nil {
			continue
		}
		if _, serving := w.active[c.ID]; serving {
			// Already in the pool. A running container's IP does not change
			// under it, and one that restarts arrives as a start event, so
			// there is nothing here worth an inspect call per sweep.
			continue
		}
		w.register(ctx, c.ID, def)
	}
	w.removeMissing(seen)
	return nil
}

// removeMissing deregisters any active container that was not listed in the
// latest sweep, and forgets the failure state of containers that are no longer
// listed at all, so a departed container is neither retried forever nor left
// behind in the bookkeeping.
func (w *Watcher) removeMissing(seen map[string]struct{}) {
	for id := range w.active {
		if _, ok := seen[id]; !ok {
			w.reg.Deregister(id)
			delete(w.active, id)
			w.logger.Info("container gone; deregistered", "container", short(id))
		}
	}
	for id := range w.failed {
		if _, ok := seen[id]; !ok {
			delete(w.failed, id)
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, msg events.Message) {
	switch msg.Action {
	case events.ActionStart:
		w.settleFn()
		def, err := w.inspect(ctx, msg.Actor.ID)
		if err != nil {
			return
		}
		w.register(ctx, msg.Actor.ID, def)
	case events.ActionDie, events.ActionStop, events.ActionKill, events.ActionDestroy:
		w.deregister(msg.Actor.ID)
	}
}

// deregister drops a container from the pool and from the watcher's own
// bookkeeping, so a container that has gone is not retried by a later sweep.
func (w *Watcher) deregister(id string) {
	w.reg.Deregister(id)
	delete(w.active, id)
	delete(w.failed, id)
}

func (w *Watcher) inspect(ctx context.Context, id string) (*labels.ServiceDef, error) {
	insp, err := w.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if insp.Config == nil {
		return nil, fmt.Errorf("container %s: no config", id)
	}
	return labels.Parse(insp.Config.Labels)
}

func (w *Watcher) register(ctx context.Context, id string, def *labels.ServiceDef) {
	ip, err := w.resolveIP(ctx, id, def.Network)
	if err != nil {
		w.noteFailure(ctx, id, "resolve-ip: "+err.Error(),
			"could not resolve container IP; will retry on the next sweep",
			"container", short(id), "service", def.Service, "network", def.Network, "err", err)
		return
	}

	backendAddr := fmt.Sprintf("%s:%d", ip, def.Port)
	if prev, registered := w.active[id]; registered {
		if prev == backendAddr {
			return // already serving on this address; nothing to do
		}
		// The backend moved. Drop the record now, not after the Register below:
		// if that fails, nothing is serving this container and leaving the old
		// address behind would claim otherwise.
		w.reg.Deregister(id)
		delete(w.active, id)
	}

	// A container is recorded active only once Register reports its backend is
	// in the pool of a serving service, so a failed one is retried by the next
	// sweep rather than being taken for registered.
	if err := w.reg.Register(id, def, ip); err != nil {
		w.noteFailure(ctx, id, "register: "+err.Error(),
			"service registration failed; will retry on the next sweep",
			"container", short(id), "service", def.Service, "err", err)
		return
	}
	delete(w.failed, id)
	w.active[id] = backendAddr
}

// noteFailure records that a running, labelled container is not serving and
// logs why: at Warn the first time and whenever the reason changes, at Debug
// while the same reason repeats, so a container that cannot be registered does
// not warn on every sweep while a failure that turns into a different one is
// still surfaced. reason is what "the same failure" means, and is forgotten
// once the container registers or goes away — so a container that comes back is
// reported afresh.
func (w *Watcher) noteFailure(ctx context.Context, id, reason, msg string, attrs ...any) {
	level := slog.LevelWarn
	if prev, failing := w.failed[id]; failing && prev == reason {
		level = slog.LevelDebug
	}
	w.failed[id] = reason
	w.logger.Log(ctx, level, msg, attrs...)
}

func (w *Watcher) resolveIP(ctx context.Context, id, network string) (string, error) {
	insp, err := w.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	if insp.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings")
	}
	ep, ok := insp.NetworkSettings.Networks[network]
	if !ok || ep == nil {
		return "", fmt.Errorf("container not attached to network %q", network)
	}
	if ep.IPAddress == "" {
		return "", fmt.Errorf("container has no IP on network %q yet", network)
	}
	return ep.IPAddress, nil
}

// Close releases the underlying Docker client.
func (w *Watcher) Close() error { return w.cli.Close() }

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
