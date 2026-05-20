// Package docker contains the Docker integration: label parsing, event
// subscription, and container IP resolution.
package docker

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const labelPrefix = "tsserve."

const (
	labelEnable  = labelPrefix + "enable"
	labelService = labelPrefix + "service"
	labelPort    = labelPrefix + "port"
	labelNetwork = labelPrefix + "network"
	labelScheme  = labelPrefix + "scheme"
	labelCaps    = labelPrefix + "caps"
)

// ServiceDef is the parsed, validated set of tsserve labels for one container.
//
// It is produced by [ParseLabels] and consumed by the proxy package's service
// manager to build a Tailscale Service listener and reverse proxy.
type ServiceDef struct {
	Service string
	Port    uint16
	Network string
	Scheme  string
	Caps    []string
}

// ErrDisabled is returned by [ParseLabels] when the container is not opted-in
// to tsserve (either the enable label is missing or not set to "true"). It is
// distinct from a validation error so callers can silently ignore unrelated
// containers.
var ErrDisabled = errors.New("tsserve.enable not set to true")

// ParseLabels parses a container's label map into a [ServiceDef]. Returns
// [ErrDisabled] when the container has not opted in.
func ParseLabels(labels map[string]string) (*ServiceDef, error) {
	if labels == nil {
		return nil, ErrDisabled
	}
	if labels[labelEnable] != "true" {
		return nil, ErrDisabled
	}

	svc := labels[labelService]
	if svc == "" {
		return nil, fmt.Errorf("missing required label %s", labelService)
	}
	if !strings.HasPrefix(svc, "svc:") {
		return nil, fmt.Errorf("label %s=%q must start with %q", labelService, svc, "svc:")
	}

	portStr := labels[labelPort]
	if portStr == "" {
		return nil, fmt.Errorf("missing required label %s", labelPort)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("label %s=%q is not a valid port", labelPort, portStr)
	}

	network := labels[labelNetwork]
	if network == "" {
		network = "bridge"
	}

	scheme := labels[labelScheme]
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("label %s=%q must be 'http' or 'https'", labelScheme, scheme)
	}

	var caps []string
	if raw := labels[labelCaps]; raw != "" {
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
