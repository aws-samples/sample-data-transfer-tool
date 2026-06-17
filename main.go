// Command go-worker 是基于 rclone rcd 守护进程的 GCS/S3→S3 迁移消费者。
//
// 与 Python worker 的关键差异：单进程多 goroutine（无 GIL，取代多进程），所有
// 传输走常驻 rcd 的 operations/copyfile（复用连接池，消灭每文件 fork+TLS 开销），
// job/stop 替代 killpg 防双写。四态/make_pk/限速/EMF/心跳语义与 Python 对齐。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"

	"github.com/aws-samples/sample-data-transfer-tool/internal/app"
	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// opsLogMaxBytes/opsLogKeep 对齐 Python RotatingFileHandler（50MB×5）。
const (
	opsLogMaxBytes = 50 * 1024 * 1024
	opsLogKeep     = 5
)

func main() {
	cfg, err := app.FromEnv(app.Getenv)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = resolveInstanceID()
	}
	// 打开 worker-ops 分级日志（WARNING+ 落盘 → CW agent 采集）。EMF 仍走 stdout，互不干扰。
	_ = obslog.Setup(cfg.OpsLogPath, opsLogMaxBytes, opsLogKeep)
	obslog.Infof("instance_id=%s region=%s", cfg.InstanceID, cfg.Region)

	// SIGTERM/SIGINT → cancel ctx → 优雅 drain（停拉取 + 在途消息 visibility=0 重投）。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := app.Run(ctx, cfg); err != nil {
		log.Fatalf("worker 退出: %v", err)
	}
}

// resolveInstanceID 从 IMDS 取 EC2 实例 id；失败退回 hostname（本地/非 EC2）。
func resolveInstanceID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err == nil {
		client := imds.NewFromConfig(awsCfg)
		if out, e := client.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-id"}); e == nil {
			defer out.Content.Close()
			buf := make([]byte, 64)
			n, _ := out.Content.Read(buf)
			if n > 0 {
				return string(buf[:n])
			}
		}
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return host
}
