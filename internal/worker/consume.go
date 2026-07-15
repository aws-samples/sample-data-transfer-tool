package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// SQSAPI SQS 客户端最小接口（便于测试注入）。
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

// Stats 运行计数（原子，供进度日志 + watchdog 进展判定）。对齐 Python 计数集合。
type Stats struct {
	Total       atomic.Int64 // 已处理消息总数（含各态，watchdog 用作进展信号）
	Success     atomic.Int64
	Retryable   atomic.Int64
	Fatal       atomic.Int64
	Unknown     atomic.Int64
	Poison      atomic.Int64 // poison 单独计（不再混进 Unknown）
	RecordFail  atomic.Int64
	DeleteFail  atomic.Int64
	RequeueFail atomic.Int64
}

// ConsumerConfig 消费循环参数。回答"怎么控制并发"：
//   - Receivers: 拉 SQS 的 goroutine 数（少数即可，2-4）
//   - Workers:   处理(提交+等待同步 HTTP)的 goroutine 数 N —— 必须 = rcd --transfers，
//     否则提交多于执行会在 rcd 内排队，破坏 timeout/visibility 语义。
type ConsumerConfig struct {
	QueueURL  string
	Receivers int
	Workers   int
}

// Consumer 竞争消费者：Receivers 个 goroutine 按 worker slots 拉 SQS → Workers 个处理。
// slots 是唯一 in-flight 闸门，不会拉过头超出本机处理能力。
type Consumer struct {
	sqs        SQSAPI
	cfg        ConsumerConfig
	instanceID string
	runCopy    func(context.Context, message.TransferMessage) model.RunResult
	store      Recorder
	stats      *Stats

	inflight sync.Map     // receipt -> struct{}，停机时批量重置 visibility=0
	progress atomic.Int64 // receiver 循环推进计数，watchdog 轮询活性信号
}

type queuedMessage struct {
	msg     sqstypes.Message
	receipt string
}

const (
	maxReceiveBatch      = 10
	sqsSideEffectTimeout = 10 * time.Second
	maxVisibilitySeconds = 43200
)

// Progress 返回 receiver 循环推进计数（空闲长轮询空返也推进）。这是 watchdog 的"轮询活性"
// 信号:覆盖队列空闲不被误杀。但所有 worker 占满、在传大文件时 receiver 阻塞在 acquireSlots,
// 轮询活性会冻结——此时 watchdog 靠 rcd 全局字节活性兜底(见 internal/watchdog)。
func (c *Consumer) Progress() int64 { return c.progress.Load() }

// Recorder 抽象 DDB 终态写入（便于测试注入 fake）。status.Store 实现它。
type Recorder interface {
	RecordTerminal(ctx context.Context, source, attemptTS string, result model.RunResult, instanceID, nowISO, body string) error
}

// NewConsumer 构造消费者。runCopy 同步执行一次传输返回四态（通常是 Runner.RunCopy，
// 测试可注入 fake）。
func NewConsumer(sqsClient SQSAPI, cfg ConsumerConfig, instanceID string,
	runCopy func(context.Context, message.TransferMessage) model.RunResult,
	store Recorder, stats *Stats) *Consumer {
	if cfg.Receivers <= 0 {
		cfg.Receivers = 2
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	return &Consumer{
		sqs: sqsClient, cfg: cfg, instanceID: instanceID,
		runCopy: runCopy, store: store, stats: stats,
	}
}

// Run 启动 receiver + worker goroutine，阻塞到 ctx 取消后优雅 drain。
func (c *Consumer) Run(ctx context.Context) {
	// jobCh 不缓冲；slots 才是唯一 in-flight 闸门。这样"已从 SQS 收到但未完成
	// delete/requeue"的消息数严格 ≤ Workers，避免本地预取时间吃掉 visibility 安全裕量。
	jobCh := make(chan queuedMessage)
	slots := make(chan struct{}, c.cfg.Workers)
	for i := 0; i < c.cfg.Workers; i++ {
		slots <- struct{}{}
	}

	var workerWG sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for m := range jobCh {
				func() {
					defer c.releaseSlots(slots, 1)
					c.handle(ctx, m)
				}()
			}
		}()
	}

	var recvWG sync.WaitGroup
	for i := 0; i < c.cfg.Receivers; i++ {
		recvWG.Add(1)
		go func() {
			defer recvWG.Done()
			c.receiveLoop(ctx, jobCh, slots)
		}()
	}
	recvWG.Wait() // ctx 取消后 receiver 退出
	close(jobCh)  // 不再有新消息
	workerWG.Wait()
	c.resetInflightVisibility() // 停机：在途消息 visibility=0 立即重投（不等 12h）
}

// receiveLoop 一个 receiver：long-poll 批量拉，逐条塞入 jobCh（满则阻塞=背压）。
func (c *Consumer) receiveLoop(ctx context.Context, jobCh chan<- queuedMessage, slots chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		taken, ok := c.acquireSlots(ctx, slots, maxReceiveBatch)
		if !ok {
			return
		}
		// 每轮推进进展（含下面的空 long-poll / 错误重试）——watchdog 据此判活，
		// 空闲也算"在转"，只有 ReceiveMessage 整个卡死才不推进。
		c.progress.Add(1)
		out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(c.cfg.QueueURL),
			MaxNumberOfMessages: safeReceiveBatch(taken),
			WaitTimeSeconds:     20, // long-poll
		})
		if err != nil {
			c.releaseSlots(slots, taken)
			if ctx.Err() != nil {
				return
			}
			obslog.Warnf("ReceiveMessage 错误: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if unused := taken - len(out.Messages); unused > 0 {
			c.releaseSlots(slots, unused)
		}
		for _, m := range out.Messages {
			receipt := aws.ToString(m.ReceiptHandle)
			c.registerInflight(receipt)
			select {
			case <-ctx.Done():
				c.resetOneVisibility(receipt)
				c.finishInflight(receipt)
				c.releaseSlots(slots, 1)
				return
			case jobCh <- queuedMessage{msg: m, receipt: receipt}:
			}
		}
	}
}

