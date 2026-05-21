package ecs

import (
	"context"
	"errors"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func TestTaskDefCache_HitAndMiss(t *testing.T) {
	ctx := context.Background()
	arn := "arn:aws:ecs:us-east-1:111:task-definition/myapi:7"
	ecs := &fakeECS{
		taskDefs: map[string]ecstypes.TaskDefinition{
			arn: {Family: strp("myapi"), Revision: 7},
		},
	}
	c := newTaskDefCache()

	first, err := c.get(ctx, ecs, arn)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if first.Revision != 7 {
		t.Fatalf("revision: got %d, want 7", first.Revision)
	}
	if ecs.descTaskDefHits != 1 {
		t.Fatalf("api calls after first get: got %d, want 1", ecs.descTaskDefHits)
	}

	second, err := c.get(ctx, ecs, arn)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if second != first {
		t.Fatal("second get returned a different pointer; expected cached")
	}
	if ecs.descTaskDefHits != 1 {
		t.Fatalf("api calls after second get: got %d, want still 1", ecs.descTaskDefHits)
	}
}

func TestHostIPCache_HitAndInvalidate(t *testing.T) {
	ctx := context.Background()
	ciArn := "arn:aws:ecs:us-east-1:111:container-instance/cluster/abc"
	ec2ID := "i-0123"

	ecs := &fakeECS{
		containerInsts: map[string]ecstypes.ContainerInstance{
			ciArn: {
				ContainerInstanceArn: strp(ciArn),
				Ec2InstanceId:        strp(ec2ID),
			},
		},
	}
	ec2 := &fakeEC2{
		instances: map[string]ec2types.Instance{
			ec2ID: {PrivateIpAddress: strp("10.0.1.42")},
		},
	}
	c := newHostIPCache()

	ip, err := c.get(ctx, ecs, ec2, "cluster", ciArn)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if ip != "10.0.1.42" {
		t.Fatalf("ip: got %q, want 10.0.1.42", ip)
	}
	if ecs.descCIHits != 1 || ec2.descInstHits != 1 {
		t.Fatalf("first get hits: ecs=%d ec2=%d", ecs.descCIHits, ec2.descInstHits)
	}

	ip2, err := c.get(ctx, ecs, ec2, "cluster", ciArn)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if ip2 != ip {
		t.Fatalf("cached ip: got %q, want %q", ip2, ip)
	}
	if ecs.descCIHits != 1 || ec2.descInstHits != 1 {
		t.Fatalf("second get must not hit AWS: ecs=%d ec2=%d", ecs.descCIHits, ec2.descInstHits)
	}

	c.invalidate(ciArn)
	if _, err := c.get(ctx, ecs, ec2, "cluster", ciArn); err != nil {
		t.Fatalf("post-invalidate get: %v", err)
	}
	if ecs.descCIHits != 2 || ec2.descInstHits != 2 {
		t.Fatalf("post-invalidate hits: ecs=%d ec2=%d", ecs.descCIHits, ec2.descInstHits)
	}
}

func TestHostIPCache_PropagatesErrors(t *testing.T) {
	ctx := context.Background()
	ciArn := "arn:aws:ecs:us-east-1:111:container-instance/cluster/abc"

	ecs := &fakeECS{descCIErr: errors.New("boom")}
	ec2 := &fakeEC2{}
	c := newHostIPCache()

	if _, err := c.get(ctx, ecs, ec2, "cluster", ciArn); err == nil {
		t.Fatal("expected error from ECS lookup, got nil")
	}
}
