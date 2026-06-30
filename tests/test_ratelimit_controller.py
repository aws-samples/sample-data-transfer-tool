"""Tests for ratelimit_controller.py — the self-contained rate-limit Lambda.

控制律(aimd_adjust/decide_bwlimit/damp_step/safety_cap_bytes)是纯函数,这里直接
单测 Lambda 部署的**真身文件**——生产跑的就是被测的代码,无内联分叉。
_network_in_gbps 的取桶逻辑用 fake CloudWatch client 注入 datapoints 测(见 TestNetworkInGbps)。
handler 等 I/O 胶水不在此测。
"""
from __future__ import annotations

import datetime as _dt

import pytest

from migration import ratelimit_controller as rc

pytestmark = pytest.mark.unit


class _FakeCw:
    """注入预置 datapoints 的假 CloudWatch client,忽略 StartTime/EndTime/Period。

    只为验证 _network_in_gbps 从返回的多个桶里**取哪个桶**——与查询时间窗无关。
    """

    def __init__(self, datapoints):
        self._datapoints = datapoints

    def get_metric_statistics(self, **_kwargs):
        return {"Datapoints": self._datapoints}


def _bucket(ts_minute: int, sum_bytes: float) -> dict:
    base = _dt.datetime(2026, 6, 9, 14, 0, tzinfo=_dt.timezone.utc)
    return {"Timestamp": base + _dt.timedelta(minutes=ts_minute), "Sum": sum_bytes}


def _gbps_to_sum(gbps: float, period_sec: int) -> float:
    """真实 Gbps + 完整周期 → 该桶应有的 Sum 字节数。"""
    return gbps * 1e9 / 8 * period_sec


