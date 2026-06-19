// feeder 是一个独立的 SQS 压测灌入器（仅测试用，不入 worker 主程序）。
//
// 从源桶列对象 → 组装 {source,destination} 消息 → 高并发 SendMessageBatch 灌入队列。
// 支持 -repeat 反复刷同一批 key（每轮目标 prefix 带轮次后缀，避免 only-SUCCESS-deletes
// 把同名消息当重复处理），用于持续制造高 QPS 负载、复现 rcd job 累积/内存问题。
//
// 用法：
//
//	go run ./tools/feeder -queue <url> -bucket <b> -prefix stress-small/ \
//	    -dst-prefix smalltest -count 20000 -conc 48 [-repeat 5]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func main() {
	var (
		queueURL  = flag.String("queue", "", "SQS 队列 URL（必填）")
		bucket    = flag.String("bucket", "", "源桶（必填）")
		prefix    = flag.String("prefix", "", "源 key 前缀，如 stress-small/")
		dstPrefix = flag.String("dst-prefix", "feedtest", "目标 key 前缀")
		count     = flag.Int("count", 10000, "最多灌多少条（单轮）")
		conc      = flag.Int("conc", 48, "并发 SendMessageBatch 协程数")
		repeat    = flag.Int("repeat", 1, "重复轮数（每轮目标 prefix 带 -rN 后缀去重）")
		region    = flag.String("region", "eu-south-2", "AWS region")
	)
	flag.Parse()
	if *queueURL == "" || *bucket == "" {
		log.Fatal("必填：-queue 和 -bucket")
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		log.Fatalf("加载 AWS 配置失败: %v", err)
	}
	s3c := s3.NewFromConfig(cfg)
	sqsc := sqs.NewFromConfig(cfg)

	// 列源 key（最多 count 个）。
	log.Printf("列源对象 s3://%s/%s ...", *bucket, *prefix)
	keys := make([]string, 0, *count)
	p := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{
		Bucket: bucket, Prefix: prefix,
	})
	for p.HasMorePages() && len(keys) < *count {
		page, err := p.NextPage(ctx)
		if err != nil {
			log.Fatalf("列对象失败: %v", err)
		}
		for _, o := range page.Contents {
			keys = append(keys, *o.Key)
			if len(keys) >= *count {
				break
			}
		}
	}
	log.Printf("拿到 %d 个 key", len(keys))
	if len(keys) == 0 {
		log.Fatal("源前缀下没有对象")
	}

	var sent atomic.Int64
	start := time.Now()
	for r := 0; r < *repeat; r++ {
		round := r
		feedRound(ctx, sqsc, *queueURL, *bucket, keys, *dstPrefix, round, *conc, &sent)
		log.Printf("轮 %d/%d 完成，累计已发 %d，用时 %s（%.0f msg/s）",
			round+1, *repeat, sent.Load(), time.Since(start).Round(time.Second),
			float64(sent.Load())/time.Since(start).Seconds())
	}
	log.Printf("全部完成：共发送 %d 条消息", sent.Load())
}

// feedRound 把 keys 切成 10 条一批，并发 SendMessageBatch 灌入队列。
func feedRound(ctx context.Context, sqsc *sqs.Client, queueURL, bucket string,
	keys []string, dstPrefix string, round, conc int, sent *atomic.Int64) {

	type batch []string
	ch := make(chan batch, conc*2)

	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range ch {
				entries := make([]types.SendMessageBatchRequestEntry, 0, len(b))
				for j, k := range b {
					rel := k
					if idx := strings.IndexByte(k, '/'); idx >= 0 {
						rel = k[idx+1:] // 去掉源前缀首段，避免目标 key 套娃
					}
					dst := fmt.Sprintf("%s/r%d/%s", dstPrefix, round, rel)
					body, err := buildBody(bucket, k, dst)
					if err != nil {
						log.Printf("buildBody 失败 key=%s: %v", k, err)
						continue
					}
					entries = append(entries, types.SendMessageBatchRequestEntry{
						Id:          aws.String(strconv.Itoa(j)),
						MessageBody: aws.String(body),
					})
				}
				if len(entries) == 0 {
					continue
				}
				out, err := sqsc.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
					QueueUrl: aws.String(queueURL),
					Entries:  entries,
				})
				if err != nil {
					log.Printf("SendMessageBatch 失败: %v", err)
					continue
				}
				if len(out.Failed) > 0 {
					log.Printf("SendMessageBatch 部分失败: failed=%d success=%d detail=%v",
						len(out.Failed), len(out.Successful), out.Failed)
				}
				sent.Add(int64(len(out.Successful)))
			}
		}()
	}

	for i := 0; i < len(keys); i += 10 {
		end := i + 10
		if end > len(keys) {
			end = len(keys)
		}
		ch <- keys[i:end]
	}
	close(ch)
	wg.Wait()
}

// buildBody 组装一条迁移消息体（与 worker message.TransferMessage 契约一致）。
// rcd 模式不支持 per-message rclone 参数，消息体只含 source/destination（op 默认 copy）。
func buildBody(bucket, srcKey, dstKey string) (string, error) {
	body := struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}{
		Source:      "s3:" + bucket + "/" + srcKey,
		Destination: "s3:" + bucket + "/" + dstKey,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
