"""ratelimit 控制律单元测试(纯函数,无 AWS)。

闭环自动限速:控制器对比实际总吞吐 vs 红线,AIMD 调 per-process-bwlimit。
"""
from __future__ import annotations

import pytest

from migration import ratelimit as rl


# ════════════════════════ format_bwlimit ════════════════════════
@pytest.mark.unit
def test_format_bwlimit_zero_is_off():
    # 0/负 → rclone "off"(不限速)
    assert rl.format_bwlimit(0) == "off"
    assert rl.format_bwlimit(-1) == "off"


@pytest.mark.unit
def test_format_bwlimit_uses_mib_suffix():
    # 🔴 rclone --bwlimit 纯数字单位是 KiB/s,不是 byte/s!统一用 M 后缀(MiB/s)。
    # 内部全用 byte/s,输出时换算成 MiB 并带 "M" 后缀,杜绝 1024 倍歧义。
    # 1 MiB = 1048576 byte;100 MiB/s = 104857600 byte/s → "100.00M"
    assert rl.format_bwlimit(104_857_600) == "100.00M"

@pytest.mark.unit
def test_format_bwlimit_fractional_mib():
    # 480_000_000 byte/s ÷ 1048576 = 457.76 MiB/s
    assert rl.format_bwlimit(480_000_000) == "457.76M"


# ════════════════════════ gbps <-> bytes/sec ════════════════════════
@pytest.mark.unit
def test_gbps_to_bytes_per_sec():
    # 8 Gbps = 1e9 字节/秒
    assert rl.gbps_to_bytes_per_sec(8.0) == 1_000_000_000


@pytest.mark.unit
def test_bytes_per_sec_to_gbps():
    assert rl.bytes_per_sec_to_gbps(1_000_000_000) == pytest.approx(8.0)


