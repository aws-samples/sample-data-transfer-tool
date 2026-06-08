"""Tests for ratelimit_controller.py — the self-contained rate-limit Lambda.

控制律(aimd_adjust/decide_bwlimit/damp_step/safety_cap_bytes)是纯函数,这里直接
单测 Lambda 部署的**真身文件**——生产跑的就是被测的代码,无内联分叉。
AWS 胶水(_network_in_gbps/handler 等)是 I/O,不在此测(需 boto3/CloudWatch)。
"""
from __future__ import annotations

import pytest

from migration import ratelimit_controller as rc

pytestmark = pytest.mark.unit


# ───────────────────────── format / parse ─────────────────────────────────
def test_format_bwlimit_zero_is_off():
    assert rc.format_bwlimit(0) == "off"
    assert rc.format_bwlimit(-1) == "off"


def test_format_bwlimit_uses_mib_suffix():
    assert rc.format_bwlimit(1_048_576).endswith("M")


def test_parse_roundtrip():
    assert rc.parse_bwlimit_bytes("off") == 0
    assert rc.parse_bwlimit_bytes("") == 0
    assert rc.parse_bwlimit_bytes("1.00M") == 1_048_576
    assert rc.parse_bwlimit_bytes("garbage") == 0


# ───────────────────────── aimd_adjust(慢回路+快刹+429)─────────────────────
class TestAimd:
    def test_target_off_returns_zero(self):
        assert rc.aimd_adjust(actual_gbps=50, target_gbps=0, current_bwlimit=1_000_000) == 0

    def test_fast_brake_on_spike(self):
        # spike > 红线×1.2 → ×0.5
        out = rc.aimd_adjust(actual_gbps=10, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=130)
        assert out == rc.clamp_floor(int(200_000_000 * rc.FAST_BRAKE_FACTOR))

    def test_throttle_override_multiplicative_decrease(self):
        out = rc.aimd_adjust(actual_gbps=10, target_gbps=100, current_bwlimit=200_000_000,
                             throttle_rate=0.10, spike_gbps=50)
        assert out == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))

    def test_over_target_multiplicative_decrease(self):
        out = rc.aimd_adjust(actual_gbps=150, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=50)
        assert out == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))

    def test_below_target_additive_increase(self):
        out = rc.aimd_adjust(actual_gbps=50, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=50)
        assert out == 200_000_000 + rc.INCREASE_STEP_BYTES

    def test_deadzone_holds(self):
        out = rc.aimd_adjust(actual_gbps=95, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=95)
        assert out == 200_000_000

    def test_cold_start_uses_base(self):
        # current=0 但需限速 → 用 COLD_START 基数
        out = rc.aimd_adjust(actual_gbps=50, target_gbps=100, current_bwlimit=0, spike_gbps=None)
        assert out == rc.COLD_START_BWLIMIT_BYTES + rc.INCREASE_STEP_BYTES

    def test_floor_respected(self):
        out = rc.aimd_adjust(actual_gbps=150, target_gbps=100,
                             current_bwlimit=rc.MIN_BWLIMIT_BYTES, spike_gbps=50)
        assert out >= rc.MIN_BWLIMIT_BYTES


# ───────────────────────── damp_step ───────────────────────────────────────
class TestDamp:
    def test_caps_increase_to_125pct(self):
        assert rc.damp_step(old=100_000_000, proposed=400_000_000) == 125_000_000

    def test_caps_decrease_to_75pct(self):
        assert rc.damp_step(old=100_000_000, proposed=10_000_000) == 75_000_000

    def test_small_change_passes(self):
        assert rc.damp_step(old=100_000_000, proposed=105_000_000) == 105_000_000

    def test_off_passthrough(self):
        assert rc.damp_step(old=100_000_000, proposed=0) == 0

    def test_cold_start_passthrough(self):
        assert rc.damp_step(old=0, proposed=400_000_000) == 400_000_000


# ───────────────────────── safety_cap_bytes ────────────────────────────────
class TestSafetyCap:
    def test_multiple_of_fair_share(self):
        cap = rc.safety_cap_bytes(target_gbps=100.0, threads=640)
        fair = int(100.0 * 1e9 / 8) // 640
        assert cap == fair * rc.SAFETY_MULTIPLIER

    def test_zero_threads_falls_back_to_global(self):
        cap = rc.safety_cap_bytes(target_gbps=100.0, threads=0)
        assert cap == int(100.0 * 1e9 / 8)

    def test_target_off_is_zero(self):
        assert rc.safety_cap_bytes(target_gbps=0, threads=10) == 0


# ───────────────────────── parse / decide tpslimit(总÷进程数)──────────────
class TestTpslimit:
    def test_parse_off_and_blank_zero(self):
        assert rc.parse_tps_target("off") == 0.0
        assert rc.parse_tps_target("") == 0.0
        assert rc.parse_tps_target("garbage") == 0.0

    def test_parse_numeric(self):
        assert rc.parse_tps_target("10000") == 10000.0
        assert rc.parse_tps_target("500.5") == 500.5

    def test_target_off_yields_off(self):
        # 总目标 off/0 → 每进程 "off"(不限)
        assert rc.decide_tpslimit(total_tps=0, process_count=192) == "off"

    def test_cold_start_no_process_yields_off(self):
        # 无实例(冷启动)→ 无从分摊 → off,下一轮有实例再算
        assert rc.decide_tpslimit(total_tps=10000, process_count=0) == "off"

    def test_divides_total_by_process_count(self):
        # 10000 总 ÷ 192 进程 = 52.08/进程
        assert rc.decide_tpslimit(total_tps=10000, process_count=192) == "52.08"

    def test_small_total_large_fleet(self):
        # 实测对照:总 ~9.6 ÷ 192 = 0.05/进程
        assert rc.decide_tpslimit(total_tps=9.6, process_count=192) == "0.05"

    def test_process_count_is_instances_x_count_x_threads(self):
        # 2 实例 × 16 进程/机 × 12 线程 = 384 并发 rclone
        assert rc.process_count_from(instances=2, worker_count=16, worker_threads=12) == 384


# ───────────────────────── decide_bwlimit(真值表)──────────────────────────
class TestDecide:
    def test_limit_disabled_unlimited(self):
        bw, mode = rc.decide_bwlimit(limit_enabled=False, auto_enabled=True, target_gbps=100,
                                     actual_gbps=50, current_bwlimit=200_000_000)
        assert (bw, mode) == (0, "unlimited")

    def test_manual_holds_current(self):
        bw, mode = rc.decide_bwlimit(limit_enabled=True, auto_enabled=False, target_gbps=100,
                                     actual_gbps=50, current_bwlimit=200_000_000)
        assert (bw, mode) == (200_000_000, "manual")

    def test_auto_over_target_decreases(self):
        bw, mode = rc.decide_bwlimit(limit_enabled=True, auto_enabled=True, target_gbps=100,
                                     actual_gbps=150, current_bwlimit=200_000_000, spike_gbps=50)
        assert mode == "auto"
        assert bw == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))
