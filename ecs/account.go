package ecs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Account is one AWS credential context to discover ECS tasks in. A tsserve
// process running with TSSERVE_DISCOVERY=ecs can be configured with a list of
// these (via TSSERVE_ECS_ACCOUNTS) to watch several accounts at once, building
// one independent Watcher per account.
//
// Every field is optional: a blank field falls back to the shared Defaults
// (derived from the single-account TSSERVE_ECS_* / AWS_REGION env vars), which
// keeps the historical single-account configuration working unchanged.
type Account struct {
	// Name is an identity label used in logs and the startup summary. When
	// empty it is resolved at runtime from the AWS account ID (via STS
	// GetCallerIdentity). Must be unique across accounts.
	Name string `json:"name,omitempty"`
	// Region overrides the shared AWS region for this account's clients.
	Region string `json:"region,omitempty"`
	// Profile selects an AWS shared-config profile for this account.
	Profile string `json:"profile,omitempty"`
	// ConfigFile points the SDK at a shared-config file for this account.
	ConfigFile string `json:"configFile,omitempty"`
	// AccessKeyID and SecretAccessKey supply static credentials. They are
	// all-or-nothing: set both or neither.
	AccessKeyID     string `json:"accessKeyID,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	// Cluster is the ECS cluster name or ARN this account's watcher polls.
	Cluster string `json:"cluster,omitempty"`
}

// Defaults holds the account-independent configuration, sourced from the
// single-account env vars. Its values fill any field an Account leaves blank.
type Defaults struct {
	Region       string
	Profile      string
	ConfigFile   string
	Cluster      string
	PollInterval time.Duration
}

// WatcherSpec is the fully-resolved, credential-context-complete description of
// one Watcher to build. Name may be empty here: it is resolved from the AWS
// account ID by the caller (which owns the STS client) when not set explicitly.
type WatcherSpec struct {
	Name            string
	Cluster         string
	Region          string
	Profile         string
	ConfigFile      string
	AccessKeyID     string
	SecretAccessKey string
	PollInterval    time.Duration
}

// ParseAccounts decodes the TSSERVE_ECS_ACCOUNTS JSON array. An empty or
// whitespace-only value yields a nil slice (the single-account path).
func ParseAccounts(raw string) ([]Account, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var accounts []Account
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		return nil, fmt.Errorf("TSSERVE_ECS_ACCOUNTS: invalid JSON: %w", err)
	}
	return accounts, nil
}

// Plan resolves a list of accounts against the shared Defaults into one
// WatcherSpec per account. It performs no AWS calls, so it is safe to unit-test
// and cheap to run at startup.
//
// With no accounts configured it returns a single spec built entirely from
// Defaults, preserving the historical single-account behaviour byte-for-byte.
func Plan(accounts []Account, d Defaults) ([]WatcherSpec, error) {
	if len(accounts) == 0 {
		spec := WatcherSpec{
			Cluster:      d.Cluster,
			Region:       d.Region,
			Profile:      d.Profile,
			ConfigFile:   d.ConfigFile,
			PollInterval: d.PollInterval,
		}
		if spec.Cluster == "" {
			return nil, fmt.Errorf("TSSERVE_ECS_CLUSTER is required when TSSERVE_DISCOVERY=ecs")
		}
		return []WatcherSpec{spec}, nil
	}

	specs := make([]WatcherSpec, 0, len(accounts))
	seenNames := make(map[string]struct{}, len(accounts))
	for i, a := range accounts {
		// A stable label for error messages, whether or not Name is set.
		label := a.Name
		if label == "" {
			label = fmt.Sprintf("accounts[%d]", i)
		}

		// Static credentials are all-or-nothing.
		if (a.AccessKeyID == "") != (a.SecretAccessKey == "") {
			return nil, fmt.Errorf("ecs account %s: accessKeyID and secretAccessKey must be set together", label)
		}

		spec := WatcherSpec{
			Name:            a.Name,
			Cluster:         firstNonEmpty(a.Cluster, d.Cluster),
			Region:          firstNonEmpty(a.Region, d.Region),
			Profile:         firstNonEmpty(a.Profile, d.Profile),
			ConfigFile:      firstNonEmpty(a.ConfigFile, d.ConfigFile),
			AccessKeyID:     a.AccessKeyID,
			SecretAccessKey: a.SecretAccessKey,
			PollInterval:    d.PollInterval,
		}

		if spec.Cluster == "" {
			return nil, fmt.Errorf("ecs account %s: cluster is required (set it on the account or via TSSERVE_ECS_CLUSTER)", label)
		}

		// Explicit names must be unique; account IDs resolved later are
		// checked for collisions by the caller after STS resolution.
		if a.Name != "" {
			if _, dup := seenNames[a.Name]; dup {
				return nil, fmt.Errorf("ecs account name %q is used more than once", a.Name)
			}
			seenNames[a.Name] = struct{}{}
		}

		specs = append(specs, spec)
	}
	return specs, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
