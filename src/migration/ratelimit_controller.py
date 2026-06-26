"""限速控制器 Lambda(自包含单文件)——闭环 AIMD,每 60s 调整每进程 --bwlimit。

这个文件**就是 Lambda 部署包**(打 zip 传 S3,CFN `Code.S3Bucket/S3Key` 引用)。
控制律(aimd_adjust/decide_bwlimit/damp_step/safety_cap_bytes)是纯函数,被
tests/test_ratelimit_controller.py 直接单测——**生产跑的就是被测的代码,无内联分叉**。

测量(每集群独立):
- 吞吐 actual/spike = 本集群 ASG 的 EC2 NetworkIn(慢窗 5min / 快窗 1min)。
- 进程数 = 本集群 ASG InService 实例数 × WorkerThreads(取自 ASG,非心跳表)。
- 429 率 throttle = EMF AttemptCount{State=RETRYABLE,ErrorClass=src_rate_limit} 占比
  (EMF namespace 跨栈共享,throttle 是源端总体信号)。

env: CLUSTERS(单元素 JSON 列表 [{"name","asg","threads"}])、SLOW/FAST_WINDOW_SEC、EMF_NAMESPACE。
"""
from __future__ import annotations

import json
import os
import time

# ── 控制律常量 ──
DECREASE_FACTOR = 0.8            # 乘性减:超 effective_target ×0.8
INCREASE_STEP_BYTES = 50_000_000  # 加性增步长 +50MB/s
MIN_BWLIMIT_BYTES = 10_000_000    # 地板 10MB/s
DEADZONE_LOWER_RATIO = 0.9        # 死区下界
DEADZONE_UPPER_RATIO = 1.1        # 死区上界(暂未单独用,保留语义)
# 保守化(防过冲):慢回路瞄准 target×0.8 而非 target,稳态收敛到红线**下方**留 20% 余量
# 吸收"--bwlimit 仅对新进程生效"的 2-3min 控制滞后造成的向上过冲。激进档(主公决策)——
# 牺牲 ~20% 稳态带宽换"基本不超物理红线"。快刹仍守物理红线(见 FAST_BRAKE_RATIO)。
SAFETY_HEADROOM_RATIO = 0.8
# 阻尼(非对称):放开方向(增)限 +25% 温和爬升防震荡;收紧方向(减)放宽到 -50%,
# 让 fast-brake 的 ×0.5 一步真正落地(对称 ±25% 会把 ×0.5 重新夹回 -25%,刹不到位)。
MAX_STEP_UP_RATIO = 0.25
MAX_STEP_DOWN_RATIO = 0.5
FAST_BRAKE_RATIO = 1.1            # 快刹触发:spike > 物理红线×1.1(守物理红线,非 effective;
                                 # 稳态瞄 0.8×红线,留 1.1/0.8≈37% 噪声空间防 sawtooth 误刹)
FAST_BRAKE_FACTOR = 0.5           # 快速收紧系数 ×0.5
THROTTLE_OVERRIDE_RATE = 0.05     # 429 率超 5% 强制收紧
COLD_START_BWLIMIT_BYTES = 1_000_000_000  # 冷启动基数(off→需限速时)
SAFETY_MULTIPLIER = 4             # 护栏:公平份额 ×4
_BITS_PER_BYTE = 8
_GIGA = 1_000_000_000
_MIB = 1_048_576


# ─────────────────────────── 纯控制律(被单测)───────────────────────────
def clamp_floor(value: int) -> int:
    """收紧后不低于地板。"""
    return max(int(value), MIN_BWLIMIT_BYTES)


def aimd_adjust(*, actual_gbps: float, target_gbps: float, current_bwlimit: int,
                throttle_rate: float = 0.0, spike_gbps: float | None = None) -> int:
    """AIMD 控制律:返回新单进程 bwlimit(字节/秒,0=off)。

    两层目标(保守化防过冲):
    - 慢回路对照 effective_target = 物理红线 × SAFETY_HEADROOM_RATIO(如 0.8×400=320G),
      稳态收敛到红线**下方**留余量;
    - 快刹对照**物理红线** target_gbps × FAST_BRAKE_RATIO(异常突增才介入,不打 headroom)。
    优先级:红线off → 快刹(spike>物理红线×1.1) → 429(>5%) → 慢回路(超 effective 乘性减/
    欠载加性增/死区保持)。actual∈[effective, 物理红线] 时慢回路主动收紧——这正是 headroom 的本意。
    """
    if target_gbps <= 0:
        return 0
    base = current_bwlimit if current_bwlimit > 0 else COLD_START_BWLIMIT_BYTES
    # 快刹守物理红线(不打 headroom):仅当 1min spike 冲过物理红线×1.1 才半速急收。
    if spike_gbps is not None and spike_gbps > target_gbps * FAST_BRAKE_RATIO:
        return clamp_floor(int(base * FAST_BRAKE_FACTOR))
    if throttle_rate > THROTTLE_OVERRIDE_RATE:
        return clamp_floor(int(base * DECREASE_FACTOR))
    # 慢回路对照 effective_target(留 headroom),稳态压到红线下方。
    effective_target = target_gbps * SAFETY_HEADROOM_RATIO
    if actual_gbps > effective_target:
        return clamp_floor(int(base * DECREASE_FACTOR))
    if actual_gbps < effective_target * DEADZONE_LOWER_RATIO:
        return base + INCREASE_STEP_BYTES
    return current_bwlimit


