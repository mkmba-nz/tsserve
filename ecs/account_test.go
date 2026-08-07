package ecs

import (
	"testing"
	"time"
)

func TestParseAccounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "empty", raw: "", want: 0},
		{name: "whitespace", raw: "   \n\t ", want: 0},
		{name: "empty array", raw: "[]", want: 0},
		{
			name: "two accounts",
			raw:  `[{"profile":"ecs-reader-111","cluster":"infra"},{"profile":"ecs-reader-222","cluster":"front","region":"ap-southeast-2"}]`,
			want: 2,
		},
		{name: "malformed", raw: `[{"profile":]`, wantErr: true},
		{name: "not an array", raw: `{"profile":"x"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAccounts(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseAccounts(%q): expected error, got nil", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAccounts(%q): unexpected error: %v", tt.raw, err)
			}
			if len(got) != tt.want {
				t.Fatalf("ParseAccounts(%q): got %d accounts, want %d", tt.raw, len(got), tt.want)
			}
		})
	}
}

func TestParseAccounts_FieldMapping(t *testing.T) {
	t.Parallel()

	raw := `[{"name":"corp","region":"ap-southeast-2","profile":"p","configFile":"/etc/aws/config","accessKeyID":"AKIA","secretAccessKey":"secret","cluster":"infra"}]`
	got, err := ParseAccounts(raw)
	if err != nil {
		t.Fatalf("ParseAccounts: %v", err)
	}
	want := Account{
		Name: "corp", Region: "ap-southeast-2", Profile: "p",
		ConfigFile: "/etc/aws/config", AccessKeyID: "AKIA",
		SecretAccessKey: "secret", Cluster: "infra",
	}
	if got[0] != want {
		t.Fatalf("field mapping mismatch:\n got %+v\nwant %+v", got[0], want)
	}
}

func TestPlan_NoAccounts_SingleAccountPath(t *testing.T) {
	t.Parallel()

	d := Defaults{Cluster: "infra", Region: "ap-southeast-2", Profile: "primary", PollInterval: 30 * time.Second}
	specs, err := Plan(nil, d)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	got := specs[0]
	want := WatcherSpec{Cluster: "infra", Region: "ap-southeast-2", Profile: "primary", PollInterval: 30 * time.Second}
	if got != want {
		t.Fatalf("single-account spec mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestPlan_NoAccounts_MissingClusterErrors(t *testing.T) {
	t.Parallel()

	if _, err := Plan(nil, Defaults{}); err == nil {
		t.Fatal("expected error when no cluster is configured, got nil")
	}
}

func TestPlan_AccountsInheritDefaults(t *testing.T) {
	t.Parallel()

	d := Defaults{
		Cluster: "shared", Region: "ap-southeast-2", Profile: "shared-profile",
		ConfigFile: "/etc/aws/config", PollInterval: 15 * time.Second,
	}
	accounts := []Account{
		// Inherits every shared default except an explicit name.
		{Name: "corp"},
		// Overrides cluster + region; inherits profile + config file.
		{Name: "staging", Cluster: "front", Region: "us-east-1"},
	}

	specs, err := Plan(accounts, d)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}

	corp := specs[0]
	if corp.Name != "corp" || corp.Cluster != "shared" || corp.Region != "ap-southeast-2" ||
		corp.Profile != "shared-profile" || corp.ConfigFile != "/etc/aws/config" ||
		corp.PollInterval != 15*time.Second {
		t.Fatalf("corp spec did not inherit defaults: %+v", corp)
	}

	staging := specs[1]
	if staging.Cluster != "front" || staging.Region != "us-east-1" || staging.Profile != "shared-profile" {
		t.Fatalf("staging spec overrides wrong: %+v", staging)
	}
}

func TestPlan_PerAccountClusterWithoutSharedDefault(t *testing.T) {
	t.Parallel()

	// No shared cluster, but each account supplies its own.
	accounts := []Account{
		{Name: "a", Cluster: "ca", Profile: "pa"},
		{Name: "b", Cluster: "cb", Profile: "pb"},
	}
	specs, err := Plan(accounts, Defaults{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if specs[0].Cluster != "ca" || specs[1].Cluster != "cb" {
		t.Fatalf("per-account clusters not honoured: %+v", specs)
	}
}

func TestPlan_MissingClusterOnAccountErrors(t *testing.T) {
	t.Parallel()

	// One account has no cluster and there is no shared default.
	accounts := []Account{{Name: "orphan", Profile: "p"}}
	if _, err := Plan(accounts, Defaults{}); err == nil {
		t.Fatal("expected error for account without a resolvable cluster, got nil")
	}
}

func TestPlan_PartialStaticCredentialsError(t *testing.T) {
	t.Parallel()

	accounts := []Account{{Name: "a", Cluster: "c", AccessKeyID: "AKIA"}} // secret missing
	if _, err := Plan(accounts, Defaults{}); err == nil {
		t.Fatal("expected error for partial static credentials, got nil")
	}
}

func TestPlan_DuplicateExplicitNameError(t *testing.T) {
	t.Parallel()

	accounts := []Account{
		{Name: "dup", Cluster: "c1"},
		{Name: "dup", Cluster: "c2"},
	}
	if _, err := Plan(accounts, Defaults{}); err == nil {
		t.Fatal("expected error for duplicate account name, got nil")
	}
}

func TestPlan_UnnamedAccountsDeferNameResolution(t *testing.T) {
	t.Parallel()

	// Accounts without names are permitted; their identity is resolved later
	// via STS, so Plan must leave Name empty and not treat two blanks as a
	// duplicate.
	accounts := []Account{
		{Cluster: "c1", Profile: "p1"},
		{Cluster: "c2", Profile: "p2"},
	}
	specs, err := Plan(accounts, Defaults{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i, s := range specs {
		if s.Name != "" {
			t.Fatalf("specs[%d].Name = %q, want empty (deferred to STS)", i, s.Name)
		}
	}
}
