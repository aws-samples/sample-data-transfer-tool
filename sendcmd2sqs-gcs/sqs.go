package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// copyMessage 是一条 copy 操作的 SQS 消息体。
// 字段与 Python 版逐一对齐；RcloneArgs 用非 nil 空切片，确保 JSON 序列化为 [] 而非 null。
type copyMessage struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Op          string   `json:"op"`
	RcloneArgs  []string `json:"rclone_args"`
}

// buildCopyMessage 从 inventory 行组装一条 copy 消息的 JSON 字节。
// 映射: {gcsRemote}:{bucket}/{name} -> s3:{targetBucket}/{name}
// targetBucket 由调用方按源 GCS 桶经 BUCKET_MAP 查表得到（见 pipeline.go），本函数保持纯函数。
func buildCopyMessage(gcsRemote, targetBucket, bucket, name string) ([]byte, error) {
	msg := copyMessage{
		Source:      gcsRemote + ":" + bucket + "/" + name,
		Destination: "s3:" + targetBucket + "/" + name,
		Op:          "copy",
		RcloneArgs:  []string{}, // 必须非 nil，JSON 输出为 [] 与 Python 一致
	}
	return json.Marshal(msg)
}

// buildDeleteMessage 组装一条 delete 消息的 JSON 字节（增量同步中删除 S3 上多余的对象，
// 对应 diff 的 DiffFlag=2/Minus：目标 S3 有而源 GCS 无）。
// 字段集与 copyMessage 完全一致，便于下游消费端用同一 struct 按 op 分派：
// source 留空（delete 无源），destination=s3:{s3Bucket}/{key}，op=delete。
// 注意：需下游 rclone 消费端支持 op=delete（deletefile 语义）；只认 copy 的消费端会忽略/报错。
func buildDeleteMessage(s3Bucket, key string) ([]byte, error) {
	msg := copyMessage{
		Source:      "",
		Destination: "s3:" + s3Bucket + "/" + key,
		Op:          "delete",
		RcloneArgs:  []string{}, // 同 copy：必须非 nil，JSON 输出为 [] 而非 null
	}
	return json.Marshal(msg)
}

// newSQSClient 创建 SQS 客户端，并调大 HTTP 连接池以支撑高并发 sender。
//
// aws-sdk-go-v2 默认 http.Transport 的 MaxIdleConnsPerHost=10；senders 达数百时
// 必须放大连接池，否则连接复用不足、TLS 握手开销吃掉并发收益（等价 Python 的
// max_pool_connections 坑）。
func newSQSClient(ctx context.Context, region string, senders int) (*sqs.Client, error) {
	maxConns := senders + 50

	// 直接配置 SDK 提供的 *http.Transport（不可整体复制，含 sync.Mutex）。
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.Proxy = http.ProxyFromEnvironment
		t.MaxIdleConns = 0 // 不限总空闲连接
		t.MaxIdleConnsPerHost = maxConns
		t.MaxConnsPerHost = 0 // 不限每主机连接
		t.IdleConnTimeout = 90 * time.Second
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ExpectContinueTimeout = 1 * time.Second
		t.ForceAttemptHTTP2 = true
		t.WriteBufferSize = 64 * 1024
		t.ReadBufferSize = 64 * 1024
	})

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败: %w", err)
	}
	return sqs.NewFromConfig(cfg), nil
}

// batchFailSampleMax 是失败明细的限量打印上限（防海量失败刷屏）；超过后只计数不打印。
const batchFailSampleMax = 30

var batchFailSampled atomic.Int64

// sampleBatchFailures 限量打印 SQS 批量发送失败条目的真实原因（Code / SenderFault / Message 摘要）。
// SenderFault=true 是客户端错误（消息体过大/格式非法等，重试无用）；false 是服务端/限流（可重试）。
// 全局上限 batchFailSampleMax 条，避免刷屏；用于诊断"为何失败"。
func sampleBatchFailures(failed []types.BatchResultErrorEntry) {
	for _, f := range failed {
		if batchFailSampled.Load() >= batchFailSampleMax {
			return
		}
		batchFailSampled.Add(1)
		code, msg := "", ""
		if f.Code != nil {
			code = *f.Code
		}
		if f.Message != nil {
			msg = *f.Message
		}
		if len(msg) > 200 { // Message 可能很长，截断
			msg = msg[:200]
		}
		logf("[发送失败明细] Code=%q SenderFault=%t Message=%q", code, f.SenderFault, msg)
	}
}

