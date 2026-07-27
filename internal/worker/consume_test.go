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
	mu              sync.Mutex
	delivered       bool
	n               int
	deletes         atomic.Int64
	visChange       atomic.Int64
	maxReceive      atomic.Int64
	recvHadDeadline atomic.Bool
	deleteCtxErr    error
	visCtxErr       error
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := ctx.Deadline(); ok {
		f.recvHadDeadline.Store(true)
	}
	f.maxReceive.Store(int64(in.MaxNumberOfMessages))
	if f.delivered {
		time.Sleep(10 * time.Millisecond)
		return &sqs.ReceiveMessageOutput{}, nil
	}
	f.delivered = true
	n := f.n
	if int32(n) > in.MaxNumberOfMessages {
		n = int(in.MaxNumberOfMessages)
	}
	msgs := make([]sqstypes.Message, n)
	for i := range msgs {
		msgs[i] = sqstypes.Message{
			ReceiptHandle: aws.String("r" + string(rune('0'+i))),
			Body:          aws.String(`{"source":"s3:b/k","destination":"s3:b/k2"}`),
		}
	}
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (f *fakeSQS) DeleteMessage(ctx context.Context, _ *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	f.deleteCtxErr = ctx.Err()
	f.mu.Unlock()
	f.deletes.Add(1)
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(ctx context.Context, _ *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.mu.Lock()
	f.visCtxErr = ctx.Err()
	f.mu.Unlock()
	f.visChange.Add(1)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func TestConsumer_ReceiveBatchCappedByWorkerSlots(t *testing.T) {
	fs := &fakeSQS{n: 5}
	rec := &fakeRecorder{}
	stats := &Stats{}
	runCopy := func(_ context.Context, _ message.TransferMessage) model.RunResult {
		return model.RunResult{State: model.StateSuccess}
	}
	c := NewConsumer(fs, ConsumerConfig{QueueURL: "q", Receivers: 1, Workers: 2}, "i#0",
		runCopy, rec, stats)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for i := 0; i < 100 && fs.deletes.Load() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if fs.maxReceive.Load() > 2 {
		t.Fatalf("ReceiveMessage batch 应受 worker slots 限制，got %d", fs.maxReceive.Load())
	}
	if fs.deletes.Load() != 2 {
		t.Fatalf("fakeSQS 首批最多应交付 2 条，got deletes=%d", fs.deletes.Load())
	}
}

// receiver 的 ReceiveMessage 必须带 per-call 超时（deadline），否则 SQS 连接楔死时
// receiver 永久挂在 http read 上、progress 冻结、watchdog 误判僵死。
func TestConsumer_ReceiveMessageHasPerCallDeadline(t *testing.T) {
	fs := &fakeSQS{n: 1}
	rec := &fakeRecorder{}
	stats := &Stats{}
	runCopy := func(_ context.Context, _ message.TransferMessage) model.RunResult {
		return model.RunResult{State: model.StateSuccess}
	}
	c := NewConsumer(fs, ConsumerConfig{QueueURL: "q", Receivers: 1, Workers: 2}, "i#0",
		runCopy, rec, stats)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for i := 0; i < 100 && !fs.recvHadDeadline.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !fs.recvHadDeadline.Load() {
		t.Fatal("ReceiveMessage 必须收到带 deadline 的 ctx（per-call 超时），got 无 deadline")
	}
}

func TestConsumer_SQSSideEffectsIgnoreCanceledParentContext(t *testing.T) {
	fs := &fakeSQS{}
	c := NewConsumer(fs, ConsumerConfig{QueueURL: "q", Receivers: 1, Workers: 1}, "i#0",
		nil, &fakeRecorder{}, &Stats{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.deleteMsg(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := c.requeue(ctx, "r1", 1); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.deleteCtxErr != nil {
		t.Fatalf("DeleteMessage 不应继承父 ctx cancellation，got %v", fs.deleteCtxErr)
	}
	if fs.visCtxErr != nil {
		t.Fatalf("ChangeMessageVisibility 不应继承父 ctx cancellation，got %v", fs.visCtxErr)
	}
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
		runCopy, rec, stats)

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

// G1 计数正交性(go-reviewer 抓的 CRITICAL):record best-effort 后,record 失败的消息
// 带真实四态(SUCCESS/FATAL/RETRYABLE, RecordFailed=true)。tally 必须让四态照常入桶 +
// RecordFail 独立 +1,不能因 RecordFailed 短路吞掉四态计数(否则 DDB 节流时 Success 塌陷
// 污染判活/可观测性,正是 G1 事故"旁路不污染主干"要防的)。对齐 Python _tally:四态照常。
func TestTally_RecordFailStillCountsFourStates(t *testing.T) {
	newC := func() (*Consumer, *Stats) {
		st := &Stats{}
		c := NewConsumer(&fakeSQS{}, ConsumerConfig{QueueURL: "q", Receivers: 1, Workers: 1},
			"i#0", nil, &fakeRecorder{}, st)
		return c, st
	}
	cases := []struct {
		name                                               string
		o                                                  Outcome
		success, fatal, retry, unknown, poison, recordFail int64
	}{
		{"success+recordfail", Outcome{State: model.StateSuccess, Counted: true, RecordFailed: true}, 1, 0, 0, 0, 0, 1},
		{"fatal+recordfail", Outcome{State: model.StateFatal, Counted: true, RecordFailed: true}, 0, 1, 0, 0, 0, 1},
		{"retryable+recordfail", Outcome{State: model.StateRetryable, Counted: true, RecordFailed: true}, 0, 0, 1, 0, 0, 1},
		{"poison+recordfail 仍只计 poison", Outcome{State: model.StateFatal, Poison: true, RecordFailed: true}, 0, 0, 0, 0, 1, 1},
		{"success 无 recordfail", Outcome{State: model.StateSuccess, Counted: true}, 1, 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st := newC()
			c.tally(tc.o)
			if got := st.Success.Load(); got != tc.success {
				t.Errorf("Success=%d 期望 %d", got, tc.success)
			}
			if got := st.Fatal.Load(); got != tc.fatal {
				t.Errorf("Fatal=%d 期望 %d", got, tc.fatal)
			}
			if got := st.Retryable.Load(); got != tc.retry {
				t.Errorf("Retryable=%d 期望 %d", got, tc.retry)
			}
			if got := st.Poison.Load(); got != tc.poison {
				t.Errorf("Poison=%d 期望 %d", got, tc.poison)
			}
			if got := st.RecordFail.Load(); got != tc.recordFail {
				t.Errorf("RecordFail=%d 期望 %d", got, tc.recordFail)
			}
		})
	}
}
