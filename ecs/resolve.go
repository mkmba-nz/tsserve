package ecs

import (
	"context"
	"fmt"

	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// backend is the resolved reverse-proxy target for one labelled container in
// one task.
type backend struct {
	host string
	port uint16
}

// resolveBackend returns the host:port the proxy should forward to for the
// named container in task.
//
// The branching follows the task definition's networkMode:
//   - "bridge": container instance host IP + the hostPort that maps to the
//     labelled container port.
//   - "awsvpc": the task ENI's private IPv4 + the labelled container port.
//
// Returns errNoBackend (wrapped) when the data needed isn't present yet — the
// caller is expected to log a debug message and retry on the next poll cycle.
func resolveBackend(
	ctx context.Context,
	ecsAPI ECSAPI,
	ec2API EC2API,
	hostCache *hostIPCache,
	cluster string,
	task ecstypes.Task,
	taskDef *ecstypes.TaskDefinition,
	containerName string,
	containerPort uint16,
) (backend, error) {
	mode := taskDef.NetworkMode

	switch mode {
	case ecstypes.NetworkModeAwsvpc:
		ip, err := awsvpcIP(task)
		if err != nil {
			return backend{}, err
		}
		return backend{host: ip, port: containerPort}, nil

	case ecstypes.NetworkModeBridge, "":
		hostPort, err := bridgeHostPort(task, containerName, containerPort)
		if err != nil {
			return backend{}, err
		}
		if task.ContainerInstanceArn == nil || *task.ContainerInstanceArn == "" {
			return backend{}, fmt.Errorf("bridge task has no container instance ARN: %w", errNoBackend)
		}
		ip, err := hostCache.get(ctx, ecsAPI, ec2API, cluster, *task.ContainerInstanceArn)
		if err != nil {
			hostCache.invalidate(*task.ContainerInstanceArn)
			return backend{}, err
		}
		return backend{host: ip, port: hostPort}, nil

	case ecstypes.NetworkModeHost:
		// host mode: container shares the host's network namespace; the labelled
		// port is reachable on the host's private IP directly.
		if task.ContainerInstanceArn == nil || *task.ContainerInstanceArn == "" {
			return backend{}, fmt.Errorf("host-mode task has no container instance ARN: %w", errNoBackend)
		}
		ip, err := hostCache.get(ctx, ecsAPI, ec2API, cluster, *task.ContainerInstanceArn)
		if err != nil {
			hostCache.invalidate(*task.ContainerInstanceArn)
			return backend{}, err
		}
		return backend{host: ip, port: containerPort}, nil

	default:
		return backend{}, fmt.Errorf("unsupported networkMode %q for task %s",
			mode, derefArn(task.TaskArn))
	}
}

// awsvpcIP extracts the ENI private IPv4 address from a task's attachments.
func awsvpcIP(task ecstypes.Task) (string, error) {
	for _, a := range task.Attachments {
		if a.Type == nil || *a.Type != "ElasticNetworkInterface" {
			continue
		}
		for _, kv := range a.Details {
			if kv.Name != nil && *kv.Name == "privateIPv4Address" && kv.Value != nil {
				return *kv.Value, nil
			}
		}
	}
	return "", fmt.Errorf("task %s has no ENI attachment yet: %w",
		derefArn(task.TaskArn), errNoBackend)
}

// bridgeHostPort finds the hostPort that maps to containerPort on the named
// container in a bridge-mode task.
func bridgeHostPort(task ecstypes.Task, containerName string, containerPort uint16) (uint16, error) {
	for _, c := range task.Containers {
		if c.Name == nil || *c.Name != containerName {
			continue
		}
		for _, nb := range c.NetworkBindings {
			if nb.ContainerPort == nil || uint16(*nb.ContainerPort) != containerPort {
				continue
			}
			if nb.HostPort == nil || *nb.HostPort == 0 {
				continue
			}
			return uint16(*nb.HostPort), nil
		}
	}
	return 0, fmt.Errorf("bridge task %s container %q has no networkBinding for container port %d: %w",
		derefArn(task.TaskArn), containerName, containerPort, errNoBackend)
}

func derefArn(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
