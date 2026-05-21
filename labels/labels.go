// Package labels parses the tsserve.* label vocabulary into a ServiceDef.
// The same parser is used by both the Docker and ECS discovery backends since
// container labels and ECS task-definition dockerLabels are both
// map[string]string.
package labels

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const Prefix = "tsserve."

const (
	Enable  = Prefix + "enable"
	Service = Prefix + "service"
	Port    = Prefix + "port"
	Network = Prefix + "network"
	Scheme  = Prefix + "scheme"
	Caps    = Prefix + "caps"
)

// ServiceDef is the parsed, validated set of tsserve labels for one container
// or labelled ECS task container.
//
// The Network field is meaningful only in Docker mode; ECS discovery ignores
// it because the network mode is fixed by the task definition.
type ServiceDef struct {
	Service string
	Port    uint16
	Network string
	Scheme  string
	Caps    []string
}

// ErrDisabled is returned by [Parse] when the container is not opted-in to
// tsserve (either the enable label is missing or not set to "true"). It is
// distinct from a validation error so callers can silently ignore unrelated
// containers.
var ErrDisabled = errors.New("tsserve.enable not set to true")

// Parse parses a label map into a [ServiceDef]. Returns [ErrDisabled] when the
// container has not opted in.
func Parse(labels map[string]string) (*ServiceDef, error) {
	if labels == nil {
		return nil, ErrDisabled
	}
	if labels[Enable] != "true" {
		return nil, ErrDisabled
	}

	svc := labels[Service]
	if svc == "" {
		return nil, fmt.Errorf("missing required label %s", Service)
	}
	if !strings.HasPrefix(svc, "svc:") {
		return nil, fmt.Errorf("label %s=%q must start with %q", Service, svc, "svc:")
	}

	portStr := labels[Port]
	if portStr == "" {
		return nil, fmt.Errorf("missing required label %s", Port)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("label %s=%q is not a valid port", Port, portStr)
	}

	network := labels[Network]
	if network == "" {
		network = "bridge"
	}

	scheme := labels[Scheme]
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("label %s=%q must be 'http' or 'https'", Scheme, scheme)
	}

	var caps []string
	if raw := labels[Caps]; raw != "" {
		for c := range strings.SplitSeq(raw, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				caps = append(caps, c)
			}
		}
	}

	return &ServiceDef{
		Service: svc,
		Port:    uint16(port),
		Network: network,
		Scheme:  scheme,
		Caps:    caps,
	}, nil
}