// sendBatch 通过 SendMessageBatch 发送一批消息（最多 SQSMaxBatch 条），处理部分失败重试。
// 只重试失败的条目，最多 SQSBatchMaxRetries 次，指数退避。返回 (成功数, 失败数)。
func sendBatch(ctx context.Context, client *sqs.Client, queueURL string, msgs [][]byte) (ok, failed int) {
	if len(msgs) == 0 {
		return 0, 0
	}

	// entries 一次性构建：Id=批内序号、body 仅在此 marshal 一次。
	// 重试时原地保留失败条目（截断复用底层数组），不重新分配、不重新 marshal。
	entries := make([]types.SendMessageBatchRequestEntry, len(msgs))
	for i, m := range msgs {
		entries[i] = types.SendMessageBatchRequestEntry{
			Id:          aws.String(strconv.Itoa(i)),
			MessageBody: aws.String(string(m)),
		}
	}
	okCount := 0
	hardFailed := 0 // 无法重试的硬失败累计（如 Failed 条目 Id 为 nil），跨 attempt 持续累加

	for attempt := 0; attempt < SQSBatchMaxRetries; attempt++ {
		out, err := client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
			QueueUrl: aws.String(queueURL),
			Entries:  entries,
		})
		if err != nil {
			// 整批调用失败（网络/限流/取消）：退避后重试整批。
			if ctx.Err() != nil {
				// context 取消，放弃剩余
				return okCount, len(entries) + hardFailed
			}
			logf("SendMessageBatch 调用失败（第%d次）: %v", attempt+1, err)
			sleepBackoff(ctx, attempt)
			continue
		}

		okCount += len(out.Successful)
		// 逐条记录已确认发送成功的消息（--log-sent；sentLogger==nil 时 logSent 直接返回，零开销）。
		// out.Successful[].Id 是构建 entries 时的批内序号（strconv.Itoa(i)），回索引原始 msgs[i]——
		// 重试只截断 entries，msgs 原数组不变，故 Id 始终指向正确的原始消息体。
		if sentLogger != nil {
			for _, sm := range out.Successful {
				if sm.Id == nil {
					continue // SQS 契约保证回显 Id，但指针型；nil 则无法定位原始消息
				}
				idx, aerr := strconv.Atoi(*sm.Id)
				if aerr != nil || idx < 0 || idx >= len(msgs) {
					continue // 解析失败或越界：跳过（防御，正常不会发生）
				}
				sentLogger.log(msgs[idx])
			}
		}
		if len(out.Failed) == 0 {
			return okCount, hardFailed
		}

		// 防御：SQS 契约保证 Failed[].Id 回显请求 Id，但其为指针类型。
		// 若出现 nil Id（SDK/代理异常），该失败条目无法定位重试，直接计为硬失败，
		// 绝不让它从 ok/failed 两侧同时漏掉（否则会静默丢消息并误标 shard 完成）。
		failedIDs := make(map[string]struct{}, len(out.Failed))
		for _, f := range out.Failed {
			if f.Id != nil {
				failedIDs[*f.Id] = struct{}{}
			} else {
				hardFailed++
			}
		}
		// 诊断：限量打印失败明细（Code/SenderFault/Message），用事实定位失败根因。
		sampleBatchFailures(out.Failed)

		// 原地保留可重试的失败条目：把 Id 命中 failedIDs 的 entry 移到前段，截断 entries。
		kept := entries[:0]
		for _, e := range entries {
			if _, bad := failedIDs[*e.Id]; bad {
				kept = append(kept, e)
			}
		}
		entries = kept
		if len(entries) == 0 {
			// 无可重试条目（失败条目都无 Id），立即返回累计的硬失败。
			return okCount, hardFailed
		}
		logf("批量发送有 %d 条失败，准备重试（第%d/%d次）", len(entries), attempt+1, SQSBatchMaxRetries)
		sleepBackoff(ctx, attempt)
	}

	failedCount := len(entries) + hardFailed
	if failedCount > 0 {
		logf("批量发送最终失败 %d 条，已放弃", failedCount)
	}
	return okCount, failedCount
}

// sleepBackoff 指数退避：SQSRetryWaitBase * 2^attempt 秒，可被 context 取消打断。
func sleepBackoff(ctx context.Context, attempt int) {
	d := time.Duration(SQSRetryWaitBase) * time.Second * (1 << attempt)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
