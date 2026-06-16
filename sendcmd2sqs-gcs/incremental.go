package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/parquet-go/parquet-go"
)

// 增量迁移模式：输入是一个本地 diff parquet 文件（GCS 源桶与 S3 目标桶的扫描差异结果），
// 按 DiffFlag 分流生产 SQS 消息，无需下载 inventory、无需 BUCKET_MAP_FILE、无断点续传。
//
// diff 文件 schema（与 inventory 不同，无 bucket 列）：
//   Key(string, 对象路径) · Size(uint64→INT64) · LastModified(uint64) · ETag(string) · DiffFlag(uint8→INT32)
// 源/目标桶名从文件名解析（见 parseDiffFileName），可由 -gcs-bucket/-s3-bucket 覆盖。

// DiffFlag 取值（来自上游 diff 工具的定义）：
const (
	diffFlagPlus    = 1 // Plus：源 GCS 有、目标 S3 无 → copy（补缺失）
	diffFlagMinus   = 2 // Minus：目标 S3 有、源 GCS 无 → delete（删多余）
	diffFlagAstrisk = 3 // Astrisk：两边都有但内容不一致 → copy（用 GCS 版覆盖）
)

// parseDiffFileName 从 diff 文件名解析 region / gcsBucket / s3Bucket / timestamp。
// 约定格式：region__gcsBucket__s3Bucket_timestamp.parquet
//   - 用 "__" 切成恰好 3 段，得 region、gcsBucket、(s3Bucket_timestamp)；
//   - 第三段按首个 "_" 切分得 s3Bucket 与 timestamp（timestamp 自身含 "_" 也无妨，只切首个）。
//
// 任一关键段（region/gcsBucket/s3Bucket）为空或段数不符则返回 error。
// 传入路径会先经 filepath.Base 取文件名，故可直接传完整路径。
func parseDiffFileName(name string) (region, gcsBucket, s3Bucket, timestamp string, err error) {
	base := strings.TrimSuffix(filepath.Base(name), ".parquet")
	parts := strings.Split(base, "__")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("diff 文件名应为 region__gcsBucket__s3Bucket_timestamp.parquet 格式，无法解析: %q", name)
	}
	region = strings.TrimSpace(parts[0])
	gcsBucket = strings.TrimSpace(parts[1])
	s3Part, ts, ok := strings.Cut(parts[2], "_")
	s3Bucket = strings.TrimSpace(s3Part)
	timestamp = strings.TrimSpace(ts)
	if !ok {
		return "", "", "", "", fmt.Errorf("diff 文件名第三段应为 s3Bucket_timestamp（缺少 \"_\" 分隔）: %q", name)
	}
	if region == "" || gcsBucket == "" || s3Bucket == "" {
		return "", "", "", "", fmt.Errorf("diff 文件名 region/gcsBucket/s3Bucket 段不得为空: %q", name)
	}
	return region, gcsBucket, s3Bucket, timestamp, nil
}

// diffReader 封装一个本地 diff Parquet 文件，按 row group 流式读取 key / diffFlag / size。
// 与 inventory 的 shardReader 平行（语义不同，故不共用结构体），但复用包级 helper
// leafColumnIndex / valueString / valueInt64（见 parquet.go）。
type diffReader struct {
	f           *os.File
	pf          *parquet.File
	keyIdx      int
	diffFlagIdx int
	sizeIdx     int // size 列叶子索引；-1 表示未启用（不读取，回调 size 恒为"未知"）
}

