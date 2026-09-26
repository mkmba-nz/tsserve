package lambda

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Account is one AWS credential context to discover functions in. A tsserve
// process can be configured with a list of these (via TSSERVE_LAMBDA_ACCOUNTS)
// to read several accounts, or one account in several regions, building one
// independent Watcher per entry.
//
// Every field is optional: a blank field falls back to the shared Defaults
// (derived from AWS_REGION and the TSSERVE_LAMBDA_AWS_* env vars).
type Account struct {
	// Name is an identity label used in logs and the readers table. When empty
	// it is resolved at runtime from the AWS account ID (via STS
	// GetCallerIdentity). Must be unique across accounts; entries for one
	// account in different regions need explicit, distinct names.
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
}

// Defaults holds the account-independent configuration. Its values fill any
// field an Account leaves blank.
type Defaults struct {
	Region       string
	Profile      string
	ConfigFile   string
	PollInterval time.Duration
}

// WatcherSpec is the fully-resolved description of one Watcher to build. Name
// may be empty: it is then resolved from the AWS account ID by the caller.
type WatcherSpec struct {
	Name            string
	Region          string
	Profile         string
	ConfigFile      string
	AccessKeyID     string
	SecretAccessKey string
	PollInterval    time.Duration
}

// ParseAccounts decodes the TSSERVE_LAMBDA_ACCOUNTS JSON array. An empty or
// whitespace-only value yields a nil slice (the single-reader path).
func ParseAccounts(raw string) ([]Account, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var accounts []Account
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		return nil, fmt.Errorf("TSSERVE_LAMBDA_ACCOUNTS: invalid JSON: %w", err)
	}
	return accounts, nil
}

// Plan resolves a list of accounts against the shared Defaults into one
// WatcherSpec per account. It performs no AWS calls. With no accounts
// configured it returns a single spec built entirely from Defaults.
func Plan(accounts []Account, d Defaults) ([]WatcherSpec, error) {
	if len(accounts) == 0 {
		return []WatcherSpec{{
			Region:       d.Region,
			Profile:      d.Profile,
			ConfigFile:   d.ConfigFile,
			PollInterval: d.PollInterval,
		}}, nil
	}

	specs := make([]WatcherSpec, 0, len(accounts))
	seenNames := make(map[string]struct{}, len(accounts))
	for i, a := range accounts {
		label := a.Name
		if label == "" {
			label = fmt.Sprintf("accounts[%d]", i)
		}

		if (a.AccessKeyID == "") != (a.SecretAccessKey == "") {
			return nil, fmt.Errorf("lambda account %s: accessKeyID and secretAccessKey must be set together", label)
		}

		// Explicit names must be unique; account IDs resolved later are
		// checked for collisions by the caller after STS resolution.
		if a.Name != "" {
			if _, dup := seenNames[a.Name]; dup {
				return nil, fmt.Errorf("lambda account name %q is used more than once", a.Name)
			}
			seenNames[a.Name] = struct{}{}
		}

		specs = append(specs, WatcherSpec{
			Name:            a.Name,
			Region:          cmp.Or(a.Region, d.Region),
			Profile:         cmp.Or(a.Profile, d.Profile),
			ConfigFile:      cmp.Or(a.ConfigFile, d.ConfigFile),
			AccessKeyID:     a.AccessKeyID,
			SecretAccessKey: a.SecretAccessKey,
			PollInterval:    d.PollInterval,
		})
	}
	return specs, nil
}
