package main

import (
	"context"
	"fmt"
	"testing"

	"cloud.google.com/go/pubsub/v2"
)

// mkPubsubMsg 造一条假 Pub/Sub 消息（attributes + JSON_API_V1 payload）。
// ackh 为 nil → Ack/Nack 是 no-op，端到端测试只关注路由 + 计数，不依赖真实 ack。
func mkPubsubMsg(eventType, bucket, key, size string) *pubsub.Message {
	return &pubsub.Message{
		Attributes: map[string]string{"eventType": eventType, "bucketId": bucket, "objectId": key},
		Data:       []byte(fmt.Sprintf(`{"size":%q}`, size)),
	}
}

// wantRoute 解析 SQS body，断言 source/destination 与预期一致。
func wantRoute(t *testing.T, body, wantSource, wantDest string) {
	t.Helper()
	m := mustMsg(t, body)
	if m.Source != wantSource {
		t.Errorf("source=%q want %q", m.Source, wantSource)
	}
	if m.Destination != wantDest {
		t.Errorf("destination=%q want %q", m.Destination, wantDest)
	}
}

// runPipelineMsgs 把假消息逐条过 handleMessage，drain 投递通道后一批投 SQS。
// 返回 fakeSender（含 sent body）与 bridgeStats（含 per-bucket 计数）。
func runPipelineMsgs(msgs []*pubsub.Message) (*fakeSender, *bridgeStats, []inboundMessage) {
	f := &fakeSender{}
	st := &bridgeStats{}
	inCh := make(chan inboundMessage, len(msgs))
	for _, m := range msgs {
		handleMessage(m, testDest, st, inCh)
	}
	close(inCh)
	var batch []inboundMessage
	for m := range inCh {
		batch = append(batch, m)
	}
	sendBatch(context.Background(), f, "q", batch, st)
	return f, st, batch
}

func countBuckets(st *bridgeStats) int {
	n := 0
	st.byBucket.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestSingleBucketMultiPrefixE2E 同一源桶、不同 key 前缀的多条假消息走完整管线：
// 验证 ① 各自按「最长前缀 / 兜底」路由到正确 S3 桶（保留完整 key）；
// ② per-bucket 统计全部归到这一个源桶（received=sent=4），不因目标桶不同而散开。
func TestSingleBucketMultiPrefixE2E(t *testing.T) {
	msgs := []*pubsub.Message{
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "hot/2026/jan/x.orc", "100"), // 最长前缀 hot/2026/
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "hot/feb/y.orc", "200"),      // 短前缀 hot/
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "cold/archive/z.orc", "300"), // cold/
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "warm/w.orc", "400"),         // 不命中 → 兜底 s3-misc
	}
	f, st, batch := runPipelineMsgs(msgs)

	if len(batch) != 4 {
		t.Fatalf("应有 4 条进投递通道，got %d", len(batch))
	}
	if len(f.sent) != 4 {
		t.Fatalf("SQS 应收到 4 条，got %d", len(f.sent))
	}
	// 路由逐条核对（source 全是同一源桶，destination 各自不同目标桶）
	wantRoute(t, f.sent[0], "gcs:special-shared-bucket/hot/2026/jan/x.orc", "s3:s3-hot-2026/hot/2026/jan/x.orc")
	wantRoute(t, f.sent[1], "gcs:special-shared-bucket/hot/feb/y.orc", "s3:s3-hot/hot/feb/y.orc")
	wantRoute(t, f.sent[2], "gcs:special-shared-bucket/cold/archive/z.orc", "s3:s3-cold/cold/archive/z.orc")
	wantRoute(t, f.sent[3], "gcs:special-shared-bucket/warm/w.orc", "s3:s3-misc/warm/w.orc")

	// per-bucket：4 条不同 prefix / 不同目标桶，统计仍归到同一个源桶
	if n := countBuckets(st); n != 1 {
		t.Fatalf("per-bucket 应只有 1 个源桶 entry，got %d", n)
	}
	bc := st.bucketFor("special-shared-bucket")
	if ld(&bc.received) != 4 || ld(&bc.sent) != 4 {
		t.Errorf("源桶计数 received=%d sent=%d want 4/4", ld(&bc.received), ld(&bc.sent))
	}
	if ld(&st.received) != 4 || ld(&st.sent) != 4 {
		t.Errorf("总计 received=%d sent=%d want 4/4", ld(&st.received), ld(&st.sent))
	}
}