def damp_step(*, old: int, proposed: int) -> int:
    """变化限幅(非对称):增向 +MAX_STEP_UP_RATIO(温和爬升),减向 -MAX_STEP_DOWN_RATIO
    (放宽,让快刹 ×0.5 一步落地)。proposed<=0(off)或 old<=0(冷启动)直接放行。"""
    if proposed <= 0:
        return proposed
    if old <= 0:
        return proposed
    lo = int(old * (1 - MAX_STEP_DOWN_RATIO))
    hi = int(old * (1 + MAX_STEP_UP_RATIO))
    return max(lo, min(proposed, hi))


def safety_cap_bytes(*, target_gbps: float, threads: int) -> int:
    """单进程宽松护栏 = 公平份额(全局目标÷进程数) × SAFETY_MULTIPLIER。"""
    if target_gbps <= 0:
        return 0
    global_target = int(target_gbps * _GIGA / _BITS_PER_BYTE)
    if threads <= 0:
        return global_target
    return global_target // threads * SAFETY_MULTIPLIER


def decide_bwlimit(*, limit_enabled: bool, auto_enabled: bool, target_gbps: float,
                   actual_gbps: float, current_bwlimit: int,
                   throttle_rate: float = 0.0, spike_gbps: float | None = None) -> tuple[int, str]:
    """两开关总决策。返回 (新 bwlimit 字节/秒, 模式名)。"""
    if not limit_enabled:
        return 0, "unlimited"
    if not auto_enabled:
        return current_bwlimit, "manual"
    new = aimd_adjust(actual_gbps=actual_gbps, target_gbps=target_gbps,
                      current_bwlimit=current_bwlimit, throttle_rate=throttle_rate,
                      spike_gbps=spike_gbps)
    return new, "auto"


def parse_bwlimit_bytes(raw: str) -> int:
    """rclone bwlimit 字符串(如 "123.45M"/"off")→ 字节/秒整数。"""
    if raw in ("", "off"):
        return 0
    s = raw.strip()
    try:
        if s.endswith("M"):
            return int(float(s[:-1]) * _MIB)
        if s.endswith("K"):
            return int(float(s[:-1]) * 1024)
        if s.endswith("G"):
            return int(float(s[:-1]) * _MIB * 1024)
        if s.endswith("B"):
            return int(float(s[:-1]))
        return int(float(s))
    except ValueError:
        return 0


def format_bwlimit(bytes_per_sec: int) -> str:
    """字节/秒 → rclone --bwlimit 值(<=0 → off,否则 "<X>M" MiB/s)。"""
    if bytes_per_sec <= 0:
        return "off"
    return f"{bytes_per_sec / _MIB:.2f}M"


# ─────────────────────────── tpslimit(全 fleet 总目标 ÷ 进程数)───────────────────────────
# 与 bwlimit 不同:tpslimit 无 AIMD 反馈,是纯静态分摊——人填"全 fleet 总 TPS 目标"
# (tpslimit-target),Lambda 每分钟测当前并发 rclone 进程数,做除法写回每进程的
# tpslimit(worker 读)。大文件 QPS 仅几十,默认大目标=不限的护栏;小文件高并发时
# 把总 QPS 钳在 GCS(读 ~5000/s)/S3(PUT ~3500/s·前缀)限制下,防 429/503。
def parse_tps_target(raw: str) -> float:
    """tpslimit-target 字符串 → 总 TPS 浮点。off/空/非法 → 0(不限)。"""
    if raw in ("", "off"):
        return 0.0
    try:
        return float(raw.strip())
    except ValueError:
        return 0.0


