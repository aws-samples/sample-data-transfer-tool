// Package watchdog 实现 systemd sd_notify（READY=1 / WATCHDOG=1），对齐 Python watchdog.py。
//
// 判活 = 两个独立信号的 OR：
//   - 轮询活性：receiver 循环在推进（含队列空闲的 long-poll 空转）——覆盖"空闲不该被杀"。
//   - 传输活性：rcd 全局 activity 指纹在变化——覆盖"所有 worker 占满、在传一个超大文件、
//     几小时不完成一条消息"这种健康但轮询冻结的状态。
//
// 只有两个信号同时冻结、且连续 maxStall 轮（吸收单次探测失败/stats reset 抖动）才停止上报
// WATCHDOG=1 → systemd WatchdogSec 超时杀进程重启。这修复了旧实现的盲点：旧版靠 worker
// "active>0 时主动推进 progress"判活，导致所有 worker 卡死在 rcd 同步 HTTP 时反被判活。
//
// 注：部署须配 WatchdogSignal=SIGTERM，使 watchdog 触发的重启也走优雅 drain（在途消息
// visibility=0 立即重投），否则默认 SIGABRT 不经 main 的信号处理 → 在途消息等满 visibility。
//
// 无 NOTIFY_SOCKET（非 systemd 环境/本地）时所有调用静默 no-op。
package watchdog

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// defaultMaxStall 连续多少轮"轮询+活性"全冻结才判僵死停报。WatchdogSec=120 / interval=60s
// 下,3 轮 ≈ 3 分钟全静止才重启——既能吸收单次 stats 抖动/探测失败,又远小于"卡死无限拖"。
const defaultMaxStall = 3

// notifier 向 systemd NOTIFY_SOCKET 发 datagram。socket 为空 = 非 systemd，no-op。
type notifier struct{ addr *net.UnixAddr }

func newNotifier() *notifier {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return &notifier{}
	}
	// abstract socket: @ 前缀转 \0
	if path[0] == '@' {
		path = "\x00" + path[1:]
	}
	return &notifier{addr: &net.UnixAddr{Name: path, Net: "unixgram"}}
}

func (n *notifier) send(state string) {
	if n.addr == nil {
		return
	}
	c, err := net.DialUnix("unixgram", nil, n.addr)
	if err != nil {
		// 低频路径(~60s 一次):不静默吞,留一条 Warn 便于排查"心跳发不出去导致被反复重启"。
		obslog.Warnf("watchdog: 连接 NOTIFY_SOCKET 失败: %v", err)
		return
	}
	defer c.Close()
	if _, err := c.Write([]byte(state)); err != nil {
		obslog.Warnf("watchdog: 发送 %s 失败: %v", state, err)
	}
}

// Ready 发 READY=1（启动就绪，systemd Type=notify 等它）。
func Ready() { newNotifier().send("READY=1") }

// healthState watchdog 判活的内部状态机（抽出便于纯逻辑单测,不依赖真实 ticker）。
type healthState struct {
	maxStall     int
	lastPoll     int64
	lastActivity int64
	haveActivity bool // 是否已有过一次成功的活性采样基准
	stallCount   int
}

// evalTick 处理一个 tick：返回是否应发 WATCHDOG=1。
//
//	poll          本轮轮询计数(receiver 循环推进数)
//	activity      本轮 rcd 全局活性指纹(bytes+transfers+errors 等的组合)
//	probeOK       本轮活性探测是否成功(rcd 短暂不可达=false)
//
// 规则：轮询推进 或 活性指纹变化 → 判活(stall 归零)。探测失败 → 退化为仅轮询单信号:轮询推进
// 续命(吸收单次 rcd 抖动)、轮询也冻结则 stall++(不能无条件续命,否则 worker 全卡在 rcd HTTP +
// rcd 不可达时永判健康——这正是本包设计要消灭的盲点)。两信号同冻结 → stall++,达 maxStall 才停报。
// 用"指纹不等(!=)"而非"递增":固定 group 池传输前 reset 会让全局汇总回退,故不能依赖单调递增——
// 真卡死时指纹绝对冻结。
func (h *healthState) evalTick(poll, activity int64, probeOK bool) bool {
	pollAdvanced := poll > h.lastPoll
	h.lastPoll = poll

	if !probeOK {
		// rcd 探测失败:活性信号不可用,只能靠轮询单信号判活。不更新活性基准(无效数据不污染)。
		//   - 轮询推进 → 健康,stall 归零续命(单次 rcd 抖动不误杀健康机)。
		//   - 轮询也冻结 → 两信号皆无进展,累计 stall。连续 maxStall 轮"rcd 不可达 + 轮询冻结"
		//     (= worker 全卡在 rcd 同步 HTTP → slot 不释放 → receiveLoop 停推 progress)才停报,
		//     让 systemd 重启。修复原盲点:旧实现此处无条件续命,worker 全卡 + rcd 挂时永判健康
		//     (与本包顶部 docstring 声称已修的正是此盲点自相矛盾)。
		if pollAdvanced {
			h.stallCount = 0
			return true
		}
		h.stallCount++
		return h.stallCount < h.maxStall
	}

	activityChanged := h.haveActivity && activity != h.lastActivity
	h.lastActivity = activity
	h.haveActivity = true

	if pollAdvanced || activityChanged {
		h.stallCount = 0
		return true
	}
	// 两信号同冻结。
	h.stallCount++
	return h.stallCount < h.maxStall
}

// Run 周期上报 WATCHDOG=1，直到 ctx 取消。interval 取自 WATCHDOG_USEC 的一半（留余量），
// 无则默认 30s。
//
//	pollProgress  返回单调递增的轮询计数(receiver 循环推进,= consumer.Progress)。
//	activityProbe 返回 rcd 全局活性指纹与探测是否成功。nil 时退化为仅轮询信号。
func Run(ctx context.Context, pollProgress func() int64, activityProbe func(context.Context) (int64, bool)) {
	n := newNotifier()
	if n.addr == nil {
		obslog.Infof("watchdog: 无 NOTIFY_SOCKET，跳过（非 systemd 环境）")
		return
	}
	interval := watchdogInterval()
	h := &healthState{maxStall: defaultMaxStall, lastPoll: pollProgress()}
	if activityProbe != nil {
		if a, ok := probe(ctx, activityProbe); ok {
			h.lastActivity = a
			h.haveActivity = true
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			activity := int64(0)
			probeOK := false
			if activityProbe != nil {
				activity, probeOK = probe(ctx, activityProbe)
			}
			if h.evalTick(pollProgress(), activity, probeOK) {
				n.send("WATCHDOG=1")
			} else {
				obslog.Warnf("watchdog: 连续 %d 轮无轮询/传输进展,判僵死,停止上报 WATCHDOG（systemd 将重启）", h.stallCount)
			}
		}
	}
}

// probe 带 5s 超时调一次活性探测,脱离父 ctx 的取消（停机由 Run 的 ctx.Done 处理,探测本身
// 不该被父 ctx 提前砍断而误报 !ok）。
func probe(ctx context.Context, activityProbe func(context.Context) (int64, bool)) (int64, bool) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return activityProbe(pctx)
}

// watchdogInterval 从 systemd WATCHDOG_USEC 取 1/2（留充足余量）；无则 30s。
func watchdogInterval() time.Duration {
	if v := os.Getenv("WATCHDOG_USEC"); v != "" {
		if usec, err := strconv.ParseInt(v, 10, 64); err == nil && usec > 0 {
			return time.Duration(usec/2) * time.Microsecond
		}
	}
	return 30 * time.Second
}
