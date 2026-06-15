// dlq-cleaner —— 分析并清理 SQS DLQ：源对象已不存在(src_not_found)的死信归档后删除，
// 其余可重试/待人工的死信转发到保留去向（主队列 replay 或指定保留队列）。
//
// 判定依据：DynamoDB transfer-status 表里该 source 最近一次 attempt 的 error_class
// （不调用 GCS/源端，不抢迁移的源端 API 配额；on-demand DDB Query 自动扩容，
// 一亿条 15-40 分钟）。
//
// 安全设计（不可逆操作）：
//   - 默认 report 模式（dry-run）：只采样统计 error_class 分布 + 估算成本，不删不转；
//   - clean 模式必须显式 --apply，且必须指定保留去向（--replay 或 --keep-queue）；
//   - 删除前先把完整 body 追加写本地 jsonl 归档（后悔药）；
//   - drain 语义：处理完即从 DLQ 移除，DLQ 单调减少，连续空 receive = 扫完；
//   - 判不准（DDB 查不到 / uncategorized）一律保守当"保留"转发，绝不当垃圾删。
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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const batchSize = 10

// 处理一批后，被判 keep 的消息转发到保留去向时用短 visibility 不适用（已 drain）；
// 这里仅 receive 用短 visibility，防止本进程崩溃后消息被锁太久。
const receiveVisibilitySeconds = 120

// sqsAPI / 工具用到的 SQS 操作，便于测试注入。
type sqsAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

// cleanerConfig 单次运行配置。
type cleanerConfig struct {
	dlqURL        string
	keepURL       string // keep 消息转发去向（主队列或保留队列）；report 模式可空
	ddbTable      string
	apply         bool // false=report（dry-run，只统计不动数据）
	maxMessages   int64
	workers       int
	sampleLimit   int64 // report 模式采样上限（0=不限，扫到空为止）
	deleteClasses map[string]bool
	archive       *archiveWriter // 删除归档（clean 模式）；report 模式 nil
	progressEvery int64
}

// stats 运行统计（原子累加，goroutine 共享）。
type stats struct {
	scanned   int64
	deleted   int64
	kept      int64
	unknown   int64
	ddbErrors int64
	noSource  int64    // 消息体无 source（poison hash 行）→ 当 unknown 保留
	byClass   sync.Map // error_class(string) → *int64
}

func (s *stats) addClass(ec string) {
	if ec == "" {
		ec = "(none)"
	}
	v, _ := s.byClass.LoadOrStore(ec, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (s *stats) classCounts() classCount {
	out := classCount{}
	s.byClass.Range(func(k, v any) bool {
		out[k.(string)] = int(atomic.LoadInt64(v.(*int64)))
		return true
	})
	return out
}

// extractSource 从 DLQ 消息体 JSON 取 source 字段（不依赖完整解析，容忍异常体）。
// 返回 "" 表示无可用 source（poison/坏体）。
func extractSource(body string) string {
	// 轻量提取：消息体形如 {"source":"...","destination":"..."}。
	// 用 strings 而非 json 解析，容忍坏 JSON（坏体本就该当 unknown 保留）。
	const key = `"source"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	c := strings.Index(rest, ":")
	if c < 0 {
		return ""
	}
	rest = rest[c+1:]
	q1 := strings.Index(rest, `"`)
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	// 找未转义的结束引号
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++ // 跳过被转义字符
			continue
		}
		if rest[j] == '"' {
			return rest[:j]
		}
	}
	return ""
}

// processBatch 处理一批 DLQ 消息：分类 → 归档/转发 → 删除。
// report 模式只分类统计，不删不转。返回本批处理条数。
func processBatch(ctx context.Context, sqsc sqsAPI, ddb ddbAPI, cfg cleanerConfig, st *stats, msgs []sqstypes.Message) {
	var toDelete []sqstypes.DeleteMessageBatchRequestEntry // 源不存在 → 删
	var toKeep []sqstypes.Message                          // 其它 → 转发后删

	for _, m := range msgs {
		atomic.AddInt64(&st.scanned, 1)
		body := aws.ToString(m.Body)
		src := extractSource(body)
		if src == "" {
			atomic.AddInt64(&st.noSource, 1)
			atomic.AddInt64(&st.unknown, 1)
			st.addClass("(no-source)")
			toKeep = append(toKeep, m) // 无 source 保守保留
			continue
		}
		ec, found, err := latestErrorClass(ctx, ddb, cfg.ddbTable, src)
		if err != nil {
			atomic.AddInt64(&st.ddbErrors, 1)
			atomic.AddInt64(&st.unknown, 1)
			st.addClass("(ddb-error)")
			toKeep = append(toKeep, m) // 查询失败保守保留
			continue
		}
		st.addClass(ec)
		switch classify(ec, found, cfg.deleteClasses) {
		case decideDelete:
			atomic.AddInt64(&st.deleted, 1)
			if cfg.apply {
				if cfg.archive != nil {
					cfg.archive.write(body)
				}
				toDelete = append(toDelete, sqstypes.DeleteMessageBatchRequestEntry{
					Id: m.MessageId, ReceiptHandle: m.ReceiptHandle,
				})
			}
		case decideKeep:
			atomic.AddInt64(&st.kept, 1)
			toKeep = append(toKeep, m)
		default:
			atomic.AddInt64(&st.unknown, 1)
			toKeep = append(toKeep, m)
		}
	}

	if !cfg.apply {
		return // report 模式：只统计，不动任何数据
	}

	// keep 消息：先转发到保留去向，转发成功的才从 DLQ 删除（先发后删，不丢）。
	if len(toKeep) > 0 && cfg.keepURL != "" {
		forwarded := forwardKeep(ctx, sqsc, cfg.keepURL, toKeep)
		toDelete = append(toDelete, forwarded...)
	}

	if len(toDelete) > 0 {
		deleteBatch(ctx, sqsc, cfg.dlqURL, toDelete)
	}
}