def process_count_from(*, instances: int, worker_count: int, worker_threads: int) -> int:
    """全 fleet 并发 rclone 进程数 = 实例数 × 每机进程数 × 每进程线程数。

    每个 (进程, 线程) 竞争消费者同一时刻最多跑 1 个 rclone,故并发 rclone 上限 =
    实例数 × WorkerCount × WorkerThreads(实测单机 16×12=192 活 rclone 印证)。
    tpslimit 是纯除法分摊,这个数必须算准(不能像 bwlimit 护栏那样漏乘 WorkerCount)。
    """
    return max(0, instances) * max(0, worker_count) * max(0, worker_threads)


def decide_tpslimit(*, total_tps: float, process_count: int) -> str:
    """每进程 --tpslimit 值 = 总目标 ÷ 进程数。

    total_tps<=0(off)→ "off"(不限);process_count<=0(冷启动无实例)→ "off"
    (无从分摊,下一轮有实例再算)。否则保留两位小数的纯数字字符串(次/秒,可小于 1)。
    """
    if total_tps <= 0:
        return "off"
    if process_count <= 0:
        return "off"
    return f"{total_tps / process_count:.2f}"


# ─────────────────────────── AWS 胶水(Lambda 运行时)───────────────────────────
def _make_clients():
    import boto3  # noqa: PLC0415 - 仅 Lambda 运行时需要,单测不触发
    return boto3.client("ssm"), boto3.client("cloudwatch")


def _get(ssm, name: str, default: str = "") -> str:
    try:
        return ssm.get_parameter(Name=name)["Parameter"]["Value"]
    except Exception as e:  # noqa: BLE001 - 读不到则用默认值,避免控制器异常退出
        print(f"SSM 读取参数失败 {name}: {e}")
        return default


def _network_in_gbps(cw, asg: str, window_sec: int) -> float:
    """ASG 的 EC2 NetworkIn 平均 Gbps(取最近一个**完整**周期桶)。

    根因修复(部分桶假尖峰):CloudWatch 的 Sum 桶按 epoch 对齐(60s 桶在 :00/:01...,
    300s 桶在 :00/:05...),而 EndTime=now 几乎不落在桶边界上 → 返回结果里**最后一个桶
    是"部分填充"**:它只累积了 now 之前已过去那几秒的字节,却仍 ÷ 完整 window_sec →
    被严重低估,且 window 越大低估越狠。旧实现取 [-1](部分桶)使 actual(300s)比
    spike(60s)虚低约 window 比(实测 5×),凭空制造 spike≫actual 的假尖峰 → 误触
    fast-brake(spike>target×1.2)把 bwlimit 砸到地板、进程饿死。
    修法:多查一个 window 余量(StartTime 回退 2×window),取**倒数第二个完整桶**,
    丢弃部分填充的最后桶。数据点不足 2 个时退回用仅有的桶(冷启动容错)。
    NetworkIn 是 period 累积字节,Gbps = Sum ÷ period × 8 ÷ 1e9(非 Average×8/1e9)。
    """
    now = int(time.time())
    res = cw.get_metric_statistics(
        Namespace="AWS/EC2", MetricName="NetworkIn",
        Dimensions=[{"Name": "AutoScalingGroupName", "Value": asg}],
        StartTime=now - window_sec * 2 - 60, EndTime=now, Period=window_sec, Statistics=["Sum"],
    )
    pts = sorted(res.get("Datapoints", []), key=lambda p: p["Timestamp"])
    if not pts:
        return 0.0
    # 倒数第二个 = 最近一个已走完的完整桶;只有一个桶时只能用它(数据不足容错)。
    pt = pts[-2] if len(pts) >= 2 else pts[-1]
    return pt["Sum"] / window_sec * _BITS_PER_BYTE / _GIGA


def _instances(cw, asg: str) -> int:
    """ASG 当前 InService 实例数(取最近一个数据点,四舍五入)。"""
    now = int(time.time())
    res = cw.get_metric_statistics(
        Namespace="AWS/AutoScaling", MetricName="GroupInServiceInstances",
        Dimensions=[{"Name": "AutoScalingGroupName", "Value": asg}],
        StartTime=now - 600, EndTime=now, Period=60, Statistics=["Average"],
    )
    pts = res.get("Datapoints", [])
    return int(round(sorted(pts, key=lambda p: p["Timestamp"])[-1]["Average"])) if pts else 0


def _active_threads(cw, asg: str, worker_threads: int) -> int:
    """bwlimit 护栏用的线程估算(实例数 × 每进程线程数)。"""
    return _instances(cw, asg) * worker_threads


