// Package ecs implements the ECS-cluster discovery backend. It polls the AWS
// ECS API for running tasks in a configured cluster, reads tsserve.* labels
// from the task definition's containerDefinitions[*].dockerLabels, resolves
// each labelled container to a host:port backend (bridge or awsvpc), and
// drives the same Registrar interface as the Docker watcher.
package ecs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"mkmba.nz/tsserve/labels"
)

// Registrar mirrors docker.Registrar. Defined locally so the ecs package does
// not import docker.
type Registrar interface {
	Register(key string, def *labels.ServiceDef, backendIP string) error
	Deregister(key string)
}

// Config configures a Watcher. Cluster is required; everything else has a
// reasonable default.
type Config struct {
	Cluster      string
	PollInterval time.Duration
}

// Watcher polls ECS for labelled tasks and drives a Registrar.
type Watcher struct {
	cfg      Config
	ecs      ECSAPI
	ec2      EC2API
	reg      Registrar
	logger   *slog.Logger
	taskDefs *taskDefCache
	hostIPs  *hostIPCache
	active   map[string]string // registration key -> "host:port" of the last successful Register
	clockNow func() time.Time  // injectable for tests
	sleepFor func(context.Context, time.Duration) error
}

// NewWatcher returns a Watcher ready to Run.
func NewWatcher(cfg Config, ecsAPI ECSAPI, ec2API EC2API, reg Registrar, logger *slog.Logger) (*Watcher, error) {
	if cfg.Cluster == "" {
		return nil, errors.New("ecs.Watcher: Cluster is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}
	return &Watcher{
		cfg:      cfg,
		ecs:      ecsAPI,
		ec2:      ec2API,
		reg:      reg,
		logger:   logger,
		taskDefs: newTaskDefCache(),
		hostIPs:  newHostIPCache(),
		active:   map[string]string{},
		clockNow: time.Now,
		sleepFor: ctxSleep,
	}, nil
}

// Run blocks running poll-and-diff cycles until ctx is cancelled. A failure of
// any single cycle is logged but does not exit the loop; the next cycle
// reconciles.
func (w *Watcher) Run(ctx context.Context) error {
	w.logger.Info("ecs watcher starting",
		"cluster", w.cfg.Cluster,
		"poll_interval", w.cfg.PollInterval)

	if err := w.cycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("ecs initial poll failed", "err", err)
	}

	for {
		if err := w.sleepFor(ctx, w.cfg.PollInterval); err != nil {
			return ctx.Err()
		}
		if err := w.cycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("ecs poll cycle failed", "err", err)
		}
	}
}

// cycle runs one poll-and-diff iteration.
func (w *Watcher) cycle(ctx context.Context) error {
	taskArns, err := w.listAllTaskArns(ctx)
	if err != nil {
		return err
	}

	seen := map[string]struct{}{}

	for _, batch := range chunk(taskArns, 100) {
		out, err := w.ecs.DescribeTasks(ctx, &awsecs.DescribeTasksInput{
			Cluster: &w.cfg.Cluster,
			Tasks:   batch,
		})
		if err != nil {
			return fmt.Errorf("DescribeTasks: %w", err)
		}
		for _, task := range out.Tasks {
			if task.LastStatus == nil || *task.LastStatus != "RUNNING" {
				continue
			}
			if task.TaskDefinitionArn == nil {
				continue
			}
			td, err := w.taskDefs.get(ctx, w.ecs, *task.TaskDefinitionArn)
			if err != nil {
				w.logger.Warn("task definition lookup failed",
					"task", derefArn(task.TaskArn), "err", err)
				continue
			}
			w.applyTask(ctx, task, td, seen)
		}
	}

	w.removeMissing(seen)
	return nil
}

// applyTask iterates the labelled containers in a task definition, resolves
// each backend, and calls Register. The seen set is populated with the
// composite registration keys observed in this cycle.
func (w *Watcher) applyTask(
	ctx context.Context,
	task ecstypes.Task,
	td *ecstypes.TaskDefinition,
	seen map[string]struct{},
) {
	for _, cd := range td.ContainerDefinitions {
		if cd.Name == nil {
			continue
		}
		def, err := labels.Parse(cd.DockerLabels)
		if err != nil {
			continue // disabled or invalid; ParseLabels filters silently for us
		}

		key := registrationKey(derefArn(task.TaskArn), *cd.Name)
		seen[key] = struct{}{}

		backend, err := resolveBackend(ctx, w.ecs, w.ec2, w.hostIPs,
			w.cfg.Cluster, task, td, *cd.Name, def.Port)
		if err != nil {
			if errors.Is(err, errNoBackend) {
				w.logger.Debug("backend not yet resolvable; will retry next cycle",
					"task", derefArn(task.TaskArn),
					"container", *cd.Name,
					"err", err)
				continue
			}
			w.logger.Warn("backend resolution failed",
				"task", derefArn(task.TaskArn),
				"container", *cd.Name,
				"err", err)
			continue
		}

		backendAddr := fmt.Sprintf("%s:%d", backend.host, backend.port)
		if prev, registered := w.active[key]; registered && prev == backendAddr {
			continue
		}
		if _, registered := w.active[key]; registered {
			w.reg.Deregister(key)
		}

		// Manager wants def.Port to point at the backend port; for bridge mode
		// that is the resolved hostPort, not the labelled containerPort.
		defForRegister := *def
		defForRegister.Port = backend.port

		if err := w.reg.Register(key, &defForRegister, backend.host); err != nil {
			w.logger.Error("Register failed", "key", key, "err", err)
			continue
		}
		w.active[key] = backendAddr
	}
}

// removeMissing deregisters any active key that wasn't seen in the latest
// cycle.
func (w *Watcher) removeMissing(seen map[string]struct{}) {
	for key := range w.active {
		if _, ok := seen[key]; !ok {
			w.reg.Deregister(key)
			delete(w.active, key)
		}
	}
}

// listAllTaskArns paginates ListTasks until all RUNNING task ARNs in the
// cluster have been collected.
func (w *Watcher) listAllTaskArns(ctx context.Context) ([]string, error) {
	var all []string
	desired := ecstypes.DesiredStatusRunning
	var next *string
	for {
		out, err := w.ecs.ListTasks(ctx, &awsecs.ListTasksInput{
			Cluster:       &w.cfg.Cluster,
			DesiredStatus: desired,
			NextToken:     next,
		})
		if err != nil {
			return nil, fmt.Errorf("ListTasks: %w", err)
		}
		all = append(all, out.TaskArns...)
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	return all, nil
}

// registrationKey returns a stable identifier for one labelled container in a
// task. Container names are unique within a task definition.
func registrationKey(taskArn, containerName string) string {
	return taskArn + "#" + containerName
}

func chunk(s []string, size int) [][]string {
	if size <= 0 || len(s) == 0 {
		return nil
	}
	var out [][]string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
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