// openDiffShard 打开本地 diff Parquet 文件并定位 Key / DiffFlag 列（必填），
// needSize 为 true 时额外定位 Size 列（缺列报错）；为 false 时不读 size（向后兼容）。
func openDiffShard(path string, needSize bool) (*diffReader, error) {
	f, pf, err := openParquetFile(path)
	if err != nil {
		return nil, err
	}

	keyIdx, ok := leafColumnIndex(pf, "Key")
	if !ok {
		f.Close()
		return nil, fmt.Errorf("diff 报告缺少必填列 \"Key\"（对象路径列）")
	}
	diffFlagIdx, ok := leafColumnIndex(pf, "DiffFlag")
	if !ok {
		f.Close()
		return nil, fmt.Errorf("diff 报告缺少必填列 \"DiffFlag\"（差异标志列）")
	}

	sizeIdx := -1
	if needSize {
		idx, ok := leafColumnIndex(pf, "Size")
		if !ok {
			f.Close()
			return nil, fmt.Errorf("配置了 MIN_SIZE/MAX_SIZE 但 diff 报告缺少 \"Size\" 列")
		}
		sizeIdx = idx
	}

	return &diffReader{f: f, pf: pf, keyIdx: keyIdx, diffFlagIdx: diffFlagIdx, sizeIdx: sizeIdx}, nil
}

func (dr *diffReader) close() error {
	return dr.f.Close()
}

// numRowGroups 返回 diff 文件的 row group 数（用于把工作分给多个 producer）。
func (dr *diffReader) numRowGroups() int {
	return len(dr.pf.RowGroups())
}

// iterDiffRowGroup 流式遍历指定 row group 的每一行，对每行回调 (key, diffFlag, size, hasSize)。
// 按批读取（ReadRows），不把整个 row group 载入内存。回调返回 error 则中止。
// diffFlag 用 valueInt64 解出；hasFlag=false（理论 null）时回调收到 diffFlag=0，由调用方按未知值处理。
func (dr *diffReader) iterDiffRowGroup(rgIndex int, fn func(key string, diffFlag, size int64, hasSize bool) error) error {
	rg := dr.pf.RowGroups()[rgIndex]
	rows := rg.Rows()
	defer rows.Close()

	buf := make([]parquet.Row, 1024)
	for {
		n, readErr := rows.ReadRows(buf)
		for i := 0; i < n; i++ {
			row := buf[i]
			var key string
			var diffFlag, size int64
			var hasSize bool
			for _, v := range row {
				switch v.Column() {
				case dr.keyIdx:
					key = valueString(v)
				case dr.diffFlagIdx:
					diffFlag, _ = valueInt64(v) // null/不可解析 → 0，落入未知 flag 分支
				case dr.sizeIdx: // sizeIdx==-1 时此 case 永不命中
					size, hasSize = valueInt64(v)
				}
			}
			if err := fn(key, diffFlag, size, hasSize); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("读取 diff Parquet row group %d 失败: %w", rgIndex, readErr)
		}
	}
}

