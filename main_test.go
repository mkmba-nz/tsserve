package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mkmba.nz/tsserve/ecs"
	"mkmba.nz/tsserve/lambda"
)

// TestBuildLambdaWatchers_DisablesOnlyTheReaderWhoseConfigFails builds two
// readers offline: one whose AWS config cannot load (a profile missing from
// its config file) and one with an explicit name and static credentials,
// which must not need STS.
func TestBuildLambdaWatchers_DisablesOnlyTheReaderWhoseConfigFails(t *testing.T) {
	emptyConfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []lambda.WatcherSpec{
		{Name: "broken", Region: "us-east-1", Profile: "absent", ConfigFile: emptyConfig},
		{Name: "prod", Region: "ap-southeast-2", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"},
	}
	readers := ecs.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	watchers := buildLambdaWatchers(context.Background(), specs, true, 0,
		&registrarAdapter{fatal: make(chan error, 1)}, readers, logger)

	if len(watchers) != 1 {
		t.Fatalf("watchers = %d, want 1 (the broken reader disabled, the other kept)", len(watchers))
	}
	snap := readers.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("readers = %+v, want both readers listed", snap)
	}
	broken, prod := snap[0], snap[1]
	if broken.Name != "broken" || broken.Healthy || !strings.Contains(broken.LastError, "aws config") {
		t.Errorf("broken reader = %+v, want unhealthy with the aws config error", broken)
	}
	if prod.Name != "prod" || prod.Region != "ap-southeast-2" || prod.LastError != "" || prod.Account != "" {
		t.Errorf("prod reader = %+v, want named, in its region, no error and no STS-resolved account", prod)
	}
	if prod.PollInterval != lambda.DefaultPollInterval {
		t.Errorf("prod poll interval = %s, want the %s default", prod.PollInterval, lambda.DefaultPollInterval)
	}
	// The mode reaches the status page through the readerSource adapter.
	for i, r := range (readerSource{readers}).Readers() {
		if r.Mode != "lambda" {
			t.Errorf("status reader[%d] mode = %q, want lambda", i, r.Mode)
		}
	}
}

