package status

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

// fakeDDB 捕获 PutItem（心跳）与 BatchWriteItem（终态）。可注入 UnprocessedItems / 整批错误。
type fakeDDB struct {
	mu sync.Mutex

	// PutItem（心跳）
	lastItem  map[string]ddbtypes.AttributeValue
	lastTable string

	// BatchWriteItem（终态）
	batchTable  string
	batchItems  []map[string]ddbtypes.AttributeValue // 所有已“成功写入”的 item（累计）
	batchCalls           int
	unprocessed          int   // 前 N 次调用返回 1 条 UnprocessedItems，模拟节流（一次性）
	unprocessedEveryCall bool  // 每次都截留 1 条未处理（模拟持续未写入，永不收敛）
	batchErr             error // 令 BatchWriteItem 返回整批错误（清零前一直返回）
	batchErrN            int   // 前 N 次返回 batchErr，之后成功
}

func (f *fakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastItem = in.Item
	f.lastTable = *in.TableName
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDDB) BatchWriteItem(_ context.Context, in *dynamodb.BatchWriteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if f.batchErrN > 0 {
		f.batchErrN--
		return nil, f.batchErr
	}
	for table, reqs := range in.RequestItems {
		f.batchTable = table
		// 末尾 unprocessed 条留作 UnprocessedItems（模拟部分未处理），其余记为已写入。
		process := len(reqs)
		var unproc []ddbtypes.WriteRequest
		if f.unprocessedEveryCall && process > 0 {
			// 持续截留最后 1 条：永不收敛（测试重试耗尽路径）。
			unproc = reqs[process-1:]
			process--
			for _, r := range reqs[:process] {
				f.batchItems = append(f.batchItems, r.PutRequest.Item)
			}
			return &dynamodb.BatchWriteItemOutput{
				UnprocessedItems: map[string][]ddbtypes.WriteRequest{table: unproc},
			}, nil
		}
		if f.unprocessed > 0 {
			hold := f.unprocessed
			if hold > process {
				hold = process
			}
			unproc = reqs[process-hold:]
			process -= hold
			f.unprocessed = 0 // 只截留一次，下次全部处理
		}
		for _, r := range reqs[:process] {
			f.batchItems = append(f.batchItems, r.PutRequest.Item)
		}
		if len(unproc) > 0 {
			return &dynamodb.BatchWriteItemOutput{
				UnprocessedItems: map[string][]ddbtypes.WriteRequest{table: unproc},
			}, nil
		}
	}
	return &dynamodb.BatchWriteItemOutput{}, nil
}

// lastBatchItem 返回最后一条写入的终态 item（同步路径下单条）。
func (f *fakeDDB) lastBatchItem() map[string]ddbtypes.AttributeValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batchItems) == 0 {
		return nil
	}
	return f.batchItems[len(f.batchItems)-1]
}

func (f *fakeDDB) batchItemCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batchItems)
}

