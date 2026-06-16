package main

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ── extractSource ──────────────────────────────────────────────────────────
func TestExtractSource(t *testing.T) {
	cases := map[string]string{
		`{"source":"gcs:b/k.bin","destination":"s3:d/k.bin"}`: "gcs:b/k.bin",
		`{"destination":"s3:d/x","source":"s3:a/x"}`:          "s3:a/x",         // 顺序无关
		`{"source":"gcs:b/a\"q.bin","destination":"s3:d"}`:    `gcs:b/a\"q.bin`, // 转义引号
		`{bad json`:       "", // 坏体 → 空
		`{"op":"delete"}`: "", // 无 source（delete 消息）→ 空
		`{"source":""}`:   "", // 空 source → 空
	}
	for body, want := range cases {
		if got := extractSource(body); got != want {
			t.Errorf("extractSource(%q) = %q want %q", body, got, want)
		}
	}
}

// ── classify ───────────────────────────────────────────────────────────────
func TestClassify(t *testing.T) {
	del := map[string]bool{"src_not_found": true}
	tests := []struct {
		ec    string
		found bool
		want  decision
	}{
		{"src_not_found", true, decideDelete},
		{"src_5xx", true, decideKeep},
		{"src_rate_limit", true, decideKeep},
		{"rclone_timeout", true, decideKeep},
		{"uncategorized", true, decideUnknown}, // 模糊 → 保守保留
		{"", true, decideUnknown},              // 行存在但无 error_class（SUCCESS）
		{"", false, decideUnknown},             // DDB 查不到 → 保守保留
	}
	for _, tc := range tests {
		if got := classify(tc.ec, tc.found, del); got != tc.want {
			t.Errorf("classify(%q,%v) = %v want %v", tc.ec, tc.found, got, tc.want)
		}
	}
}

// ── fake DDB ─────────────────────────────────────────────────────────────────
type fakeDDB struct {
	// source → error_class（""=行存在但无 class；缺键=查不到）
	classBySource map[string]string
	errOnSource   map[string]bool
}

func (f *fakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	pk := in.ExpressionAttributeValues[":pk"].(*ddbtypes.AttributeValueMemberS).Value
	// pk = "<shard>#<source>"，剥掉 shard 前缀取 source
	src := pk
	for i := 0; i < len(pk); i++ {
		if pk[i] == '#' {
			src = pk[i+1:]
			break
		}
	}
	if f.errOnSource[src] {
		return nil, context.DeadlineExceeded
	}
	ec, ok := f.classBySource[src]
	if !ok {
		return &dynamodb.QueryOutput{Items: nil}, nil // 查不到
	}
	item := map[string]ddbtypes.AttributeValue{}
	if ec != "" {
		item["error_class"] = &ddbtypes.AttributeValueMemberS{Value: ec}
	}
	return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{item}}, nil
}

// ── fake SQS ─────────────────────────────────────────────────────────────────
type fakeSQS struct {
	mu      sync.Mutex
	batches [][]sqstypes.Message
	sent    []string // 转发到 keep 的 MessageId
	deleted []string // 从 DLQ 删除的 ReceiptHandle
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return &sqs.ReceiveMessageOutput{Messages: b}, nil
}

