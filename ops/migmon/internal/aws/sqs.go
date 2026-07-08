package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// QueueDepth is the near-realtime SQS depth (cheap, no CloudWatch delay).
type QueueDepth struct {
	Pending  string // ApproximateNumberOfMessages
	InFlight string // ApproximateNumberOfMessagesNotVisible
}

// Depth reads pending + in-flight for one queue. Missing attrs → "?".
func (c *Clients) Depth(ctx context.Context, queueURL string) QueueDepth {
	d := QueueDepth{Pending: "?", InFlight: "?"}
	out, err := c.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: ptr(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return d
	}
	if v, ok := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]; ok {
		d.Pending = v
	}
	if v, ok := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]; ok {
		d.InFlight = v
	}
	return d
}

// DLQDepth reads the DLQ pending count; "-" if no DLQ url.
func (c *Clients) DLQDepth(ctx context.Context, dlqURL string) string {
	if dlqURL == "" {
		return "-"
	}
	out, err := c.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       ptr(dlqURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		return "-"
	}
	if v, ok := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]; ok {
		return v
	}
	return "-"
}