func sval(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

// ── 终态 item 构造（同步路径：未 StartWriter → RecordTerminal 走单条 BatchWriteItem）──

func TestRecordTerminal_SuccessOmitsBody(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	err := s.RecordTerminal(context.Background(), "s3:b/k", "2026-01-01T00:00:00.000",
		model.RunResult{State: model.StateSuccess, Stats: model.TransferStats{Bytes: 100}},
		"i#0", "2026-01-01T00:00:00.000", "") // body 空
	if err != nil {
		t.Fatal(err)
	}
	item := f.lastBatchItem()
	if got := sval(item["source_hash"]); got != MakePK("s3:b/k") {
		t.Errorf("source_hash = %q, want %q", got, MakePK("s3:b/k"))
	}
	if _, ok := item["message_body"]; ok {
		t.Error("SUCCESS 不应写 message_body")
	}
	if _, ok := item["error_class"]; ok {
		t.Error("SUCCESS 不应有 error_class")
	}
}

func TestRecordTerminal_FailureIncludesBodyAndError(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	_ = s.RecordTerminal(context.Background(), "s3:b/k", "ts",
		model.RunResult{
			State: model.StateRetryable, ErrorClass: "src_rate_limit",
			ErrorMessage: "429 too many requests",
		},
		"i#0", "ts", `{"source":"s3:b/k","destination":"s3:d/k"}`)
	item := f.lastBatchItem()
	if _, ok := item["message_body"]; !ok {
		t.Error("失败态应写完整 message_body")
	}
	if sval(item["error_class"]) != "src_rate_limit" {
		t.Error("应写 error_class")
	}
	if f.batchTable != "status-tbl" {
		t.Errorf("表名错: %s", f.batchTable)
	}
}

func TestRecordTerminal_TruncatesLargeErrorAndBodyUTF8(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	long := strings.Repeat("界", maxTerminalTextBytes)
	_ = s.RecordTerminal(context.Background(), "s3:b/k", "ts",
		model.RunResult{
			State: model.StateFatal, ErrorClass: "fatal",
			ErrorMessage: long,
		},
		"i#0", "ts", long)

	item := f.lastBatchItem()
	for _, key := range []string{"error_message", "message_body"} {
		got := sval(item[key])
		if len(got) > maxTerminalTextBytes {
			t.Errorf("%s 未截断: len=%d", key, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s 截断后不是合法 UTF-8", key)
		}
	}
}

func TestWriteHeartbeat_TTL(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	_ = s.WriteHeartbeat(context.Background(), "i#0", "ts", 1000, 64)
	if f.lastTable != "hb-tbl" {
		t.Errorf("心跳应写 heartbeat 表, got %s", f.lastTable)
	}
	ttl := f.lastItem["ttl"].(*ddbtypes.AttributeValueMemberN).Value
	if ttl != "1300" { // 1000 + 300
		t.Errorf("ttl = %s, want 1300 (now+300)", ttl)
	}
}

// ── 异步批量 writer ──

// StartWriter → 多条入队 → StopWriter drain → 全部写入。
func TestAsyncWriter_DrainsAllOnStop(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	var failed int64
	s.StartWriter(func(n int64) { failed += n })
	const n = 60 // > 25，跨多批
	for i := 0; i < n; i++ {
		src := "s3:b/obj" + strconv.Itoa(i)
		if err := s.RecordTerminal(context.Background(), src, "ts"+strconv.Itoa(i),
			model.RunResult{State: model.StateSuccess}, "i#0", "now", ""); err != nil {
			t.Fatalf("入队不应失败: %v", err)
		}
	}
	s.StopWriter()
	if got := f.batchItemCount(); got != n {
		t.Errorf("应写入 %d 条，got %d", n, got)
	}
	if failed != 0 {
		t.Errorf("不应有 RecordFail，got %d", failed)
	}
}

// UnprocessedItems 应被重试写入（不丢）。
func TestAsyncWriter_RetriesUnprocessed(t *testing.T) {
	f := &fakeDDB{unprocessed: 1}
	s := NewStore(f, "status-tbl", "hb-tbl")
	var failed int64
	s.StartWriter(func(n int64) { failed += n })
	for i := 0; i < 3; i++ {
		_ = s.RecordTerminal(context.Background(), "s3:b/o"+strconv.Itoa(i), "ts"+strconv.Itoa(i),
			model.RunResult{State: model.StateSuccess}, "i#0", "now", "")
	}
	s.StopWriter()
	if got := f.batchItemCount(); got != 3 {
		t.Errorf("UnprocessedItems 应重试至全部写入=3，got %d", got)
	}
	if failed != 0 {
		t.Errorf("重试成功不应计 RecordFail，got %d", failed)
	}
}

// 整批持续错误 → 重试耗尽 → 计 RecordFail（best-effort 放弃）。
func TestAsyncWriter_PersistentErrorCountsRecordFail(t *testing.T) {
	f := &fakeDDB{batchErr: errors.New("throttled"), batchErrN: 100} // 一直失败
	s := NewStore(f, "status-tbl", "hb-tbl")
	var failed int64
	var mu sync.Mutex
	s.StartWriter(func(n int64) { mu.Lock(); failed += n; mu.Unlock() })
	_ = s.RecordTerminal(context.Background(), "s3:b/o", "ts",
		model.RunResult{State: model.StateFatal}, "i#0", "now", "")
	s.StopWriter()
	mu.Lock()
	defer mu.Unlock()
	if failed != 1 {
		t.Errorf("持续失败应计 1 条 RecordFail，got %d", failed)
	}
}

// 同步 fallback（未 StartWriter）：持续 UnprocessedItems → 重试耗尽后 RecordTerminal 必须返回
// error（而非误报 nil 成功），否则调用方不会标 RecordFailed → 静默丢记录（Codex 审出的回归）。
func TestSyncFallback_PersistentUnprocessedReturnsError(t *testing.T) {
	// unprocessed 每次都截留 1 条：单条 batch 永远剩 1 条未处理，重试耗尽仍未写入。
	f := &fakeDDB{unprocessedEveryCall: true}
	s := NewStore(f, "status-tbl", "hb-tbl") // 不 StartWriter → 同步路径
	err := s.RecordTerminal(context.Background(), "s3:b/k", "ts",
		model.RunResult{State: model.StateSuccess}, "i#0", "now", "")
	if err == nil {
		t.Error("同步 fallback 持续 UnprocessedItems 应返回 error（供调用方标 RecordFailed），不应误报 nil")
	}
}

// 批内同 (PK,SK) 去重（避免 BatchWriteItem 同批同键整批 ValidationException）。
func TestDedupeToWriteRequests(t *testing.T) {
	items := []terminalItem{
		{pk: "p1", sk: "s1", item: map[string]ddbtypes.AttributeValue{"v": &ddbtypes.AttributeValueMemberS{Value: "a"}}},
		{pk: "p1", sk: "s1", item: map[string]ddbtypes.AttributeValue{"v": &ddbtypes.AttributeValueMemberS{Value: "b"}}}, // 同键，后写覆盖
		{pk: "p2", sk: "s1", item: map[string]ddbtypes.AttributeValue{"v": &ddbtypes.AttributeValueMemberS{Value: "c"}}},
	}
	reqs := dedupeToWriteRequests(items)
	if len(reqs) != 2 {
		t.Fatalf("去重后应剩 2 条，got %d", len(reqs))
	}
	// 第一条键 (p1,s1) 应保留后写的 "b"
	if got := sval(reqs[0].PutRequest.Item["v"]); got != "b" {
		t.Errorf("同键应后写覆盖，got %q want b", got)
	}
}