class _ScenarioCw:
    """按 MetricName/Period/Dimensions 返回对应桶的假 CloudWatch，复现生产场景。

    NetworkIn:返回 [完整桶(real_gbps), 部分桶(只填 partial_ratio)] —— 修复后取完整桶,
    旧实现取部分桶(虚低)。GroupInServiceInstances:返回 Average=instances。
    AttemptCount:按 State 维度返回 throttle/success 计数(算 429 率)。
    """

    def __init__(self, *, real_gbps: float, instances: int,
                 partial_ratio: float = 0.1, throttle_attempts: float = 0.0,
                 success_attempts: float = 1000.0):
        self.real_gbps = real_gbps
        self.instances = instances
        self.partial_ratio = partial_ratio
        self.throttle_attempts = throttle_attempts
        self.success_attempts = success_attempts

    def get_metric_statistics(self, **kw):
        metric = kw["MetricName"]
        if metric == "NetworkIn":
            period = kw["Period"]
            full = _gbps_to_sum(self.real_gbps, period)
            # 完整桶 + 一个只填 partial_ratio 的部分桶(模拟 EndTime=now 落桶中间)
            return {"Datapoints": [_bucket(0, full), _bucket(period // 60 or 1, full * self.partial_ratio)]}
        if metric == "GroupInServiceInstances":
            return {"Datapoints": [_bucket(0, 0)] and [{"Timestamp": _bucket(0, 0)["Timestamp"],
                                                        "Average": float(self.instances)}]}
        if metric == "AttemptCount":
            dims = {d["Name"]: d["Value"] for d in kw.get("Dimensions", [])}
            if dims.get("State") == "RETRYABLE":
                return {"Datapoints": [{"Timestamp": _bucket(0, 0)["Timestamp"], "Sum": self.throttle_attempts}]}
            return {"Datapoints": [{"Timestamp": _bucket(0, 0)["Timestamp"], "Sum": self.success_attempts}]}
        return {"Datapoints": []}


class _FakeSsm:
    """内存 SSM:预置参数 + 记录 put_parameter 写回值,用于断言 Lambda 决策结果。"""

    def __init__(self, params: dict):
        self.params = dict(params)
        self.writes: dict = {}

    def get_parameter(self, Name):  # noqa: N803 - 对齐 boto3 签名
        if Name not in self.params:
            raise KeyError(Name)
        return {"Parameter": {"Value": self.params[Name]}}

    def put_parameter(self, Name, Value, Type, Overwrite):  # noqa: N803, ARG002
        self.params[Name] = Value
        self.writes[Name] = Value


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
        # spike > 物理红线×1.1 → ×0.5(快刹守物理红线,不打 headroom)
        out = rc.aimd_adjust(actual_gbps=10, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=130)
        assert out == rc.clamp_floor(int(200_000_000 * rc.FAST_BRAKE_FACTOR))

    def test_fast_brake_boundary_at_1_1(self):
        # 边界:spike 在 物理红线×1.1(=110) 之下不快刹,之上快刹。actual 落新死区隔离慢回路。
        base = 200_000_000
        below = rc.aimd_adjust(actual_gbps=76, target_gbps=100, current_bwlimit=base, spike_gbps=105)
        assert below == base, "spike<红线×1.1 不应快刹"
        above = rc.aimd_adjust(actual_gbps=76, target_gbps=100, current_bwlimit=base, spike_gbps=115)
        assert above == rc.clamp_floor(int(base * rc.FAST_BRAKE_FACTOR)), "spike>红线×1.1 应快刹"

    def test_headroom_decrease_between_effective_and_physical(self):
        # headroom 核心:actual 在 effective(target×0.8=80) 与物理红线(100) 之间 →
        # 慢回路主动 ×0.8 收紧,把稳态拉到红线下方。spike=85<110 不触发快刹。
        out = rc.aimd_adjust(actual_gbps=85, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=85)
        assert out == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))

    def test_throttle_override_multiplicative_decrease(self):
        out = rc.aimd_adjust(actual_gbps=10, target_gbps=100, current_bwlimit=200_000_000,
                             throttle_rate=0.10, spike_gbps=50)
        assert out == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))

    def test_over_target_multiplicative_decrease(self):
        out = rc.aimd_adjust(actual_gbps=150, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=50)
        assert out == rc.clamp_floor(int(200_000_000 * rc.DECREASE_FACTOR))

    def test_below_target_additive_increase(self):
        # actual 远低于 effective(=target×0.8=80)的死区下界(72)→ 加性增。
        # actual=50 不算空闲(>target×IDLE_ACTUAL_RATIO=5),不受 fair-share 冷启动夹制。
        out = rc.aimd_adjust(actual_gbps=50, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=50)
        assert out == 200_000_000 + rc.INCREASE_STEP_BYTES

    # ── 公平份额冷启动(防空闲后来活聚合过冲)──
    def test_idle_cold_start_clamped_to_fair_share(self):
        # 长时间空闲:actual≈0(< target×IDLE_ACTUAL_RATIO)。给了 fair_share → 加性增被夹到
        # fair_share,而非顶到 cap。根因:空闲后大量进程同时满速会聚合冲顶,初值必须是
        # "全满速正好=红线"的公平份额,不能是补偿有效并发率的高 cap 值。
        fair = int(400 * 1e9 / 8) // 7680   # ≈6.51M
        out = rc.aimd_adjust(actual_gbps=0.0, target_gbps=400, current_bwlimit=0,
                             spike_gbps=0.0, fair_share_bytes=fair)
        assert out == fair, f"空闲冷启动应夹到 fair_share({fair}),实际={out}"

    def test_idle_existing_high_bwlimit_pulled_down_to_fair_share(self):
        # 空闲但上轮残留高值(49.67M cap):actual≈0 → 加性增分支,夹到 fair_share,
        # 把残留高值拉回公平份额(下一轮 damp 限幅平滑下降)。
        fair = int(400 * 1e9 / 8) // 7680
        out = rc.aimd_adjust(actual_gbps=0.0, target_gbps=400, current_bwlimit=50_000_000,
                             spike_gbps=0.0, fair_share_bytes=fair)
        assert out == fair, "空闲应拉回 fair_share,不停留在残留高值"

    def test_genuine_underload_not_clamped_by_fair_share(self):
        # 真欠载:actual=200(> target400×0.05=20,不算空闲)→ 正常加性增爬向 cap 补偿
        # 有效并发率,绝不被 fair_share 夹住(否则稳态 14.9M 达不到)。
        fair = int(400 * 1e9 / 8) // 7680   # 6.51M,远低于 base
        out = rc.aimd_adjust(actual_gbps=200, target_gbps=400, current_bwlimit=100_000_000,
                             spike_gbps=200, fair_share_bytes=fair)
        assert out == 100_000_000 + rc.INCREASE_STEP_BYTES, "真欠载应正常爬,不被 fair_share 夹"

    def test_fair_share_none_keeps_legacy_behavior(self):
        # 向后兼容:不传 fair_share → 空闲也走原加性增(顶到 cap 由外层处理),行为不变。
        out = rc.aimd_adjust(actual_gbps=0.0, target_gbps=400, current_bwlimit=0, spike_gbps=0.0)
        assert out == rc.COLD_START_BWLIMIT_BYTES + rc.INCREASE_STEP_BYTES

    def test_deadzone_holds(self):
        # 新死区围绕 effective_target=target×0.8=80 → [effective×0.9, effective]=[72,80]。
        # actual=76 落入死区 → 保持;spike=76<物理红线×1.1=110 不触发快刹。
        out = rc.aimd_adjust(actual_gbps=76, target_gbps=100, current_bwlimit=200_000_000,
                             spike_gbps=76)
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
        # 放开方向(增)仍限幅 +25%:温和爬升防震荡。
        assert rc.damp_step(old=100_000_000, proposed=400_000_000) == 125_000_000

    def test_caps_decrease_to_50pct(self):
        # 收紧方向(减)放宽到 -50%:让 fast-brake 的 ×0.5 真正一步落地。
        assert rc.damp_step(old=100_000_000, proposed=10_000_000) == 50_000_000

    def test_decrease_within_limit_passes(self):
        # 降幅在 -50% 内 → 原样通过(80M 相对 100M 只降 20%)。
        assert rc.damp_step(old=100_000_000, proposed=80_000_000) == 80_000_000

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


