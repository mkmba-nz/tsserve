package ecs

import (
	"context"
	"io"
	"log/slog"
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
