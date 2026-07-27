package watchdog

import (
	"os"
	"testing"
	"time"
)

// evalTick 是 watchdog 判活的核心纯逻辑：给定本 tick 的轮询计数、活性指纹、探测是否成功,
// 返回是否应发 WATCHDOG=1（true=判活续命；false=停报让 systemd 重启）。
// 下列测试覆盖六个可靠性边界——尤其"饱和传大文件不误杀"与"饱和卡死能停报"。

const testMaxStall = 3

// T1 饱和传大文件:轮询冻结(所有 slot 占满 receiver 阻塞),但活性指纹持续递增 → 持续判活。
func TestEvalTick_SaturatedTransferringStaysAlive(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 0, haveActivity: true}
	bytes := int64(0)
	for i := 0; i < 10; i++ {
		bytes += 1 << 20 // 字节在涨(传输推进)
		if !h.evalTick(100 /*poll 冻结*/, bytes, true) {
			t.Fatalf("第 %d 轮:饱和传大文件(字节在涨)被误判停报", i)
		}
	}
}

// T2 饱和卡死:轮询冻结 + 活性指纹冻结,连续 maxStall 轮后必须停报(触发 systemd 重启)。
func TestEvalTick_SaturatedDeadlockWithholdsAfterMaxStall(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 555, haveActivity: true}
	// 前 maxStall-1 轮:仍续命(吸收抖动)。
	for i := 0; i < testMaxStall-1; i++ {
		if !h.evalTick(100, 555, true) {
			t.Fatalf("第 %d 轮:未到 maxStall 不应停报", i)
		}
	}
	// 第 maxStall 轮:停报。
	if h.evalTick(100, 555, true) {
		t.Fatalf("连续 %d 轮全冻结后应停报让 systemd 重启", testMaxStall)
	}
}

// T3 空闲不误杀:轮询持续推进(long-poll 空转),活性指纹冻结 → 持续判活。
func TestEvalTick_IdlePollAdvancingStaysAlive(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 0, lastActivity: 0, haveActivity: true}
	poll := int64(0)
	for i := 0; i < 10; i++ {
		poll++ // receiver 循环每轮推进
		if !h.evalTick(poll, 0 /*无传输,字节不变*/, true) {
			t.Fatalf("第 %d 轮:空闲(轮询在推进)被误判停报", i)
		}
	}
}

// T4a rcd 探测失败但轮询仍推进:退化为轮询单信号判活,续命且 stall 归零
// (单次/短暂 rcd 抖动不该误杀仍在正常拉取消息的健康机)。
func TestEvalTick_ProbeFailurePollAdvancingStaysAlive(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 500, haveActivity: true}
	poll := int64(100)
	for i := 0; i < testMaxStall+2; i++ {
		poll++ // 轮询推进
		if !h.evalTick(poll, 0, false) {
			t.Fatalf("第 %d 轮:探测失败但轮询推进应续命", i)
		}
	}
	if h.stallCount != 0 {
		t.Errorf("轮询推进时不应累计 stallCount,got %d", h.stallCount)
	}
}

// T4b 原盲点回归:rcd 持续不可达 + 轮询冻结(worker 全卡在 rcd 同步 HTTP → slot 不释放 →
// receiveLoop 停推 progress)。旧实现无条件续命 → 永判健康;新实现必须累计 stall,达 maxStall 停报。
func TestEvalTick_ProbeFailurePollFrozenWithholdsAfterMaxStall(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 500, haveActivity: true}
	// 前 maxStall-1 轮:探测失败 + 轮询冻结,仍续命(吸收短暂抖动)。
	for i := 0; i < testMaxStall-1; i++ {
		if !h.evalTick(100 /*poll 冻结*/, 0, false /*rcd 不可达*/) {
			t.Fatalf("第 %d 轮:未到 maxStall 不应停报", i)
		}
	}
	// 第 maxStall 轮:两信号持续冻结,必须停报让 systemd 重启。
	if h.evalTick(100, 0, false) {
		t.Fatal("rcd 持续不可达 + 轮询冻结达 maxStall 必须停报(原盲点回归)")
	}
}

// T5 抖动吸收:stall 未到 maxStall 又恢复(活性指纹再次变化)→ stallCount 归零,不停报。
func TestEvalTick_StallRecoveryResets(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 500, haveActivity: true}
	// 两轮冻结(未到 maxStall=3)。
	if !h.evalTick(100, 500, true) || !h.evalTick(100, 500, true) {
		t.Fatal("未到 maxStall 不应停报")
	}
	if h.stallCount != 2 {
		t.Fatalf("应累计 2 轮 stall,got %d", h.stallCount)
	}
	// 活性恢复(字节变化)→ 归零。
	if !h.evalTick(100, 600, true) {
		t.Fatal("活性恢复应判活")
	}
	if h.stallCount != 0 {
		t.Errorf("活性恢复后 stallCount 应归零,got %d", h.stallCount)
	}
}

// T6 interval 解析:WATCHDOG_USEC/2;非法/缺失回退 30s。
func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "120000000") // 120s
	if got := watchdogInterval(); got != 60*time.Second {
		t.Errorf("WATCHDOG_USEC=120s 应得 60s,got %v", got)
	}
	_ = os.Unsetenv("WATCHDOG_USEC")
	if got := watchdogInterval(); got != 30*time.Second {
		t.Errorf("缺 WATCHDOG_USEC 应回退 30s,got %v", got)
	}
	t.Setenv("WATCHDOG_USEC", "garbage")
	if got := watchdogInterval(); got != 30*time.Second {
		t.Errorf("非法 WATCHDOG_USEC 应回退 30s,got %v", got)
	}
	// 边界加固:usec=1 时 usec/2=0,若不判除法结果会 NewTicker(0) panic 带崩进程;
	// 微秒级则退化忙轮询。低于 1s 的 interval 一律回退默认。
	t.Setenv("WATCHDOG_USEC", "1")
	if got := watchdogInterval(); got != 30*time.Second {
		t.Errorf("WATCHDOG_USEC=1(usec/2=0) 应回退 30s 防 NewTicker(0) panic,got %v", got)
	}
	t.Setenv("WATCHDOG_USEC", "1000000") // 1s → interval 0.5s < 1s 下限
	if got := watchdogInterval(); got != 30*time.Second {
		t.Errorf("亚秒 interval 应回退 30s 防忙轮询,got %v", got)
	}
}