func (f *fakeSQS) SendMessageBatch(_ context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &sqs.SendMessageBatchOutput{}
	for _, e := range in.Entries {
		f.sent = append(f.sent, aws.ToString(e.Id))
		out.Successful = append(out.Successful, sqstypes.SendMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func (f *fakeSQS) DeleteMessageBatch(_ context.Context, in *sqs.DeleteMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &sqs.DeleteMessageBatchOutput{}
	for _, e := range in.Entries {
		f.deleted = append(f.deleted, aws.ToString(e.ReceiptHandle))
		out.Successful = append(out.Successful, sqstypes.DeleteMessageBatchResultEntry{Id: e.Id})
	}
	return out, nil
}

func dlqMsg(id, source string) sqstypes.Message {
	return sqstypes.Message{
		MessageId:     aws.String(id),
		ReceiptHandle: aws.String("rcpt-" + id),
		Body:          aws.String(`{"source":"` + source + `","destination":"s3:d/x"}`),
	}
}

func baseCfg() cleanerConfig {
	return cleanerConfig{
		dlqURL: "https://sqs/dlq", keepURL: "https://sqs/main", ddbTable: "t",
		workers: 1, deleteClasses: map[string]bool{"src_not_found": true},
	}
}

// ── report 模式：只统计，不删不转 ─────────────────────────────────────────────
func TestReportModeNoMutation(t *testing.T) {
	sqsc := &fakeSQS{batches: [][]sqstypes.Message{{
		dlqMsg("1", "gcs:b/gone1"), dlqMsg("2", "gcs:b/throttled"),
	}}}
	ddb := &fakeDDB{classBySource: map[string]string{
		"gcs:b/gone1": "src_not_found", "gcs:b/throttled": "src_rate_limit",
	}}
	cfg := baseCfg()
	cfg.apply = false
	st := &stats{}
	run(context.Background(), sqsc, ddb, cfg, st)

	if st.scanned != 2 || st.deleted != 1 || st.kept != 1 {
		t.Fatalf("scanned=%d deleted=%d kept=%d want 2/1/1", st.scanned, st.deleted, st.kept)
	}
	if len(sqsc.deleted) != 0 || len(sqsc.sent) != 0 {
		t.Fatalf("report 模式不应删除/转发，got del=%d sent=%d", len(sqsc.deleted), len(sqsc.sent))
	}
}

// ── clean 模式：src_not_found 删除 + 其它转发后删除 ───────────────────────────
func TestCleanModeDeletesAndForwards(t *testing.T) {
	sqsc := &fakeSQS{batches: [][]sqstypes.Message{{
		dlqMsg("1", "gcs:b/gone"),      // src_not_found → 删
		dlqMsg("2", "gcs:b/throttled"), // src_rate_limit → 转发
		dlqMsg("3", "gcs:b/unknown"),   // 查不到 → 保守转发
	}}}
	ddb := &fakeDDB{classBySource: map[string]string{
		"gcs:b/gone": "src_not_found", "gcs:b/throttled": "src_rate_limit",
		// gcs:b/unknown 故意不放 → 查不到
	}}
	cfg := baseCfg()
	cfg.apply = true
	st := &stats{}
	run(context.Background(), sqsc, ddb, cfg, st)

	if st.deleted != 1 || st.kept != 1 || st.unknown != 1 {
		t.Fatalf("deleted=%d kept=%d unknown=%d want 1/1/1", st.deleted, st.kept, st.unknown)
	}
	// 转发的是 keep + unknown = msg 2 和 3
	if len(sqsc.sent) != 2 {
		t.Fatalf("转发 %d 条 want 2 (keep+unknown)", len(sqsc.sent))
	}
	// 删除的是：src_not_found(1 条) + 转发成功后删的 keep/unknown(2 条) = 3 条
	if len(sqsc.deleted) != 3 {
		t.Fatalf("从 DLQ 删 %d 条 want 3 (1 源不存在 + 2 转发后删)", len(sqsc.deleted))
	}
}

// ── DDB 查询出错 → 保守保留，绝不误删 ────────────────────────────────────────
func TestDDBErrorIsConservativelyKept(t *testing.T) {
	sqsc := &fakeSQS{batches: [][]sqstypes.Message{{dlqMsg("1", "gcs:b/x")}}}
	ddb := &fakeDDB{errOnSource: map[string]bool{"gcs:b/x": true}}
	cfg := baseCfg()
	cfg.apply = true
	st := &stats{}
	run(context.Background(), sqsc, ddb, cfg, st)

	if st.deleted != 0 {
		t.Fatalf("DDB 出错绝不能删，got deleted=%d", st.deleted)
	}
	if st.ddbErrors != 1 || st.unknown != 1 {
		t.Fatalf("ddbErrors=%d unknown=%d want 1/1", st.ddbErrors, st.unknown)
	}
}

// ── 无 source 的坏体 → 保守保留 ──────────────────────────────────────────────
func TestNoSourceIsKept(t *testing.T) {
	bad := sqstypes.Message{
		MessageId: aws.String("1"), ReceiptHandle: aws.String("r1"),
		Body: aws.String(`{bad json`),
	}
	sqsc := &fakeSQS{batches: [][]sqstypes.Message{{bad}}}
	ddb := &fakeDDB{}
	cfg := baseCfg()
	cfg.apply = true
	st := &stats{}
	run(context.Background(), sqsc, ddb, cfg, st)

	if st.deleted != 0 || st.noSource != 1 {
		t.Fatalf("坏体不能删，deleted=%d noSource=%d want 0/1", st.deleted, st.noSource)
	}
}

// ── drain 收敛：空 receive 即停 ──────────────────────────────────────────────
func TestDrainStopsOnEmpty(t *testing.T) {
	sqsc := &fakeSQS{batches: [][]sqstypes.Message{
		{dlqMsg("1", "gcs:b/gone")},
		// 第二次 receive 返回空 → 应停止
	}}
	ddb := &fakeDDB{classBySource: map[string]string{"gcs:b/gone": "src_not_found"}}
	cfg := baseCfg()
	cfg.apply = false
	st := &stats{}
	run(context.Background(), sqsc, ddb, cfg, st)
	if st.scanned != 1 {
		t.Fatalf("scanned=%d want 1（空 receive 应收敛停止）", st.scanned)
	}
}
