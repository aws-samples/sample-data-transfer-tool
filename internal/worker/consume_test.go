package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

// fakeSQS 模拟 SQS：首批返回 n 条消息，之后空（long-poll 模拟）。
type fakeSQS struct {
	mu        sync.Mutex
	delivered bool
	n         int
	deletes   atomic.Int64
	visChange atomic.Int64
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delivered {
		time.Sleep(10 * time.Millisecond)
		return &sqs.ReceiveMessageOutput{}, nil
	}
	f.delivered = true
	msgs := make([]sqstypes.Message, f.n)
	for i := range msgs {
		msgs[i] = sqstypes.Message{
			ReceiptHandle: aws.String("r" + string(rune('0'+i))),
			Body:          aws.String(`{"source":"s3:b/k","destination":"s3:b/k2"}`),
		}
	}
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, _ *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deletes.Add(1)
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, _ *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.visChange.Add(1)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

type fakeRecorder struct{ count atomic.Int64 }

func (f *fakeRecorder) RecordTerminal(_ context.Context, _, _ string, _ model.RunResult, _, _, _ string) error {
	f.count.Add(1)
	return nil
}

// 全成功路径：3 条消息 → 全 SUCCESS → 全删除 + 记录 3 次。
func TestConsumer_SuccessDeletesAll(t *testing.T) {
	fs := &fakeSQS{n: 3}
	rec := &fakeRecorder{}
	stats := &Stats{}

	// 注入 runCopy：直接返回 SUCCESS（不接真 rcd）。
	runCopy := func(_ context.Context, _ message.TransferMessage) model.RunResult {
		return model.RunResult{State: model.StateSuccess}
	}

	c := NewConsumer(fs, ConsumerConfig{QueueURL: "q", Receivers: 1, Workers: 4}, "i#0",
		runCopy, rec, func(EMFEvent) {}, stats)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	// 等消息处理完
	for i := 0; i < 100 && fs.deletes.Load() < 3; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if fs.deletes.Load() != 3 {
		t.Errorf("应删除 3 条，got %d", fs.deletes.Load())
	}
	if stats.Success.Load() != 3 {
		t.Errorf("应计 3 次 SUCCESS，got %d", stats.Success.Load())
	}
	if rec.count.Load() != 3 {
		t.Errorf("应记录 3 次，got %d", rec.count.Load())
	}
}
