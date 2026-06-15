package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// fakeSender 可注入失败的 SQS 桩，记录发送内容。
type fakeSender struct {
	mu       sync.Mutex
	sent     []string // 收到的 MessageBody
	failAll  bool     // 整批失败
	failIDs  map[string]bool
	gotAttrs []map[string]sqstypes.MessageAttributeValue
}

func (f *fakeSender) SendMessageBatch(_ context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll {
		return nil, errors.New("boom")
	}
	out := &sqs.SendMessageBatchOutput{}
	for _, e := range in.Entries {
		if f.failIDs[aws.ToString(e.Id)] {
			out.Failed = append(out.Failed, sqstypes.BatchResultErrorEntry{Id: e.Id, Code: aws.String("X")})
			continue
		}
		f.sent = append(f.sent, aws.ToString(e.MessageBody))
		f.gotAttrs = append(f.gotAttrs, e.MessageAttributes)
		out.Successful = append(out.Successful, sqstypes.SendMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func mkInbound(body string, size int64, acked, nacked *int64) inboundMessage {
	var amu sync.Mutex
	return inboundMessage{
		mapped: mappedMessage{Body: body, ObjectSize: size},
		ackFn:  func() { amu.Lock(); *acked++; amu.Unlock() },
		nackFn: func() { amu.Lock(); *nacked++; amu.Unlock() },
	}
}

func TestSendBatchAcksOnSuccess(t *testing.T) {
	f := &fakeSender{}
	st := &bridgeStats{}
	var acked, nacked int64
	batch := []inboundMessage{
		mkInbound(`{"source":"gcs:b/1"}`, 100, &acked, &nacked),
		mkInbound(`{"source":"gcs:b/2"}`, 0, &acked, &nacked),
	}
	sendBatch(context.Background(), f, "q", batch, st)
	if acked != 2 || nacked != 0 {
		t.Fatalf("acked=%d nacked=%d want 2/0", acked, nacked)
	}
	if st.sent != 2 {
		t.Fatalf("sent=%d want 2", st.sent)
	}
	if len(f.sent) != 2 {
		t.Fatalf("SQS 收到 %d 条 want 2", len(f.sent))
	}
}

func TestSendBatchObjectSizeAttribute(t *testing.T) {
	f := &fakeSender{}
	st := &bridgeStats{}
	var a, n int64
	// 第一条 size>0 带 object_size 属性，第二条 size=0 无属性（边界：size 缺失/未知）
	batch := []inboundMessage{
		mkInbound(`{"source":"gcs:b/1"}`, 2048, &a, &n),
		mkInbound(`{"source":"gcs:b/2"}`, 0, &a, &n),
	}
	sendBatch(context.Background(), f, "q", batch, st)
	// gotAttrs 顺序与成功发送顺序一致
	if f.gotAttrs[0]["object_size"].StringValue == nil || *f.gotAttrs[0]["object_size"].StringValue != "2048" {
		t.Errorf("size>0 应带 object_size=2048 属性")
	}
	if f.gotAttrs[1] != nil {
		t.Errorf("size=0 不应带属性，got %v", f.gotAttrs[1])
	}
}

func TestSendBatchNacksWholeOnError(t *testing.T) {
	f := &fakeSender{failAll: true}
	st := &bridgeStats{}
	var acked, nacked int64
	batch := []inboundMessage{
		mkInbound(`{}`, 0, &acked, &nacked),
		mkInbound(`{}`, 0, &acked, &nacked),
	}
	sendBatch(context.Background(), f, "q", batch, st)
	// 先发后 ack：整批失败 → 全 nack，一条都不 ack（不丢，Pub/Sub 重投）
	if acked != 0 || nacked != 2 {
		t.Fatalf("整批失败应全 nack：acked=%d nacked=%d want 0/2", acked, nacked)
	}
	if st.sendFails != 2 {
		t.Fatalf("sendFails=%d want 2", st.sendFails)
	}
}

func TestSendBatchPartialFailure(t *testing.T) {
	f := &fakeSender{failIDs: map[string]bool{"1": true}} // batch 下标 1 失败
	st := &bridgeStats{}
	var acked, nacked int64
	batch := []inboundMessage{
		mkInbound(`{"k":0}`, 0, &acked, &nacked),
		mkInbound(`{"k":1}`, 0, &acked, &nacked),
		mkInbound(`{"k":2}`, 0, &acked, &nacked),
	}
	sendBatch(context.Background(), f, "q", batch, st)
	if acked != 2 || nacked != 1 {
		t.Fatalf("部分失败：acked=%d nacked=%d want 2/1", acked, nacked)
	}
	if st.sent != 2 || st.sendFails != 1 {
		t.Fatalf("sent=%d sendFails=%d want 2/1", st.sent, st.sendFails)
	}
}
