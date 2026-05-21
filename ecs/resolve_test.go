package ecs

import (
	"context"
	"errors"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func TestResolveBackend_AwsvpcSuccess(t *testing.T) {
	ctx := context.Background()
	task := ecstypes.Task{
		TaskArn: strp("arn:aws:ecs:us-east-1:111:task/c/abc"),
		Attachments: []ecstypes.Attachment{{
			Type: strp("ElasticNetworkInterface"),
			Details: []ecstypes.KeyValuePair{
				{Name: strp("privateIPv4Address"), Value: strp("10.0.5.7")},
				{Name: strp("subnetId"), Value: strp("subnet-xxx")},
			},
		}},
	}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeAwsvpc}

	be, err := resolveBackend(ctx, &fakeECS{}, &fakeEC2{}, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if be.host != "10.0.5.7" || be.port != 3000 {
		t.Fatalf("got %s:%d, want 10.0.5.7:3000", be.host, be.port)
	}
}

func TestResolveBackend_AwsvpcMissingENI(t *testing.T) {
	ctx := context.Background()
	task := ecstypes.Task{TaskArn: strp("arn:t")}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeAwsvpc}

	_, err := resolveBackend(ctx, &fakeECS{}, &fakeEC2{}, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if !errors.Is(err, errNoBackend) {
		t.Fatalf("err = %v, want errNoBackend", err)
	}
}

func TestResolveBackend_BridgeSuccess(t *testing.T) {
	ctx := context.Background()
	ciArn := "arn:aws:ecs:us-east-1:111:container-instance/c/ci"
	ec2ID := "i-0123"

	task := ecstypes.Task{
		TaskArn:              strp("arn:t"),
		ContainerInstanceArn: strp(ciArn),
		Containers: []ecstypes.Container{{
			Name: strp("api"),
			NetworkBindings: []ecstypes.NetworkBinding{{
				ContainerPort: int32p(3000),
				HostPort:      int32p(32768),
			}},
		}},
	}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeBridge}

	ecs := &fakeECS{
		containerInsts: map[string]ecstypes.ContainerInstance{
			ciArn: {Ec2InstanceId: strp(ec2ID)},
		},
	}
	ec2 := &fakeEC2{
		instances: map[string]ec2types.Instance{
			ec2ID: {PrivateIpAddress: strp("10.0.1.42")},
		},
	}

	be, err := resolveBackend(ctx, ecs, ec2, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if be.host != "10.0.1.42" || be.port != 32768 {
		t.Fatalf("got %s:%d, want 10.0.1.42:32768", be.host, be.port)
	}
}

func TestResolveBackend_BridgeMissingNetworkBinding(t *testing.T) {
	ctx := context.Background()
	task := ecstypes.Task{
		TaskArn:              strp("arn:t"),
		ContainerInstanceArn: strp("arn:ci"),
		Containers:           []ecstypes.Container{{Name: strp("api")}},
	}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeBridge}

	_, err := resolveBackend(ctx, &fakeECS{}, &fakeEC2{}, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if !errors.Is(err, errNoBackend) {
		t.Fatalf("err = %v, want errNoBackend", err)
	}
}

func TestResolveBackend_BridgeNoContainerInstance(t *testing.T) {
	ctx := context.Background()
	task := ecstypes.Task{
		TaskArn: strp("arn:t"),
		Containers: []ecstypes.Container{{
			Name: strp("api"),
			NetworkBindings: []ecstypes.NetworkBinding{{
				ContainerPort: int32p(3000),
				HostPort:      int32p(32768),
			}},
		}},
	}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeBridge}

	_, err := resolveBackend(ctx, &fakeECS{}, &fakeEC2{}, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if !errors.Is(err, errNoBackend) {
		t.Fatalf("err = %v, want errNoBackend", err)
	}
}

func TestResolveBackend_HostMode(t *testing.T) {
	ctx := context.Background()
	ciArn := "arn:aws:ecs:us-east-1:111:container-instance/c/ci"
	ec2ID := "i-0123"

	task := ecstypes.Task{
		TaskArn:              strp("arn:t"),
		ContainerInstanceArn: strp(ciArn),
	}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkModeHost}

	ecs := &fakeECS{
		containerInsts: map[string]ecstypes.ContainerInstance{
			ciArn: {Ec2InstanceId: strp(ec2ID)},
		},
	}
	ec2 := &fakeEC2{
		instances: map[string]ec2types.Instance{
			ec2ID: {PrivateIpAddress: strp("10.0.1.42")},
		},
	}

	be, err := resolveBackend(ctx, ecs, ec2, newHostIPCache(),
		"cluster", task, td, "api", 8080)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if be.host != "10.0.1.42" || be.port != 8080 {
		t.Fatalf("got %s:%d, want 10.0.1.42:8080", be.host, be.port)
	}
}

func TestResolveBackend_UnknownNetworkMode(t *testing.T) {
	ctx := context.Background()
	task := ecstypes.Task{TaskArn: strp("arn:t")}
	td := &ecstypes.TaskDefinition{NetworkMode: ecstypes.NetworkMode("none")}

	_, err := resolveBackend(ctx, &fakeECS{}, &fakeEC2{}, newHostIPCache(),
		"cluster", task, td, "api", 3000)
	if err == nil || errors.Is(err, errNoBackend) {
		t.Fatalf("expected hard error for unknown mode, got %v", err)
	}
}