class TestFloorBelowRedline:
    """不变式:地板 MIN_BWLIMIT_BYTES 必须 ≤ 公平份额,使"全 fleet 满速"聚合不超红线。
    否则冷启动/空闲后大量进程同时满速,光靠地板就能聚合过冲(10M 地板时 644G>400G)。
    锁死此关系,防将来改回高地板而不自知。"""

    def test_floor_at_or_below_fair_share_keeps_aggregate_under_redline(self):
        target = 400.0
        procs = 7680  # m6in-pool1 = 30×16×16(最大并发集群)
        fair = rc.fair_share_bytes_of(target_gbps=target, threads=procs)
        assert rc.MIN_BWLIMIT_BYTES <= fair, (
            f"地板({rc.MIN_BWLIMIT_BYTES})必须 ≤ 公平份额({fair}),否则满速聚合超红线")
        # 全满速理论聚合(Gbps)必须 < 红线
        agg_gbps = rc.MIN_BWLIMIT_BYTES * 8 * procs / 1e9
        assert agg_gbps < target, f"地板满速聚合 {agg_gbps:.0f}G 必须 < 红线 {target}G"


class TestActiveThreads:
    """护栏除数 = 真实并发 rclone 进程数 = instances × worker_count × worker_threads。
    根因修复:旧实现漏乘 worker_count,使护栏除数偏小 worker_count 倍 → cap 被放大
    同等倍数 → 形同虚设(注释 process_count_from 已点名"不能像 bwlimit 护栏那样漏乘")。
    """

    def test_includes_worker_count(self):
        cw = _ScenarioCw(real_gbps=0.0, instances=30)
        # 30 实例 × 16 进程/机 × 16 线程 = 7680,而非旧实现的 30×16=480
        assert rc._active_threads(cw, "asg-x", worker_threads=16, worker_count=16) == 7680

    def test_aligns_with_process_count_from(self):
        cw = _ScenarioCw(real_gbps=0.0, instances=30)
        # 与 tpslimit 用的 process_count_from 必须算出同一个并发进程数(单一真相)
        assert rc._active_threads(cw, "asg-x", worker_threads=16, worker_count=16) == \
            rc.process_count_from(instances=30, worker_count=16, worker_threads=16)


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


