package ecs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRegistry_SnapshotOrderAndCopy(t *testing.T) {
	reg := NewRegistry()
	reg.Add(ReaderStatus{Name: "a", Cluster: "c1"})
	reg.Add(ReaderStatus{Name: "b", Cluster: "c2"})

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Name != "a" || snap[1].Name != "b" {
		t.Errorf("insertion order not preserved: %+v", snap)
	}

	// Snapshot is a copy: mutating it must not affect the registry.
	snap[0].Name = "mutated"
	if reg.Snapshot()[0].Name != "a" {
		t.Errorf("snapshot should be a defensive copy")
	}
}

func TestReader_PollLifecycle(t *testing.T) {
	reg := NewRegistry()
	h := reg.Add(ReaderStatus{Name: "acct"})

	t0 := time.Unix(1000, 0)
	h.PollStarted(t0)
	h.PollSucceeded(t0.Add(time.Second))

	s := reg.Snapshot()[0]
	if !s.Healthy {
		t.Errorf("reader should be healthy after a successful poll")
	}
	if s.LastError != "" {
		t.Errorf("LastError = %q, want empty", s.LastError)
	}
	if s.Polls != 1 {
		t.Errorf("Polls = %d, want 1", s.Polls)
	}
	if !s.LastPollOK.Equal(t0.Add(time.Second)) {
		t.Errorf("LastPollOK = %v", s.LastPollOK)
	}

	// A subsequent failure marks unhealthy but preserves the last-OK timestamp.
	h.PollFailed(t0.Add(2*time.Second), errors.New("boom"))
	s = reg.Snapshot()[0]
	if s.Healthy {
		t.Errorf("reader should be unhealthy after a failed poll")
	}
	if s.LastError != "boom" {
		t.Errorf("LastError = %q, want boom", s.LastError)
	}
	if s.Polls != 2 {
		t.Errorf("Polls = %d, want 2", s.Polls)
	}
	if !s.LastPollOK.Equal(t0.Add(time.Second)) {
		t.Errorf("LastPollOK should be preserved on failure, got %v", s.LastPollOK)
	}
}

func TestReader_NoteErrorAndSetAccount(t *testing.T) {
	reg := NewRegistry()
	h := reg.Add(ReaderStatus{Name: "accounts[1]"})

	h.NoteError(errors.New("AccessDenied"))
	s := reg.Snapshot()[0]
	if s.Healthy || s.LastError != "AccessDenied" {
		t.Errorf("NoteError not applied: %+v", s)
	}
	if s.Polls != 0 {
		t.Errorf("NoteError must not count as a poll, Polls = %d", s.Polls)
	}

	h.SetAccount("077542728448")
	if reg.Snapshot()[0].Account != "077542728448" {
		t.Errorf("SetAccount not applied")
	}
}

// A nil *Reader is a valid no-op handle (single-account / test paths).
func TestReader_NilHandleIsNoop(t *testing.T) {
	var h *Reader
	h.PollStarted(time.Now())
	h.PollSucceeded(time.Now())
	h.PollFailed(time.Now(), errors.New("x"))
	h.NoteError(errors.New("x"))
	h.SetAccount("x")
}

