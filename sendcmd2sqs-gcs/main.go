package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

// errLimitReached 是哨兵 error，用于 producer 在达到 --limit 时提前中止 row group 迭代。
var errLimitReached = errors.New("limit reached")

func logf(format string, args ...any) {
	stdLogger.Printf("%s - %s", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

type cliArgs struct {
	config    string
	senders   int
	producers int
	limit     int64
	dryRun    bool
	logSent   bool   // 是否逐条记录已发送成功的消息到 {logDir}/sent-{ts}.log.gz（默认开）
	logDir    string // 所有日志的输出目录（默认 ./logs；可指向大容量数据盘）

	// 增量模式（diffParquet 非空即进入）：基于本地 diff parquet 文件生产消息，不读 inventory。
	diffParquet string
	gcsBucket   string // 手动覆盖从文件名解析出的源 GCS 桶（增量模式）
	s3Bucket    string // 手动覆盖从文件名解析出的目标 S3 桶（增量模式）
}

func parseArgs() cliArgs {
	var a cliArgs
	flag.StringVar(&a.config, "config", "", "配置文件路径（KEY=value 格式；不传则用环境变量）")
	flag.IntVar(&a.senders, "senders", DefaultSenders, "发送 goroutine 数（= 并发在途 SQS 请求数；可扫描 150/300/600 找吞吐拐点）")
	flag.IntVar(&a.producers, "producers", DefaultProducers, "并行解码 producer 数（默认 0 = CPU 核数）")
	limitFlag := flag.Int64("limit", -1, "最多处理 N 个对象（测试用，默认 -1 不限；跨所有桶全局生效）")
	flag.BoolVar(&a.dryRun, "dry-run", false, "试运行：解析+组装消息并打印前若干条，但不发送 SQS（验证消息体用，不依赖队列）")
	flag.BoolVar(&a.logSent, "log-sent", true, "逐条记录已确认发送成功的消息到 {log-dir}/sent-{时间戳}.log.gz（TSV: op\\tsource\\tdestination；--log-sent=false 关闭）")
	flag.StringVar(&a.logDir, "log-dir", "logs", "所有日志（桶日志/审计日志/发送日志）的输出目录（建议指向大容量数据盘）")
	flag.StringVar(&a.diffParquet, "diff-parquet", "", "增量模式：本地 diff parquet 文件路径（非空即进入增量模式，按 DiffFlag 分流，不读 inventory/不需 BUCKET_MAP_FILE）")
	flag.StringVar(&a.gcsBucket, "gcs-bucket", "", "增量模式：手动覆盖从 diff 文件名解析出的源 GCS 桶")
	flag.StringVar(&a.s3Bucket, "s3-bucket", "", "增量模式：手动覆盖从 diff 文件名解析出的目标 S3 桶")
	flag.Parse()
	a.limit = *limitFlag
	return a
}

func main() {
	os.Exit(run())
}

func run() int {
	args := parseArgs()

	// 日志目录：在任何日志器/桶日志创建之前生效（全局 logDir 被 createLogFile 统一使用）。
	if args.logDir != "" {
		logDir = args.logDir
	}

	// 增量模式：diff-parquet 非空即进入。该模式不需 BUCKET_MAP_FILE（桶名来自文件名）。
	incremental := args.diffParquet != ""

	cfg, err := loadConfigForMode(args.config, incremental)
	if err != nil {
		logf("错误：%v", err)
		return 1
	}

	producers := args.producers
	if producers <= 0 {
		producers = runtime.NumCPU()
	}
	senders := args.senders
	if senders <= 0 {
		senders = DefaultSenders
	}

	// 优雅关闭：Ctrl-C / SIGTERM 触发 context 取消，producer 停投、sender 排空、不丢已发数据。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// dry-run 漏发追踪：两模式共用，故在分流前初始化。只把"通过过滤、即将发送、但本应被
	// MAX_SIZE 拦下"的可疑记录写 stdout（正常零输出，有输出即漏发）；进度日志走 stderr。
	if args.dryRun {
		dryRunTracer = newSizeTracer()
		defer dryRunTracer.flush()
	}

	// 逐条发送日志：两模式共用，分流前初始化。仅非 dry-run（dry-run 不真发送，无"成功"可记）
	// 且 --log-sent 开启时启用。初始化失败仅告警降级（不写日志），不中断迁移。
	if args.logSent && !args.dryRun {
		sl, lerr := newSentLogger()
		if lerr != nil {
			logf("发送日志初始化失败，降级为不记录: %v", lerr)
		} else {
			sentLogger = sl
		}
	}

	// 各类过滤的逐条审计日志（目录占位/已删除/ignore/size）：两模式共用，分流前初始化。
	// 全部无开关——被过滤的对象必须可回查；dry-run 不建文件（无副作用），仅统计计数。
	// 任一初始化失败仅告警降级（该类不记录），不中断迁移。
	if !args.dryRun {
		for _, init := range []struct {
			target **asyncFileLog
			create func() (*asyncFileLog, error)
		}{
			{&dirSkipLogger, newDirSkipLogger},         // 目录占位（name 结尾 /）
			{&deletedSkipLogger, newDeletedSkipLogger}, // 已删除（timeDeleted 非 null，仅全量模式产生）
			{&ignoreSkipLogger, newIgnoreSkipLogger},   // .ignore-gcs 规则命中
			{&sizeSkipLogger, newSizeSkipLogger},       // size 超出 [MIN,MAX] 区间
		} {
			l, lerr := init.create()
			if lerr != nil {
				logf("过滤日志初始化失败，该类降级为不记录: %v", lerr)
				continue
			}
			*init.target = l
		}
	}
	// 统一兜底关闭全部审计/发送日志器（幂等，closeOnce 保护）：保证任何返回路径下文件都被 flush+收尾。
	// 对正常完成路径，收尾的 uploadAuditLogs 已先关闭，此 defer 是空操作；但对 store/sqsClient/tmpDir
	// 创建失败的早退出（return 1，在日志器创建之后），此 defer 是唯一的 flush+gzip 收尾保证，不可删。
	defer closeAuditLogs()

	// 增量迁移走独立路径（不读 inventory、不下载 shard、无断点续传）。
	if incremental {
		return runIncremental(ctx, cfg, args, producers, senders)
	}

	logf("=== GCS -> SQS 全量迁移（Go 版）===")
	if args.dryRun {
		logf("*** DRY-RUN 模式：只解析+组装，不发送 SQS ***")
		if cfg.hasSizeFilter {
			logf("*** 漏发追踪已启用（MAX_SIZE=%d）：可疑记录写 stdout，正常零输出 ***", cfg.MaxSize)
		} else {
			logf("*** 注意：未配置 MIN/MAX_SIZE，无 size 过滤，漏发追踪不会产生输出 ***")
		}
	}
	logf("并发: %d producers × 解码, %d senders × 发送", producers, senders)

	// 清单读取后端（从 AWS S3 读 manifest + shard）
	store, err := newInventoryStore(ctx, cfg.InventoryS3Region)
	if err != nil {
		logf("%v", err)
		return 1
	}
	defer func() {
		if cErr := store.close(); cErr != nil {
			logf("关闭清单 store 失败: %v", cErr)
		}
	}()

	// SQS 客户端（共享，连接池按 senders 放大）
	sqsClient, err := newSQSClient(ctx, cfg.AWSRegion, senders)
	if err != nil {
		logf("%v", err)
		return 1
	}

	ig := loadIgnorePatterns()

	// 临时 shard 目录（所有桶共用，处理完每个 shard 即删）
	tmpDir, err := os.MkdirTemp("", "gcs-shards-")
	if err != nil {
		logf("创建临时目录失败: %v", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)

	// 跨所有桶的全局统计：
	//   failedMessages —— 失败的消息「条」数（来自 processShard 的 sendBatch 失败）
	//   failedShards   —— 失败/未能处理的 shard「个」数（下载失败、解码错误等）
	//   failedBuckets  —— inventory 解析失败而被整桶跳过的源 GCS 桶
	var totalRecords, totalSent, failedMessages, totalSkipped, totalSizeFiltered, totalDirsFiltered, totalDeletedFiltered, producedSoFar int64
	var failedShards int64
	var failedBuckets []string

	if cfg.hasSizeFilter {
		logf("size 过滤启用：仅迁移 [%d, %d] 字节区间内的对象", cfg.MinSize, cfg.MaxSize)
	}
	logf("共 %d 个桶待处理", len(cfg.Mappings))

	// 外层：逐桶处理各自的 inventory。
	for bi, m := range cfg.Mappings {
		if ctx.Err() != nil {
			logf("收到中断信号，停止处理后续桶（已处理 %d/%d）", bi, len(cfg.Mappings))
			break
		}
		if args.limit >= 0 && producedSoFar >= args.limit {
			logf("已达到 --limit %d，停止处理后续桶", args.limit)
			break
		}

		// 单桶处理包进闭包：使 defer 在「每桶结束」触发（关日志+上传），
		// 而非 run() 返回时。中断/continue/正常三路径都能正确清理。
		func() {
			// inventory 解析失败的桶不建日志文件（失败日志走 stderr+汇总仍可见）。
			shardURIs, records, mErr := parseManifest(ctx, store, m.InventoryURI, cfg.InventoryGCSKeyPrefix, cfg.InventoryS3KeyPrefix)
			if mErr != nil {
				logf("桶 %s 的 inventory 解析失败，跳过该桶: %v", m.GCSBucket, mErr)
				failedBuckets = append(failedBuckets, m.GCSBucket)
				return
			}
			totalRecords += records

			// 解析成功后建桶日志（失败则降级为仅 stderr，不中断）。
			blog, lerr := startBucketLog(m.GCSBucket)
			if lerr != nil {
				logf("创建桶 %s 的日志文件失败，降级为仅 stderr: %v", m.GCSBucket, lerr)
				blog = nil
			}
			defer func() {
				if blog == nil {
					return
				}
				blog.stop() // 摘 tee + 关本地文件（此后 logf 进 stderr）
				// dry-run 是试运行，不应产生外部副作用：日志只留本地，不上传 S3。
				if cfg.LogS3Bucket != "" && !args.dryRun {
					uploadBucketLog(store, cfg, blog)
				}
			}()

			logf("=== [%d/%d] 处理桶 %s -> %s（inventory=%s）===", bi+1, len(cfg.Mappings), m.GCSBucket, m.S3Bucket, m.InventoryURI)

			// 每桶独立断点文件（按 inventory URI 派生），重跑同一 CSV 自动逐桶续传。
			ckpt := newCheckpoint(defaultCheckpointPath(m.InventoryURI))

			// 内层：遍历本桶的 shard。
			for idx, shardURI := range shardURIs {
				if ctx.Err() != nil {
					logf("收到中断信号，停止处理后续 shard（桶 %s 已处理 %d/%d）", m.GCSBucket, idx, len(shardURIs))
					break
				}
				if ckpt.isDone(shardURI) {
					logf("[%d/%d] 跳过已完成 shard: %s", idx+1, len(shardURIs), shardURI)
					continue
				}

				// 计算剩余 limit（跨所有桶全局生效）
				var remaining int64 = -1
				if args.limit >= 0 {
					remaining = args.limit - producedSoFar
					if remaining <= 0 {
						logf("已达到 --limit %d，停止处理", args.limit)
						break
					}
				}

				logf("[%d/%d] 处理 shard: %s", idx+1, len(shardURIs), shardURI)

				localPath, dErr := downloadShard(ctx, store, shardURI, tmpDir)
				if dErr != nil {
					logf("下载 shard 失败，跳过: %v", dErr)
					failedShards++ // 记一笔 shard 失败；不标记 done，下次可重试
					continue
				}

				res, pErr := processShard(
					ctx, cfg, sqsClient, localPath, ig, producers, senders, remaining, args.dryRun,
				)
				if rmErr := os.Remove(localPath); rmErr != nil { // 处理完即删临时文件
					logf("删除临时文件失败 %s: %v", localPath, rmErr)
				}

				// 即便 processShard 返回错误，已成功发送/跳过的计数仍然有效，先累加。
				totalSent += res.sent
				failedMessages += res.failed
				totalSkipped += res.skipped
				totalSizeFiltered += res.sizeFiltered
				totalDirsFiltered += res.dirsFiltered
				totalDeletedFiltered += res.deletedFiltered
				producedSoFar += res.produced

				if pErr != nil {
					// 解码/读取错误：该 shard 视为失败，不标记 done，下次重跑。
					logf("处理 shard 失败 %s: %v", shardURI, pErr)
					failedShards++
					continue
				}

				logf("shard 完成 %s: 发送成功=%d, 失败=%d, ignore跳过=%d, size过滤=%d, 目录占位过滤=%d, 已删除过滤=%d",
					shardURI, res.sent, res.failed, res.skipped, res.sizeFiltered, res.dirsFiltered, res.deletedFiltered)

				// 仅当无失败、未被中断、且未被 --limit 截断时才标记完成（否则下次重跑该 shard）。
				// dry-run 不写断点：试运行不应产生副作用，否则之后真实跑会因断点全量跳过。
				// res.limited：本 shard 因 --limit 额度用尽被提前中止（只处理了一部分），
				// 绝不能标记完成——否则重跑会跳过其剩余对象（漏发）。--limit 1 时尤其会跳过整个 shard。
				if args.dryRun {
					// 不标记，避免污染真实运行的续传状态。
				} else if res.limited {
					logf("shard %s 因 --limit 提前中止（未处理完），不标记完成，可去掉 --limit 重跑续传", shardURI)
				} else if res.failed == 0 && ctx.Err() == nil {
					ckpt.markDone(shardURI)
				} else if res.failed > 0 {
					logf("shard %s 有 %d 条失败，不标记完成，可重跑续传", shardURI, res.failed)
				}
			}
		}()
	}

	interrupted := ctx.Err() != nil

	logf("%s", "============================================================")
	if interrupted {
		logf("已中断（收到信号），未全量完成 —— 重跑同一命令将从断点续传:")
	} else {
		logf("全部处理完成:")
	}
	logf("  - 处理桶数: %d/%d", len(cfg.Mappings)-len(failedBuckets), len(cfg.Mappings))
	logf("  - inventory 总对象数（各桶 manifest 合计）: %d", totalRecords)
	logf("  - 发送成功: %d", totalSent)
	logf("  - 发送失败(消息条数): %d", failedMessages)
	logf("  - 失败/未处理 shard 数: %d", failedShards)
	logf("  - ignore 规则跳过: %d", totalSkipped)
	logf("  - size 区间过滤: %d", totalSizeFiltered)
	logf("  - 目录占位过滤(name结尾/): %d", totalDirsFiltered)
	logf("  - 已删除对象过滤(timeDeleted非空): %d", totalDeletedFiltered)
	if len(failedBuckets) > 0 {
		logf("  - inventory 解析失败被跳过的桶（%d 个）: %v", len(failedBuckets), failedBuckets)
	}
	logf("%s", "============================================================")

	// 上传审计/发送日志到 S3（配了 LOG_S3_URI 且非 dry-run）。与增量分支共用 uploadAuditLogs。
	if cfg.LogS3Bucket != "" && !args.dryRun {
		uploadAuditLogs(ctx, cfg)
	}

	// 退出码：被信号中断 130（128+SIGINT，区别于真实失败）；有失败 2；全成功 0。
	switch {
	case interrupted:
		return 130
	case failedMessages > 0 || failedShards > 0 || len(failedBuckets) > 0:
		return 2
	default:
		return 0
	}
}
