package docker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Registrar is implemented by the proxy package. The watcher calls Register
// when a labelled container becomes ready and Deregister when one goes away.
type Registrar interface {
	Register(containerID string, def *ServiceDef, backendIP string) error
	Deregister(containerID string)
}

// Watcher subscribes to Docker events and drives the [Registrar] in response
// to container lifecycle changes.
type Watcher struct {
	cli      *client.Client
	reg      Registrar
	logger   *slog.Logger
	settleFn func() // injectable for tests; real impl sleeps 1s before resolving IP
}

// NewWatcher builds a Watcher using Docker environment variables
// (DOCKER_HOST, etc.) for client configuration.
func NewWatcher(reg Registrar, logger *slog.Logger) (*Watcher, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Watcher{
		cli:      cli,
		reg:      reg,
		logger:   logger,
		settleFn: func() { time.Sleep(time.Second) },
	}, nil
}

// Run lists existing containers, then blocks reading the Docker event stream
// until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if _, err := w.cli.Ping(ctx); err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}

	if err := w.scanExisting(ctx); err != nil {
		return fmt.Errorf("initial container scan: %w", err)
	}

	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))
	msgs, errs := w.cli.Events(ctx, events.ListOptions{Filters: f})

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
		}
	}
}

func (w *Watcher) scanExisting(ctx context.Context) error {
	cs, err := w.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return err
	}
	for _, c := range cs {
		def, err := ParseLabels(c.Labels)
		if err != nil {
			continue
		}
		w.register(ctx, c.ID, def)
	}
	return nil
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
		w.reg.Deregister(msg.Actor.ID)
	}
}

func (w *Watcher) inspect(ctx context.Context, id string) (*ServiceDef, error) {
	insp, err := w.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if insp.Config == nil {
		return nil, fmt.Errorf("container %s: no config", id)
	}
	return ParseLabels(insp.Config.Labels)
}

func (w *Watcher) register(ctx context.Context, id string, def *ServiceDef) {
	ip, err := w.resolveIP(ctx, id, def.Network)
	if err != nil {
		w.logger.Warn("could not resolve container IP",
			"container", short(id), "service", def.Service, "network", def.Network, "err", err)
		return
	}
	if err := w.reg.Register(id, def, ip); err != nil {
		w.logger.Error("service registration failed",
			"container", short(id), "service", def.Service, "err", err)
	}
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
