package worker

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/classify"
	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/rcd"
)

// RunnerConfig runner 行为参数。
type RunnerConfig struct {
	// Timeout 单次传输上限 = RCLONE_TIMEOUT_SECONDS（必须 ≤ 0.7×visibility）。
	// 同步调用以此为 ctx deadline：到点 HTTP 断开 → rcd 传输中止（防双写）。
	Timeout time.Duration
}

// Runner 用 rcd 客户端同步执行一次传输并归类四态。实现 Effects.RunCopy。
type Runner struct {
	client  *rcd.Client
	cfg     RunnerConfig
	now     func() time.Time // 可注入时钟，便于测试 wall-clock 耗时
	groupID atomic.Int64     // 单调递增，生成唯一 stats group
}

// NewRunner 构造。
func NewRunner(client *rcd.Client, cfg RunnerConfig) *Runner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30240 * time.Second // 0.7×43200 兜底
	}
	return &Runner{client: client, cfg: cfg, now: time.Now}
}

// nextGroup 生成本进程内唯一的 stats group 名（并发安全）。
func (r *Runner) nextGroup() string {
	return "go-worker-" + strconv.FormatInt(r.groupID.Add(1), 10)
}

// RunCopy 同步执行 copyfile/deletefile，阻塞到完成，返回四态。
//
// 防双写：ctx 派生出带 Timeout deadline 的子 ctx 传给 HTTP 请求。worker 停机（父 ctx
// 取消）或超时 → HTTP 断开 → rcd 的传输 context 随之取消 → daemon 中止传输（实测确认
// 断开后 core/stats 字节冻结）。无需 job/stop——HTTP 生命周期即传输生命周期。
// 限速不在此注入（已改为 ratelimit 层调 rcd 全局 core/bwlimit + options/set）。
func (r *Runner) RunCopy(ctx context.Context, msg message.TransferMessage) model.RunResult {
	cmdStr := equivCmd(msg)

	tctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	// 每次传输唯一 stats group：传完按 group 查 rcd 真实字节/速率（object_size 是
	// 不可靠的 SQS 属性、常为 0，故弃用）。wall-clock 仍兜底测端到端耗时。
	group := r.nextGroup()
	start := r.now()
	err := r.execute(tctx, msg, group)
	elapsed := r.now().Sub(start).Seconds()

	if err == nil {
		// 同步成功。从 rcd 该 group 查真实传输字节/速率（copy/refresh 才有；delete 无字节）。
		stats := model.TransferStats{ElapsedSeconds: elapsed}
		if msg.Op == model.OpCopy || msg.Op == model.OpRefresh {
			if gs, e := r.client.StatsByGroup(context.Background(), group); e == nil {
				stats.Bytes = gs.Bytes
				if gs.ElapsedTime > 0 {
					stats.ElapsedSeconds = gs.ElapsedTime // rcd 实测传输时长更准
				}
				if stats.ElapsedSeconds > 0 {
					stats.Speed = float64(gs.Bytes) / stats.ElapsedSeconds
				}
			}
		}
		r.client.DeleteStatsGroup(context.Background(), group) // 用完即清，防内存涨
		return model.RunResult{
			State: model.StateSuccess, ExitCode: 0, CmdStr: cmdStr, Stats: stats,
		}
	}
	r.client.DeleteStatsGroup(context.Background(), group) // 失败也清理 group

	// 区分三类失败：父 ctx 取消（停机）/ 超时 / rcd 业务错误。
	errText := err.Error()
	if ctx.Err() != nil {
		// 父 ctx 取消 = 优雅停机：传输已随 HTTP 断开中止，判 UNKNOWN 重投（不计数）。
		return model.RunResult{
			State: model.StateUnknown, ExitCode: -1,
			ErrorClass: "worker_shutdown", ErrorMessage: "worker 停机，传输随 HTTP 断开中止", CmdStr: cmdStr,
		}
	}
	if tctx.Err() == context.DeadlineExceeded {
		// 超时 = 传输跑满 RCLONE_TIMEOUT 被 HTTP 断开中止（防双写）。判 UNKNOWN 重投。
		return model.RunResult{
			State: model.StateUnknown, ExitCode: -1,
			ErrorClass: "rclone_timeout", ErrorMessage: "传输超时，已随 HTTP 断开中止", CmdStr: cmdStr,
		}
	}

	// rcd 业务错误：delete 幂等（目标已不存在）→ SUCCESS。
	if msg.Op == model.OpDelete && classify.IsDeleteNoop(errText) {
		return model.RunResult{State: model.StateSuccess, ExitCode: 0, CmdStr: cmdStr}
	}
	// 其余按错误文本分类四态。
	errClass := classify.ClassifyError(1, errText)
	state := decideFailState(errText, errClass)
	return model.RunResult{
		State: state, ExitCode: 1,
		ErrorClass: errClass, ErrorMessage: errText, CmdStr: cmdStr,
	}
}

// execute 按 op 同步调 rcd copyfile/deletefile。group 用于 copy/refresh 的 stats 归集。
// refresh 与 copy 同走 copyfile，但注入 IgnoreTimes 强制重传以刷新 metadata。
func (r *Runner) execute(ctx context.Context, msg message.TransferMessage, group string) error {
	if msg.Op == model.OpDelete {
		fs, remote, err := message.SplitEndpoint(msg.Destination, "destination")
		if err != nil {
			return err
		}
		return r.client.DeleteFile(ctx, fs, remote)
	}
	srcFs, srcRemote, err := message.SplitEndpoint(msg.Source, "source")
	if err != nil {
		return err
	}
	dstFs, dstRemote, err := message.SplitEndpoint(msg.Destination, "destination")
	if err != nil {
		return err
	}
	forceRefresh := msg.Op == model.OpRefresh
	return r.client.CopyFile(ctx, srcFs, srcRemote, dstFs, dstRemote, group, forceRefresh)
}

// decideFailState 失败时定四态：源不存在=FATAL；瞬时(429/5xx/网络)=RETRYABLE；
// 限流/5xx 分类=RETRYABLE；其余=FATAL（烧进 DLQ）。
func decideFailState(errText, errClass string) model.State {
	if classify.IsSourceMissing(errText) {
		return model.StateFatal
	}
	switch errClass {
	case "src_rate_limit", "src_5xx", "dst_5xx":
		return model.StateRetryable
	}
	if classify.IsTransientError(errText) {
		return model.StateRetryable
	}
	return model.StateFatal
}

// equivCmd 生成等价 rclone 命令字符串（DDB 留痕，便于人工复现）。
// 限速是 rcd 全局（core/bwlimit + options/set），不随单条命令，故不拼进来。
func equivCmd(msg message.TransferMessage) string {
	if msg.Op == model.OpDelete {
		return fmt.Sprintf("rclone deletefile %s [via rcd]", msg.Destination)
	}
	if msg.Op == model.OpRefresh {
		return fmt.Sprintf("rclone copyto %s %s --ignore-times [via rcd]", msg.Source, msg.Destination)
	}
	return fmt.Sprintf("rclone copyto %s %s [via rcd]", msg.Source, msg.Destination)
}
