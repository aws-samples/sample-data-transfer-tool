package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// dry-run 模式下限量打印消息体：每类（copy / delete）各自最多打印 dryRunMaxPrint 条后不再打印
// （防海量消息刷屏）。copy 与 delete 分开计额度，否则 delete 排在文件后段时会被前面的 copy
// 占满额度而永远抽不到样例（增量 diff 文件常见：copy 占绝大多数、delete 极少且靠后）。
// 两个计数器是包级全局:本程序是一次性 CLI,每次运行都是新进程,无进程内重复运行的状态污染问题。
// (若将来此逻辑被复用到长生命周期进程,需把它移入处理函数作用域以隔离每次运行。)
const dryRunMaxPrint = 20

var (
	dryRunPrintedCopy   atomic.Int64
	dryRunPrintedDelete atomic.Int64
)

// dryRunMsgIsDelete 判断一条已序列化的消息是否为 delete（用于 dry-run 按类型分别限流打印）。
// 用字节子串匹配而非 JSON 反序列化：仅在 dry-run 打印路径调用，且 encoding/json 输出字段无空格，
// `"op":"delete"` 形态固定（见 buildDeleteMessage）。
func dryRunMsgIsDelete(msg []byte) bool {
	return bytes.Contains(msg, []byte(`"op":"delete"`))
}

// dryRunTracer 是 dry-run 下的 size 追踪器（非 nil 即启用，由 main 在 dry-run 时初始化）。
// 包级全局：本程序是一次性 CLI，单次运行内所有 shard 共享同一 stdout 写入器。
var dryRunTracer *sizeTracer

// sizeTracer 是 dry-run 下的"漏发"追踪输出器：只把【通过了过滤、即将发送，但本应被
// MAX_SIZE 拦下】的可疑记录写到 stdout（紧凑 TSV）。正常情况零输出——有输出即为漏发。
// 与走 stderr 的进度日志(logf)分离，便于分别重定向 / 管道处理。
//
// 两类会被打印（reason 列区分）：
//   - UNKNOWN_SIZE：size 读不出(hasSize=false)被放行——这正是当初 size 过滤短路的根因。
//   - OVER_MAX   ：size 已知且 > MaxSize 却仍通过——当前过滤逻辑下不可能发生，作哨兵；
//     若哪天过滤被改坏，这里立即报警。
//
// 用 bufio + mutex 串行写：并发安全；正常零输出，故几乎无 IO 开销。
type sizeTracer struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newSizeTracer() *sizeTracer {
	// 1MB 缓冲（正常零输出，缓冲主要为异常爆发时降 syscall）。
	return &sizeTracer{w: bufio.NewWriterSize(os.Stdout, 1<<20)}
}

// traceLeak 打印一条漏发记录：<reason>\t<size>\t<hasSize>\t<maxSize>\t<bucket/name>
// bucket/name 放最后一列；GCS 对象名可含 TAB/换行（实测约 2% 的 name 含 TAB），
// 转义为可见字面以保证列结构不被破坏。
func (t *sizeTracer) traceLeak(reason string, size int64, hasSize bool, maxSize int64, bucket, name string) {
	t.mu.Lock()
	fmt.Fprintf(t.w, "%s\t%d\t%t\t%d\t%s/%s\n", reason, size, hasSize, maxSize, bucket, escapeTabs(name))
	t.mu.Unlock()
}

// escapeTabs 把字符串里的 TAB/CR/LF 替换为可见字面，避免破坏 TSV 列结构。
var tabEscaper = strings.NewReplacer("\t", `\t`, "\r", `\r`, "\n", `\n`)

func escapeTabs(s string) string {
	return tabEscaper.Replace(s)
}

func (t *sizeTracer) flush() {
	if t == nil {
		return
	}
	t.mu.Lock()
	_ = t.w.Flush()
	t.mu.Unlock()
}

