// gcs-event-bridge —— 跨云事件桥：消费 GCP Pub/Sub 的 GCS 对象事件，映射成迁移消息
// 投递到 AWS SQS（喂给现有 rclone 迁移 worker 管道）。
//
// 链路：GCS bucket notification → Pub/Sub topic → subscription
//
//	→ 本程序 StreamingPull 消费 → 事件映射 → SQS SendMessageBatch → ack
//
// 设计要点：
//   - 多 pipeline 并发：一个进程跑 N 条独立管道（多源），各自 client/凭证/SQS/故障隔离；
//   - SA JSON key 存 Secrets Manager，配置只放 ARN（绝不内联密钥）；
//   - 事件映射：OBJECT_FINALIZE / OBJECT_METADATA_UPDATE → copy，其余事件跳过；
//   - 先发后 ack：投 SQS 成功才 ack Pub/Sub，崩溃时未 ack 消息自动重投（at-least-once，
//     消费侧幂等——与现有迁移管道哲学一致）；
//   - StreamingPull 拉取端 + 应用层攒批 10 条投 SQS（默认 send_workers=64）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub/v2"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"google.golang.org/api/option"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（YAML）")
	logFile := flag.String("log-file", "", "日志落盘路径（同时输出控制台），自动按大小轮转并删除过期日志")
	logMaxSizeMB := flag.Int("log-max-size-mb", 100, "单个日志文件最大 MB，超过即切割轮转")
	logMaxBackups := flag.Int("log-max-backups", 7, "保留的轮转旧日志份数（超出最旧的删除）")
	logMaxAgeDays := flag.Int("log-max-age-days", 14, "轮转旧日志最长保留天数（超过删除）")
	logCompress := flag.Bool("log-compress", true, "轮转的旧日志是否 gzip 压缩")
	flag.Parse()

	if *logFile != "" {
		// lumberjack 按大小切割 + 保留份数/天数自动删旧日志（生产级轮转，避免撑爆磁盘）。
		// 同时写 stderr（journald/控制台可见）与轮转文件。
		lj := &lumberjack.Logger{
			Filename:   *logFile,
			MaxSize:    *logMaxSizeMB,
			MaxBackups: *logMaxBackups,
			MaxAge:     *logMaxAgeDays,
			Compress:   *logCompress,
		}
		defer lj.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, lj))
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "必须指定 --config")
		os.Exit(2)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	// 优雅停机：SIGTERM/SIGINT → cancel ctx → 各 pipeline 停止 Receive、发完在途批次。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryMaxAttempts(10),
	)
	if err != nil {
		log.Fatalf("加载 AWS 配置失败: %v", err)
	}
	smClient := secretsmanager.NewFromConfig(awsCfg)
	sqsClient := sqs.NewFromConfig(awsCfg)

	log.Printf("启动 gcs-event-bridge：region=%s，%d 条 pipeline", cfg.Region, len(cfg.Pipelines))

	var wg sync.WaitGroup
	for _, p := range cfg.Pipelines {
		wg.Add(1)
		go func(p Pipeline) {
			defer wg.Done()
			if err := runPipeline(ctx, p, smClient, sqsClient); err != nil {
				// 单条 pipeline 失败不拖垮其它（故障隔离）；记错误，该管道退出。
				log.Printf("pipeline %q 退出: %v", p.Name, err)
			}
		}(p)
	}
	wg.Wait()
	log.Printf("gcs-event-bridge 已停止")
}

// smGetter 抽象 Secrets Manager 取值，便于测试。
type smGetter interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// runPipeline 跑单条 pipeline：取 SA 凭证 → 建 Pub/Sub client → Receive → 攒批投 SQS。
func runPipeline(ctx context.Context, p Pipeline, sm smGetter, sqsClient sqsSender) error {
	// 1. 从 Secrets Manager 取 GCP SA JSON key（配置只存 ARN）。
	saJSON, err := fetchSAKey(ctx, sm, p.Source.SASecretARN)
	if err != nil {
		return fmt.Errorf("取 SA 凭证: %w", err)
	}

	// 2. 建 Pub/Sub client（用 SA key 认证）。
	// project id：显式 project_id 优先；缺省从订阅完整路径解析；都拿不到则 fail-fast
	// （否则空 projectID 传给 pubsub.NewClient 会得到不直观的下游错误）。
	projectID, err := resolveProjectID(p.Source.ProjectID, p.Source.Subscription)
	if err != nil {
		return err
	}
	psClient, err := pubsub.NewClient(ctx, projectID, option.WithCredentialsJSON(saJSON))
	if err != nil {
		return fmt.Errorf("建 Pub/Sub client: %w", err)
	}
	defer psClient.Close()

	// v2 API：client.Subscriber(name) 取代 v1 的 client.Subscription(name)；
	// 接受完整订阅路径 projects/<proj>/subscriptions/<sub> 或短 id。
	sub := psClient.Subscriber(p.Source.Subscription)
	sub.ReceiveSettings.NumGoroutines = p.Tuning.NumGoroutines
	sub.ReceiveSettings.MaxOutstandingMessages = p.Tuning.MaxOutstandingMessages
	// StreamingPull 默认；MaxOutstandingBytes 留默认 1GB。

	// 3. 攒批投递通道 + send worker 池。
	st := &bridgeStats{}
	inCh := make(chan inboundMessage, p.Tuning.SendWorkers*sqsBatchSize)
	var sendWG sync.WaitGroup
	for i := 0; i < p.Tuning.SendWorkers; i++ {
		sendWG.Add(1)
		go batchSender(ctx, sqsClient, p.Dest.QueueURL, inCh, st, &sendWG)
	}

	// 4. 定时打印进度。
	stopProgress := make(chan struct{})
	go progressLoop(p.Name, st, stopProgress)

	// 5. Receive：逐条回调 → 映射 → 入 channel（ack 推迟到 SQS 投递成功）。
	log.Printf("pipeline %q 开始消费: sub=%s → %s (workers=%d)",
		p.Name, p.Source.Subscription, p.Dest.QueueURL, p.Tuning.SendWorkers)
	recvErr := sub.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		handleMessage(m, p.Dest, st, inCh)
	})

	// 6. 收尾：Receive 返回后关 channel，等 send worker 发完在途批次。
	close(inCh)
	sendWG.Wait()
	close(stopProgress)
	logFinal(p.Name, st)
	return recvErr
}

