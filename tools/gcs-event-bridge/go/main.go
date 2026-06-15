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
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（YAML）")
	logFile := flag.String("log-file", "", "日志落盘路径（同时输出控制台）")
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
	projectID := p.Source.ProjectID
	if projectID == "" {
		projectID = projectFromSubscription(p.Source.Subscription)
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
	atomicAdd(&st.received)
	mapped, err := mapEvent(m.Attributes, m.Data, dest)
	if err != nil {
		atomicAdd(&st.mapErrors)
		m.Nack() // 映射失败：nack，Pub/Sub 重投（或最终进 dead-letter）
		return
	}
	if mapped.skip {
		atomicAdd(&st.skipped)
		m.Ack() // 非关注事件：直接 ack 丢弃，不占订阅
		return
	}
	// 投递通道：ack 推迟到 batchSender 投 SQS 成功后调用（先发后 ack）。
	inCh <- inboundMessage{mapped: mapped, ackFn: m.Ack, nackFn: m.Nack}
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
func projectFromSubscription(full string) string {
	parts := strings.Split(full, "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

// subscriptionID 取订阅短 id（pubsub.Subscription 要短名，不要完整路径）。
func subscriptionID(full string) string {
	parts := strings.Split(full, "/")
	return parts[len(parts)-1]
}

func progressLoop(name string, st *bridgeStats, stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			log.Printf("pipeline %q 进度: 收 %d / 投 %d / 跳过 %d / 映射错 %d / 投递失败 %d",
				name, ldAdd(&st.received, 0), ldAdd(&st.sent, 0), ldAdd(&st.skipped, 0),
				ldAdd(&st.mapErrors, 0), ldAdd(&st.sendFails, 0))
		case <-stop:
			return
		}
	}
}

func logFinal(name string, st *bridgeStats) {
	log.Printf("pipeline %q 汇总: 收 %d / 投 %d / 跳过 %d / 映射错 %d / 投递失败 %d",
		name, ldAdd(&st.received, 0), ldAdd(&st.sent, 0), ldAdd(&st.skipped, 0),
		ldAdd(&st.mapErrors, 0), ldAdd(&st.sendFails, 0))
}

// dualWriter 日志同时写控制台和文件。
type dualWriter struct{ console, file *os.File }

func (w *dualWriter) Write(p []byte) (int, error) {
	w.console.Write(p)
	return w.file.Write(p)
}
