package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
}
