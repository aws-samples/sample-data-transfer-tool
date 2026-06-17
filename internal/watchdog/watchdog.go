// Package watchdog 实现 systemd sd_notify（READY=1 / WATCHDOG=1），对齐 Python watchdog.py：
// 仅当 worker 有进展（progress 在涨）时才上报 WATCHDOG=1；真僵死（死锁/卡死）→ 停止上报
// → systemd WatchdogSec 超时杀进程重启（在途消息靠 SQS visibility 重投，不丢）。
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
		return
	}
	defer c.Close()
	_, _ = c.Write([]byte(state))
}

// Ready 发 READY=1（启动就绪，systemd Type=notify 等它）。
func Ready() { newNotifier().send("READY=1") }

// Run 周期上报 WATCHDOG=1，直到 ctx 取消。interval 取自 WATCHDOG_USEC 的一半（留余量），
// 无则默认 30s。progress() 返回单调递增的进展计数（= 已处理消息数）；两次上报间无进展
// 视为僵死，停止上报让 systemd 重启。
func Run(ctx context.Context, progress func() int64) {
	n := newNotifier()
	if n.addr == nil {
		obslog.Infof("watchdog: 无 NOTIFY_SOCKET，跳过（非 systemd 环境）")
		return
	}
	interval := watchdogInterval()
	last := progress()
	stallCount := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := progress()
			if cur > last {
				n.send("WATCHDOG=1")
				last = cur
				stallCount = 0
			} else {
				// 无进展：可能是真僵死，也可能只是没消息（空闲）。空闲不该被杀，
				// 故连续多轮无进展才停报。但 receiver 的 long-poll 也会推进 progress
				// （含空轮），所以真卡死才会持续无进展。
				stallCount++
				obslog.Warnf("watchdog: %v 内无进展（第 %d 轮），暂不上报 WATCHDOG", interval, stallCount)
			}
		}
	}
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
