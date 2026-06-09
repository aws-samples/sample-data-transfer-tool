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

    def test_steady_at_target_does_not_brake_to_floor(self):
        # 真实稳态恒 200Gbps(=target)。修复后取完整桶 → actual≈spike≈200 → 落死区,保持不变;
        # 旧实现会把 actual 读成 ~20、spike 读成 ~100(假),且某些桶 >240 误触 fast-brake 砸地板。
        ssm = self._ssm()
        cw = _ScenarioCw(real_gbps=200.0, instances=30)
        out = rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = ssm.writes[self.BASE + "bwlimit"]
        # 死区保持上轮 100.00M,绝不是地板 9.54M
        assert written == "100.00M", f"稳态在目标应保持,不该砸地板,实际写={written}"
        assert out["mode"] == "auto"

    def test_genuinely_underloaded_increases_not_brakes(self):
        # 真实欠载:恒 50Gbps(<target×0.9=180)。修复后 actual=spike=50 → 加性增(不触 fast-brake)。
        ssm = self._ssm(bwlimit="50.00M")
        cw = _ScenarioCw(real_gbps=50.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 应比上轮 50M 高(加性增 +50MB/s,受 damp ±25% 限幅),而不是被砸到地板
        assert written > rc.parse_bwlimit_bytes("50.00M"), "真实欠载应加性增放开,而非刹车"
        assert written > rc.MIN_BWLIMIT_BYTES

    def test_genuine_spike_still_fast_brakes(self):
        # 真实尖峰:恒 300Gbps(>target×1.2=240)。修复后完整桶仍读到 300 → fast-brake 仍生效(没误删功能)。
        ssm = self._ssm(bwlimit="100.00M")
        cw = _ScenarioCw(real_gbps=300.0, instances=30)
        rc._process_cluster(ssm, cw, self._cluster(), slow=300, fast=60, emf_ns="GcsS3Migration")
        written = rc.parse_bwlimit_bytes(ssm.writes[self.BASE + "bwlimit"])
        # 真超速该收紧(受 damp ±25% 限幅,从 100M 最多降到 75M)
        assert written < rc.parse_bwlimit_bytes("100.00M"), "真实尖峰仍应 fast-brake 收紧"

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
        # 稳态在目标 → bwlimit 保持 100M(不砸地板),tpslimit=0.65
        assert ssm.writes[base + "bwlimit"] == "100.00M"
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