// shardStats 是单 shard 的处理统计（原子累加，多 goroutine 安全）。
type shardStats struct {
	sent    atomic.Int64
	failed  atomic.Int64
	skipped atomic.Int64
	// sizeFiltered 是被 size 区间过滤掉的对象数（与 ignore 的 skipped 分开计）。
	sizeFiltered atomic.Int64
	// produced 是已组装（通过过滤）的消息数，用于 --limit 全局封顶。
	produced atomic.Int64
	// copies / deletes 仅增量模式使用，分别记 copy / delete 消息数（全量模式恒 0）。
	copies  atomic.Int64
	deletes atomic.Int64
	// dirsFiltered 是被过滤的 GCS 目录占位对象数（name 以 / 结尾；它们被 copy 会引发
	// 下游 rclone 对整个 prefix 执行 copyto，必须拦下）。与 skipped/sizeFiltered 分开计。
	dirsFiltered atomic.Int64
	// deletedFiltered 是被过滤的已删除对象数（inventory 的 timeDeleted 非 null：对象已不在
	// GCS，copy 必失败）。仅全量模式使用（diff 流程对比的是存活对象）。
	deletedFiltered atomic.Int64
	// limited 表示本 shard 因 --limit 额度用尽而被提前中止（未处理完整个 shard）。
	// 关键：被截断的 shard 绝不能标记 checkpoint 完成，否则重跑会跳过其剩余对象（漏发）。
	limited atomic.Bool
}

// shardResult 是 processShard / processDiffShard 处理完成后的统计快照（普通 int64，非 atomic）。
// 与 shardStats 同款取舍：单一 struct 服务两种模式，模式不适用的字段恒 0
// （全量模式 copies/deletes 恒 0；增量模式 deletedFiltered 恒 0）。
type shardResult struct {
	sent            int64
	failed          int64
	skipped         int64
	sizeFiltered    int64
	dirsFiltered    int64
	deletedFiltered int64
	copies          int64
	deletes         int64
	produced        int64
	// limited 表示本 shard 因 --limit 额度用尽被提前中止（未处理完）。调用方据此决定不标记 checkpoint。
	limited bool
}

// snapshot 把并发累加器的当前值读成快照（处理结束、所有 goroutine 已汇合后调用）。
func (s *shardStats) snapshot() shardResult {
	return shardResult{
		sent:            s.sent.Load(),
		failed:          s.failed.Load(),
		skipped:         s.skipped.Load(),
		sizeFiltered:    s.sizeFiltered.Load(),
		dirsFiltered:    s.dirsFiltered.Load(),
		deletedFiltered: s.deletedFiltered.Load(),
		copies:          s.copies.Load(),
		deletes:         s.deletes.Load(),
		produced:        s.produced.Load(),
		limited:         s.limited.Load(),
	}
}

// runSenders 启动 senders 个发送 goroutine，从 batchCh 消费 batch：dry-run 时限量打印消息体
// 并全部计为成功，否则 sendBatch 发送 SQS；结果累加进 stats.sent/failed。返回 *WaitGroup，
// 调用方在 close(batchCh) 后 Wait() 等待排空。processShard 与 processDiffShard 共用，
// 使 dry-run 打印与计数逻辑只有一份、不发散。
func runSenders(ctx context.Context, client *sqs.Client, queueURL string, batchCh <-chan [][]byte, senders int, dryRun bool, stats *shardStats) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batchCh {
				if dryRun {
					// dry-run：不发 SQS，限量打印消息体供人工核对，全部计为"成功"。
					// copy / delete 各自独立额度，确保少量且靠后的 delete 也能抽到样例。
					// 先 Load 判断：超限后只读不自增，避免海量消息时的无谓原子写与 cache line 竞争。
					for _, msg := range batch {
						counter := &dryRunPrintedCopy
						if dryRunMsgIsDelete(msg) {
							counter = &dryRunPrintedDelete
						}
						if counter.Load() < dryRunMaxPrint {
							counter.Add(1)
							logf("[dry-run] %s", string(msg))
						}
					}
					stats.sent.Add(int64(len(batch)))
					continue
				}
				ok, fail := sendBatch(ctx, client, queueURL, batch)
				stats.sent.Add(int64(ok))
				stats.failed.Add(int64(fail))
			}
		}()
	}
	return &wg
}