def _sum_emf_attempt(cw, namespace: str, state: str, error_class: str | None, window_sec: int) -> float:
    now = int(time.time())
    dims = [{"Name": "State", "Value": state}]
    if error_class is not None:
        dims.append({"Name": "ErrorClass", "Value": error_class})
    try:
        res = cw.get_metric_statistics(
            Namespace=namespace, MetricName="AttemptCount", Dimensions=dims,
            StartTime=now - window_sec, EndTime=now, Period=window_sec, Statistics=["Sum"],
        )
        return sum(p["Sum"] for p in res.get("Datapoints", []))
    except Exception as e:  # noqa: BLE001
        print(f"EMF 指标查询失败: {e}")
        return 0.0


def _throttle_rate(cw, namespace: str, window_sec: int) -> float:
    throttled = _sum_emf_attempt(cw, namespace, "RETRYABLE", "src_rate_limit", window_sec)
    if throttled <= 0:
        return 0.0
    success = _sum_emf_attempt(cw, namespace, "SUCCESS", None, window_sec)
    total = throttled + success
    return throttled / total if total > 0 else 0.0


def _process_cluster(ssm, cw, cluster: dict, slow: int, fast: int, emf_ns: str) -> dict:
    name = cluster["name"]
    asg = cluster["asg"]
    threads_cfg = int(cluster["threads"])
    # worker_count 用于 tpslimit 精确进程数(实例×进程×线程);旧 CLUSTERS 无此键时默认 1。
    worker_count = int(cluster.get("worker_count", 1))
    base = "/migration/" + name + "/ratelimit/"
    limit_enabled = _get(ssm, base + "limit-enabled", "false").lower() == "true"
    auto_enabled = _get(ssm, base + "auto-enabled", "true").lower() == "true"
    current_bw = parse_bwlimit_bytes(_get(ssm, base + "bwlimit", "off"))
    target_gbps = float(_get(ssm, base + "target-gbps", "0") or "0")

    actual_gbps = spike_gbps = throttle = 0.0
    if limit_enabled and auto_enabled:
        actual_gbps = _network_in_gbps(cw, asg, slow)
        spike_gbps = _network_in_gbps(cw, asg, fast)
        throttle = _throttle_rate(cw, emf_ns, fast)

    new_bw, mode = decide_bwlimit(
        limit_enabled=limit_enabled, auto_enabled=auto_enabled, target_gbps=target_gbps,
        actual_gbps=actual_gbps, current_bwlimit=current_bw,
        throttle_rate=throttle, spike_gbps=spike_gbps)
    if mode == "auto" and new_bw > 0:
        threads = _active_threads(cw, asg, threads_cfg)
        cap = safety_cap_bytes(target_gbps=target_gbps, threads=threads)
        if cap > 0:
            new_bw = min(new_bw, cap)
        new_bw = damp_step(old=current_bw, proposed=new_bw)
        new_bw = max(new_bw, MIN_BWLIMIT_BYTES)
    value = format_bwlimit(new_bw)
    ssm.put_parameter(Name=base + "bwlimit", Value=value, Type="String", Overwrite=True)

    # ── tpslimit:全 fleet 总目标 ÷ 当前并发进程数(独立于 bwlimit/AIMD,无开关门控)──
    total_tps = parse_tps_target(_get(ssm, base + "tpslimit-target", "off"))
    instances = _instances(cw, asg)
    pcount = process_count_from(instances=instances, worker_count=worker_count,
                                worker_threads=threads_cfg)
    tps_value = decide_tpslimit(total_tps=total_tps, process_count=pcount)
    ssm.put_parameter(Name=base + "tpslimit", Value=tps_value, Type="String", Overwrite=True)

    print(f"[{name}] mode={mode} target={target_gbps:.1f} actual={actual_gbps:.1f} "
          f"spike={spike_gbps:.1f} throttle={throttle:.3f} -> bwlimit={value} | "
          f"tps_total={total_tps:.0f} procs={pcount}({instances}x{worker_count}x{threads_cfg}) "
          f"-> tpslimit={tps_value}")
    return {"cluster": name, "mode": mode, "bwlimit": value, "tpslimit": tps_value}


def handler(event, context):  # noqa: ARG001 - Lambda 签名
    ssm, cw = _make_clients()
    clusters = json.loads(os.environ["CLUSTERS"])
    slow = int(os.environ.get("SLOW_WINDOW_SEC", "300"))
    fast = int(os.environ.get("FAST_WINDOW_SEC", "60"))
    emf_ns = os.environ.get("EMF_NAMESPACE", "GcsS3Migration")
    results = []
    for cluster in clusters:
        try:
            results.append(_process_cluster(ssm, cw, cluster, slow, fast, emf_ns))
        except Exception as e:  # noqa: BLE001 - 单集群失败不影响其他
            print(f"[{cluster.get('name', '?')}] FAILED: {e}")
            results.append({"cluster": cluster.get("name", "?"), "error": str(e)})
    return {"clusters": results}