# ════════════════════════ 慢而稳:纯加性调节 additive_adjust ════════════════════════
class TestAdditiveAdjust:
    # 根治震荡:去掉快刹(×0.5)和乘性减(×0.8)的猛砍,改对称小步加性。
    # 超红线 -step,低于红线 +step,死区保持。step 小(=公平份额比例)→ 单调慢收敛不超调。
    TARGET = 100.0
    CUR = 40_000_000
    STEP = 9_000_000  # 步长(字节/秒)

    @pytest.mark.unit
    def test_over_target_subtracts_step(self):
        # 超红线 → 减一步(不再乘性砍半)
        new = rl.additive_adjust(
            primary_gbps=150.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR - self.STEP

    @pytest.mark.unit
    def test_below_target_adds_step(self):
        # 低于红线×0.9 → 加一步
        new = rl.additive_adjust(
            primary_gbps=50.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR + self.STEP

    @pytest.mark.unit
    def test_deadzone_holds(self):
        # 死区 [90,110] → 保持(防红线附近抖)
        new = rl.additive_adjust(
            primary_gbps=95.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR

    @pytest.mark.unit
    def test_upper_deadzone_holds_within_110pct(self):
        # 超红线但 ≤ 红线×1.1(100~110)→ 不收紧(容忍10%瞬时过冲)
        new = rl.additive_adjust(
            primary_gbps=108.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR

    @pytest.mark.unit
    def test_over_110pct_subtracts_step(self):
        # 超红线×1.1(>110)才减一步
        new = rl.additive_adjust(
            primary_gbps=115.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR - self.STEP

    @pytest.mark.unit
    def test_over_target_respects_floor(self):
        # 连续减不破地板
        new = rl.additive_adjust(
            primary_gbps=150.0, target_gbps=self.TARGET, current_bwlimit=rl.MIN_BWLIMIT_BYTES, step_bytes=self.STEP
        )
        assert new >= rl.MIN_BWLIMIT_BYTES

    @pytest.mark.unit
    def test_target_off_returns_zero(self):
        new = rl.additive_adjust(
            primary_gbps=150.0, target_gbps=0.0, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == 0

    @pytest.mark.unit
    def test_cold_start_from_zero_uses_step(self):
        # current=0(从off恢复)且低于红线 → 从 step 起步(非0)
        new = rl.additive_adjust(
            primary_gbps=50.0, target_gbps=self.TARGET, current_bwlimit=0, step_bytes=self.STEP
        )
        assert new > 0

    @pytest.mark.unit
    def test_no_multiplicative_no_fast_brake(self):
        # 即使严重超红线(2倍),也只减一步(对称),不再半速急砍
        new = rl.additive_adjust(
            primary_gbps=300.0, target_gbps=self.TARGET, current_bwlimit=self.CUR, step_bytes=self.STEP
        )
        assert new == self.CUR - self.STEP  # 不是 CUR×0.5


# ════════════════════════ 变化限幅 damp_step(阻尼,压震荡) ════════════════════════
class TestDampStep:
    # 根因:bwlimit 只对新进程生效 + 大文件周期长 → 控制滞后 2-3min >> 控制周期 1min
    # → AIMD 拿旧果调新因,一轮从 74M 砸到 9M 造成锯齿。阻尼:限制每轮变化幅度。
    @pytest.mark.unit
    def test_damp_caps_increase(self):
        # 旧 100M,AIMD 想涨到 400M(×4),限幅后最多 ×(1+MAX_STEP_RATIO)
        capped = rl.damp_step(old=100_000_000, proposed=400_000_000)
        assert capped == int(100_000_000 * (1 + rl.MAX_STEP_RATIO))

    @pytest.mark.unit
    def test_damp_caps_decrease(self):
        # 旧 100M,AIMD 想砸到 10M,限幅后最多 ×(1-MAX_STEP_RATIO)
        capped = rl.damp_step(old=100_000_000, proposed=10_000_000)
        assert capped == int(100_000_000 * (1 - rl.MAX_STEP_RATIO))

    @pytest.mark.unit
    def test_damp_passes_small_change(self):
        # 变化在限幅内 → 原样通过(105M 相对 100M 只涨5%,< MAX_STEP_RATIO)
        capped = rl.damp_step(old=100_000_000, proposed=105_000_000)
        assert capped == 105_000_000

    @pytest.mark.unit
    def test_damp_off_passthrough(self):
        # proposed=0(off)直接放行,不限幅(关限速要即时生效)
        assert rl.damp_step(old=100_000_000, proposed=0) == 0

    @pytest.mark.unit
    def test_damp_cold_start_passthrough(self):
        # old=0(冷启动/从off恢复)→ 无基数可限幅,放行 proposed
        assert rl.damp_step(old=0, proposed=400_000_000) == 400_000_000

    @pytest.mark.unit
    def test_damp_ratio_reasonable(self):
        # 限幅比例应在 (0,1) 之间(太大无阻尼效果,太小反应太慢)
        assert 0 < rl.MAX_STEP_RATIO < 1


# ════════════════════════ 宽松护栏 safety_cap(防单进程失控,非主控) ════════════════════════
class TestSafetyCap:
    # 旧设计用 目标÷进程数 当硬上限(假设满额并发),导致实测吞吐够不到红线。
    # 新设计:护栏 = 目标÷进程数 × SAFETY_MULTIPLIER,只防单进程失控,不当主控。
    # AIMD 在护栏内按实测自由增减,单进程额度自动收敛到"让实测吞吐=红线"的值。
    @pytest.mark.unit
    def test_safety_cap_is_multiple_of_fair_share(self):
        # 100Gbps ÷ 640进程 = 18.63MiB 公平份额;护栏应是它的 SAFETY_MULTIPLIER 倍
        cap = rl.safety_cap_bytes(target_gbps=100.0, threads=640)
        fair_share = int(100e9 / 8 / 640)
        assert cap == fair_share * rl.SAFETY_MULTIPLIER

    @pytest.mark.unit
    def test_safety_multiplier_gt_one(self):
        # 护栏必须 > 公平份额(否则又退化成硬钳死,实测够不到红线)
        assert rl.SAFETY_MULTIPLIER > 1

    @pytest.mark.unit
    def test_safety_cap_zero_threads_falls_back_to_global(self):
        # 进程数为 0(冷启动无心跳)→ 退化为全局目标(不除),不崩
        cap = rl.safety_cap_bytes(target_gbps=100.0, threads=0)
        assert cap == int(100e9 / 8)

    @pytest.mark.unit
    def test_safety_cap_target_off_is_zero(self):
        assert rl.safety_cap_bytes(target_gbps=0.0, threads=640) == 0


# ════════════════════════ AIMD 控制律(核心) ════════════════════════
class TestAimdAdjust:
    # 红线 300 Gbps,当前单进程 bwlimit 480M(字节/秒)
    TARGET = 300.0
    CUR = 480_000_000

    @pytest.mark.unit
    def test_over_target_multiplicative_decrease(self):
        # 实际 360 > 红线 300 → 乘性减(×0.8)
        new = rl.aimd_adjust(actual_gbps=360.0, target_gbps=self.TARGET, current_bwlimit=self.CUR)
        assert new == int(self.CUR * rl.DECREASE_FACTOR)
        assert new < self.CUR

    @pytest.mark.unit
    def test_well_below_target_additive_increase(self):
        # 实际 200 < 红线×0.9(270) → 加性增(+步长)
        new = rl.aimd_adjust(actual_gbps=200.0, target_gbps=self.TARGET, current_bwlimit=self.CUR)
        assert new == self.CUR + rl.INCREASE_STEP_BYTES
        assert new > self.CUR

    @pytest.mark.unit
    def test_in_deadzone_holds(self):
        # 实际 285 落在死区(270~300) → 保持不变(防震荡)
        new = rl.aimd_adjust(actual_gbps=285.0, target_gbps=self.TARGET, current_bwlimit=self.CUR)
        assert new == self.CUR

    @pytest.mark.unit
    def test_at_target_boundary_holds(self):
        # 恰好等于红线 → 不算超(死区上界含红线),保持
        new = rl.aimd_adjust(actual_gbps=300.0, target_gbps=self.TARGET, current_bwlimit=self.CUR)
        assert new == self.CUR

    @pytest.mark.unit
    def test_decrease_respects_floor(self):
        # 连续收紧不会低于地板(防限到 0 致传输停滞)
        new = rl.aimd_adjust(actual_gbps=9999.0, target_gbps=self.TARGET, current_bwlimit=1)
        assert new >= rl.MIN_BWLIMIT_BYTES

    @pytest.mark.unit
    def test_cold_start_from_off_uses_initial_estimate(self):
        # 当前不限速(0/off)且实际超红线 → 不能从 0 乘,要给一个初始估算上限
        new = rl.aimd_adjust(actual_gbps=360.0, target_gbps=self.TARGET, current_bwlimit=0)
        assert new > 0  # 必须给出一个有限上限,不能还是 0(否则永远收不住)

    @pytest.mark.unit
    def test_target_off_means_no_limit(self):
        # 红线设 0/off(不限速)→ 返回 0(off),不管实际多少
        assert rl.aimd_adjust(actual_gbps=999.0, target_gbps=0.0, current_bwlimit=480_000_000) == 0


# ════════════════════════ 429 自适应叠加 ════════════════════════
class TestThrottleOverride:
    @pytest.mark.unit
    def test_high_429_rate_forces_decrease_even_under_target(self):
        # 即使实际吞吐没到红线,429 率高也强制收紧(防雪崩)
        new = rl.aimd_adjust(
            actual_gbps=200.0, target_gbps=300.0, current_bwlimit=480_000_000,
            throttle_rate=0.10,  # 10% 429
        )
        assert new < 480_000_000  # 被 429 触发收紧

    @pytest.mark.unit
    def test_low_429_rate_no_override(self):
        # 429 率低于阈值 → 不干预,正常 AIMD(此处实际<红线本应增)
        new = rl.aimd_adjust(
            actual_gbps=200.0, target_gbps=300.0, current_bwlimit=480_000_000,
            throttle_rate=0.001,
        )
        assert new >= 480_000_000


# ════════════════════════ 双阈值快刹(防雪崩) ════════════════════════
class TestFastBrake:
    # 慢回路看 5min 平均(actual),快保护看最近 1min(spike)。
    TARGET = 300.0
    CUR = 480_000_000

    @pytest.mark.unit
    def test_spike_over_fast_brake_threshold_forces_hard_decrease(self):
        # 5min 平均没超红线(280<300),但最近 1min 飙到 380 > 红线×FAST_BRAKE_RATIO(360)
        # → 跳过慢回路,立即快刹(比常规 ×0.8 更狠)。
        new = rl.aimd_adjust(
            actual_gbps=280.0, target_gbps=self.TARGET, current_bwlimit=self.CUR,
            spike_gbps=380.0,
        )
        assert new == int(self.CUR * rl.FAST_BRAKE_FACTOR)
        assert new < int(self.CUR * rl.DECREASE_FACTOR), "快刹应比常规减速更狠"

    @pytest.mark.unit
    def test_spike_below_threshold_uses_normal_aimd(self):
        # 最近 1min 小幅高于红线(320)但未触快刹阈值(360),仍走慢回路:
        # 5min 平均 200 < 红线×0.9 → 加性增。
        new = rl.aimd_adjust(
            actual_gbps=200.0, target_gbps=self.TARGET, current_bwlimit=self.CUR,
            spike_gbps=320.0,
        )
        assert new == self.CUR + rl.INCREASE_STEP_BYTES

    @pytest.mark.unit
    def test_fast_brake_respects_floor(self):
        # 快刹也不会低于地板
        new = rl.aimd_adjust(
            actual_gbps=100.0, target_gbps=self.TARGET, current_bwlimit=1,
            spike_gbps=9999.0,
        )
        assert new >= rl.MIN_BWLIMIT_BYTES

    @pytest.mark.unit
    def test_no_spike_arg_disables_fast_brake(self):
        # 不传 spike(None)→ 快刹关闭,纯慢回路(向后兼容已有调用)。
        new = rl.aimd_adjust(
            actual_gbps=200.0, target_gbps=self.TARGET, current_bwlimit=self.CUR,
        )
        assert new == self.CUR + rl.INCREASE_STEP_BYTES

    @pytest.mark.unit
    def test_fast_brake_threshold_is_stricter_than_target(self):
        # 快刹阈值必须严格高于红线,否则常态轻微超标就误触发快刹
        assert rl.FAST_BRAKE_RATIO > 1.0


# ════════════════════════ 两开关决策 decide_bwlimit ════════════════════════
class TestDecideBwlimit:
    TARGET = 300.0
    CUR = 480_000_000

    @pytest.mark.unit
    def test_limit_disabled_returns_unlimited(self):
        # 旋钮①关 → 不限速(off=0),忽略其余一切
        new, mode = rl.decide_bwlimit(
            limit_enabled=False, auto_enabled=True, target_gbps=self.TARGET,
            actual_gbps=999.0, current_bwlimit=self.CUR,
        )
        assert new == 0
        assert mode == "unlimited"

    @pytest.mark.unit
    def test_limit_on_auto_off_holds_manual_value(self):
        # 旋钮①开 + 旋钮②关 → 用 bwlimit 手动固定值,不自动调
        new, mode = rl.decide_bwlimit(
            limit_enabled=True, auto_enabled=False, target_gbps=self.TARGET,
            actual_gbps=999.0, current_bwlimit=self.CUR,
        )
        assert new == self.CUR
        assert mode == "manual"

    @pytest.mark.unit
    def test_limit_on_auto_on_delegates_to_aimd(self):
        # 旋钮①②都开 → AIMD 自动调(此处超红线应减速)
        new, mode = rl.decide_bwlimit(
            limit_enabled=True, auto_enabled=True, target_gbps=self.TARGET,
            actual_gbps=360.0, current_bwlimit=self.CUR,
        )
        assert new == int(self.CUR * rl.DECREASE_FACTOR)
        assert mode == "auto"

    @pytest.mark.unit
    def test_auto_mode_propagates_fast_brake(self):
        # 自动模式下 spike 触发快刹同样生效
        new, mode = rl.decide_bwlimit(
            limit_enabled=True, auto_enabled=True, target_gbps=self.TARGET,
            actual_gbps=280.0, current_bwlimit=self.CUR, spike_gbps=380.0,
        )
        assert new == int(self.CUR * rl.FAST_BRAKE_FACTOR)
        assert mode == "auto"


# ════════════════════════ SSM 读取（fail-safe 不限速） ════════════════════════
class _FakeSsm:
    def __init__(self, value=None, raise_exc=None):
        self._value = value
        self._raise = raise_exc

    def get_parameter(self, *, Name):  # noqa: N803 - 对齐 boto3 大写参数名
        if self._raise is not None:
            raise self._raise
        return {"Parameter": {"Name": Name, "Value": self._value}}


@pytest.mark.unit
class TestReadBwlimit:
    def test_reads_value(self):
        ssm = _FakeSsm(value="480000000")
        assert rl.read_bwlimit(ssm, "/migration/ratelimit/bwlimit") == "480000000"

    def test_off_value_passthrough(self):
        ssm = _FakeSsm(value="off")
        assert rl.read_bwlimit(ssm, "/migration/ratelimit/bwlimit") == "off"

    def test_missing_param_failsafe_off(self):
        # 参数不存在(ParameterNotFound 等任何异常)→ 退化为不限速 "off"。
        ssm = _FakeSsm(raise_exc=RuntimeError("ParameterNotFound"))
        assert rl.read_bwlimit(ssm, "/migration/ratelimit/bwlimit") == "off"

    def test_blank_value_failsafe_off(self):
        # 空值视为不限速。
        ssm = _FakeSsm(value="")
        assert rl.read_bwlimit(ssm, "/migration/ratelimit/bwlimit") == "off"


@pytest.mark.unit
class TestReadSsmLimit:
    """通用 SSM 限速读取（tpslimit 复用，与 bwlimit 同 fail-safe 语义）。"""

    def test_reads_value(self):
        ssm = _FakeSsm(value="4000")
        assert rl.read_ssm_limit(ssm, "/migration/x/ratelimit/tpslimit") == "4000"

    def test_off_value_passthrough(self):
        ssm = _FakeSsm(value="off")
        assert rl.read_ssm_limit(ssm, "/migration/x/ratelimit/tpslimit") == "off"

    def test_missing_param_failsafe_off(self):
        ssm = _FakeSsm(raise_exc=RuntimeError("ParameterNotFound"))
        assert rl.read_ssm_limit(ssm, "/migration/x/ratelimit/tpslimit") == "off"

    def test_blank_value_failsafe_off(self):
        ssm = _FakeSsm(value="")
        assert rl.read_ssm_limit(ssm, "/migration/x/ratelimit/tpslimit") == "off"
