package aws

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// latestSum returns the most recent 1-min Sum datapoint for a metric, or NaN.
func (c *Clients) latestSum(ctx context.Context, ns, metric, dimName, dimVal string) float64 {
	return c.latest(ctx, ns, metric, dimName, dimVal, cwtypes.StatisticSum)
}

func (c *Clients) latestAvg(ctx context.Context, ns, metric, dimName, dimVal string) float64 {
	return c.latest(ctx, ns, metric, dimName, dimVal, cwtypes.StatisticAverage)
}

func (c *Clients) latest(ctx context.Context, ns, metric, dimName, dimVal string, stat cwtypes.Statistic) float64 {
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	out, err := c.CW.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  ptr(ns),
		MetricName: ptr(metric),
		Dimensions: []cwtypes.Dimension{{Name: ptr(dimName), Value: ptr(dimVal)}},
		StartTime:  aws.Time(start),
		EndTime:    aws.Time(end),
		Period:     ptr(int32(60)),
		Statistics: []cwtypes.Statistic{stat},
	})
	if err != nil || len(out.Datapoints) == 0 {
		return nan()
	}
	dps := out.Datapoints
	sort.Slice(dps, func(i, j int) bool { return dps[i].Timestamp.Before(*dps[j].Timestamp) })
	last := dps[len(dps)-1]
	switch stat {
	case cwtypes.StatisticSum:
		if last.Sum != nil {
			return *last.Sum
		}
	case cwtypes.StatisticAverage:
		if last.Average != nil {
			return *last.Average
		}
	}
	return nan()
}

// QueueRates returns (recv/s, done/s) as formatted integer strings.
func (c *Clients) QueueRates(ctx context.Context, queueName string) (recv, done string) {
	r := c.latestSum(ctx, "AWS/SQS", "NumberOfMessagesReceived", "QueueName", queueName)
	d := c.latestSum(ctx, "AWS/SQS", "NumberOfMessagesDeleted", "QueueName", queueName)
	return perSec(r), perSec(d)
}

// ASGGbps returns NetworkIn/Out as Gbps strings ("-" if unavailable).
// period-accumulated bytes ÷ 60 × 8 ÷ 1e9.
func (c *Clients) ASGGbps(ctx context.Context, asg, metric string) string {
	if asg == "" {
		return "-"
	}
	v := c.latestSum(ctx, "AWS/EC2", metric, "AutoScalingGroupName", asg)
	if isNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.2f", v/60*8/1e9)
}

// ASGCPUPct returns average CPU% for the ASG. Tries ASG-aggregated dimension
// first; falls back to averaging per-instance InstanceId datapoints (CWAgent
// often only reports per-instance). "-" if nothing available.
func (c *Clients) ASGCPUPct(ctx context.Context, asg string, instanceIDs []string) string {
	if asg == "" {
		return "-"
	}
	if v := c.latestAvg(ctx, "AWS/EC2", "CPUUtilization", "AutoScalingGroupName", asg); !isNaN(v) {
		return fmt.Sprintf("%.0f", v)
	}
	var sum float64
	var n int
	for _, id := range instanceIDs {
		if v := c.latestAvg(ctx, "AWS/EC2", "CPUUtilization", "InstanceId", id); !isNaN(v) {
			sum += v
			n++
		}
	}
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", sum/float64(n))
}

func perSec(sum float64) string {
	if isNaN(sum) {
		return "0"
	}
	return fmt.Sprintf("%.0f", sum/60)
}

// small NaN helpers without importing math everywhere.
func nan() float64      { return nanVal }
func isNaN(f float64) bool { return f != f }

var nanVal = func() float64 { z := 0.0; return z / z }()