// forwardKeep 把保留消息批量发到 keepURL，返回发送成功、可安全从 DLQ 删除的 entry。
func forwardKeep(ctx context.Context, sqsc sqsAPI, keepURL string, msgs []sqstypes.Message) []sqstypes.DeleteMessageBatchRequestEntry {
	entries := make([]sqstypes.SendMessageBatchRequestEntry, 0, len(msgs))
	rcptByID := make(map[string]string, len(msgs))
	for _, m := range msgs {
		id := aws.ToString(m.MessageId)
		entries = append(entries, sqstypes.SendMessageBatchRequestEntry{
			Id:                m.MessageId,
			MessageBody:       m.Body,
			MessageAttributes: forwardableAttributes(m.MessageAttributes),
		})
		rcptByID[id] = aws.ToString(m.ReceiptHandle)
	}
	out, err := sqsc.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(keepURL), Entries: entries,
	})
	if err != nil {
		log.Printf("转发保留消息失败（本批留 DLQ 不删，可重跑）: %v", err)
		return nil
	}
	var ok []sqstypes.DeleteMessageBatchRequestEntry
	for _, s := range out.Successful {
		id := aws.ToString(s.Id)
		ok = append(ok, sqstypes.DeleteMessageBatchRequestEntry{
			Id: s.Id, ReceiptHandle: aws.String(rcptByID[id]),
		})
	}
	for _, f := range out.Failed {
		log.Printf("转发失败留 DLQ: id=%s code=%s", aws.ToString(f.Id), aws.ToString(f.Code))
	}
	return ok
}

func deleteBatch(ctx context.Context, sqsc sqsAPI, dlqURL string, entries []sqstypes.DeleteMessageBatchRequestEntry) {
	// SQS DeleteMessageBatch 单次最多 10 条。
	for i := 0; i < len(entries); i += batchSize {
		end := i + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if _, err := sqsc.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
			QueueUrl: aws.String(dlqURL), Entries: entries[i:end],
		}); err != nil {
			log.Printf("从 DLQ 删除失败（消息会 visibility 超时后回 DLQ，可重跑）: %v", err)
		}
	}
}

func forwardableAttributes(attrs map[string]sqstypes.MessageAttributeValue) map[string]sqstypes.MessageAttributeValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]sqstypes.MessageAttributeValue, len(attrs))
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