// processDiffShard 处理单个本地 diff Parquet 文件：P 个 producer 并行按 row group 解码，
// 按 DiffFlag 分流组装 copy/delete 消息，S 个 sender 从 batchCh 并发发送 SQS。
//
// 分流（已与用户确认）：
//   - DiffFlag=1/3 → copy（GCS→S3，补缺失 / 用 GCS 版覆盖不一致），受 size 区间过滤。
//   - DiffFlag=2   → delete（删 S3 多余对象），不受 size 过滤。
//   - 其他值 / null → 跳过并计入 skipped（限量告警）。
//
// gcsBucket/s3Bucket 由调用方从文件名解析（或 flag 覆盖）传入。
// 返回统计快照（shardResult，与全量模式共用；deletedFiltered 在增量模式恒 0）与首个读取错误。
func processDiffShard(
	ctx context.Context,
	cfg *Config,
	client *sqs.Client,
	localPath, gcsBucket, s3Bucket string,
	ig *ignoreMatcher,
	producers, senders int,
	limit int64,
	dryRun bool,
) (shardResult, error) {

	dr, err := openDiffShard(localPath, cfg.hasSizeFilter)
	if err != nil {
		return shardResult{}, err
	}
	defer dr.close()

	stats := &shardStats{}

	numRG := dr.numRowGroups()
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

	// 内部 context：任一 producer 遇到真实读取错误时 cancel，停止其余 producer 与发送。
	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var readErr error
	var readErrOnce sync.Once

	// 未知 DiffFlag 只告警一次（实测无未知值，纯防御），避免异常文件刷屏。
	var unknownFlagOnce sync.Once

	batchCh := make(chan [][]byte, senders*2)

	// ---- 启动 sender goroutine（与 processShard 共用 runSenders）----
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
			// appendMsg 把组装好的消息加入批，满批即 flush。
			appendMsg := func(msg []byte) {
				batch = append(batch, msg)
				if len(batch) >= SQSMaxBatch {
					flush()
				}
			}
			for rg := workerID; rg < numRG; rg += producers {
				if procCtx.Err() != nil {
					break
				}
				iterErr := dr.iterDiffRowGroup(rg, func(key string, diffFlag, size int64, hasSize bool) error {
					if procCtx.Err() != nil {
						return procCtx.Err()
					}
					// 防御空 Key：会生成畸形目标 s3:bucket/，跳过。
					if key == "" {
						stats.skipped.Add(1)
						return nil
					}
					// GCS 目录占位对象（Key 以 / 结尾）：绝不能 copy/delete——copy 会让下游 rclone
					// 把它当 prefix 整体 copyto（事故根因）。拦下并异步记录（与全量模式同一规则）。
					if strings.HasSuffix(key, "/") {
						stats.dirsFiltered.Add(1)
						dirSkipLogger.logPath(gcsBucket, key) // nil-safe（dry-run 时为 nil，只计数）
						return nil
					}
					// ignore 规则作用于对象路径（与全量一致）：跳过的行不占 --limit 额度。
					if ig.shouldSkip(key) {
						stats.skipped.Add(1)
						ignoreSkipLogger.logPath(gcsBucket, key) // nil-safe（dry-run 时为 nil，只计数）
						return nil
					}

					switch diffFlag {
					case diffFlagPlus, diffFlagAstrisk:
						// copy：受 size 区间过滤（仅当配了 MIN/MAX 时）。未知 size 放行。
						if cfg.hasSizeFilter && hasSize {
							if size < cfg.MinSize || size > cfg.MaxSize {
								stats.sizeFiltered.Add(1)
								sizeSkipLogger.logSize(size, gcsBucket, key) // 带 size 值，与全量模式同格式
								return nil
							}
						}
						if !stats.tryProduce(limit) {
							return errLimitReached
						}
						msg, mErr := buildCopyMessage(cfg.GCSRemote, s3Bucket, gcsBucket, key)
						if mErr != nil {
							stats.failed.Add(1)
							return nil
						}
						stats.copies.Add(1)
						appendMsg(msg)
					case diffFlagMinus:
						// delete：删 S3 多余对象，不受 size 过滤。
						if !stats.tryProduce(limit) {
							return errLimitReached
						}
						msg, mErr := buildDeleteMessage(s3Bucket, key)
						if mErr != nil {
							stats.failed.Add(1)
							return nil
						}
						stats.deletes.Add(1)
						appendMsg(msg)
					default:
						// 未知 DiffFlag（含 null→0）：跳过、计数、告警一次，不中止。
						stats.skipped.Add(1)
						unknownFlagOnce.Do(func() {
							logf("跳过未知 DiffFlag=%d 的对象（后续同类不再告警）: %s", diffFlag, key)
						})
					}
					return nil
				})
				if iterErr == errLimitReached {
					break
				}
				if iterErr != nil && procCtx.Err() == nil {
					readErrOnce.Do(func() { readErr = iterErr })
					cancel()
					break
				}
			}
			flush()
		}(p)
	}

	prodWG.Wait()
	close(batchCh)
	senderWG.Wait()

	return stats.snapshot(), readErr
}

