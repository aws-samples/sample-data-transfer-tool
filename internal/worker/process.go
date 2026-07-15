// Package worker 实现 SQS 消费循环与单条消息的四态处理。
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/classify"
	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// Effects 单条消息处理的副作用接口（全注入，便于纯逻辑测试，不依赖真实 AWS/rcd）。
type Effects struct {
	// RunCopy 同步执行一次传输，返回四态结果。封装 rcd copyfile/deletefile。
	RunCopy func(ctx context.Context, msg message.TransferMessage) model.RunResult
	// Delete 删 SQS 消息（仅 SUCCESS 调用）。
	Delete func() error
	// Requeue 改 VisibilityTimeout 为 delay 秒，快速重投（失败态调用）。
	Requeue func(delaySeconds int) error
	// Record 写 DDB 终态。SUCCESS 不传 body（百万级行省容量），失败传完整 body。
	Record func(source, attemptTS string, result model.RunResult, body string) error
}

// Outcome 处理结果：四态 + 是否计数（UNKNOWN 不计数）。
type Outcome struct {
	State        model.State
	Counted      bool
	Poison       bool // 解析失败的脏消息（单独计数，不混进 unknown）
	RecordFailed bool
}

// ProcessMessage 处理单条消息，返回四态。所有副作用经 eff 注入。
//
// 四态 → 副作用（对齐 Python worker.process_message，"only SUCCESS deletes"）：
//   - SUCCESS:   Delete + Record(无body) + 计数
//   - RETRYABLE: Requeue(分级退避) + Record(带body) + 计数
//   - FATAL:     Requeue(0) + Record(带body) + 计数
//   - UNKNOWN:   Requeue(0) + Record(带body) + 不计数
//
// poison（解析失败）：Record(FATAL/poison_message, 带body) + Requeue(0)，不删，不计数。
func ProcessMessage(
	ctx context.Context,
	body string,
	instanceID, attemptTS string,
	eff Effects,
) Outcome {
	msg, err := message.Parse(body)
	if err != nil {
		// poison：DDB 留痕（用真实/摘要 source 作可搜索 key），立即重投烧进 DLQ，不删不计数。
		key := poisonKey(body)
		poison := model.RunResult{
			State: model.StateFatal, ExitCode: -1,
			ErrorClass: "poison_message", ErrorMessage: err.Error(),
		}
		// G1 best-effort：poison 的 record 失败同样不阻断——仍 Requeue(0) 烧进 DLQ（对齐 Python
		// poison 路径）。旧实现 Requeue(60)+UNKNOWN 会在 DDB 节流时让 poison 消息滞留重投。
		poisonRecFailed := false
		if recErr := eff.Record(key, attemptTS, poison, body); recErr != nil {
			obslog.Errorf("poison 终态写 DDB 失败(降级 best-effort，仍走 DLQ): %v | key=%s | body=%.512s", recErr, key, body)
			poisonRecFailed = true
		}
		_ = eff.Requeue(0)
		// 打 ERROR 到 worker-ops（对齐 Python）：哪条消息坏了、坏在哪，不登机即可定位。
		obslog.Errorf("poison 消息: %v | key=%s | body=%.512s", err, key, body)
		return Outcome{State: model.StateFatal, Counted: false, Poison: true, RecordFailed: poisonRecFailed}
	}

	result := eff.RunCopy(ctx, msg)

	// stats（bytes/elapsed/speed）由 runner 从 rcd 真实传输结果（core/stats group）填好并落 DDB。

	// 终态记录：SUCCESS 不存 body，失败存完整 body（便于定位/replay）。
	recordBody := body
	if result.State == model.StateSuccess {
		recordBody = ""
	}
	// DDB key：copy 用 source；delete 消息无 source，用 destination（删的就是它），
	// 否则空 source 让所有 delete 记录挤在同一个 "<shard>#" key 上、不可按对象搜索。
	recordKey := msg.Source
	if recordKey == "" {
		recordKey = msg.Destination
	}
	// G1 best-effort（对齐 Python worker，907K inflight 事故根治）：DDB record 是旁路留痕，
	// 失败绝不能阻断下方 SQS 生命周期动作（delete/requeue）。旧实现 record 失败→Requeue(60)
	// →UNKNOWN，在 DDB 持续节流时会让 SUCCESS 消息无限重投堆积 inflight（Python 旧版堆到 907K）。
	// 现改为：只记日志 + 标记 RecordFailed（→ Stats.RecordFail 计数 + 进度日志暴露），四态照常往下走。
	// 代价：丢一条 DDB 追溯记录 ≪ 消息卡死/成功对象反复重传（rcd copyfile 幂等，重跑不出错）。
	recordFailed := false
	if recErr := eff.Record(recordKey, attemptTS, result, recordBody); recErr != nil {
		obslog.Errorf("终态写 DDB 失败(降级 best-effort，继续 SQS 生命周期) state=%s error_class=%s source=%s err=%v",
			result.State, emptyToNone(result.ErrorClass), recordKey, recErr)
		recordFailed = true
	}

	// 失败打 ERROR 到 worker-ops（对齐 Python）：不登机即可在 CW 看失败根因。
	// SUCCESS 不打（百万级成功会刷爆日志）。
	if result.State != model.StateSuccess {
		obslog.Errorf("rclone 失败 state=%s exit=%d error_class=%s source=%s cmd=%s err=%.512s",
			result.State, result.ExitCode, emptyToNone(result.ErrorClass),
			recordKey, result.CmdStr, result.ErrorMessage)
	}

	switch result.State {
	case model.StateSuccess:
		if err := eff.Delete(); err != nil {
			// 删失败：消息会占 visibility 直到超时重投。不能上报 SUCCESS，否则 FileCount 会虚高；
			// 判 UNKNOWN（不计数）暴露副作用失败，delete_fail 计数在 consumer.deleteMsg 里独立 +1。
			return Outcome{State: model.StateUnknown, Counted: false, RecordFailed: recordFailed}
		}
		return Outcome{State: model.StateSuccess, Counted: true, RecordFailed: recordFailed}
	case model.StateRetryable:
		_ = eff.Requeue(classify.RetryDelaySeconds(result.ErrorClass))
		return Outcome{State: model.StateRetryable, Counted: true, RecordFailed: recordFailed}
	case model.StateFatal:
		_ = eff.Requeue(0)
		return Outcome{State: model.StateFatal, Counted: true, RecordFailed: recordFailed}
	default: // UNKNOWN
		_ = eff.Requeue(0)
		return Outcome{State: model.StateUnknown, Counted: false, RecordFailed: recordFailed}
	}
}

// poisonKey 坏消息的可搜索 DDB key。能解析出 source 时用真实 source（按源路径可直接
// 搜索）；烂 JSON 提不出时退回 poison:<前缀摘要>。
func poisonKey(body string) string {
	// 尽力提取 source 字段（即便整体校验失败，JSON 本身可能合法，如目录路径/缺字段）。
	var probe struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err == nil && probe.Source != "" {
		return probe.Source
	}
	n := len(body)
	if n > 64 {
		n = 64
	}
	return fmt.Sprintf("poison:%s", body[:n])
}

func emptyToNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// nowAttemptTS 生成 DDB SK 用的 attempt 时间戳（UTC, 毫秒，对齐 Python now_iso 形态）。
func nowAttemptTS() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000")
}