// run 多 goroutine drain DLQ。返回处理总条数。
func run(ctx context.Context, sqsc sqsAPI, ddb ddbAPI, cfg cleanerConfig, st *stats) {
	var stop int32 // 任一 worker 连续空 receive 或达到采样上限 → 置位，全员停

	worker := func() {
		for atomic.LoadInt32(&stop) == 0 {
			if cfg.sampleLimit > 0 && atomic.LoadInt64(&st.scanned) >= cfg.sampleLimit {
				atomic.StoreInt32(&stop, 1)
				return
			}
			out, err := sqsc.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
				QueueUrl:              aws.String(cfg.dlqURL),
				MaxNumberOfMessages:   batchSize,
				WaitTimeSeconds:       2,
				VisibilityTimeout:     receiveVisibilitySeconds,
				MessageAttributeNames: []string{"All"},
			})
			if err != nil {
				log.Printf("receive 失败（重试）: %v", err)
				continue
			}
			if len(out.Messages) == 0 {
				atomic.StoreInt32(&stop, 1) // 队列空，收敛
				return
			}
			processBatch(ctx, sqsc, ddb, cfg, st, out.Messages)
			if cfg.progressEvery > 0 {
				n := atomic.LoadInt64(&st.scanned)
				if n%cfg.progressEvery < int64(len(out.Messages)) {
					log.Printf("进度: 已扫描 %d（删 %d / 留 %d / 未知 %d）",
						n, atomic.LoadInt64(&st.deleted), atomic.LoadInt64(&st.kept),
						atomic.LoadInt64(&st.unknown))
				}
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()
}

func main() {
	dlq := flag.String("dlq", "", "DLQ 队列 URL（必填）")
	mode := flag.String("mode", "report", "report=只统计分布(dry-run) | clean=实际清理")
	apply := flag.Bool("apply", false, "clean 模式下必须显式置 true 才真正删除/转发")
	replay := flag.Bool("replay", false, "keep 消息转发回主队列（RedrivePolicy 反查）")
	keepQueue := flag.String("keep-queue", "", "keep 消息转发到指定保留队列 URL（与 --replay 二选一）")
	ddbTable := flag.String("ddb-table", "", "transfer-status 表名（必填）")
	region := flag.String("region", os.Getenv("AWS_REGION"), "AWS region")
	workers := flag.Int("workers", 32, "并发 goroutine 数")
	sampleLimit := flag.Int64("sample", 50000, "report 模式采样上限（0=全扫）")
	archivePath := flag.String("archive", "", "删除归档 jsonl 路径（clean 模式必填）")
	logFile := flag.String("log-file", "", "日志落盘路径（同时输出控制台）")
	progressEvery := flag.Int64("progress-every", 10000, "每处理 N 条打进度")
	flag.Parse()

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打不开日志文件: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		log.SetOutput(&dualWriter{os.Stderr, f})
	}

	if *dlq == "" || *ddbTable == "" {
		fmt.Fprintln(os.Stderr, "必须指定 --dlq 和 --ddb-table")
		os.Exit(2)
	}
	if *region == "" {
		fmt.Fprintln(os.Stderr, "缺 region：--region 或 AWS_REGION")
		os.Exit(2)
	}

	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(*region), config.WithRetryMaxAttempts(10))
	if err != nil {
		log.Fatalf("加载 AWS 配置失败: %v", err)
	}
	sqsc := sqs.NewFromConfig(awsCfg)
	ddb := dynamodb.NewFromConfig(awsCfg)

	cfg := cleanerConfig{
		dlqURL:        *dlq,
		ddbTable:      *ddbTable,
		workers:       *workers,
		deleteClasses: map[string]bool{"src_not_found": true},
		progressEvery: *progressEvery,
	}

	switch *mode {
	case "report":
		cfg.apply = false
		cfg.sampleLimit = *sampleLimit
		log.Printf("=== REPORT 模式（dry-run，不删不转）采样上限 %d ===", *sampleLimit)
	case "clean":
		if !*apply {
			fmt.Fprintln(os.Stderr, "clean 模式必须显式加 --apply（不可逆操作保护）")
			os.Exit(2)
		}
		if *archivePath == "" {
			fmt.Fprintln(os.Stderr, "clean 模式必须指定 --archive（删除归档，后悔药）")
			os.Exit(2)
		}
		// 保留去向二选一
		switch {
		case *replay:
			keepURL, err := resolveMainQueueFromDLQ(ctx, sqsc, *dlq)
			if err != nil {
				log.Fatalf("--replay 反查主队列失败: %v", err)
			}
			cfg.keepURL = keepURL
			log.Printf("keep 消息将 replay 回主队列: %s", keepURL)
		case *keepQueue != "":
			cfg.keepURL = *keepQueue
			log.Printf("keep 消息将转发到保留队列: %s", *keepQueue)
		default:
			fmt.Fprintln(os.Stderr, "clean 模式必须指定保留去向：--replay 或 --keep-queue")
			os.Exit(2)
		}
		aw, err := newArchiveWriter(*archivePath)
		if err != nil {
			log.Fatalf("打开归档文件失败: %v", err)
		}
		defer aw.close()
		cfg.archive = aw
		cfg.apply = true
		cfg.sampleLimit = 0 // 全扫
		log.Printf("=== CLEAN 模式（--apply）归档=%s ===", *archivePath)
	default:
		fmt.Fprintf(os.Stderr, "未知 mode: %s（report|clean）\n", *mode)
		os.Exit(2)
	}

	st := &stats{}
	start := time.Now()
	run(ctx, sqsc, ddb, cfg, st)
	printReport(st, cfg, time.Since(start))
}

// dualWriter 日志同时写控制台和文件。
type dualWriter struct{ console, file *os.File }

func (w *dualWriter) Write(p []byte) (int, error) {
	w.console.Write(p)
	return w.file.Write(p)
}