// An ECS reader is listed with its discovery mode, which reaches the status
// page through the readerSource adapter. Static credentials and an explicit
// name keep the build offline.
func TestBuildECSWatchers_ReaderCarriesMode(t *testing.T) {
	specs := []ecs.WatcherSpec{
		{Name: "prod", Cluster: "prod-ecs", Region: "ap-southeast-2", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"},
	}
	readers := ecs.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := buildECSWatchers(context.Background(), specs, false, 0,
		&registrarAdapter{fatal: make(chan error, 1)}, readers, logger); err != nil {
		t.Fatalf("buildECSWatchers: %v", err)
	}
	got := (readerSource{readers}).Readers()
	if len(got) != 1 || got[0].Mode != "ecs" || got[0].Cluster != "prod-ecs" {
		t.Errorf("status readers = %+v, want one ecs reader for prod-ecs", got)
	}
}

func TestParseDiscoveryModes_Accepts(t *testing.T) {
	cases := map[string][]string{
		"docker":                 {"docker"},
		"ecs":                    {"ecs"},
		"LAMBDA":                 {"lambda"},
		"ecs,lambda":             {"ecs", "lambda"},
		"ecs, lambda":            {"ecs", "lambda"},
		" docker , Ecs ,lambda ": {"docker", "ecs", "lambda"},
	}
	for raw, want := range cases {
		got, err := parseDiscoveryModes(raw)
		if err != nil {
			t.Errorf("parseDiscoveryModes(%q) error: %v", raw, err)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("parseDiscoveryModes(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseDiscoveryModes_Rejects(t *testing.T) {
	cases := map[string]string{
		"kubernetes":     `unknown discovery mode "kubernetes"`,
		"ecs,consul":     `unknown discovery mode "consul"`,
		"ecs,,lambda":    `TSSERVE_DISCOVERY="ecs,,lambda" has an empty entry`,
		"ecs, ,lambda":   `TSSERVE_DISCOVERY="ecs, ,lambda" has an empty entry`,
		",ecs":           `TSSERVE_DISCOVERY=",ecs" has an empty entry`,
		"ecs,":           `TSSERVE_DISCOVERY="ecs," has an empty entry`,
		"   ":            `TSSERVE_DISCOVERY="   " has an empty entry`,
		"ecs,lambda,ECS": `discovery mode "ECS" is repeated`,
	}
	for raw, want := range cases {
		got, err := parseDiscoveryModes(raw)
		if err == nil {
			t.Errorf("parseDiscoveryModes(%q) = %q, want error", raw, got)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("parseDiscoveryModes(%q) error = %q, want it to contain %q", raw, err, want)
		}
	}
}

// fakeModes starts the given modes through startModes, handing each mode's
// report channel back to the test so it can finish that mode at will.
func fakeModes(t *testing.T, ctx context.Context, modes ...string) (map[string]chan<- error, <-chan error) {
	t.Helper()
	reports := make(map[string]chan<- error)
	watchErr, err := startModes(ctx, modes, func(mode string, modeErr chan<- error) error {
		reports[mode] = modeErr
		return nil
	})
	if err != nil {
		t.Fatalf("startModes: %v", err)
	}
	if len(reports) != len(modes) {
		t.Fatalf("started %d modes, want %d", len(reports), len(modes))
	}
	return reports, watchErr
}

func expectNothing(t *testing.T, watchErr <-chan error, why string) {
	t.Helper()
	select {
	case err := <-watchErr:
		t.Fatalf("watchErr = %v; want nothing (%s)", err, why)
	case <-time.After(100 * time.Millisecond):
	}
}

func expectValue(t *testing.T, watchErr <-chan error) error {
	t.Helper()
	select {
	case err := <-watchErr:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("watchErr received nothing")
		return nil
	}
}

func TestStartModes_ForwardsFirstErrorAtOnce(t *testing.T) {
	report, watchErr := fakeModes(t, context.Background(), "docker", "ecs", "lambda")
	boom := errors.New("docker: daemon gone")
	report["docker"] <- boom // ecs and lambda are still running
	if err := expectValue(t, watchErr); err != boom {
		t.Fatalf("watchErr = %v, want %v", err, boom)
	}
	report["ecs"] <- errors.New("second failure")
	report["lambda"] <- nil
	expectNothing(t, watchErr, "only the first error is forwarded")
}

func TestStartModes_SendsNilOnlyAfterAllFinish(t *testing.T) {
	report, watchErr := fakeModes(t, context.Background(), "ecs", "lambda")
	report["ecs"] <- context.Canceled
	expectNothing(t, watchErr, "lambda is still running")
	report["lambda"] <- nil
	if err := expectValue(t, watchErr); err != nil {
		t.Fatalf("watchErr = %v after every mode finished cleanly, want nil", err)
	}
}

func TestStartModes_SilentModeWithholdsNilButNotErrors(t *testing.T) {
	// A mode that started no readers never reports. The others finishing
	// cleanly must not produce the nil that would end the process, but an
	// error from another mode still gets through.
	report, watchErr := fakeModes(t, context.Background(), "docker", "ecs", "lambda")
	report["docker"] <- nil
	expectNothing(t, watchErr, "ecs and lambda have not reported")
	boom := errors.New("lambda: reader failed")
	report["lambda"] <- boom
	if err := expectValue(t, watchErr); err != boom {
		t.Fatalf("watchErr = %v, want %v", err, boom)
	}
}

func TestStartModes_StartErrorStopsStartup(t *testing.T) {
	var started []string
	bad := errors.New("TSSERVE_ECS_CLUSTER is required when TSSERVE_DISCOVERY includes ecs")
	_, err := startModes(context.Background(), []string{"docker", "ecs", "lambda"}, func(mode string, _ chan<- error) error {
		started = append(started, mode)
		if mode == "ecs" {
			return bad
		}
		return nil
	})
	if err != bad {
		t.Fatalf("startModes error = %v, want %v", err, bad)
	}
	if strings.Join(started, ",") != "docker,ecs" {
		t.Fatalf("started %q; modes after the failing one must not start", started)
	}
}
