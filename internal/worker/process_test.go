package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

// recorder 捕获注入副作用的调用，供断言。
type recorder struct {
	deleted        bool
	requeueDelay   int
	requeued       bool
	recordedBody   string
	recordedSrc    string
	recordedResult model.RunResult
	reported       bool
	reportState    string
	reportEvent    EMFEvent
	recordErr      error
	deleteErr      error
}

func (r *recorder) effects(result model.RunResult) Effects {
	return Effects{
		RunCopy: func(_ context.Context, _ message.TransferMessage) model.RunResult { return result },
		Delete:  func() error { r.deleted = true; return r.deleteErr },
		Requeue: func(d int) error { r.requeued = true; r.requeueDelay = d; return nil },
		Record: func(src, _ string, res model.RunResult, body string) error {
			r.recordedSrc = src
			r.recordedResult = res
			r.recordedBody = body
			return r.recordErr
		},
		Report: func(ev EMFEvent) { r.reported = true; r.reportState = ev.State; r.reportEvent = ev },
	}
}

const okBody = `{"source":"s3:b/k","destination":"s3:b/k2"}`

// SUCCESS 记录的传输信息（bytes/elapsed/speed）由 runner 从 rcd group stats 填好，
// process 原样透传到 DDB + EMF（不再用 object_size 推断）。
func TestSuccessStatsPassThrough(t *testing.T) {
	r := &recorder{}
	// runner 返回的 RunResult 已带 rcd 真实 stats
	res := model.RunResult{State: model.StateSuccess, Stats: model.TransferStats{
		Bytes: 12345, ElapsedSeconds: 0.5, Speed: 24690,
	}}
	// object_size 传 0（模拟消息体没有 object_size 的真实情况）
	out := ProcessMessage(context.Background(), okBody, 0, "i#0", "ts", r.effects(res))
	if out.State != model.StateSuccess {
		t.Fatal(out)
	}
	// 即使 object_size=0，bytes 仍来自 rcd 真实结果
	if r.recordedResult.Stats.Bytes != 12345 {
		t.Errorf("transferred_bytes 应=rcd 真实 12345（非 object_size）, got %d", r.recordedResult.Stats.Bytes)
	}
	if r.reportEvent.Bytes != 12345 || r.reportEvent.Elapsed != 0.5 {
		t.Errorf("EMF 应透传 rcd stats, got %+v", r.reportEvent)
	}
}

func TestSuccessDeletesNotRequeue(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if !out.Counted || out.State != model.StateSuccess {
		t.Fatalf("outcome=%+v", out)
	}
	if !r.deleted {
		t.Error("SUCCESS 必须删消息")
	}
	if r.requeued {
		t.Error("SUCCESS 不应 requeue")
	}
	if r.recordedBody != "" {
		t.Error("SUCCESS 不应存 body")
	}
}

func TestRetryableRequeuesWithBackoff(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateRetryable, ErrorClass: "src_rate_limit"}))
	if !out.Counted || out.State != model.StateRetryable {
		t.Fatalf("outcome=%+v", out)
	}
	if r.deleted {
		t.Error("RETRYABLE 不应删消息")
	}
	if !r.requeued || r.requeueDelay != 300 {
		t.Errorf("RETRYABLE/src_rate_limit 应 requeue 300s，got requeued=%v delay=%d", r.requeued, r.requeueDelay)
	}
	if r.recordedBody == "" {
		t.Error("失败态应存完整 body")
	}
}

func TestFatalRequeuesZeroCounted(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateFatal, ErrorClass: "src_not_found"}))
	if !out.Counted || out.State != model.StateFatal {
		t.Fatalf("outcome=%+v", out)
	}
	if r.deleted {
		t.Error("FATAL 不删消息（保留走 DLQ）")
	}
	if r.requeueDelay != 0 {
		t.Errorf("FATAL 应 requeue(0)，got %d", r.requeueDelay)
	}
}

func TestUnknownNotCounted(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateUnknown}))
	if out.Counted {
		t.Error("UNKNOWN 不计数")
	}
	if r.deleted || r.requeueDelay != 0 {
		t.Error("UNKNOWN 不删、requeue(0)")
	}
}

func TestPoisonMessageNotDeletedNotCounted(t *testing.T) {
	r := &recorder{}
	// 缺 destination → 解析失败 → poison
	out := ProcessMessage(context.Background(), `{"source":"s3:b/orphan"}`, 0, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess})) // RunCopy 不该被调用
	if out.Counted {
		t.Error("poison 不计数")
	}
	if !out.Poison {
		t.Error("应标记 Outcome.Poison（供 tally 单独计数，不混进 unknown）")
	}
	if r.deleted {
		t.Error("poison 不删消息")
	}
	if !r.requeued || r.requeueDelay != 0 {
		t.Error("poison 应 requeue(0) 烧进 DLQ")
	}
	// poison key 应能用真实 source（按源路径可搜索）
	if r.recordedSrc != "s3:b/orphan" {
		t.Errorf("poison key 应用真实 source，got %q", r.recordedSrc)
	}
	if r.recordedBody == "" {
		t.Error("poison 应存完整 body 便于定位")
	}
}

// 回归：delete 消息无 source，DDB key 应回退用 destination（否则空 source 不可搜索）。
func TestDeleteRecordKeyUsesDestination(t *testing.T) {
	r := &recorder{}
	delBody := `{"destination":"s3:b/victim.bin","op":"delete"}`
	ProcessMessage(context.Background(), delBody, 0, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if r.recordedSrc != "s3:b/victim.bin" {
		t.Errorf("delete DDB key 应用 destination，got %q", r.recordedSrc)
	}
}

func TestQueueTypeDimension(t *testing.T) {
	r := &recorder{}
	ProcessMessage(context.Background(), okBody, 200*1024*1024, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if !r.reported {
		t.Fatal("应上报 EMF")
	}
	// 200MB ≥ 100MB 阈值 → large（仅维度，非路由）
}

func TestRecordFailureRequeuesAndReportsUnknown(t *testing.T) {
	r := &recorder{recordErr: errors.New("ddb down")}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if !out.RecordFailed || out.Counted || out.State != model.StateUnknown {
		t.Fatalf("record failure outcome=%+v", out)
	}
	if r.deleted {
		t.Error("DDB 终态写失败时不能删除 SQS 消息")
	}
	if !r.requeued || r.requeueDelay != 60 {
		t.Errorf("DDB 终态写失败应 60s 后重试，got requeued=%v delay=%d", r.requeued, r.requeueDelay)
	}
	if r.reportEvent.State != string(model.StateUnknown) || r.reportEvent.ErrorClass != "record_fail" {
		t.Errorf("应上报 UNKNOWN/record_fail，got %+v", r.reportEvent)
	}
}

func TestDeleteFailureReportsUnknownNotSuccess(t *testing.T) {
	r := &recorder{deleteErr: errors.New("sqs delete failed")}
	out := ProcessMessage(context.Background(), okBody, 10, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if out.Counted || out.State != model.StateUnknown {
		t.Fatalf("delete failure outcome=%+v", out)
	}
	if !r.deleted {
		t.Error("SUCCESS 应尝试删除消息")
	}
	if r.requeued {
		t.Error("delete failure 不应显式 requeue，等待 visibility 超时重投")
	}
	if r.reportEvent.State != string(model.StateUnknown) || r.reportEvent.ErrorClass != "delete_fail" {
		t.Errorf("应上报 UNKNOWN/delete_fail，got %+v", r.reportEvent)
	}
}
