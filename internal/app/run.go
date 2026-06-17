package app

import (
	"context"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/aws-samples/sample-data-transfer-tool/internal/emf"
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
	rcdClient := rcd.New(cfg.RCDAddr, cfg.RCDUser, cfg.RCDPass)
	if err := waitRCDReady(ctx, rcdClient, 30*time.Second); err != nil {
		return fmt.Errorf("rcd 未就绪: %w", err)
	}
	obslog.Infof("rcd 就绪 @ %s", cfg.RCDAddr)

	// 限速刷新（每 60s 从 SSM 读 → 设 rcd 全局 core/bwlimit + options/set TPSLimit）。
	// 60s 与控制器 Lambda 写频率对齐（读写同频）。
	rl := ratelimit.New(ssmClient, rcdClient, cfg.BwlimitParam, cfg.TpslimitParam)
	go rl.Run(ctx, 60*time.Second)

	// runner：同步 copyfile，ctx deadline = RCLONE_TIMEOUT（HTTP 断开即中止，防双写）。
	run := worker.NewRunner(rcdClient, worker.RunnerConfig{
		Timeout: time.Duration(cfg.RcloneTimeout) * time.Second,
	})

	store := status.NewStore(ddbClient, cfg.StatusTable, cfg.HeartbeatTable)
	stats := &worker.Stats{}

	// 心跳 + 进度日志。
	go heartbeatLoop(ctx, store, cfg.InstanceID, cfg.Workers)
	go progressLoop(ctx, stats)

	report := func(ev worker.EMFEvent) {
		_ = emf.Emit(ev, func(s string) { fmt.Println(s) })
	}

	consumer := worker.NewConsumer(sqsClient, worker.ConsumerConfig{
		QueueURL:  cfg.QueueURL,
		Receivers: cfg.Receivers,
		Workers:   cfg.Workers,
	}, cfg.InstanceID, run.RunCopy, store, report, stats)

	// systemd watchdog：consumer.Progress 推进才上报 WATCHDOG=1，僵死则停报让 systemd 重启。
	watchdog.Ready()
	go watchdog.Run(ctx, consumer.Progress)

	obslog.Infof("worker 启动: %d receivers / %d workers → %s", cfg.Receivers, cfg.Workers, cfg.QueueURL)
	consumer.Run(ctx) // 阻塞到 ctx 取消后优雅 drain
	obslog.Infof("worker 已停止 | total=%d success=%d retryable=%d fatal=%d unknown=%d poison=%d delete_fail=%d requeue_fail=%d",
		stats.Total.Load(), stats.Success.Load(), stats.Retryable.Load(), stats.Fatal.Load(),
		stats.Unknown.Load(), stats.Poison.Load(), stats.DeleteFail.Load(), stats.RequeueFail.Load())
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
			obslog.Infof("进度: total=%d success=%d retryable=%d fatal=%d unknown=%d poison=%d delete_fail=%d requeue_fail=%d",
				stats.Total.Load(), stats.Success.Load(), stats.Retryable.Load(), stats.Fatal.Load(),
				stats.Unknown.Load(), stats.Poison.Load(), stats.DeleteFail.Load(), stats.RequeueFail.Load())
		}
	}
}
