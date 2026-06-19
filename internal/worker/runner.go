package worker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/classify"
	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
	"github.com/aws-samples/sample-data-transfer-tool/internal/rcd"
)

const rcdStatsTimeout = 5 * time.Second

// RunnerConfig runner 行为参数。
type RunnerConfig struct {
	// Timeout 单次传输上限 = RCLONE_TIMEOUT_SECONDS（必须 ≤ 0.7×visibility）。
	// 同步调用以此为 ctx deadline：到点 HTTP 断开 → rcd 传输中止（防双写）。
	Timeout time.Duration
	// GroupPoolSize stats group 池大小。rcd 为每个唯一 _group 常驻一个 StatsInfo，
	// 且该对象即便 stats-delete 也不被回收 → per-call 用唯一 group 会随文件数无界
	// 泄漏（实测 64G OOM 的根因）。改用固定大小的 group 池：group 总数恒定 = 池大小，
	// rcd 的 StatsInfo 数封顶，内存随累计文件数稳定不涨（实测验证）。应 ≈ Workers，
	// 使每个并发传输借到独立 group、统计互不串扰。
	GroupPoolSize int
}

// Runner 用 rcd 客户端同步执行一次传输并归类四态。实现 Effects.RunCopy。
type Runner struct {
	client *rcd.Client
	cfg    RunnerConfig
	now    func() time.Time // 可注入时钟，便于测试 wall-clock 耗时
	groups chan string      // 固定大小的 stats group 池（借出独占→统计精确，归还复用→不泄漏）
}

// NewRunner 构造。预填一个固定大小的 stats group 池（默认 64）。
func NewRunner(client *rcd.Client, cfg RunnerConfig) *Runner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30240 * time.Second // 0.7×43200 兜底
	}
	if cfg.GroupPoolSize <= 0 {
		cfg.GroupPoolSize = 64
	}
	groups := make(chan string, cfg.GroupPoolSize)
	for i := 0; i < cfg.GroupPoolSize; i++ {
		groups <- "go-worker-" + strconv.Itoa(i)
	}
	return &Runner{client: client, cfg: cfg, now: time.Now, groups: groups}
}

// borrowGroup 从池借一个 group（阻塞直到有空闲），releaseGroup 归还。借出期间该 group
// 由当前 goroutine 独占 → 可用「传输前后 group 累计字节之差」精确归因本次传输字节。
func (r *Runner) borrowGroup() string   { return <-r.groups }
func (r *Runner) releaseGroup(g string) { r.groups <- g }

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

	// 从池借一个 stats group（独占到归还）。group 总数恒定=池大小 → rcd 的 StatsInfo
	// 数封顶，不随文件数无界泄漏（per-call 唯一 group 才是 OOM 根因）。
	group := r.borrowGroup()
	defer r.releaseGroup(group)

	// 传输前 reset 该 group 计数（独占借出，安全）：传后读 bytes 即本次传输字节，
	// 无累计、无溢出、不受 rcd 重启基准失效影响。reset 只清计数不删 StatsInfo，不泄漏。
	if msg.Op == model.OpCopy || msg.Op == model.OpRefresh {
		resetCtx, resetCancel := context.WithTimeout(tctx, rcdStatsTimeout)
		err := r.client.ResetStatsGroup(resetCtx, group)
		resetCancel()
		if err != nil {
			if ctx.Err() != nil {
				return model.RunResult{
					State: model.StateUnknown, ExitCode: -1,
					ErrorClass: "worker_shutdown", ErrorMessage: "worker 停机，stats reset 未完成", CmdStr: cmdStr,
				}
			}
			if tctx.Err() == context.DeadlineExceeded {
				return model.RunResult{
					State: model.StateUnknown, ExitCode: -1,
					ErrorClass: "rclone_timeout", ErrorMessage: "传输超时预算内 stats reset 未完成", CmdStr: cmdStr,
				}
			}
			return model.RunResult{
				State: model.StateRetryable, ExitCode: 1,
				ErrorClass: "rcd_stats_reset", ErrorMessage: err.Error(), CmdStr: cmdStr,
			}
		}
	}

	start := r.now()
	err := r.execute(tctx, msg, group)
	elapsed := r.now().Sub(start).Seconds()

	if err == nil {
		// 同步成功。group 传输前已清零，当前 bytes 即本次传输字节。
		stats := model.TransferStats{ElapsedSeconds: elapsed}
		if msg.Op == model.OpCopy || msg.Op == model.OpRefresh {
			statsCtx, statsCancel := context.WithTimeout(context.WithoutCancel(ctx), rcdStatsTimeout)
			if gs, e := r.client.StatsByGroup(statsCtx, group); e == nil {
				stats.Bytes = gs.Bytes
				if stats.ElapsedSeconds > 0 {
					stats.Speed = float64(stats.Bytes) / stats.ElapsedSeconds
				}
			} else {
				obslog.Warnf("读取 rcd stats 失败 group=%s err=%v", group, e)
			}
			statsCancel()
		}
		return model.RunResult{
			State: model.StateSuccess, ExitCode: 0, CmdStr: cmdStr, Stats: stats,
		}
	}

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