// tryProduce 原子地为一条待组装消息占用 limit 额度：成功占用返回 true 并使 produced+1；
// 已达 limit 则回滚、返回 false（并置 limited 标志：本 shard 被 limit 截断、未处理完）。
// limit<0 表示不限。把"占额-回滚"收拢为单一原子操作。
func (s *shardStats) tryProduce(limit int64) bool {
	if limit < 0 {
		s.produced.Add(1)
		return true
	}
	if s.produced.Add(1) > limit {
		s.produced.Add(-1)
		s.limited.Store(true) // 额度用尽 → 该 shard 被提前中止，调用方不可标记 checkpoint
		return false
	}
	return true
}

// processShard 处理单个本地 shard 文件：P 个 producer 并行按 row group 解码+组装+攒批，
// S 个 sender 从 batchCh 并发发送 SQS。返回统计快照与首个读取错误。
//
//   - producers: 并行解码的 producer 数（会被 row group 数截断）
//   - senders:   并发发送 goroutine 数（= 在途 SQS 请求数）
//   - limit:     本 shard 最多组装多少条（<0 表示不限），用于 --limit
func processShard(
	ctx context.Context,
	cfg *Config,
	client *sqs.Client,
	localPath string,
	ig *ignoreMatcher,
	producers, senders int,
	limit int64,
	dryRun bool,
) (shardResult, error) {

	sr, err := openShard(localPath, cfg.hasSizeFilter)
	if err != nil {
		return shardResult{}, err
	}
	defer sr.close()

	stats := &shardStats{}

	numRG := sr.numRowGroups()
	if numRG == 0 {
		return shardResult{}, nil
	}
	if producers > numRG {
		producers = numRG // producer 数不超过 row group 数
	}
	if producers < 1 {
		producers = 1
	}
	if senders < 1 {
		senders = 1 // 下界保护：senders==0 会使 batchCh 无接收者，producer flush 永久阻塞
	}

	// 内部 context：任一 producer 遇到真实读取错误时 cancel，停止其余 producer 与发送，
	// 让整个 shard 以错误失败（而非静默丢弃损坏 row group 的剩余行）。
	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 记录首个真实的 row group 读取错误（非 errLimitReached、非 ctx 取消）。
	var readErr error
	var readErrOnce sync.Once

	// 有界 batch channel：容量 = senders*2，提供背压，内存恒定。
	batchCh := make(chan [][]byte, senders*2)

	// ---- 启动 sender goroutine（与 processDiffShard 共用 runSenders）----
	senderWG := runSenders(procCtx, client, cfg.SQSQueueURL, batchCh, senders, dryRun, stats)

	// ---- 启动 producer goroutine：按 row group 取模分配 ----
	var prodWG sync.WaitGroup
	for p := 0; p < producers; p++ {
		prodWG.Add(1)
		go func(workerID int) {
			defer prodWG.Done()
			batch := make([][]byte, 0, SQSMaxBatch)
			flush := func() {
				if len(batch) > 0 {
					select {
					case batchCh <- batch:
					case <-procCtx.Done():
					}
					batch = make([][]byte, 0, SQSMaxBatch)
				}
			}
			for rg := workerID; rg < numRG; rg += producers {
				if procCtx.Err() != nil {
					break
				}
				iterErr := sr.iterRowGroup(rg, func(bucket, name string, size int64, hasSize, deleted bool) error {
					if procCtx.Err() != nil {
						return procCtx.Err()
					}
					// 已删除对象（inventory 的 timeDeleted 非 null）：对象已不在 GCS，copy 必失败，
					// 拦下并异步记录到专用日志文件（清单无 timeDeleted 列时 deleted 恒 false）。
					if deleted {
						stats.deletedFiltered.Add(1)
						deletedSkipLogger.logPath(bucket, name) // nil-safe（dry-run 时为 nil，只计数）
						return nil
					}
					// GCS 目录占位对象（name 以 / 结尾，size 恒 0）：绝不能 copy——下游 rclone
					// 会把它当 prefix 整体 copyto（事故根因）。拦下并异步记录到专用日志文件。
					// 注意只看结尾斜杠，不看 size==0：size 为 0 但不以 / 结尾的是真实空文件，必须迁移。
					if strings.HasSuffix(name, "/") {
						stats.dirsFiltered.Add(1)
						dirSkipLogger.logPath(bucket, name) // nil-safe（dry-run 时为 nil，只计数）
						return nil
					}
					// 先过滤：跳过的行不占 --limit 额度。
					if ig.shouldSkip(name) {
						stats.skipped.Add(1)
						ignoreSkipLogger.logPath(bucket, name) // nil-safe（dry-run 时为 nil，只计数）
						return nil
					}
					// size 区间过滤（仅当配了 MIN/MAX 时启用）。未知 size 放行——
					// 过滤本意是"明确超界才跳过"，缺证据不等于超界，避免漏迁。
					if cfg.hasSizeFilter && hasSize {
						if size < cfg.MinSize || size > cfg.MaxSize {
							stats.sizeFiltered.Add(1)
							sizeSkipLogger.logSize(size, bucket, name) // 带 size 值，回查"为何被滤"一目了然
							return nil
						}
					}
					// 【漏发追踪】走到这=通过过滤、即将发送。dry-run 下只在"本应被拦却放行"
					// 时打印（正常零输出，有输出即漏发）。两类可疑：
					//   - size 读不出(hasSize=false)：当初过滤短路的根因，被放行。
					//   - size 已知却 >MaxSize：当前逻辑不可能走到，作哨兵防过滤被改坏。
					if dryRunTracer != nil && cfg.hasSizeFilter {
						if !hasSize {
							dryRunTracer.traceLeak("UNKNOWN_SIZE", size, hasSize, cfg.MaxSize, bucket, name)
						} else if size > cfg.MaxSize {
							dryRunTracer.traceLeak("OVER_MAX", size, hasSize, cfg.MaxSize, bucket, name)
						}
					}
					// 目标桶由源 GCS 桶经 BUCKET_MAP 派生；未命中即返回错误，
					// 经 iterRowGroup 上抛后由下方致命错误分支 fail-fast（该 shard 不标记完成，可重跑）。
					targetBucket, ok := cfg.BucketMap[bucket]
					if !ok {
						return fmt.Errorf("源 GCS 桶未在 BUCKET_MAP 中，无法确定目标 S3 桶: %q", bucket)
					}
					// --limit 全局封顶：占额失败说明已达上限，停止本 row group。
					if !stats.tryProduce(limit) {
						return errLimitReached
					}
					msg, mErr := buildCopyMessage(cfg.GCSRemote, targetBucket, bucket, name)
					if mErr != nil {
						stats.failed.Add(1)
						return nil
					}
					batch = append(batch, msg)
					if len(batch) >= SQSMaxBatch {
						flush()
					}
					return nil
				})
				if iterErr == errLimitReached {
					break
				}
				// 真实的 row group 读取错误（损坏/截断的 Parquet）：记录首个错误并 cancel，
				// 阻止该 shard 被误标完成、让其下次重跑。ctx 取消归一为 procCtx.Err()，不算读取错误。
				if iterErr != nil && procCtx.Err() == nil {
					readErrOnce.Do(func() { readErr = iterErr })
					cancel()
					break
				}
			}
			flush()
		}(p)
	}

	// producers 完成 → 关闭 channel → senders 自然退出。
	prodWG.Wait()
	close(batchCh)
	senderWG.Wait()

	return stats.snapshot(), readErr
}
