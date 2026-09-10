package ecs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"mkmba.nz/tsserve/labels"
)

type recordingReg struct {
	registered   map[string]string // key -> "host:port"
	deregistered []string
}

func newRecordingReg() *recordingReg {
	return &recordingReg{registered: map[string]string{}}
}

func (r *recordingReg) Register(key string, def *labels.ServiceDef, backendIP string) error {
	r.registered[key] = backendIP
	return nil
}

func (r *recordingReg) Deregister(key string) {
	r.deregistered = append(r.deregistered, key)
	delete(r.registered, key)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCycle_RegistersAndDeregisters exercises one polling cycle end-to-end with
// a fake ECS/EC2 backend covering a mix of bridge and awsvpc tasks, then a
// second cycle where one task disappears.
func TestCycle_RegistersAndDeregisters(t *testing.T) {
	ctx := context.Background()

	bridgeArn := "arn:aws:ecs:us-east-1:111:task/c/bridge"
	awsvpcArn := "arn:aws:ecs:us-east-1:111:task/c/awsvpc"
	bridgeTDArn := "arn:aws:ecs:us-east-1:111:task-definition/bridge-api:1"
	awsvpcTDArn := "arn:aws:ecs:us-east-1:111:task-definition/awsvpc-api:1"
	ciArn := "arn:aws:ecs:us-east-1:111:container-instance/c/ci"
	ec2ID := "i-0123"

	ecs := &fakeECS{
		taskArns: []string{bridgeArn, awsvpcArn},
		tasks: map[string]ecstypes.Task{
			bridgeArn: {
				TaskArn:              strp(bridgeArn),
				TaskDefinitionArn:    strp(bridgeTDArn),
				LastStatus:           strp("RUNNING"),
				ContainerInstanceArn: strp(ciArn),
				Containers: []ecstypes.Container{{
					Name: strp("api"),
					NetworkBindings: []ecstypes.NetworkBinding{{
						ContainerPort: int32p(3000),
						HostPort:      int32p(32768),
					}},
				}},
			},
			awsvpcArn: {
				TaskArn:           strp(awsvpcArn),
				TaskDefinitionArn: strp(awsvpcTDArn),
				LastStatus:        strp("RUNNING"),
				Attachments: []ecstypes.Attachment{{
					Type: strp("ElasticNetworkInterface"),
					Details: []ecstypes.KeyValuePair{
						{Name: strp("privateIPv4Address"), Value: strp("10.0.5.7")},
					},
				}},
			},
		},
		taskDefs: map[string]ecstypes.TaskDefinition{
			bridgeTDArn: {
				NetworkMode: ecstypes.NetworkModeBridge,
				ContainerDefinitions: []ecstypes.ContainerDefinition{{
					Name: strp("api"),
					DockerLabels: map[string]string{
						"tsserve.enable":  "true",
						"tsserve.service": "svc:bridge-api",
						"tsserve.port":    "3000",
					},
				}},
			},
			awsvpcTDArn: {
				NetworkMode: ecstypes.NetworkModeAwsvpc,
				ContainerDefinitions: []ecstypes.ContainerDefinition{{
					Name: strp("api"),
					DockerLabels: map[string]string{
						"tsserve.enable":  "true",
						"tsserve.service": "svc:awsvpc-api",
						"tsserve.port":    "3000",
					},
				}},
			},
		},
		containerInsts: map[string]ecstypes.ContainerInstance{
			ciArn: {Ec2InstanceId: strp(ec2ID)},
		},
	}
	ec2 := &fakeEC2{instances: map[string]ec2types.Instance{
		ec2ID: {PrivateIpAddress: strp("10.0.1.42")},
	}}

	reg := newRecordingReg()
	w, err := NewWatcher(Config{Cluster: "c"}, ecs, ec2, reg, discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	if err := w.cycle(ctx); err != nil {
		t.Fatalf("first cycle: %v", err)
	}

	wantBridgeKey := registrationKey(bridgeArn, "api")
	wantAwsvpcKey := registrationKey(awsvpcArn, "api")
	if reg.registered[wantBridgeKey] != "10.0.1.42" {
		t.Errorf("bridge backend host: got %q, want 10.0.1.42", reg.registered[wantBridgeKey])
	}
	if reg.registered[wantAwsvpcKey] != "10.0.5.7" {
		t.Errorf("awsvpc backend host: got %q, want 10.0.5.7", reg.registered[wantAwsvpcKey])
	}
	if len(reg.deregistered) != 0 {
		t.Errorf("unexpected deregistrations: %v", reg.deregistered)
	}

	// Second cycle: bridge task is gone. Watcher should deregister it but leave awsvpc alone.
	ecs.taskArns = []string{awsvpcArn}
	delete(ecs.tasks, bridgeArn)

	if err := w.cycle(ctx); err != nil {
		t.Fatalf("second cycle: %v", err)
	}

	if _, still := reg.registered[wantBridgeKey]; still {
		t.Errorf("bridge task should have been deregistered")
	}
	if reg.registered[wantAwsvpcKey] != "10.0.5.7" {
		t.Errorf("awsvpc task should still be registered")
	}
	if len(reg.deregistered) != 1 || reg.deregistered[0] != wantBridgeKey {
		t.Errorf("deregistrations: got %v, want [%s]", reg.deregistered, wantBridgeKey)
	}

	// Caches: second cycle should not have re-fetched task defs or container instance.
	if ecs.descTaskDefHits != 2 {
		t.Errorf("task def lookups: got %d, want 2 (one per task def, once total)", ecs.descTaskDefHits)
	}
	if ecs.descCIHits != 1 {
		t.Errorf("container instance lookups: got %d, want 1 (cached after first cycle)", ecs.descCIHits)
	}
}

// TestCycle_NonRunningTaskSkipped verifies tasks not in RUNNING state are ignored.
func TestCycle_NonRunningTaskSkipped(t *testing.T) {
	ctx := context.Background()
	arn := "arn:t"
	tdArn := "arn:td"

	ecs := &fakeECS{
		taskArns: []string{arn},
		tasks: map[string]ecstypes.Task{
			arn: {
				TaskArn:           strp(arn),
				TaskDefinitionArn: strp(tdArn),
				LastStatus:        strp("PENDING"),
			},
		},
		taskDefs: map[string]ecstypes.TaskDefinition{
			tdArn: {
				NetworkMode: ecstypes.NetworkModeAwsvpc,
				ContainerDefinitions: []ecstypes.ContainerDefinition{{
					Name:         strp("api"),
					DockerLabels: map[string]string{"tsserve.enable": "true", "tsserve.service": "svc:x", "tsserve.port": "80"},
				}},
			},
		},
	}

	reg := newRecordingReg()
	w, _ := NewWatcher(Config{Cluster: "c"}, ecs, &fakeEC2{}, reg, discardLogger())
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(reg.registered) != 0 {
		t.Fatalf("non-running task should be skipped, got registrations: %v", reg.registered)
	}
}

// failingReg fails Register with a configurable error and counts the calls per
// key, so tests can prove a failed key is retried on the next cycle.
type failingReg struct {
	recordingReg
	err      error
	attempts map[string]int
}

func newFailingReg(err error) *failingReg {
	return &failingReg{
		recordingReg: *newRecordingReg(),
		err:          err,
		attempts:     map[string]int{},
	}
}

func (r *failingReg) Register(key string, def *labels.ServiceDef, backendIP string) error {
	r.attempts[key]++
	if r.err != nil {
		return r.err
	}
	return r.recordingReg.Register(key, def, backendIP)
}

// oneTaskFake builds a fake ECS API serving a single awsvpc task with one
// labelled container, and returns it with the registration key that task
// produces.
func oneTaskFake() (*fakeECS, string) {
	const arn = "arn:aws:ecs:us-east-1:111:task/c/only"
	const tdArn = "arn:aws:ecs:us-east-1:111:task-definition/only:1"
	f := &fakeECS{
		taskArns: []string{arn},
		tasks: map[string]ecstypes.Task{
			arn: {
				TaskArn:           strp(arn),
				TaskDefinitionArn: strp(tdArn),
				LastStatus:        strp("RUNNING"),
				Attachments: []ecstypes.Attachment{{
					Type: strp("ElasticNetworkInterface"),
					Details: []ecstypes.KeyValuePair{
						{Name: strp("privateIPv4Address"), Value: strp("10.0.5.7")},
					},
				}},
			},
		},
		taskDefs: map[string]ecstypes.TaskDefinition{
			tdArn: {
				NetworkMode: ecstypes.NetworkModeAwsvpc,
				ContainerDefinitions: []ecstypes.ContainerDefinition{{
					Name: strp("api"),
					DockerLabels: map[string]string{
						"tsserve.enable":  "true",
						"tsserve.service": "svc:api",
						"tsserve.port":    "3000",
					},
				}},
			},
		},
	}
	return f, registrationKey(arn, "api")
}

// A key whose Register failed is not recorded as active, so the next cycle
// registers it again rather than short-circuiting on an unchanged backend
// address.
func TestCycle_FailedRegisterIsNotActiveAndIsRetried(t *testing.T) {
	ctx := context.Background()
	ecsAPI, key := oneTaskFake()
	reg := newFailingReg(errors.New("ListenService(svc:api): transient"))

	w, err := NewWatcher(Config{Cluster: "c"}, ecsAPI, &fakeEC2{}, reg, discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	if err := w.cycle(ctx); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if _, ok := w.active[key]; ok {
		t.Errorf("key recorded active despite Register failing: %v", w.active)
	}
	if reg.attempts[key] != 1 {
		t.Fatalf("Register attempts = %d, want 1", reg.attempts[key])
	}

	if err := w.cycle(ctx); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if reg.attempts[key] != 2 {
		t.Errorf("Register attempts = %d after two cycles, want 2 (a failed key is retried)", reg.attempts[key])
	}

	// Third cycle succeeds: the key joins the pool and is recorded active.
	reg.err = nil
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("third cycle: %v", err)
	}
	if got := w.active[key]; got != "10.0.5.7:3000" {
		t.Errorf("active[%s] = %q, want 10.0.5.7:3000 after the successful cycle", key, got)
	}
	if _, still := w.failed[key]; still {
		t.Errorf("failure state kept after success: %v", w.failed)
	}
}

// The same failure repeating across cycles is logged at Warn once, not once per
// poll interval.
func TestCycle_RepeatedRegisterFailureWarnsOnce(t *testing.T) {
	ctx := context.Background()
	ecsAPI, key := oneTaskFake()
	reg := newFailingReg(errors.New("backend cid2 refused for service svc:api: scheme conflict"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, err := NewWatcher(Config{Cluster: "c"}, ecsAPI, &fakeEC2{}, reg, logger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := w.cycle(ctx); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if reg.attempts[key] != 3 {
		t.Fatalf("Register attempts = %d, want 3", reg.attempts[key])
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want exactly 1 across three failing cycles\n--- log ---\n%s", n, out)
	}
	if n := strings.Count(out, "level=ERROR"); n != 0 {
		t.Errorf("ERROR lines = %d, want 0\n--- log ---\n%s", n, out)
	}
}

// A key that stops being seen has its failure state forgotten, so the map does
// not grow one entry per task that ever failed.
func TestCycle_FailureStateDroppedWhenTaskDisappears(t *testing.T) {
	ctx := context.Background()
	ecsAPI, key := oneTaskFake()
	reg := newFailingReg(errors.New("transient"))

	w, err := NewWatcher(Config{Cluster: "c"}, ecsAPI, &fakeEC2{}, reg, discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if _, ok := w.failed[key]; !ok {
		t.Fatalf("failure not recorded: %v", w.failed)
	}

	ecsAPI.taskArns = nil
	ecsAPI.tasks = map[string]ecstypes.Task{}
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if len(w.failed) != 0 {
		t.Errorf("failure state = %v, want empty once the task is gone", w.failed)
	}
}

// When a backend moves and the re-registration fails, the key is left out of
// active — the old address must not be reported as still serving.
func TestCycle_FailedReRegisterAfterBackendMoveClearsActive(t *testing.T) {
	ctx := context.Background()
	ecsAPI, key := oneTaskFake()
	reg := newFailingReg(nil)

	w, err := NewWatcher(Config{Cluster: "c"}, ecsAPI, &fakeEC2{}, reg, discardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if got := w.active[key]; got != "10.0.5.7:3000" {
		t.Fatalf("active[%s] = %q, want 10.0.5.7:3000", key, got)
	}

	// The task's ENI address changes and the re-registration fails.
	task := ecsAPI.tasks["arn:aws:ecs:us-east-1:111:task/c/only"]
	task.Attachments[0].Details[0].Value = strp("10.0.9.9")
	ecsAPI.tasks["arn:aws:ecs:us-east-1:111:task/c/only"] = task
	reg.err = errors.New("ListenService(svc:api): transient")

	if err := w.cycle(ctx); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got, ok := w.active[key]; ok {
		t.Errorf("active[%s] = %q, want the key absent: it was deregistered and its re-registration failed", key, got)
	}
	if n := len(reg.deregistered); n != 1 {
		t.Errorf("deregistrations = %d, want 1", n)
	}
}

// The Warn-once demotion is scoped to one outcome. A key that starts failing a
// different way is a new fact for an operator — a refused conflict giving way
// to a listen failure means the misconfiguration was fixed and something else
// is now wrong — so it gets its own Warn rather than staying demoted to Debug.
func TestCycle_ChangedRegisterFailureWarnsAgain(t *testing.T) {
	ctx := context.Background()
	ecsAPI, key := oneTaskFake()
	conflict := errors.New("backend cid2 refused for service svc:api: scheme conflict")
	reg := newFailingReg(conflict)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, err := NewWatcher(Config{Cluster: "c"}, ecsAPI, &fakeEC2{}, reg, logger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	// Two cycles failing the same way: one Warn, then demoted.
	for i := 0; i < 2; i++ {
		if err := w.cycle(ctx); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("WARN lines = %d after two identical failures, want 1\n--- log ---\n%s", n, buf.String())
	}

	// The outcome changes; the new one must be surfaced.
	reg.err = errors.New("listen: a Service handler already exists for this port")
	if err := w.cycle(ctx); err != nil {
		t.Fatalf("third cycle: %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Errorf("WARN lines = %d, want 2 — the changed failure must not stay demoted\n--- log ---\n%s", n, out)
	}
	if !strings.Contains(out, "a Service handler already exists") {
		t.Errorf("changed failure not reported at Warn\n--- log ---\n%s", out)
	}
	if reg.attempts[key] != 3 {
		t.Errorf("Register attempts = %d, want 3", reg.attempts[key])
	}
}
