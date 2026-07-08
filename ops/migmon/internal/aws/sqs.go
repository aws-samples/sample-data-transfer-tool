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

// QueueLine is one queue's name + depth for the "list all queues" overlay.
type QueueLine struct {
	Name     string
	Pending  string
	InFlight string
}

// AllQueues lists every queue matching prefix (incl. -dlq) with its depth.
// Pass an empty prefix to list ALL queues in the region.
func (c *Clients) AllQueues(ctx context.Context, prefix string) ([]QueueLine, error) {
	urls, err := c.ListQueues(ctx, prefix)
	if err != nil {
		return nil, err
	}
	lines := make([]QueueLine, 0, len(urls))
	for _, u := range urls {
		name := u
		if i := lastSlash(u); i >= 0 {
			name = u[i+1:]
		}
		d := c.Depth(ctx, u)
		lines = append(lines, QueueLine{Name: name, Pending: d.Pending, InFlight: d.InFlight})
	}
	return lines, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
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
