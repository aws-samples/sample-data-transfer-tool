// Package model 定义跨模块共享的数据契约：四态、传输消息、运行结果。
// 与 Python src/migration/models.py 语义对齐（同一套 SQS 消息契约 + 四态）。
package model

// Op 消息操作类型。COPY 默认（旧消息无 op 字段即此），DELETE 删目标端单对象，
// REFRESH 强制重传刷新元数据（CDC METADATA_UPDATE 用）。
type Op string

const (
	OpCopy   Op = "copy"
	OpDelete Op = "delete"
	// OpRefresh：copyfile 注入 IgnoreTimes=true 强制重传，使源端 metadata 变更（数据未变
	// 时 rclone 默认 skip）也能刷新到目标。代价=重传整个对象数据。
	OpRefresh Op = "refresh"
)

// State 四态传输结果（对齐 Python State / spec §4.1）。
//
// 副作用映射（worker 据此决定 SQS/DDB 动作，2026-06-10 失败快速重投）：
//   - SUCCESS:   删 SQS 消息 + 记终态 + 计数
//   - RETRYABLE: 不删 + 分级退避重投 + 记 FAILED + 计数
//   - FATAL:     不删 + 立即重投(0) + 记 FAILED + 计数（烧满 3 次进 DLQ）
//   - UNKNOWN:   不删 + 立即重投(0) + 不计数（崩溃/未预期/job 超时）
//
// 删除原则：只有 SUCCESS 删消息，其余一律保留并经 requeue 改 VisibilityTimeout，
// maxReceiveCount=3 后 SQS 转 DLQ，保证"处理不了的消息最终都在 DLQ 留底"。
type State string

const (
	StateSuccess   State = "SUCCESS"
	StateRetryable State = "RETRYABLE"
	StateFatal     State = "FATAL"
	StateUnknown   State = "UNKNOWN"
)

// TransferStats 从 rclone job 的 stats 解析出的传输统计。
type TransferStats struct {
	Bytes          int64
	ElapsedSeconds float64
	Speed          float64
	Errors         int64
	Transfers      int64
}

// RunResult 一次 copyfile/deletefile 的结果。
type RunResult struct {
	State        State
	ExitCode     int
	Stats        TransferStats
	ErrorClass   string // 低基数 error_class 标签；空串表示无
	ErrorMessage string // 失败时的原始错误文本；空串表示无
	CmdStr       string // 等价 rclone 命令（DDB 留痕，便于人工复现）
}

func (r RunResult) Success() bool { return r.State == StateSuccess }
