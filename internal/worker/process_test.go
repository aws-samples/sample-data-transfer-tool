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
	}
}

const okBody = `{"source":"s3:b/k","destination":"s3:b/k2"}`

// SUCCESS 记录的传输信息（bytes/elapsed/speed）由 runner 从 rcd group stats 填好，
// process 原样透传到 DDB（无 EMF 路径后，DDB 终态是唯一落点）。
func TestSuccessStatsPassThrough(t *testing.T) {
	r := &recorder{}
	// runner 返回的 RunResult 已带 rcd 真实 stats
	res := model.RunResult{State: model.StateSuccess, Stats: model.TransferStats{
		Bytes: 12345, ElapsedSeconds: 0.5, Speed: 24690,
	}}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts", r.effects(res))
	if out.State != model.StateSuccess {
		t.Fatal(out)
	}
	// bytes 来自 rcd 真实结果，落 DDB 终态
	if r.recordedResult.Stats.Bytes != 12345 {
		t.Errorf("transferred_bytes 应=rcd 真实 12345, got %d", r.recordedResult.Stats.Bytes)
	}
	if r.recordedResult.Stats.ElapsedSeconds != 0.5 {
		t.Errorf("elapsed 应透传 rcd stats, got %v", r.recordedResult.Stats.ElapsedSeconds)
	}
}

func TestSuccessDeletesNotRequeue(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
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
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
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
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
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
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateUnknown}))
	if out.Counted {
		t.Error("UNKNOWN 不计数")
	}
	if r.deleted || r.requeueDelay != 0 {
		t.Error("UNKNOWN 不删、requeue(0)")
	}
}

// watchdog 杀 worker（SIGTERM→worker_shutdown）时在途消息必须带退避重投：0 秒重投会让
// 重启后的 worker 立刻重新咬住同批消息，若卡死源在 rcd 侧未清除即成"重启-再卡死"死循环
// （2026-07-23 缓冲池死锁事故实证）。
func TestWorkerShutdownRequeuesWithBackoff(t *testing.T) {
	r := &recorder{}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateUnknown, ErrorClass: "worker_shutdown"}))
	if out.Counted {
		t.Error("worker_shutdown 不计数")
	}
	if r.deleted {
		t.Error("worker_shutdown 不删消息")
	}
	if !r.requeued || r.requeueDelay != 60 {
		t.Errorf("worker_shutdown 应 requeue(60)，got requeued=%v delay=%d", r.requeued, r.requeueDelay)
	}
}

func TestPoisonMessageNotDeletedNotCounted(t *testing.T) {
	r := &recorder{}
	// 缺 destination → 解析失败 → poison
	out := ProcessMessage(context.Background(), `{"source":"s3:b/orphan"}`, "i#0", "ts",
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
	ProcessMessage(context.Background(), delBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if r.recordedSrc != "s3:b/victim.bin" {
		t.Errorf("delete DDB key 应用 destination，got %q", r.recordedSrc)
	}
}

// G1 best-effort（对齐 Python worker，907K inflight 事故根治）：DDB record 是旁路，
// 失败绝不能阻断 SQS 生命周期动作。record 失败时四态该干嘛还干嘛（SUCCESS 照删、
// 失败态照 requeue），只标记 RecordFailed（→ Stats.RecordFail 计数 + 进度日志暴露）+ 保持原四态结果。
// 根因：DDB 持续节流时若因 record 失败改判 UNKNOWN/Requeue(60)，SUCCESS 消息会无限
// 重投堆积 inflight（Python 旧实现正是此坑，堆到 907K）。
func TestRecordFailureSuccessStillDeletes(t *testing.T) {
	r := &recorder{recordErr: errors.New("ddb throttled")}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateSuccess}))
	if out.State != model.StateSuccess || !out.Counted {
		t.Fatalf("record 失败不应改变 SUCCESS 四态结果，outcome=%+v", out)
	}
	if !r.deleted {
		t.Error("★ record 失败也必须删 SUCCESS 消息（传输已成功，旁路记录无否决权）")
	}
	if r.requeued {
		t.Error("SUCCESS 不应 requeue")
	}
	if !out.RecordFailed {
		t.Error("应标记 RecordFailed 供观测（→ Stats.RecordFail 计数）")
	}
}

func TestRecordFailureFatalStillRequeues(t *testing.T) {
	r := &recorder{recordErr: errors.New("ddb throttled")}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateFatal, ErrorClass: "src_not_found"}))
	if out.State != model.StateFatal || !out.Counted {
		t.Fatalf("record 失败不应改变 FATAL 四态结果，outcome=%+v", out)
	}
	if r.deleted {
		t.Error("FATAL 不应删消息")
	}
	if !r.requeued || r.requeueDelay != 0 {
		t.Errorf("★ record 失败也必须 requeue(0) 让 FATAL 烧进 DLQ，got requeued=%v delay=%d", r.requeued, r.requeueDelay)
	}
}

func TestRecordFailureRetryableStillRequeuesWithBackoff(t *testing.T) {
	r := &recorder{recordErr: errors.New("ddb throttled")}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
		r.effects(model.RunResult{State: model.StateRetryable, ErrorClass: "src_rate_limit"}))
	if out.State != model.StateRetryable || !out.Counted {
		t.Fatalf("record 失败不应改变 RETRYABLE 四态结果，outcome=%+v", out)
	}
	if !r.requeued || r.requeueDelay != 300 {
		t.Errorf("★ record 失败也必须按 error_class 分级退避 requeue(300)，got delay=%d", r.requeueDelay)
	}
}

// delete 失败：不能上报 SUCCESS（否则 FileCount 虚高），判 UNKNOWN 不计数，
// 等 visibility 超时重投（不显式 requeue）。delete_fail 计数在 consumer.deleteMsg 独立 +1。
func TestDeleteFailureYieldsUnknownNotSuccess(t *testing.T) {
	r := &recorder{deleteErr: errors.New("sqs delete failed")}
	out := ProcessMessage(context.Background(), okBody, "i#0", "ts",
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
}