// runIncremental 执行增量迁移：解析桶名 → 建 SQS 客户端 → 处理 diff 文件 → 打印统计 → 返回退出码。
// 与全量 run() 共用 producers/senders/dryRunTracer/limit 等已初始化好的参数。
func runIncremental(ctx context.Context, cfg *Config, args cliArgs, producers, senders int) int {
	// 1) 桶名：优先文件名解析，flag 可覆盖任一侧；最终任一为空则报错。
	region, gcsBucket, s3Bucket, timestamp, perr := parseDiffFileName(args.diffParquet)
	if args.gcsBucket != "" {
		gcsBucket = args.gcsBucket
	}
	if args.s3Bucket != "" {
		s3Bucket = args.s3Bucket
	}
	if gcsBucket == "" || s3Bucket == "" {
		// 解析失败且未被 flag 补全。
		if perr != nil {
			logf("错误：%v（可用 -gcs-bucket/-s3-bucket 手动指定桶名）", perr)
		} else {
			logf("错误：未能确定源/目标桶名（gcsBucket=%q s3Bucket=%q），请用 -gcs-bucket/-s3-bucket 指定", gcsBucket, s3Bucket)
		}
		return 1
	}

	logf("=== GCS -> SQS 增量迁移（基于 diff 文件）===")
	logf("diff 文件: %s", args.diffParquet)
	if perr == nil {
		logf("文件名解析: region=%s, 源 GCS 桶=%s, 目标 S3 桶=%s, 时间戳=%s", region, gcsBucket, s3Bucket, timestamp)
	}
	if args.gcsBucket != "" || args.s3Bucket != "" {
		logf("桶名（含 flag 覆盖）: 源 GCS 桶=%s, 目标 S3 桶=%s", gcsBucket, s3Bucket)
	}
	logf("分流规则: DiffFlag 1/3 → copy, 2 → delete, 其他 → 跳过")
	if args.dryRun {
		logf("*** DRY-RUN 模式：只解析+组装，不发送 SQS ***")
	}
	if cfg.hasSizeFilter {
		logf("size 过滤启用（仅作用于 copy）：仅迁移 [%d, %d] 字节区间内的对象", cfg.MinSize, cfg.MaxSize)
	}
	logf("并发: %d producers × 解码, %d senders × 发送", producers, senders)

	// 2) SQS 客户端：dry-run 不发送，免建（也免依赖 AWS 凭证，可离线验证消息体）。
	var sqsClient *sqs.Client
	if !args.dryRun {
		var cErr error
		sqsClient, cErr = newSQSClient(ctx, cfg.AWSRegion, senders)
		if cErr != nil {
			logf("%v", cErr)
			return 1
		}
	}

	// 3) ignore 规则（作用于对象路径，与全量一致）。
	ig := loadIgnorePatterns()

	// 4) 处理 diff 文件（本地，无需下载、无断点）。
	res, procErr := processDiffShard(
		ctx, cfg, sqsClient, args.diffParquet, gcsBucket, s3Bucket, ig, producers, senders, args.limit, args.dryRun,
	)

	interrupted := ctx.Err() != nil

	logf("%s", "============================================================")
	if interrupted {
		logf("已中断（收到信号），未全量完成:")
	} else if procErr != nil {
		logf("处理 diff 文件出错: %v", procErr)
	} else {
		logf("增量迁移处理完成:")
	}
	logf("  - 组装消息总数: %d（copy=%d, delete=%d）", res.produced, res.copies, res.deletes)
	logf("  - 发送成功: %d", res.sent)
	logf("  - 发送失败(消息条数): %d", res.failed)
	logf("  - 跳过(空Key/ignore/未知DiffFlag): %d", res.skipped)
	logf("  - size 区间过滤(仅copy): %d", res.sizeFiltered)
	logf("  - 目录占位过滤(Key结尾/): %d", res.dirsFiltered)
	logf("%s", "============================================================")

	// 上传审计/发送日志到 S3（配了 LOG_S3_URI 且非 dry-run）。与全量分支共用 uploadAuditLogs。
	if cfg.LogS3Bucket != "" && !args.dryRun {
		uploadAuditLogs(ctx, cfg)
	}

	// 退出码：被信号中断 130；有发送失败或读取错误 2；全成功 0。
	switch {
	case interrupted:
		return 130
	case res.failed > 0 || procErr != nil:
		return 2
	default:
		return 0
	}
}
