package main

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const srcURL = "https://sqs.eu/123/a-queue"
const dstURL = "https://sqs.eu/123/b-queue"

// fakeSQS 线程安全的 SQS 桩，支持注入批次与 send 失败。
type fakeSQS struct {
	mu          sync.Mutex
	batches     [][]types.Message
	sentIDs     []string
	deletedRcpt []string
	failSendIDs map[string]bool
}

func newFakeSQS(batches [][]types.Message) *fakeSQS {
	return &fakeSQS{batches: batches, failSendIDs: map[string]bool{}}
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	limit := int(in.MaxNumberOfMessages)
	batch := f.batches[0]
	if limit < len(batch) {
		out := batch[:limit]
		f.batches[0] = batch[limit:]
		return &sqs.ReceiveMessageOutput{Messages: out}, nil
	}
	f.batches = f.batches[1:]
	return &sqs.ReceiveMessageOutput{Messages: batch}, nil
}

func (f *fakeSQS) SendMessageBatch(_ context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &sqs.SendMessageBatchOutput{}
	for _, e := range in.Entries {
		id := aws.ToString(e.Id)
		if f.failSendIDs[id] {
			out.Failed = append(out.Failed, types.BatchResultErrorEntry{
				Id: e.Id, Code: aws.String("InternalError"), SenderFault: false,
			})
			continue
		}
		f.sentIDs = append(f.sentIDs, id)
		out.Successful = append(out.Successful, types.SendMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func (f *fakeSQS) DeleteMessageBatch(_ context.Context, in *sqs.DeleteMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &sqs.DeleteMessageBatchOutput{}
	for _, e := range in.Entries {
		f.deletedRcpt = append(f.deletedRcpt, aws.ToString(e.ReceiptHandle))
		out.Successful = append(out.Successful, types.DeleteMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func msg(receipt, body string) types.Message {
	return types.Message{
		MessageId:     aws.String("mid-" + receipt),
		ReceiptHandle: aws.String(receipt),
		Body:          aws.String(body),
	}
}

func baseCfg() moveConfig {
	return moveConfig{srcURL: srcURL, dstURL: dstURL, maxMessages: 1000, workers: 1, progressEvery: 0}
}

func TestMovesAllAndDeletes(t *testing.T) {
	f := newFakeSQS([][]types.Message{{msg("r1", `{"k":1}`), msg("r2", `{"k":2}`)}})
	cfg := baseCfg()
	n, err := moveMessages(context.Background(), f, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 2 {
		t.Fatalf("moved=%d want 2", n)
	}
	if len(f.sentIDs) != 2 || len(f.deletedRcpt) != 2 {
		t.Fatalf("sent=%d deleted=%d want 2/2", len(f.sentIDs), len(f.deletedRcpt))
	}
}

func TestRespectsMax(t *testing.T) {
	var batches [][]types.Message
	for b := 0; b < 5; b++ {
		var grp []types.Message
		for i := 0; i < 10; i++ {
			grp = append(grp, msg(fmt.Sprintf("r%d-%d", b, i), "{}"))
		}
		batches = append(batches, grp)
	}
	f := newFakeSQS(batches)
	cfg := baseCfg()
	cfg.maxMessages = 4
	n, _ := moveMessages(context.Background(), f, cfg)
	if n != 4 {
		t.Fatalf("moved=%d want 4 (max)", n)
	}
}

func TestEmptySourceReturnsZero(t *testing.T) {
	f := newFakeSQS(nil)
	n, _ := moveMessages(context.Background(), f, baseCfg())
	if n != 0 || len(f.sentIDs) != 0 {
		t.Fatalf("moved=%d sent=%d want 0/0", n, len(f.sentIDs))
	}
}

func TestPartialSendFailureKeepsFailedOnSource(t *testing.T) {
	f := newFakeSQS([][]types.Message{{msg("r1", "{}"), msg("r2", "{}"), msg("r3", "{}")}})
	f.failSendIDs["mid-r2"] = true
	n, _ := moveMessages(context.Background(), f, baseCfg())
	if n != 2 {
		t.Fatalf("moved=%d want 2 (r2 failed)", n)
	}
	// r2 不能被删除（留源队列自愈）
	for _, r := range f.deletedRcpt {
		if r == "r2" {
			t.Fatalf("r2 不应被删除")
		}
	}
	if len(f.deletedRcpt) != 2 {
		t.Fatalf("deleted=%d want 2", len(f.deletedRcpt))
	}
}

func TestForwardableAttributesStripsAWSPrefix(t *testing.T) {
	attrs := map[string]types.MessageAttributeValue{
		"AWS.Trace":   {DataType: aws.String("String"), StringValue: aws.String("x")},
		"object_size": {DataType: aws.String("Number"), StringValue: aws.String("123")},
	}
	out := forwardableAttributes(attrs)
	if _, ok := out["AWS.Trace"]; ok {
		t.Fatalf("AWS. 前缀应被过滤")
	}
	if _, ok := out["object_size"]; !ok {
		t.Fatalf("object_size 应保留")
	}
}

func TestForwardableAttributesEmptyReturnsNil(t *testing.T) {
	if forwardableAttributes(nil) != nil {
		t.Fatalf("空属性应返回 nil")
	}
	onlyAWS := map[string]types.MessageAttributeValue{
		"AWS.X": {DataType: aws.String("String"), StringValue: aws.String("y")},
	}
	if forwardableAttributes(onlyAWS) != nil {
		t.Fatalf("仅 AWS. 属性过滤后应返回 nil")
	}
}

func TestPreservesAttributesThroughSend(t *testing.T) {
	m := msg("r1", "{}")
	m.MessageAttributes = map[string]types.MessageAttributeValue{
		"object_size": {DataType: aws.String("Number"), StringValue: aws.String("200")},
	}
	f := newFakeSQS([][]types.Message{{m}})
	var captured map[string]types.MessageAttributeValue
	// 包一层捕获 send entry 的属性
	wrap := &captureSQS{fakeSQS: f, onSend: func(e []types.SendMessageBatchRequestEntry) {
		captured = e[0].MessageAttributes
	}}
	moveMessages(context.Background(), wrap, baseCfg())
	if captured["object_size"].StringValue == nil || *captured["object_size"].StringValue != "200" {
		t.Fatalf("object_size 属性未透传")
	}
}

func TestConcurrentWorkersNoLossNoDup(t *testing.T) {
	var batches [][]types.Message
	for b := 0; b < 20; b++ {
		var grp []types.Message
		for i := 0; i < 10; i++ {
			grp = append(grp, msg(fmt.Sprintf("r%d-%d", b, i), "{}"))
		}
		batches = append(batches, grp)
	}
	f := newFakeSQS(batches)
	cfg := baseCfg()
	cfg.workers = 8
	n, _ := moveMessages(context.Background(), f, cfg)
	if n != 200 {
		t.Fatalf("moved=%d want 200", n)
	}
	seen := map[string]bool{}
	for _, id := range f.sentIDs {
		if seen[id] {
			t.Fatalf("重复发送 id=%s", id)
		}
		seen[id] = true
	}
	if len(seen) != 200 {
		t.Fatalf("unique sent=%d want 200", len(seen))
	}
}

func TestDryRunNeitherSendsNorDeletes(t *testing.T) {
	f := newFakeSQS([][]types.Message{{msg("r1", "{}"), msg("r2", "{}")}})
	cfg := baseCfg()
	cfg.dryRun = true
	n, _ := moveMessages(context.Background(), f, cfg)
	if n != 2 {
		t.Fatalf("dry-run moved=%d want 2 (preview count)", n)
	}
	if len(f.sentIDs) != 0 || len(f.deletedRcpt) != 0 {
		t.Fatalf("dry-run 不应发送/删除")
	}
}

func TestReceiveUsesShortVisibility(t *testing.T) {
	f := &visCheckSQS{fakeSQS: newFakeSQS([][]types.Message{{msg("r1", "{}")}})}
	moveMessages(context.Background(), f, baseCfg())
	if f.lastVisibility != moverVisibilitySeconds {
		t.Fatalf("visibility=%d want %d", f.lastVisibility, moverVisibilitySeconds)
	}
}

// captureSQS 包装 fakeSQS，回调捕获 send entries。
type captureSQS struct {
	*fakeSQS
	onSend func([]types.SendMessageBatchRequestEntry)
}

func (c *captureSQS) SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	if c.onSend != nil {
		c.onSend(in.Entries)
	}
	return c.fakeSQS.SendMessageBatch(ctx, in, optFns...)
}

// visCheckSQS 记录 receive 用的 VisibilityTimeout。
type visCheckSQS struct {
	*fakeSQS
	lastVisibility int32
}

func (v *visCheckSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	v.lastVisibility = in.VisibilityTimeout
	return v.fakeSQS.ReceiveMessage(ctx, in, optFns...)
}
