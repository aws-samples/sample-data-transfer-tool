// Command create-markers 为 GCS 空目录清单在 S3 侧批量创建 _$folder$ 0 字节标记对象。
//
// 高并发 Go 版本，对齐 Python create_s3_folder_markers.py 的能力（逐条日志、断点续传、
// failed_keys、dry-run、verify、retry-failed、limit），用 goroutine 绕开 Python GIL。
//
// 自动探测输入列名：name（原始路径，末尾 / -> _$folder$）或 key（已是最终 key，原样）。
// put_object 幂等，重复行/重跑均无副作用。
//
// 用法:
//
//	create-markers --bucket B --dry-run --limit 1000   # 干跑：打印 src\tkey
//	create-markers --bucket B --limit 1000             # 小批真实创建
//	create-markers --bucket B                          # 全量（断点续传）
//	create-markers --bucket B --verify 200             # 抽样核对 0 字节
//	create-markers --bucket B --retry-failed           # 重跑失败清单
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// errLimitReached 是哨兵 error，用于 producer 达到 --limit 时提前中止 row group 迭代。
var errLimitReached = errors.New("limit reached")

var stdLogger = log.New(os.Stderr, "", 0)

func logf(format string, args ...any) {
	stdLogger.Printf("%s - %s", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// s3Putter 抽象 put/head，便于单测注入假实现（真实实现为 *s3RealClient）。
type s3Putter interface {
	put(ctx context.Context, bucket, key string) error
	head(ctx context.Context, bucket, key string) (headResult, int64, error)
}

type s3RealClient struct{ c *s3.Client }

func (r *s3RealClient) put(ctx context.Context, bucket, key string) error {
	return putMarker(ctx, r.c, bucket, key)
}
func (r *s3RealClient) head(ctx context.Context, bucket, key string) (headResult, int64, error) {
	return headMarker(ctx, r.c, bucket, key)
}

func main() {
	var (
		bucket    = flag.String("bucket", "", "目标 S3 bucket 名（必填）")
		input     = flag.String("input", "empty_dir_markers.parquet", "输入 parquet（列 name 或 key）")
		workers   = flag.Int("workers", 256, "put-worker goroutine 数（吞吐旋钮，可扫 128/256/512）")
		producers = flag.Int("producers", 0, "parquet 解码 goroutine 数（0=CPU核数，上限=RG数）")
		region    = flag.String("region", os.Getenv("AWS_REGION"), "AWS region（默认读 AWS_REGION）")
		profile   = flag.String("profile", "", "AWS profile（默认用默认凭证链）")
		ckptPath  = flag.String("checkpoint", "markers.ckpt", "RG 级断点续传文件（空字符串禁用）")
		logPath   = flag.String("log", "transfer.log", "逐条明细日志（追加）")
		failPath  = flag.String("failed", "failed_keys.txt", "失败 key 记录")
		limit     = flag.Int64("limit", -1, "仅处理前 N 个（-1 不限，小批验证用）")
		dryRun    = flag.Bool("dry-run", false, "只打印 源<TAB>目标 映射，不写 S3")
		verifyN   = flag.Int("verify", 0, "抽样 N 个 key 做 head_object 核对")
		retryFail = flag.Bool("retry-failed", false, "重跑 failed_keys.txt 中的 key")
	)
	flag.Parse()

	if *bucket == "" {
		fmt.Fprintln(os.Stderr, "错误: --bucket 必填")
		flag.Usage()
		os.Exit(2)
	}
	if *producers == 0 {
		*producers = runtime.NumCPU()
	}

	cfg := &runConfig{
		bucket:    *bucket,
		input:     *input,
		workers:   *workers,
		producers: *producers,
		limit:     *limit,
		dryRun:    *dryRun,
		logPath:   *logPath,
		failPath:  *failPath,
		ckptPath:  *ckptPath,
	}

	// dry-run 不需 S3 client / 凭证，先处理。
	if *dryRun {
		if err := dryRunMode(context.Background(), cfg); err != nil {
			logf("dry-run 失败: %v", err)
			os.Exit(1)
		}
		return
	}

	// SIGINT/SIGTERM 优雅退出：producer 停发、worker 排空、logger flush、未完成 RG 不 markDone。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s3cli, err := newS3Client(ctx, *region, *profile, *workers)
	if err != nil {
		logf("创建 S3 client 失败: %v", err)
		os.Exit(1)
	}
	putter := &s3RealClient{c: s3cli}

	switch {
	case *verifyN > 0:
		code, err := verifyMode(ctx, cfg, putter, *verifyN)
		if err != nil {
			logf("verify 失败: %v", err)
			os.Exit(1)
		}
		os.Exit(code)
	case *retryFail:
		code, err := retryFailedMode(ctx, cfg, putter)
		if err != nil {
			logf("retry-failed 失败: %v", err)
			os.Exit(1)
		}
		os.Exit(code)
	default:
		fail, err := run(ctx, cfg, putter)
		if err != nil {
			logf("run 失败: %v", err)
			os.Exit(1)
		}
		if ctx.Err() != nil { // 被信号中断：退出码 130，便于区分真实失败与可续传中断
			logf("被信号中断，已保存断点，可重跑续传")
			os.Exit(130)
		}
		if fail > 0 {
			os.Exit(2)
		}
	}
}