# ───────────────────── _network_in_gbps 取桶(部分桶 bug 回归)──────────────
class TestNetworkInGbps:
    """根因回归:CloudWatch 按 epoch 对齐切桶,EndTime=now 落在桶中间 → 最后一个桶
    是"部分填充"(只累积了已过去的秒数,却仍 ÷ 完整 period)→ 被严重低估,且 period
    越大低估越狠 → 制造出 spike(1min)≫actual(5min) 的假尖峰,误触 fast-brake。
    修法:取倒数第二个**完整桶**,丢弃部分填充的最后桶。
    """

    @staticmethod
    def _pt(ts_minute: int, sum_bytes: float) -> dict:
        # 用固定基准时间构造可排序的 Timestamp(不依赖 now,适配受限环境)。
        base = _dt.datetime(2026, 6, 9, 14, 0, tzinfo=_dt.timezone.utc)
        return {"Timestamp": base + _dt.timedelta(minutes=ts_minute), "Sum": sum_bytes}

    def test_takes_last_complete_bucket_not_partial(self):
        # 完整 300s 桶:真实 200Gbps → Sum = 200e9/8*300 = 7.5e12 字节
        full_sum = 200 * 1e9 / 8 * 300
        # 部分桶:同样 200Gbps 但只过了 30s → Sum 仅 1/10 = 7.5e11(代码旧实现会取它 → 算成 20Gbps)
        partial_sum = full_sum / 10
        cw = _FakeCw([self._pt(0, full_sum), self._pt(5, partial_sum)])
        gbps = rc._network_in_gbps(cw, "asg-x", 300)
        # 应取完整桶 → 还原 200Gbps,而非部分桶的 20Gbps
        assert gbps == pytest.approx(200.0, abs=0.5), (
            f"应取倒数第二个完整桶(200Gbps),却取了部分桶得 {gbps:.1f}Gbps"
        )

    def test_spike_and_actual_agree_on_steady_traffic(self):
        # 稳态恒定 200Gbps:spike(60s 桶)与 actual(300s 桶)取完整桶后应一致,无假尖峰。
        spike_full = 200 * 1e9 / 8 * 60      # 完整 60s 桶
        actual_full = 200 * 1e9 / 8 * 300    # 完整 300s 桶
        # 各自带一个部分桶(只过了 30s)在最后
        cw_spike = _FakeCw([self._pt(0, spike_full), self._pt(1, spike_full / 2)])
        cw_actual = _FakeCw([self._pt(0, actual_full), self._pt(5, actual_full / 10)])
        spike = rc._network_in_gbps(cw_spike, "asg-x", 60)
        actual = rc._network_in_gbps(cw_actual, "asg-x", 300)
        assert spike == pytest.approx(actual, abs=1.0), (
            f"稳态下 spike({spike:.1f}) 与 actual({actual:.1f}) 应一致,差异=部分桶假尖峰"
        )

    def test_single_bucket_still_works(self):
        # 只有一个桶(数据点不足)时不能崩,直接用它。
        full_sum = 100 * 1e9 / 8 * 300
        cw = _FakeCw([self._pt(0, full_sum)])
        assert rc._network_in_gbps(cw, "asg-x", 300) == pytest.approx(100.0, abs=0.5)

    def test_no_datapoints_returns_zero(self):
        assert rc._network_in_gbps(_FakeCw([]), "asg-x", 300) == 0.0


