package aws

import (
	"context"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Cluster is a discovered main queue plus its resolved ASG.
type Cluster struct {
	QueueURL  string // main queue url
	QueueName string // basename of QueueURL
	ClusterID string // QueueName without the -queue suffix
	DLQURL    string // resolved DLQ url ("" if none)
	ASG       string // resolved ASG physical name ("" if none)
}

// ListQueues returns all queue URLs matching the prefix (main + dlq mixed).
func (c *Clients) ListQueues(ctx context.Context, prefix string) ([]string, error) {
	var urls []string
	p := sqs.NewListQueuesPaginator(c.SQS, &sqs.ListQueuesInput{
		QueueNamePrefix: ptr(prefix),
		MaxResults:      ptr(int32(1000)),
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		urls = append(urls, out.QueueUrls...)
	}
	return urls, nil
}

// asgNamesByTag returns ASG physical names tagged Project=projectTag.
// One paginated call, then local matching — avoids scanning every ASG in the
// account (EKS node groups etc.) on each collection cycle.
func (c *Clients) asgNamesByTag(ctx context.Context, projectTag string) ([]string, error) {
	var names []string
	p := autoscaling.NewDescribeAutoScalingGroupsPaginator(c.ASG, &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []types.Filter{{Name: ptr("tag:Project"), Values: []string{projectTag}}},
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range out.AutoScalingGroups {
			if g.AutoScalingGroupName != nil {
				names = append(names, *g.AutoScalingGroupName)
			}
		}
	}
	return names, nil
}

// Discover finds main queues (excluding -dlq) matching prefix, pairs each with
// its DLQ and ASG. ASG match rule: the ASG name contains the cluster id
// (compatible with the <id>-AgentASG-xxx CFN naming).
func (c *Clients) Discover(ctx context.Context, prefix, projectTag string) ([]Cluster, error) {
	urls, err := c.ListQueues(ctx, prefix)
	if err != nil {
		return nil, err
	}
	asgNames, err := c.asgNamesByTag(ctx, projectTag)
	if err != nil {
		// ASG discovery failing is non-fatal — queues still show, ASG cols blank.
		asgNames = nil
	}

	// Index basenames for DLQ pairing.
	byName := map[string]string{} // basename -> url
	for _, u := range urls {
		byName[path.Base(u)] = u
	}

	var clusters []Cluster
	for _, u := range urls {
		name := path.Base(u)
		if strings.HasSuffix(name, "-dlq") {
			continue
		}
		cid := strings.TrimSuffix(name, "-queue")
		cl := Cluster{QueueURL: u, QueueName: name, ClusterID: cid}
		// DLQ candidates: <url>-dlq or <id>-queue-dlq.
		for _, cand := range []string{name + "-dlq", cid + "-queue-dlq"} {
			if durl, ok := byName[cand]; ok {
				cl.DLQURL = durl
				break
			}
		}
		// ASG: first tagged ASG name containing the cluster id.
		for _, an := range asgNames {
			if strings.Contains(an, cid) {
				cl.ASG = an
				break
			}
		}
		clusters = append(clusters, cl)
	}
	return clusters, nil
}

// ResolveASG returns the (asg, short) for a filter substring, matching against
// tagged ASG names. Used by the ps/top/refresh subcommands.
func (c *Clients) ResolveASG(ctx context.Context, prefix, projectTag, filter string) (asg, short string, err error) {
	names, err := c.asgNamesByTag(ctx, projectTag)
	if err != nil {
		return "", "", err
	}
	for _, an := range names {
		if strings.Contains(an, filter) {
			short = strings.TrimPrefix(an, prefix+"-")
			if i := strings.Index(short, "-AgentASG-"); i >= 0 {
				short = short[:i]
			}
			return an, short, nil
		}
	}
	return "", "", nil
}
