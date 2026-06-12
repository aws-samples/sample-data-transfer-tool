// queue-mover (Go) —— 跨集群 SQS 消息搬运工具：A 集群队列 → B 集群队列。
//
// 与 Python 版同一不丢消息契约：
//   - 先发后删：只有 SendMessageBatch 确认成功（不在 Failed）的消息才从 A 删除；
//   - 部分失败：Failed 条不删，留源队列靠 visibility 自愈重投；
//   - 崩溃自愈：receive 用短 VisibilityTimeout，进程挂掉后消息很快回 A 队列；
//   - 属性透传：MessageAttributes 原样带到 B（过滤 AWS. 保留前缀）；
//   - at-least-once：极端时序 B 可能收重复，worker 端幂等无害。
//
// Go 用单进程 N 个 goroutine 替代 Python 的多进程×多线程（无 GIL，省内存/机器）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQS 批量上限（receive / send / delete 共用），与 SQS API 硬上限一致。
const batchSize = 10

// receive 时显式覆盖 visibility：搬运是秒级操作，不能继承队列自身的 12h 配置——
// 否则进程崩溃后已 receive 的消息被锁 12h 才回源队列。5 分钟足够一批的处理余量。
const moverVisibilitySeconds = 300

// sqsAPI 抽象出工具用到的 3 个 SQS 操作，便于单元测试注入 fake。
type sqsAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

// moveConfig 单次搬运配置。
type moveConfig struct {
	srcURL        string
	dstURL        string
	maxMessages   int64
	workers       int
	dryRun        bool
	progressEvery int64
}

// forwardableAttributes 过滤掉 AWS. 保留前缀，返回可透传的 MessageAttributes。
func forwardableAttributes(attrs map[string]types.MessageAttributeValue) map[string]types.MessageAttributeValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]types.MessageAttributeValue, len(attrs))
	for k, v := range attrs {
		if strings.HasPrefix(k, "AWS.") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// moveOneBatch 搬一批（≤10 条）：receive → send_batch → 按 send 结果选择性 delete_batch。
// 返回成功搬运条数；源队列空返回 (0, nil)。
func moveOneBatch(ctx context.Context, client sqsAPI, cfg moveConfig, want int32) (int, error) {
	if want > batchSize {
		want = batchSize
	}
	recv, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              &cfg.srcURL,
		MaxNumberOfMessages:   want,
		WaitTimeSeconds:       1,
		VisibilityTimeout:     moverVisibilitySeconds,
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		return 0, fmt.Errorf("receive: %w", err)
	}
	if len(recv.Messages) == 0 {
		return 0, nil
	}
	if cfg.dryRun {
		for _, m := range recv.Messages {
			body := aws.ToString(m.Body)
			if len(body) > 200 {
				body = body[:200]
			}
			log.Printf("[dry-run] 将搬运: %s", body)
		}
		return len(recv.Messages), nil
	}

	sendEntries := make([]types.SendMessageBatchRequestEntry, 0, len(recv.Messages))
	receiptByID := make(map[string]string, len(recv.Messages))
	for _, m := range recv.Messages {
		id := aws.ToString(m.MessageId)
		sendEntries = append(sendEntries, types.SendMessageBatchRequestEntry{
			Id:                aws.String(id),
			MessageBody:       m.Body,
			MessageAttributes: forwardableAttributes(m.MessageAttributes),
		})
		receiptByID[id] = aws.ToString(m.ReceiptHandle)
	}

	sendOut, err := client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: &cfg.dstURL,
		Entries:  sendEntries,
	})
	if err != nil {
		// 整批 send 失败：一条都不删，全部留源队列靠 visibility 自愈。
		return 0, fmt.Errorf("send batch: %w", err)
	}
	for _, f := range sendOut.Failed {
		log.Printf("send 失败留源队列: id=%s code=%s", aws.ToString(f.Id), aws.ToString(f.Code))
	}

	// 只删 send 成功的；Failed 条留在源队列（visibility 过期自动重投，不丢）。
	delEntries := make([]types.DeleteMessageBatchRequestEntry, 0, len(sendOut.Successful))
	for _, s := range sendOut.Successful {
		id := aws.ToString(s.Id)
		delEntries = append(delEntries, types.DeleteMessageBatchRequestEntry{
			Id:            aws.String(id),
			ReceiptHandle: aws.String(receiptByID[id]),
		})
	}
	if len(delEntries) > 0 {
		if _, err := client.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
			QueueUrl: &cfg.srcURL,
			Entries:  delEntries,
		}); err != nil {
			// 删失败：消息已发到 B，但源队列没删 → visibility 过期后重投 = B 收重复。
			// at-least-once 契约下可接受（worker 幂等）。仍记错误便于观测。
			return len(delEntries), fmt.Errorf("delete batch: %w", err)
		}
	}
	return len(sendOut.Successful), nil
}

