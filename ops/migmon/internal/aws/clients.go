// Package aws wraps the AWS SDK v2 calls migmon needs, one file per service.
// The UI never imports the SDK directly — it goes through model.Collect which
// calls these. Credentials use the SDK default chain (IMDS on EC2, env, or
// ~/.aws), so the same binary works on a bastion or a worker host unchanged.
package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Clients bundles the service clients so collection code takes one dependency.
type Clients struct {
	Region string
	SQS    *sqs.Client
	CW     *cloudwatch.Client
	ASG    *autoscaling.Client
	SSM    *ssm.Client
	EC2    *ec2.Client
}

// New builds all service clients from the default config for the given region.
func New(ctx context.Context, region string) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &Clients{
		Region: region,
		SQS:    sqs.NewFromConfig(cfg),
		CW:     cloudwatch.NewFromConfig(cfg),
		ASG:    autoscaling.NewFromConfig(cfg),
		SSM:    ssm.NewFromConfig(cfg),
		EC2:    ec2.NewFromConfig(cfg),
	}, nil
}

// ptr is a tiny helper for the many *string / *int32 SDK fields.
func ptr[T any](v T) *T { return &v }

// aws.String etc. are also available; keep our own ptr for non-string types.
var _ = aws.String