// handleMessage 单条 Pub/Sub 消息处理：映射 + 入投递通道。ack/nack 推迟到投递结果。
func handleMessage(m *pubsub.Message, dest Dest, st *bridgeStats, inCh chan<- inboundMessage) {
	mapped, err := mapEvent(m.Attributes, m.Data, dest)
	// mapEvent 失败时拿不到 mapped.Bucket，从 attributes 兜底取 bucketId 做分类。
	bucket := mapped.Bucket
	if bucket == "" {
		bucket = m.Attributes["bucketId"]
	}
	st.bump(bucket, cReceived)
	if err != nil {
		st.bump(bucket, cMapErrors)
		m.Nack() // 映射失败：nack，Pub/Sub 重投（或最终进 dead-letter）
		return
	}
	if mapped.skip {
		st.bump(bucket, cSkipped)
		m.Ack() // 非关注事件：直接 ack 丢弃，不占订阅
		return
	}
	if mapped.unknownBkt {
		// 源桶不在映射表（或前缀无命中且无兜底）：ack 丢弃 + 单独计数告警。
		// 每条都输出（不限流），便于排查哪些 key 未配映射；磁盘占用由 --log-file 轮转兜底。
		st.bump(bucket, cUnknownBkt)
		log.Printf("未映射的源桶事件已跳过: %s", mapped.unknownInfo)
		m.Ack()
		return
	}
	// 投递通道：ack 推迟到 batchSender 投 SQS 成功后调用（先发后 ack）。bucket 透传供分类。
	inCh <- inboundMessage{mapped: mapped, bucket: bucket, ackFn: m.Ack, nackFn: m.Nack}
}

// fetchSAKey 从 Secrets Manager 取 SA JSON（SecretString）。
func fetchSAKey(ctx context.Context, sm smGetter, arn string) ([]byte, error) {
	out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
	if err != nil {
		return nil, err
	}
	if out.SecretString == nil {
		return nil, fmt.Errorf("secret %s 无 SecretString", arn)
	}
	return []byte(*out.SecretString), nil
}

// projectFromSubscription 从 projects/<proj>/subscriptions/<sub> 解析 project id。
// 短订阅名（无 projects/ 前缀）解析不出，返回空串。
func projectFromSubscription(full string) string {
	parts := strings.Split(full, "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

// resolveProjectID 定 GCP project id：显式 explicitID 优先；为空则从订阅完整路径解析；
// 两者都拿不到 → fail-fast 报错（不把空 projectID 传给 pubsub.NewClient 得不直观的下游错误）。
func resolveProjectID(explicitID, subscription string) (string, error) {
	if explicitID != "" {
		return explicitID, nil
	}
	if id := projectFromSubscription(subscription); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("无法确定 GCP project id：subscription %q 非完整路径（projects/<proj>/subscriptions/<sub>），请在配置显式填 project_id", subscription)
}

func progressLoop(name string, st *bridgeStats, stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			logStats(name, "进度", st)
		case <-stop:
			return
		}
	}
}

// logStats 打印 pipeline 总计 + 按源桶分类的计数（多桶共用订阅时一桶一行）。
func logStats(name, tag string, st *bridgeStats) {
	log.Printf("pipeline %q %s[总计]: 收 %d / 投 %d / 跳过 %d / 未映射桶 %d / 映射错 %d / 投递失败 %d",
		name, tag, ld(&st.received), ld(&st.sent), ld(&st.skipped),
		ld(&st.unknownBkt), ld(&st.mapErrors), ld(&st.sendFails))
	st.byBucket.Range(func(k, v any) bool {
		bc := v.(*bucketCounters)
		log.Printf("pipeline %q %s[桶 %s]: 收 %d / 投 %d / 跳过 %d / 未映射 %d / 映射错 %d / 投递失败 %d",
			name, tag, k.(string),
			ld(&bc.received), ld(&bc.sent), ld(&bc.skipped),
			ld(&bc.unknownBkt), ld(&bc.mapErrors), ld(&bc.sendFails))
		return true
	})
}

func logFinal(name string, st *bridgeStats) {
	logStats(name, "汇总", st)
}
