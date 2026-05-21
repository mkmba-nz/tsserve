package ecs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ECSAPI is the subset of the AWS ECS client used by the watcher. Defined here
// so tests can substitute a fake.
type ECSAPI interface {
	ListTasks(ctx context.Context, in *awsecs.ListTasksInput, opts ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
	DescribeTaskDefinition(ctx context.Context, in *awsecs.DescribeTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error)
	DescribeContainerInstances(ctx context.Context, in *awsecs.DescribeContainerInstancesInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeContainerInstancesOutput, error)
}

// EC2API is the subset of the AWS EC2 client used by the watcher.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// taskDefCache memoises DescribeTaskDefinition. Task definitions are immutable
// per ARN, so entries never expire.
type taskDefCache struct {
	mu      sync.Mutex
	entries map[string]*ecstypes.TaskDefinition
}

func newTaskDefCache() *taskDefCache {
	return &taskDefCache{entries: map[string]*ecstypes.TaskDefinition{}}
}

// get returns the task definition for arn, calling the API on cache miss.
func (c *taskDefCache) get(ctx context.Context, api ECSAPI, arn string) (*ecstypes.TaskDefinition, error) {
	c.mu.Lock()
	if td, ok := c.entries[arn]; ok {
		c.mu.Unlock()
		return td, nil
	}
	c.mu.Unlock()

	out, err := api.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{
		TaskDefinition: &arn,
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTaskDefinition(%s): %w", arn, err)
	}
	if out.TaskDefinition == nil {
		return nil, fmt.Errorf("DescribeTaskDefinition(%s): empty response", arn)
	}

	c.mu.Lock()
	c.entries[arn] = out.TaskDefinition
	c.mu.Unlock()
	return out.TaskDefinition, nil
}

// hostIPCache maps a container-instance ARN to its EC2 host's private IPv4.
// Host IPs are stable for the lifetime of the EC2 instance; on lookup failure
// the cached entry is dropped so the next call retries cleanly.
type hostIPCache struct {
	mu      sync.Mutex
	entries map[string]string // containerInstanceArn -> privateIP
}

func newHostIPCache() *hostIPCache {
	return &hostIPCache{entries: map[string]string{}}
}

func (c *hostIPCache) get(ctx context.Context, ecsAPI ECSAPI, ec2API EC2API, cluster, ciArn string) (string, error) {
	c.mu.Lock()
	if ip, ok := c.entries[ciArn]; ok {
		c.mu.Unlock()
		return ip, nil
	}
	c.mu.Unlock()

	ip, err := lookupHostIP(ctx, ecsAPI, ec2API, cluster, ciArn)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.entries[ciArn] = ip
	c.mu.Unlock()
	return ip, nil
}

// invalidate drops the cached host IP for ciArn so the next lookup re-queries.
func (c *hostIPCache) invalidate(ciArn string) {
	c.mu.Lock()
	delete(c.entries, ciArn)
	c.mu.Unlock()
}

func lookupHostIP(ctx context.Context, ecsAPI ECSAPI, ec2API EC2API, cluster, ciArn string) (string, error) {
	ciOut, err := ecsAPI.DescribeContainerInstances(ctx, &awsecs.DescribeContainerInstancesInput{
		Cluster:            &cluster,
		ContainerInstances: []string{ciArn},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeContainerInstances(%s): %w", ciArn, err)
	}
	if len(ciOut.ContainerInstances) == 0 {
		return "", fmt.Errorf("container instance %s not found", ciArn)
	}
	ec2ID := ciOut.ContainerInstances[0].Ec2InstanceId
	if ec2ID == nil || *ec2ID == "" {
		return "", fmt.Errorf("container instance %s has no EC2 instance id", ciArn)
	}

	instOut, err := ec2API.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{*ec2ID},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeInstances(%s): %w", *ec2ID, err)
	}
	for _, r := range instOut.Reservations {
		for _, i := range r.Instances {
			if ip := privateIP(i); ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("EC2 instance %s has no private IP", *ec2ID)
}

func privateIP(i ec2types.Instance) string {
	if i.PrivateIpAddress != nil && *i.PrivateIpAddress != "" {
		return *i.PrivateIpAddress
	}
	return ""
}

// errNoBackend is returned by the resolver when a backend address cannot be
// determined yet (e.g. an awsvpc task with no ENI attachment, or a bridge task
// with no matching networkBinding). Callers should log and retry on the next
// poll cycle rather than treating it as a hard failure.
var errNoBackend = errors.New("backend not yet resolvable")