// moveMessages 用 cfg.workers 个 goroutine 并发搬运，返回实际搬运总条数。
// 共享 claimed/moved 计数器用原子操作（receive 前先预留配额，严格不超 max）。
func moveMessages(ctx context.Context, client sqsAPI, cfg moveConfig) (int64, error) {
	var claimed, moved, lastReport int64
	var reportMu sync.Mutex
	var firstErr error
	var errMu sync.Mutex

	worker := func() {
		for {
			// 原子预留本轮配额：claimed 不超过 max，receive 实际拿到 n ≤ 预留数。
			var quota int32
			for {
				cur := atomic.LoadInt64(&claimed)
				remain := cfg.maxMessages - cur
				if remain <= 0 {
					return
				}
				q := int64(batchSize)
				if remain < q {
					q = remain
				}
				if atomic.CompareAndSwapInt64(&claimed, cur, cur+q) {
					quota = int32(q)
					break
				}
			}
			n, err := moveOneBatch(ctx, client, cfg, quota)
			// 没用完的配额退回（receive 不足额 / 出错）。
			atomic.AddInt64(&claimed, -(int64(quota) - int64(n)))
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				log.Printf("批次错误（继续）: %v", err)
			}
			if n > 0 {
				total := atomic.AddInt64(&moved, int64(n))
				if cfg.progressEvery > 0 {
					reportMu.Lock()
					if total-lastReport >= cfg.progressEvery {
						lastReport = total
						log.Printf("进度: 已搬运 %d 条", total)
					}
					reportMu.Unlock()
				}
			}
			if n == 0 {
				return // 源队列空（long-poll 1s 后仍无消息）
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()

	if cfg.progressEvery > 0 && moved != lastReport {
		log.Printf("进度: 已搬运 %d 条", moved)
	}
	return moved, firstErr
}

func main() {
	src := flag.String("src-queue", "", "源队列 URL（A 集群）")
	dst := flag.String("dst-queue", "", "目标队列 URL（B 集群）")
	maxMsg := flag.Int64("max", 100000, "最多搬运条数")
	workers := flag.Int("workers", 64, "并发 goroutine 数（替代 Python 的 procs×threads）")
	region := flag.String("region", os.Getenv("AWS_REGION"), "AWS region")
	logFile := flag.String("log-file", "", "搬运日志落地路径（同时输出到控制台）")
	progressEvery := flag.Int64("progress-every", 5000, "每搬运 N 条打一条进度日志")
	dryRun := flag.Bool("dry-run", false, "只 receive 预览，不发送不删除")
	flag.Parse()

	// 日志：控制台 + 可选落盘双写。
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打不开日志文件 %s: %v\n", *logFile, err)
			os.Exit(1)
		}
		defer f.Close()
		log.SetOutput(&dualWriter{console: os.Stderr, file: f})
	}

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "必须指定 --src-queue 和 --dst-queue")
		os.Exit(2)
	}
	if *src == *dst {
		fmt.Fprintln(os.Stderr, "源队列与目标队列不能相同")
		os.Exit(2)
	}
	if *region == "" {
		fmt.Fprintln(os.Stderr, "缺 region：--region 或 AWS_REGION")
		os.Exit(2)
	}
	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "--workers 必须 >= 1")
		os.Exit(2)
	}

	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(*region),
		config.WithRetryMaxAttempts(10),
	)
	if err != nil {
		log.Fatalf("加载 AWS 配置失败: %v", err)
	}
	client := sqs.NewFromConfig(awsCfg)

	cfg := moveConfig{
		srcURL:        *src,
		dstURL:        *dst,
		maxMessages:   *maxMsg,
		workers:       *workers,
		dryRun:        *dryRun,
		progressEvery: *progressEvery,
	}
	start := time.Now()
	n, moveErr := moveMessages(ctx, client, cfg)
	elapsed := time.Since(start).Seconds()
	action := "已搬运"
	if *dryRun {
		action = "[dry-run] 可搬运"
	}
	rate := float64(0)
	if elapsed > 0 {
		rate = float64(n) / elapsed
	}
	log.Printf("%s %d 条消息，耗时 %.1fs，平均 %.0f 条/秒", action, n, elapsed, rate)
	if moveErr != nil {
		log.Printf("注意：过程中有批次出错（消息靠 visibility 自愈，可重跑）: %v", moveErr)
	}
}

// dualWriter 把日志同时写控制台和文件。
type dualWriter struct {
	console *os.File
	file    *os.File
}

func (w *dualWriter) Write(p []byte) (int, error) {
	w.console.Write(p)
	return w.file.Write(p)
}
