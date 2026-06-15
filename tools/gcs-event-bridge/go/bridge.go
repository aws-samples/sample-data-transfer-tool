package main

import (
	"context"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQS 单批上限（SendMessageBatch 硬上限 10 条）。
const sqsBatchSize = 10

// 攒批 flush 间隔：尾部不足 10 条的消息最多等这么久就发，避免低流量时饿等。
const batchFlushInterval = 500 * time.Millisecond

// sqsSender 抽象 SQS 投递，便于测试注入。
type sqsSender interface {
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
}

// inboundMessage Pub/Sub 消息经映射后、待投递 SQS 的中间结构。
// ackFn 在「SQS 投递成功」后调用 → ack Pub/Sub（先发后 ack，不丢）。
type inboundMessage struct {
	mapped mappedMessage
	ackFn  func()
	nackFn func()
}

// bridgeStats 单条 pipeline 的运行计数。
type bridgeStats struct {
	received  int64 // Pub/Sub 收到
	skipped   int64 // 非关注事件跳过
	sent      int64 // 成功投递 SQS
	mapErrors int64 // 映射失败（payload 坏等）
	sendFails int64 // SQS send 失败（nack 让 Pub/Sub 重投）
	// 失败日志限流：上次打 SQS 错误日志的 unix 纳秒时间戳（原子）。
	// 防止 SQS 故障风暴下 per-batch 日志刷爆磁盘——失败总量看 30s 进度行的 sendFails。
	lastErrLogNanos int64
}

// 失败日志最小间隔：SQS 持续故障时，最多每隔这么久打一条错误日志（其余只累加计数）。
const errLogThrottle = 5 * time.Second

// batchSender 从 channel 收映射好的消息，攒批 10 条投 SQS，整批成功后逐条 ack。
// 多个 send worker 并发跑本函数，提升投递吞吐（send_workers 默认 64）。
func batchSender(ctx context.Context, sender sqsSender, queueURL string, in <-chan inboundMessage, st *bridgeStats, wg *sync.WaitGroup) {
	defer wg.Done()
	batch := make([]inboundMessage, 0, sqsBatchSize)
	timer := time.NewTimer(batchFlushInterval)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		sendBatch(ctx, sender, queueURL, batch, st)
		batch = batch[:0]
	}

	for {
		select {
		case msg, ok := <-in:
			if !ok {
				flush() // channel 关闭，发完最后一批
				return
			}
			batch = append(batch, msg)
			if len(batch) >= sqsBatchSize {
				flush()
			}
		case <-timer.C:
			flush() // 定时 flush 尾部
			timer.Reset(batchFlushInterval)
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// sendBatch 投一批到 SQS，按 send 结果分别 ack（成功）/ nack（失败，让 Pub/Sub 重投）。
func sendBatch(ctx context.Context, sender sqsSender, queueURL string, batch []inboundMessage, st *bridgeStats) {
	entries := make([]sqstypes.SendMessageBatchRequestEntry, len(batch))
	for i, m := range batch {
		entries[i] = sqstypes.SendMessageBatchRequestEntry{
			Id:          aws.String(strconv.Itoa(i)),
			MessageBody: aws.String(m.mapped.Body),
		}
		// object_size 走 MessageAttribute（对齐 worker 大小路由；仅 copy 有真实大小）。
		if m.mapped.ObjectSize > 0 {
			entries[i].MessageAttributes = map[string]sqstypes.MessageAttributeValue{
				"object_size": {
					DataType:    aws.String("Number"),
					StringValue: aws.String(strconv.FormatInt(m.mapped.ObjectSize, 10)),
				},
			}
		}
	}

	out, err := sender.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(queueURL),
		Entries:  entries,
	})
	if err != nil {
		// 整批失败：全部 nack，Pub/Sub 稍后重投（不丢，at-least-once）。
		atomic.AddInt64(&st.sendFails, int64(len(batch)))
		// 限流打日志：SQS 故障风暴下不刷爆磁盘；失败总量看 30s 进度行的 sendFails。
		throttledErrLog(st, "SQS send 整批失败（%d 条 nack 重投，最近一次错误）: %v", len(batch), err)
		for _, m := range batch {
			m.nackFn()
		}
		return
	}
	// 成功的 ack，失败的 nack（Id 是 batch 下标）。
	// 单条失败不再 per-message 打日志（高频会爆盘）——只累加 sendFails，由进度行汇总。
	failed := map[string]bool{}
	for _, f := range out.Failed {
		failed[aws.ToString(f.Id)] = true
	}
	for i, m := range batch {
		if failed[strconv.Itoa(i)] {
			atomic.AddInt64(&st.sendFails, 1)
			m.nackFn()
		} else {
			atomic.AddInt64(&st.sent, 1)
			m.ackFn() // 先发后 ack：投 SQS 成功才 ack Pub/Sub
		}
	}
}

// throttledErrLog 限流打错误日志：距上次同类日志不足 errLogThrottle 则只累计不打，
// 防止 SQS 持续故障时 per-batch 日志（高吞吐下每秒数千条）刷爆磁盘。
// 用原子 CAS 抢占时间窗，多 send worker 并发安全。失败总量仍由 30s 进度行的 sendFails 反映。
func throttledErrLog(st *bridgeStats, format string, args ...any) {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&st.lastErrLogNanos)
	if now-last < int64(errLogThrottle) {
		return
	}
	if !atomic.CompareAndSwapInt64(&st.lastErrLogNanos, last, now) {
		return // 另一个 worker 刚打过，本次跳过
	}
	log.Printf(format, args...)
}