# ─────────── _process_cluster / handler 端到端(复现生产自残场景)────────────
class TestProcessClusterIntegration:
    """完整编排集成测试:读 SSM → 测 CloudWatch → AIMD → 写回 SSM。
    重点复现你的生产场景(target=200,真实 actual≈200 但旧实现因部分桶虚低到 57.6
    + 假尖峰 288 误触 fast-brake 砸地板),验证修复后不再误刹车。
    """

    BASE = "/migration/m6in-pool1/ratelimit/"

    def _ssm(self, **over):
        params = {
            self.BASE + "limit-enabled": "true",
            self.BASE + "auto-enabled": "true",
            self.BASE + "bwlimit": "100.00M",      # 上轮值
            self.BASE + "target-gbps": "200",
            self.BASE + "tpslimit-target": "off",
        }
        params.update({self.BASE + k: v for k, v in over.items()})
        return _FakeSsm(params)

    def _cluster(self):
        return {"name": "m6in-pool1", "asg": "asg-m6in", "threads": 16, "worker_count": 16}

    def test_steady_at_target_decreases_for_headroom(self):
        # 真实稳态恒 200Gbps(=物理 target)。headroom 后 effective=160,actual=200>160
        # → 慢回路主动 ×0.8 收紧,把稳态拉到红线下方(留 20% 余量,防过冲)。
        # spike=200<物理红线×1.1=220 → 不触发 fast-brake;收紧受非对称 damp -50% 限幅。
        ssm = self._ssm()
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        out = rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 上轮 100M,慢回路 ×0.8=80M,降幅 20% 在 damp -50% 内 → 写 80M(主动收紧,不再贴红线)
        assert written < rc.parse_bwlimit_bytes("100.00M"), "稳态在物理红线应为 headroom 主动收紧"
        assert out["mode"] == "auto"

    def test_genuinely_underloaded_increases_not_brakes(self):
        # 真实欠载:恒 50Gbps(<effective×0.9=144)。actual=spike=50 → 加性增(不触 fast-brake)。
        # 上轮取 10M(< target200 cap 24.8M),确保加性增有空间、不被 cap 提前夹住。
        ssm = self._ssm(bwlimit="10.00M")
        cw = _ScenarioCw(real_gbps=50.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 应比上轮 10M 高(加性增 +50MB/s,受 damp +25% 限幅 → 12.5M),而不是被砸到地板
        assert written > rc.parse_bwlimit_bytes("10.00M"), "真实欠载应加性增放开,而非刹车"
        assert written > rc.MIN_BWLIMIT_BYTES

    def test_in_headroom_band_decreases(self):
        # actual 落在 effective(160) 与物理红线(200) 之间(=180):headroom 的核心场景——
        # 慢回路认定"超 effective"主动收紧,把吞吐压回红线下方。spike=180<220 不快刹。
        ssm = self._ssm()
        cw = _ScenarioCw(real_gbps=180.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        assert written < rc.parse_bwlimit_bytes("100.00M"), "headroom 区间应主动收紧"

    def test_genuine_spike_still_fast_brakes(self):
        # 真实尖峰:恒 300Gbps(>物理红线×1.1=220)。完整桶读到 300 → fast-brake 仍生效。
        ssm = self._ssm(bwlimit="100.00M")
        cw = _ScenarioCw(real_gbps=300.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 快刹 ×0.5=50M,降幅 50% 恰在 damp -50% 边界内 → 一步落地到 50M
        assert written < rc.parse_bwlimit_bytes("100.00M"), "真实尖峰仍应 fast-brake 收紧"
        assert written <= rc.parse_bwlimit_bytes("50.00M") + 1, "非对称 damp 应让 ×0.5 一步落地"

    def test_cold_start_no_instances_does_not_emit_giant_bwlimit(self):
        # 为空爆高根治:冷启动 instances 指标读到 0(GroupInServiceInstances 无数据点)时,
        # AIMD 冷启动基数=1GB + 旧 cap 退化(threads<=0→global_target≈50G)→ 放行 1GB/进程
        # → 7680 进程瞬间爆破红线(520G 过冲图的冷启动那一冲)。修复:proc_count<=0 时不放行
        # AIMD 高值,夹到上一轮 current_bw(无则地板),等指标恢复再爬。
        ssm = self._ssm(bwlimit="off")          # 从 off 恢复 = 冷启动
        cw = _ScenarioCw(real_gbps=0.0, instances=0)   # 指标缺失
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 绝不能是 1GB 级:current=off → 夹到地板量级(~10M,format 两位小数 MiB 有微小取整)
        assert written <= rc.parse_bwlimit_bytes("11.00M"), f"冷启动无实例应夹到地板量级,实际={written}"

    def test_cold_start_no_instances_holds_previous_when_present(self):
        # 指标抖动(中途某轮 instances 读到 0)且已有上轮值 → 维持 current_bw,不波动也不放行高值。
        ssm = self._ssm(bwlimit="50.00M")
        cw = _ScenarioCw(real_gbps=0.0, instances=0)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        assert written == rc.parse_bwlimit_bytes("50.00M"), f"指标抖动应维持上轮,实际={written}"

    def test_empty_queue_climb_capped_by_adaptive_safety_cap(self):
        # 空队列持续爬升:actual≈0 → AIMD 想加性增。修 worker_count 后自适应 cap 收敛到
        # target200/8 /7680进程 ×8 ≈ 24.8M(而非旧漏乘的 ~400M)。上轮 30M 已超 cap → 被钳到 cap。
        # (上轮取 30M 而非 200M:damp 每轮限降 50%,200M 一轮只能到 100M 看不出 cap 生效;
        #  30M 距 cap 24.8M 在 damp -50% 内,可一步钳到 cap,直接验证 cap 而非 damp。)
        ssm = self._ssm(bwlimit="30.00M")
        cw = _ScenarioCw(real_gbps=0.0, instances=30)   # 空队列,实例正常上报
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        expected_cap = int(200 * 1e9 / 8) // 7680 * rc.SAFETY_MULTIPLIER   # mult=8 → ≈24.8M
        # 容忍 format_bwlimit 两位小数 MiB 往返取整(~0.01M 误差)→ 用 1% 相对容差
        assert written <= expected_cap * 1.01, f"空爬应被自适应 cap({expected_cap})钳住,实际={written}"
        assert written < rc.parse_bwlimit_bytes("60.00M"), "应远低于旧漏乘 cap(~400M)"

    def test_idle_cluster_converges_toward_fair_share_not_cap(self):
        # 端到端:空闲集群(actual=0)上轮残留 50M cap 值 → fair_share 冷启动夹制生效,bwlimit
        # 开始向 fair_share(3.26M,<地板→实际目标地板10M)收敛。但单轮受 damp -50% 限幅:
        # 50M→25M(本轮),非一步到位;多轮 50→25→12.5→10(地板)。关键:不再停在 50M(过冲源)。
        ssm = self._ssm(bwlimit="50.00M")          # 上轮残留高值
        cw = _ScenarioCw(real_gbps=0.0, instances=30)   # 空闲,实例正常
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 单轮 damp 限降 50% → 25M;关键断言:严格小于残留 50M(在向 fair_share 下降,不再停在过冲值)
        assert written < rc.parse_bwlimit_bytes("50.00M"), "空闲应开始向 fair_share 下降,不停在残留高值"
        assert written <= rc.parse_bwlimit_bytes("25.00M") + 100_000, f"单轮应 damp 到上轮一半,实际={written}"

    def test_idle_cluster_from_off_starts_at_fair_share_floor(self):
        # 从 off 冷启动 + 空闲:current=0 无 damp 下界束缚 → 直接落 fair_share(3.26M→地板10M),
        # 而非旧逻辑的 cap 49.67M(=447G 过冲源)。这是冷启动过冲的直接修复验证。
        ssm = self._ssm(bwlimit="off")
        cw = _ScenarioCw(real_gbps=0.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # fair_share(3.26M)<地板 → 落地板 10M,远低于旧 cap 25M(target200)/49.67M(target400)
        assert written <= rc.MIN_BWLIMIT_BYTES + 100_000, f"off 冷启动空闲应落 fair_share/地板,实际={written}"

    def test_tpslimit_written_from_target_and_proccount(self):
        # tpslimit = target ÷ (实例×进程×线程)。tps_target=5000,30×16×16=7680 → 0.65
        ssm = self._ssm(**{"tpslimit-target": "5000"})
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        assert ssm.writes[self.BASE + "tpslimit"] == "0.65"

    def test_tpslimit_off_writes_off(self):
        ssm = self._ssm(**{"tpslimit-target": "off"})
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        assert ssm.writes[self.BASE + "tpslimit"] == "off"

    def test_manual_mode_holds_bwlimit(self):
        ssm = self._ssm(**{"auto-enabled": "false", "bwlimit": "42.00M"})
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        assert ssm.writes[self.BASE + "bwlimit"] == "42.00M"  # manual 不动

    def test_limit_disabled_writes_off(self):
        ssm = self._ssm(**{"limit-enabled": "false"})
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        assert ssm.writes[self.BASE + "bwlimit"] == "off"


class TestHandlerEndToEnd:
    """handler 端到端:monkeypatch _make_clients 注入 fake,验证 Lambda 入口完整跑通。"""

    def test_handler_processes_cluster_and_writes_ssm(self, monkeypatch):
        base = "/migration/m6in-pool1/ratelimit/"
        ssm = _FakeSsm({
            base + "limit-enabled": "true", base + "auto-enabled": "true",
            base + "bwlimit": "100.00M", base + "target-gbps": "200",
            base + "tpslimit-target": "5000",
        })
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        monkeypatch.setattr(rc, "_make_clients", lambda: (ssm, cw))
        monkeypatch.setenv("CLUSTERS",
            '[{"name":"m6in-pool1","asg":"asg-m6in","threads":16,"worker_count":16}]')
        monkeypatch.setenv("SLOW_WINDOW_SEC", "300")
        monkeypatch.setenv("FAST_WINDOW_SEC", "60")
        result = rc.handler({}, None)
        assert result["clusters"][0]["cluster"] == "m6in-pool1"
        # 稳态 200G > effective160 → headroom ×0.8=80M → 自适应 cap(24.8M)夹 → damp 下界(上轮100M×0.5)
        # 把单轮降幅限制住 → 落 50M。多轮会继续向 cap 收敛。tpslimit=0.65 不变。
        assert ssm.writes[base + "bwlimit"] == "50.00M"
        assert ssm.writes[base + "tpslimit"] == "0.65"

    def test_handler_one_cluster_failure_isolated(self, monkeypatch):
        # 读 SSM 是 fail-safe(_get 吞错返回默认值),不会冒泡;但写 SSM 无兜底 →
        # put_parameter 抛错触发 handler 的单集群隔离 catch(代码 :290),整体不崩。
        class _BoomSsm:
            def get_parameter(self, Name):  # noqa: N803
                return {"Parameter": {"Value": "true"}}
            def put_parameter(self, **_):
                raise RuntimeError("boom-on-write")
        monkeypatch.setattr(rc, "_make_clients", lambda: (_BoomSsm(), _ScenarioCw(real_gbps=200, instances=30)))
        monkeypatch.setenv("CLUSTERS", '[{"name":"bad","asg":"asg-bad","threads":16,"worker_count":16}]')
        result = rc.handler({}, None)
        assert "error" in result["clusters"][0]


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
