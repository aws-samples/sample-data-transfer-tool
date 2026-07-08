package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// InstanceCount returns the total instance count of an ASG (for the refresh
// confirmation prompt).
func (c *Clients) InstanceCount(ctx context.Context, asg string) int {
	out, err := c.ASG.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asg},
	})
	if err != nil || len(out.AutoScalingGroups) == 0 {
		return 0
	}
	return len(out.AutoScalingGroups[0].Instances)
}

// StartRefresh issues a rolling instance refresh. minHealthy is the minimum
// healthy percentage kept during the roll; warmup is per-instance seconds.
func (c *Clients) StartRefresh(ctx context.Context, asg string, minHealthy, warmup int32) (string, error) {
	out, err := c.ASG.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: ptr(asg),
		Preferences: &astypes.RefreshPreferences{
			MinHealthyPercentage: ptr(minHealthy),
			InstanceWarmup:       ptr(warmup),
		},
	})
	if err != nil {
		return "", err
	}
	if out.InstanceRefreshId == nil {
		return "", nil
	}
	return *out.InstanceRefreshId, nil
}