// TestPerBucketStatsSplitByBucket 一批消息混多个源桶（共用一个订阅的真实场景）：
// 验证 per-bucket 统计严格按「消息的源桶」拆分——special-shared-bucket（前缀路由）、
// mallfile-gcp（strip_prefix）、never-configured-bucket（unknownBkt，不投 SQS 但按桶计数）。
func TestPerBucketStatsSplitByBucket(t *testing.T) {
	msgs := []*pubsub.Message{
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "hot/a.orc", "1"),  // → s3-hot
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "cold/b.orc", "1"), // → s3-cold
		mkPubsubMsg(eventFinalize, "special-shared-bucket", "warm/c.orc", "1"), // → 兜底 s3-misc
		mkPubsubMsg(eventFinalize, "mallfile-gcp", "gcsprodorms/o1.json", "1"), // strip → s3euprodorms/o1.json
		mkPubsubMsg(eventFinalize, "mallfile-gcp", "riskgcp/r1.json", "1"),     // strip → s3euprodrisk1/r1.json
		mkPubsubMsg(eventFinalize, "never-configured-bucket", "k.bin", "1"),    // 未配 → unknownBkt
	}
	f, st, batch := runPipelineMsgs(msgs)

	// unknown 桶不进投递通道（直接 ack 丢弃 + 计数告警）
	if len(batch) != 5 {
		t.Fatalf("应有 5 条进投递通道（unknown 不进），got %d", len(batch))
	}
	if len(f.sent) != 5 {
		t.Fatalf("SQS 应收到 5 条，got %d", len(f.sent))
	}
	// 路由逐条核对（顺序 = 构造顺序，含 strip_prefix 端到端验证）
	wantRoute(t, f.sent[0], "gcs:special-shared-bucket/hot/a.orc", "s3:s3-hot/hot/a.orc")
	wantRoute(t, f.sent[1], "gcs:special-shared-bucket/cold/b.orc", "s3:s3-cold/cold/b.orc")
	wantRoute(t, f.sent[2], "gcs:special-shared-bucket/warm/c.orc", "s3:s3-misc/warm/c.orc")
	wantRoute(t, f.sent[3], "gcs:mallfile-gcp/gcsprodorms/o1.json", "s3:s3euprodorms/o1.json")
	wantRoute(t, f.sent[4], "gcs:mallfile-gcp/riskgcp/r1.json", "s3:s3euprodrisk1/r1.json")

	// 计数按源桶拆分
	sp := st.bucketFor("special-shared-bucket")
	if ld(&sp.received) != 3 || ld(&sp.sent) != 3 {
		t.Errorf("special received=%d sent=%d want 3/3", ld(&sp.received), ld(&sp.sent))
	}
	mf := st.bucketFor("mallfile-gcp")
	if ld(&mf.received) != 2 || ld(&mf.sent) != 2 {
		t.Errorf("mallfile received=%d sent=%d want 2/2", ld(&mf.received), ld(&mf.sent))
	}
	nc := st.bucketFor("never-configured-bucket")
	if ld(&nc.received) != 1 || ld(&nc.unknownBkt) != 1 || ld(&nc.sent) != 0 {
		t.Errorf("unknown 桶 received=%d unknownBkt=%d sent=%d want 1/1/0",
			ld(&nc.received), ld(&nc.unknownBkt), ld(&nc.sent))
	}
	// 总计 = 各桶之和
	if ld(&st.received) != 6 || ld(&st.sent) != 5 || ld(&st.unknownBkt) != 1 {
		t.Errorf("总计 received=%d sent=%d unknownBkt=%d want 6/5/1",
			ld(&st.received), ld(&st.sent), ld(&st.unknownBkt))
	}
	if n := countBuckets(st); n != 3 {
		t.Errorf("应有 3 个源桶 entry，got %d", n)
	}
}
