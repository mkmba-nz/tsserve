package ecs

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// fakeECS implements ECSAPI from prefab fixtures and counts calls so tests can
// assert caching.
type fakeECS struct {
	taskArns        []string
	tasks           map[string]ecstypes.Task             // arn -> task
	taskDefs        map[string]ecstypes.TaskDefinition   // arn -> task def
	containerInsts  map[string]ecstypes.ContainerInstance // arn -> ci
	descTaskDefHits int
	descCIHits      int

	listTasksErr error
	descTasksErr error
	descTDErr    error
	descCIErr    error
}

func (f *fakeECS) ListTasks(ctx context.Context, in *awsecs.ListTasksInput, opts ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error) {
	if f.listTasksErr != nil {
		return nil, f.listTasksErr
	}
	return &awsecs.ListTasksOutput{TaskArns: append([]string{}, f.taskArns...)}, nil
}

func (f *fakeECS) DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	if f.descTasksErr != nil {
		return nil, f.descTasksErr
	}
	var out []ecstypes.Task
	for _, arn := range in.Tasks {
		if t, ok := f.tasks[arn]; ok {
			out = append(out, t)
		}
	}
	return &awsecs.DescribeTasksOutput{Tasks: out}, nil
}

func (f *fakeECS) DescribeTaskDefinition(ctx context.Context, in *awsecs.DescribeTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error) {
	f.descTaskDefHits++
	if f.descTDErr != nil {
		return nil, f.descTDErr
	}
	td, ok := f.taskDefs[aws.ToString(in.TaskDefinition)]
	if !ok {
		return nil, errors.New("task definition not found")
	}
	return &awsecs.DescribeTaskDefinitionOutput{TaskDefinition: &td}, nil
}

func (f *fakeECS) DescribeContainerInstances(ctx context.Context, in *awsecs.DescribeContainerInstancesInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeContainerInstancesOutput, error) {
	f.descCIHits++
	if f.descCIErr != nil {
		return nil, f.descCIErr
	}
	var out []ecstypes.ContainerInstance
	for _, arn := range in.ContainerInstances {
		if ci, ok := f.containerInsts[arn]; ok {
			out = append(out, ci)
		}
	}
	return &awsecs.DescribeContainerInstancesOutput{ContainerInstances: out}, nil
}

// fakeEC2 implements EC2API.
type fakeEC2 struct {
	instances    map[string]ec2types.Instance // instance id -> instance
	descInstHits int
	descInstErr  error
}

func (f *fakeEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.descInstHits++
	if f.descInstErr != nil {
		return nil, f.descInstErr
	}
	var inst []ec2types.Instance
	for _, id := range in.InstanceIds {
		if i, ok := f.instances[id]; ok {
			inst = append(inst, i)
		}
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: inst}},
	}, nil
}

// helpers to build fixtures concisely

func strp(s string) *string { return &s }
func int32p(i int32) *int32 { return &i }