func (c *Consumer) acquireSlots(ctx context.Context, slots <-chan struct{}, max int) (int, bool) {
	if max > c.cfg.Workers {
		max = c.cfg.Workers
	}
	select {
	case <-ctx.Done():
		return 0, false
	case <-slots:
	}
	n := 1
	for n < max {
		select {
		case <-slots:
			n++
		default:
			return n, true
		}
	}
	return n, true
}

func safeReceiveBatch(n int) int32 {
	if n < 1 {
		return 1
	}
	if n > maxReceiveBatch {
		return maxReceiveBatch
	}
	return int32(n) // #nosec G115 -- n is clamped to SQS batch range [1,10].
}

func (c *Consumer) releaseSlots(slots chan<- struct{}, n int) {
	for i := 0; i < n; i++ {
		slots <- struct{}{}
	}
}

func (c *Consumer) registerInflight(receipt string) {
	c.inflight.Store(receipt, struct{}{})
}

func (c *Consumer) finishInflight(receipt string) {
	c.inflight.LoadAndDelete(receipt)
}

// handle 处理单条消息：登记 in-flight → ProcessMessage(注入副作用) → 移除 in-flight。
func (c *Consumer) handle(ctx context.Context, qm queuedMessage) {
	m := qm.msg
	receipt := qm.receipt
	defer c.finishInflight(receipt)

	attemptTS := nowAttemptTS()
	body := aws.ToString(m.Body)

	eff := Effects{
		RunCopy: c.runCopy,
		Delete:  func() error { return c.deleteMsg(ctx, receipt) },
		Requeue: func(d int) error { return c.requeue(ctx, receipt, d) },
		Record: func(source, ts string, result model.RunResult, b string) error {
			// ts = attemptTS（开始/SK）；nowAttemptTS() = 终态写入时刻（≈完成）。
			// 两者分开，DDB 里能看到开始时间 + 完成时间 + 中间的 elapsed。
			return c.store.RecordTerminal(ctx, source, ts, result, c.instanceID, nowAttemptTS(), b)
		},
	}
	out := ProcessMessage(ctx, body, c.instanceID, attemptTS, eff)
	c.tally(out)
}

func (c *Consumer) deleteMsg(ctx context.Context, receipt string) error {
	sideCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sqsSideEffectTimeout)
	defer cancel()
	_, err := c.sqs.DeleteMessage(sideCtx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(receipt),
	})
	if err != nil {
		c.stats.DeleteFail.Add(1)
		obslog.Errorf("DeleteMessage 失败 receipt=%s err=%v", receipt, err)
	}
	return err
}

func (c *Consumer) requeue(ctx context.Context, receipt string, delay int) error {
	if delay < 0 {
		delay = 0
	}
	if delay > maxVisibilitySeconds {
		delay = maxVisibilitySeconds
	}
	sideCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sqsSideEffectTimeout)
	defer cancel()
	_, err := c.sqs.ChangeMessageVisibility(sideCtx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(receipt),
		VisibilityTimeout: int32(delay),
	})
	if err != nil {
		c.stats.RequeueFail.Add(1)
		obslog.Errorf("ChangeMessageVisibility 失败 receipt=%s delay=%d err=%v", receipt, delay, err)
	}
	return err
}

// resetInflightVisibility 停机时把所有在途消息 visibility 改 0，立即重投（不等 12h）。
func (c *Consumer) resetInflightVisibility() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.inflight.Range(func(k, _ any) bool {
		receipt := k.(string)
		_, err := c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(k.(string)),
			VisibilityTimeout: 0,
		})
		if err != nil {
			c.stats.RequeueFail.Add(1)
			obslog.Errorf("停机重置 visibility 失败 receipt=%s err=%v", receipt, err)
		}
		c.finishInflight(receipt)
		return true
	})
}

func (c *Consumer) resetOneVisibility(receipt string) {
	ctx, cancel := context.WithTimeout(context.Background(), sqsSideEffectTimeout)
	defer cancel()
	_, err := c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(receipt),
		VisibilityTimeout: 0,
	})
	if err != nil {
		c.stats.RequeueFail.Add(1)
		obslog.Errorf("取消投递前重置 visibility 失败 receipt=%s err=%v", receipt, err)
	}
}

func (c *Consumer) tally(o Outcome) {
	c.stats.Total.Add(1) // 进展信号 + 总处理数（对齐 Python total）
	if o.RecordFailed {
		c.stats.RecordFail.Add(1)
	}
	// RecordFail 与四态正交：已在上面独立 +1。此处不能因 RecordFailed 短路——record
	// best-effort（G1）后，失败消息带真实四态（SUCCESS/FATAL/RETRYABLE），必须照常入四态桶。
	// 否则 DDB 节流时成千上万条 SUCCESS 全 RecordFailed=true → Success 计数塌陷 → 污染
	// 判活/可观测性，正是 G1 事故"旁路不污染主干"要防的。对齐 Python _tally（四态照常）。
	switch {
	case o.Poison:
		c.stats.Poison.Add(1) // poison 单独计，不混进 unknown/四态
	case !o.Counted:
		c.stats.Unknown.Add(1)
	case o.State == model.StateSuccess:
		c.stats.Success.Add(1)
	case o.State == model.StateRetryable:
		c.stats.Retryable.Add(1)
	case o.State == model.StateFatal:
		c.stats.Fatal.Add(1)
	}
}
