package worker

import (
	"context"
	"strconv"
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
	DeleteFail  atomic.Int64
	RequeueFail atomic.Int64
}

// ConsumerConfig 消费循环参数。回答"怎么控制并发"：
//   - Receivers: 拉 SQS 的 goroutine 数（少数即可，2-4）
//   - Workers:   处理(提交+轮询)的 goroutine 数 N —— 必须 ≈ rcd --transfers，
//     否则提交多于执行会在 rcd 内排队、撞轮询 deadline 被误 stop。
type ConsumerConfig struct {
	QueueURL  string
	Receivers int
	Workers   int
}

// Consumer 竞争消费者：Receivers 个 goroutine 批量拉 SQS → channel → Workers 个处理。
// channel 满则 receiver 阻塞 = 天然背压，不会拉过头超出 in-flight。
type Consumer struct {
	sqs        SQSAPI
	cfg        ConsumerConfig
	instanceID string
	runCopy    func(context.Context, message.TransferMessage) model.RunResult
	store      Recorder
	report     func(EMFEvent)
	stats      *Stats

	inflight sync.Map     // receipt -> struct{}，停机时批量重置 visibility=0
	progress atomic.Int64 // receiver 每轮 ReceiveMessage（含空 long-poll）+1，watchdog 判活
}

// Progress 返回 receiver 循环推进计数。空闲（长轮询空返）也推进，故只有真卡死才不增，
// watchdog 据此区分"空闲"与"僵死"，避免误杀空闲机。
func (c *Consumer) Progress() int64 { return c.progress.Load() }

// Recorder 抽象 DDB 终态写入（便于测试注入 fake）。status.Store 实现它。
type Recorder interface {
	RecordTerminal(ctx context.Context, source, attemptTS string, result model.RunResult, instanceID, nowISO, body string) error
}

// NewConsumer 构造消费者。runCopy 提交并轮询一次传输返回四态（通常是 Runner.RunCopy，
// 测试可注入 fake）。
func NewConsumer(sqsClient SQSAPI, cfg ConsumerConfig, instanceID string,
	runCopy func(context.Context, message.TransferMessage) model.RunResult,
	store Recorder, report func(EMFEvent), stats *Stats) *Consumer {
	if cfg.Receivers <= 0 {
		cfg.Receivers = 2
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	return &Consumer{
		sqs: sqsClient, cfg: cfg, instanceID: instanceID,
		runCopy: runCopy, store: store, report: report, stats: stats,
	}
}

// Run 启动 receiver + worker goroutine，阻塞到 ctx 取消后优雅 drain。
func (c *Consumer) Run(ctx context.Context) {
	// buffer ≈ workers，背压：满则 receiver 阻塞。
	jobCh := make(chan sqstypes.Message, c.cfg.Workers)

	var workerWG sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for m := range jobCh {
				c.handle(ctx, m)
			}
		}()
	}

	var recvWG sync.WaitGroup
	for i := 0; i < c.cfg.Receivers; i++ {
		recvWG.Add(1)
		go func() {
			defer recvWG.Done()
			c.receiveLoop(ctx, jobCh)
		}()
	}

	recvWG.Wait() // ctx 取消后 receiver 退出
	close(jobCh)  // 不再有新消息
	workerWG.Wait()
	c.resetInflightVisibility() // 停机：在途消息 visibility=0 立即重投（不等 12h）
}

// receiveLoop 一个 receiver：long-poll 批量拉，逐条塞入 jobCh（满则阻塞=背压）。
func (c *Consumer) receiveLoop(ctx context.Context, jobCh chan<- sqstypes.Message) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// 每轮推进进展（含下面的空 long-poll / 错误重试）——watchdog 据此判活，
		// 空闲也算"在转"，只有 ReceiveMessage 整个卡死才不推进。
		c.progress.Add(1)
		out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(c.cfg.QueueURL),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       20, // long-poll
			MessageAttributeNames: []string{"object_size"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			obslog.Warnf("ReceiveMessage 错误: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, m := range out.Messages {
			select {
			case <-ctx.Done():
				return
			case jobCh <- m:
			}
		}
	}
}

// handle 处理单条消息：登记 in-flight → ProcessMessage(注入副作用) → 移除 in-flight。
func (c *Consumer) handle(ctx context.Context, m sqstypes.Message) {
	receipt := aws.ToString(m.ReceiptHandle)
	c.inflight.Store(receipt, struct{}{})
	defer c.inflight.Delete(receipt)

	size := parseObjectSize(m)
	attemptTS := nowAttemptTS()
	body := aws.ToString(m.Body)

	eff := Effects{
		RunCopy: c.runCopy,
		Delete:  func() error { return c.deleteMsg(ctx, receipt) },
		Requeue: func(d int) error { return c.requeue(ctx, receipt, d) },
		Record: func(source, ts string, result model.RunResult, b string) {
			// ts = attemptTS（开始/SK）；nowAttemptTS() = 终态写入时刻（≈完成）。
			// 两者分开，DDB 里能看到开始时间 + 完成时间 + 中间的 elapsed。
			_ = c.store.RecordTerminal(ctx, source, ts, result, c.instanceID, nowAttemptTS(), b)
		},
		Report: c.report,
	}
	out := ProcessMessage(ctx, body, size, c.instanceID, attemptTS, eff)
	c.tally(out)
}

func (c *Consumer) deleteMsg(ctx context.Context, receipt string) error {
	_, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(receipt),
	})
	if err != nil {
		c.stats.DeleteFail.Add(1)
	}
	return err
}

func (c *Consumer) requeue(ctx context.Context, receipt string, delay int) error {
	_, err := c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(receipt),
		VisibilityTimeout: int32(delay),
	})
	if err != nil {
		c.stats.RequeueFail.Add(1)
	}
	return err
}

// resetInflightVisibility 停机时把所有在途消息 visibility 改 0，立即重投（不等 12h）。
func (c *Consumer) resetInflightVisibility() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.inflight.Range(func(k, _ any) bool {
		_, _ = c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: aws.String(k.(string)),
			VisibilityTimeout: 0,
		})
		return true
	})
}

// parseObjectSize 从 SQS MessageAttribute object_size 读对象大小（缺省 0=small 维度）。
func parseObjectSize(m sqstypes.Message) int64 {
	attr, ok := m.MessageAttributes["object_size"]
	if !ok || attr.StringValue == nil {
		return 0
	}
	n, err := strconv.ParseInt(*attr.StringValue, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (c *Consumer) tally(o Outcome) {
	c.stats.Total.Add(1) // 进展信号 + 总处理数（对齐 Python total）
	switch {
	case o.Poison:
		c.stats.Poison.Add(1) // poison 单独计，不混进 unknown
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