// The watcher should report poll outcomes to its reader handle.
func TestWatcher_ReportsPollOutcome(t *testing.T) {
	reg := NewRegistry()
	h := reg.Add(ReaderStatus{Name: "acct", Cluster: "c"})

	fake := &fakeECS{listTasksErr: errors.New("AccessDenied: not authorized")}
	w, err := NewWatcher(Config{Cluster: "c", Reader: h}, fake, &fakeEC2{}, newRecordingReg(), discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	// A failing cycle should be recorded as a poll failure, not crash.
	if err := w.reportCycle(t.Context()); err == nil {
		t.Fatalf("expected cycle error")
	}
	s := reg.Snapshot()[0]
	if s.Healthy {
		t.Errorf("reader should be unhealthy after failing poll")
	}
	if s.LastError == "" {
		t.Errorf("expected LastError to be recorded")
	}

	// A subsequent healthy cycle flips it back.
	fake.listTasksErr = nil
	if err := w.reportCycle(t.Context()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	s = reg.Snapshot()[0]
	if !s.Healthy || s.LastError != "" {
		t.Errorf("reader should be healthy after successful poll: %+v", s)
	}
}

// nextInterval polls fast while healthy and backs off to the (slower) retry
// interval while degraded, never faster than the poll interval.
func TestWatcher_NextInterval(t *testing.T) {
	w, err := NewWatcher(
		Config{Cluster: "c", PollInterval: 10 * time.Second, RetryInterval: 2 * time.Minute},
		&fakeECS{}, &fakeEC2{}, newRecordingReg(), discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	w.healthy = true
	if got := w.nextInterval(); got != 10*time.Second {
		t.Errorf("healthy interval = %v, want 10s", got)
	}
	w.healthy = false
	if got := w.nextInterval(); got != 2*time.Minute {
		t.Errorf("degraded interval = %v, want 2m", got)
	}

	// A retry interval faster than the poll interval must never speed polling up.
	w2, _ := NewWatcher(
		Config{Cluster: "c", PollInterval: 5 * time.Minute, RetryInterval: time.Minute},
		&fakeECS{}, &fakeEC2{}, newRecordingReg(), discardLogger())
	w2.healthy = false
	if got := w2.nextInterval(); got != 5*time.Minute {
		t.Errorf("degraded interval = %v, want 5m (never faster than poll)", got)
	}
}

// A reader whose identity cannot be resolved yet keeps failing (and stays
// unhealthy) until resolution succeeds, at which point OnAccountResolved fires
// exactly once and normal polling proceeds.
func TestWatcher_LazyIdentityResolution(t *testing.T) {
	reg := NewRegistry()
	h := reg.Add(ReaderStatus{Name: "accounts[1]", Cluster: "c"})

	resolveErr := errors.New("AccessDenied: not authorized to perform: sts:AssumeRole")
	fail := true
	var resolvedCalls int
	var gotAccount string

	w, err := NewWatcher(Config{
		Cluster: "c",
		Reader:  h,
		ResolveAccount: func(context.Context) (string, error) {
			if fail {
				return "", resolveErr
			}
			return "077542728448", nil
		},
		OnAccountResolved: func(account string) {
			resolvedCalls++
			gotAccount = account
		},
	}, &fakeECS{}, &fakeEC2{}, newRecordingReg(), discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	// While identity is unresolvable, cycles fail and never touch ECS.
	if err := w.reportCycle(t.Context()); err == nil {
		t.Fatalf("expected cycle failure while identity unresolved")
	}
	if reg.Snapshot()[0].Healthy {
		t.Errorf("reader should be unhealthy while identity unresolved")
	}
	if resolvedCalls != 0 {
		t.Errorf("OnAccountResolved should not fire on failure, got %d calls", resolvedCalls)
	}

	// Role becomes assumable: next cycle resolves identity and proceeds.
	fail = false
	if err := w.reportCycle(t.Context()); err != nil {
		t.Fatalf("cycle after recovery: %v", err)
	}
	if resolvedCalls != 1 || gotAccount != "077542728448" {
		t.Errorf("OnAccountResolved calls=%d account=%q, want 1 / 077542728448", resolvedCalls, gotAccount)
	}
	if !reg.Snapshot()[0].Healthy {
		t.Errorf("reader should be healthy after recovery")
	}

	// A further cycle must not re-resolve (identity is sticky).
	if err := w.reportCycle(t.Context()); err != nil {
		t.Fatalf("third cycle: %v", err)
	}
	if resolvedCalls != 1 {
		t.Errorf("identity resolved more than once: %d", resolvedCalls)
	}
}
