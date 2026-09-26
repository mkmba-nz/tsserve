package lambda

import (
	"strings"
	"testing"
	"time"
)

func TestParseAccounts(t *testing.T) {
	got, err := ParseAccounts(`[{"name":"prod","region":"us-east-1","profile":"p","configFile":"/c","accessKeyID":"AKIA","secretAccessKey":"s"}]`)
	if err != nil {
		t.Fatalf("ParseAccounts: %v", err)
	}
	want := Account{Name: "prod", Region: "us-east-1", Profile: "p", ConfigFile: "/c", AccessKeyID: "AKIA", SecretAccessKey: "s"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ParseAccounts = %+v, want [%+v]", got, want)
	}

	if got, err := ParseAccounts("  \n"); err != nil || got != nil {
		t.Fatalf("ParseAccounts(blank) = %v, %v; want nil, nil", got, err)
	}
	if _, err := ParseAccounts(`{"name":"x"}`); err == nil || !strings.Contains(err.Error(), "TSSERVE_LAMBDA_ACCOUNTS") {
		t.Fatalf("ParseAccounts(object) error = %v, want an error naming TSSERVE_LAMBDA_ACCOUNTS", err)
	}
}

var defaults = Defaults{
	Region:       "ap-southeast-2",
	Profile:      "lambda-reader",
	ConfigFile:   "/etc/tsserve/aws-config",
	PollInterval: 45 * time.Second,
}

func TestPlan_UnsetYieldsOneDefaultReader(t *testing.T) {
	specs, err := Plan(nil, defaults)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := WatcherSpec{Region: "ap-southeast-2", Profile: "lambda-reader", ConfigFile: "/etc/tsserve/aws-config", PollInterval: 45 * time.Second}
	if len(specs) != 1 || specs[0] != want {
		t.Fatalf("Plan(nil) = %+v, want [%+v]", specs, want)
	}
}

func TestPlan_BlankFieldsInheritDefaults(t *testing.T) {
	specs, err := Plan([]Account{
		{Name: "sydney"},
		{Name: "virginia", Region: "us-east-1", Profile: "other", ConfigFile: "/other", AccessKeyID: "AKIA", SecretAccessKey: "s"},
	}, defaults)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []WatcherSpec{
		{Name: "sydney", Region: "ap-southeast-2", Profile: "lambda-reader", ConfigFile: "/etc/tsserve/aws-config", PollInterval: 45 * time.Second},
		{Name: "virginia", Region: "us-east-1", Profile: "other", ConfigFile: "/other", AccessKeyID: "AKIA", SecretAccessKey: "s", PollInterval: 45 * time.Second},
	}
	if len(specs) != len(want) {
		t.Fatalf("Plan = %+v, want %+v", specs, want)
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, specs[i], want[i])
		}
	}
}

func TestPlan_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		accounts []Account
		wantErr  string
	}{
		{
			name:     "duplicate explicit name",
			accounts: []Account{{Name: "prod", Region: "us-east-1"}, {Name: "prod", Region: "us-west-2"}},
			wantErr:  `lambda account name "prod" is used more than once`,
		},
		{
			name:     "access key without secret",
			accounts: []Account{{Name: "prod", AccessKeyID: "AKIA"}},
			wantErr:  "lambda account prod: accessKeyID and secretAccessKey must be set together",
		},
		{
			name:     "secret without access key on an unnamed account",
			accounts: []Account{{}, {SecretAccessKey: "s"}},
			wantErr:  "lambda account accounts[1]: accessKeyID and secretAccessKey must be set together",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Plan(tt.accounts, defaults)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Plan error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPlan_UnnamedAccountsMayRepeat(t *testing.T) {
	// Unnamed entries are told apart after STS resolution, not here.
	specs, err := Plan([]Account{{Profile: "a"}, {Profile: "b"}}, defaults)
	if err != nil || len(specs) != 2 {
		t.Fatalf("Plan = %+v, %v; want two specs", specs, err)
	}
}
