package app

import (
	"context"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
	"github.com/aws-samples/sample-data-transfer-tool/internal/ratelimit"
	"github.com/aws-samples/sample-data-transfer-tool/internal/rcd"
	"github.com/aws-samples/sample-data-transfer-tool/internal/status"
	"github.com/aws-samples/sample-data-transfer-tool/internal/watchdog"
	"github.com/aws-samples/sample-data-transfer-tool/internal/worker"
)

// Run 组装并运行 worker，阻塞到 ctx 取消（SIGTERM/SIGINT）后优雅停机。
func Run(ctx context.Context, cfg Config) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryMaxAttempts(10),
	)
	if err != nil {
		return fmt.Errorf("加载 AWS 配置: %w", err)
	}
	sqsClient := sqs.NewFromConfig(awsCfg)
	ddbClient := dynamodb.NewFromConfig(awsCfg)
	ssmClient := ssm.NewFromConfig(awsCfg)

	// rcd 客户端 + 就绪探测（最多等 30s，rcd 由 systemd 先拉起）。
	// 连接池上限 = Workers + Receivers + 余量：所有 goroutine 打同一 localhost host，
	// 池太小（默认 2）会让 stats 等短调用连接反复重建。
	rcdClient := rcd.New(cfg.RCDAddr, cfg.RCDUser, cfg.RCDPass, cfg.Workers+cfg.Receivers+4)
	if err := waitRCDReady(ctx, rcdClient, 30*time.Second); err != nil {
		return fmt.Errorf("rcd 未就绪: %w", err)
	}
	obslog.Infof("rcd 就绪 @ %s", cfg.RCDAddr)

	// 限速：启动时从 SSM 读一次固定上限（bwlimit/tpslimit）设给 rcd 全局，之后不再变动。
	// 无 AIMD 控制器 Lambda、无周期重读——每机 rcd 一个固定上限；调整靠改 SSM + 滚动实例。
	// fail-fast（硬红线）：读/设失败 → 启动中止。限速是必须的红线,漏限=打爆源端配额/429,
	// 宁可实例起不来也不裸奔无 cap。仅当 SSM 明确为 off/空(运维有意不限)才放行。
	if err := ratelimit.New(ssmClient, rcdClient, cfg.BwlimitParam, cfg.TpslimitParam).ApplyOnce(ctx); err != nil {
		return fmt.Errorf("限速初始化失败（fail-fast,拒绝无 cap 裸奔）: %w", err)
	}

	// runner：同步 copyfile，ctx deadline = RCLONE_TIMEOUT（HTTP 断开即中止，防双写）。
	// GroupPoolSize=Workers：每个并发传输借一个独立 stats group，group 总数恒定=Workers，
	// rcd 的 StatsInfo 数封顶（消灭 per-call 唯一 group 导致的 StatsInfo 无界泄漏 → OOM）。
	run := worker.NewRunner(rcdClient, worker.RunnerConfig{
		Timeout:       time.Duration(cfg.RcloneTimeout) * time.Second,
		GroupPoolSize: cfg.Workers,
	})

	store := status.NewStore(ddbClient, cfg.StatusTable, cfg.HeartbeatTable)
	stats := &worker.Stats{}

	// 心跳 + 进度日志。
	go heartbeatLoop(ctx, store, cfg.InstanceID, cfg.Workers)
	go progressLoop(ctx, stats)

	// 无 EMF 指标路径：四态/失败计数经 progressLoop + 停机日志打到 worker-ops,DDB 存终态,
	// 省 CloudWatch 摄入+指标费。观测靠 ops 日志 + DDB,不再有 stdout EMF。
	consumer := worker.NewConsumer(sqsClient, worker.ConsumerConfig{
		QueueURL:  cfg.QueueURL,
		Receivers: cfg.Receivers,
		Workers:   cfg.Workers,
	}, cfg.InstanceID, run.RunCopy, store, stats)

	// systemd watchdog：轮询活性(consumer.Progress) OR 传输活性(rcd 全局字节)推进才上报
	// WATCHDOG=1;两信号连续多轮全冻结(真僵死/worker 全卡死在 rcd HTTP)才停报让 systemd 重启。
	// activityProbe 返回 rcd 全局活性指纹(bytes+transfers+errors)与探测是否成功。
	watchdog.Ready()
	go watchdog.Run(ctx, consumer.Progress, func(pctx context.Context) (int64, bool) {
		gs, err := rcdClient.GlobalStats(pctx)
		if err != nil {
			return 0, false
		}
		return gs.Bytes + gs.Transfers + gs.Errors, true
	})

	obslog.Infof("worker 启动: %d receivers / %d workers / rclone_transfers=%d → %s",
		cfg.Receivers, cfg.Workers, cfg.RcloneTransfers, cfg.QueueURL)
	consumer.Run(ctx) // 阻塞到 ctx 取消后优雅 drain
	obslog.Infof("worker 已停止 | total=%d success=%d retryable=%d fatal=%d unknown=%d poison=%d record_fail=%d delete_fail=%d requeue_fail=%d",
		stats.Total.Load(), stats.Success.Load(), stats.Retryable.Load(), stats.Fatal.Load(),
		stats.Unknown.Load(), stats.Poison.Load(), stats.RecordFail.Load(), stats.DeleteFail.Load(), stats.RequeueFail.Load())
	return nil
}

func waitRCDReady(ctx context.Context, c *rcd.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := c.Noop(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("等待 %s 超时", timeout)
}

func heartbeatLoop(ctx context.Context, store *status.Store, instanceID string, workers int) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	write := func() {
		hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		now := time.Now()
		_ = store.WriteHeartbeat(hbCtx, instanceID, status.NowISO(), now.Unix(), workers)
	}
	write()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

func progressLoop(ctx context.Context, stats *worker.Stats) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			obslog.Infof("进度: total=%d success=%d retryable=%d fatal=%d unknown=%d poison=%d record_fail=%d delete_fail=%d requeue_fail=%d",
				stats.Total.Load(), stats.Success.Load(), stats.Retryable.Load(), stats.Fatal.Load(),
				stats.Unknown.Load(), stats.Poison.Load(), stats.RecordFail.Load(), stats.DeleteFail.Load(), stats.RequeueFail.Load())
		}
	}
}
