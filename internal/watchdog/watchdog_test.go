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

// T4 rcd 短暂不可达:探测失败(!ok)应视为中性,续命一轮,不把单次探测失败推向 systemd 边界。
func TestEvalTick_ProbeFailureIsNeutral(t *testing.T) {
	h := &healthState{maxStall: testMaxStall, lastPoll: 100, lastActivity: 500, haveActivity: true}
	// 轮询冻结 + 探测失败 → 续命(中性),且不累计 stall。
	for i := 0; i < testMaxStall+2; i++ {
		if !h.evalTick(100, 0, false) {
			t.Fatalf("第 %d 轮:探测失败应中性续命,不应停报", i)
		}
	}
	if h.stallCount != 0 {
		t.Errorf("探测失败不应累计 stallCount,got %d", h.stallCount)
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
}
