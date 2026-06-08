"""闭环自动限速控制律（纯函数，无 AWS 依赖）。

控制器对比"全机群实际吞吐 vs 红线",用 AIMD（加性增、乘性减）调整每进程
``--bwlimit``。两个时间尺度解耦：

- 慢回路（``actual_gbps``）：过去 5min 滑动平均，抗 1min 抖动，平缓收放。
- 快保护（``spike_gbps``）：最近 1min 值，超过红线×``FAST_BRAKE_RATIO`` 即
  跳过慢回路立即快速收紧（fast brake，防 GCS 配额突增 / 429 激增）。

本模块只做数学决策，不读 SSM、不调 CloudWatch——便于 100% 单测。
"""
from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

# ── AIMD 常量 ────────────────────────────────────────────────────────────────
# 乘性减系数：超红线时 bwlimit ×0.8（快速压下，不对称收敛到红线下方）。
DECREASE_FACTOR = 0.8
# 加性增步长（字节/秒）：低于红线时 +50MB/s 温和试探，约 0.4Gbps/进程。
INCREASE_STEP_BYTES = 50_000_000
# 地板：单进程 bwlimit 不低于 10MB/s，防连续收紧把传输限到停滞。
MIN_BWLIMIT_BYTES = 10_000_000
# 死区下界比例：实际在 [红线×0.9, 红线×1.1] 之间不动，防在红线附近震荡。
DEADZONE_LOWER_RATIO = 0.9
# 死区上界比例：实际超红线但未超 红线×1.1 时也不收紧（容忍 10% 瞬时过冲，
# 减少测量滞后导致的无谓收紧/震荡）。仅当超过 红线×1.1 才 -step。
DEADZONE_UPPER_RATIO = 1.1
# 变化限幅（阻尼）：每轮 bwlimit 相对上一轮最多变化 ±25%。
# 根因——bwlimit 仅对新进程生效 + 大文件传输周期长 → 控制响应滞后 2-3min，远大于
# 控制周期 1min。AIMD 拿"旧 bwlimit 的果"调"新 bwlimit 的因"，一轮从 74M 骤降到 9M
# 造成输出震荡。限制每步变化率，把震荡平滑掉（牺牲反应速度换稳定），是滞后系统的标准阻尼。
MAX_STEP_RATIO = 0.25

# ── 双阈值快速收紧（fast brake）─────────────────────────────────────────────────
# 触发比例：最近 1min spike > 红线×1.2 即认定吞吐突增，立即快速收紧。
FAST_BRAKE_RATIO = 1.2
# 收紧系数：比常规 DECREASE_FACTOR 收紧幅度更大（×0.5），一步降至半速。
FAST_BRAKE_FACTOR = 0.5

# ── 429 防雪崩 ───────────────────────────────────────────────────────────────
# 429 率超此阈值即使吞吐没到红线也强制收紧（源端已在拒绝，再加压只会更糟）。
THROTTLE_OVERRIDE_RATE = 0.05

# ── 冷启动 ───────────────────────────────────────────────────────────────────
# current_bwlimit=0（不限速/off）却需收紧时的初始估算上限（字节/秒）。
# 取一个有限大值，让 AIMD 有可乘的基数，否则从 0 乘永远是 0、收不住。
COLD_START_BWLIMIT_BYTES = 1_000_000_000

# ── 宽松护栏 ─────────────────────────────────────────────────────────────────
# 旧设计用「目标÷进程数」当单进程硬上限，隐含假设「所有进程 100% 同时跑满」。
# 现实有效并发率只有 ~50-60%（部分进程在 SQS 拉取/multipart 协调），按满额分摊后
# 实测吞吐打折、永远够不到红线。改为：AIMD 按实测自由增减，单进程额度自动收敛到
# 「让全机群实测吞吐=红线」的真实值（可能 > 公平份额，补偿无效并发）。
# safety_cap 仅作「防单进程失控」的宽松护栏（公平份额 × 倍数），不再是主控钳制。
SAFETY_MULTIPLIER = 4

# 8 bit/byte，Gbps = G bit per second。
_BITS_PER_BYTE = 8
_GIGA = 1_000_000_000
# 1 MiB = 1024×1024 byte（rclone 的 M 后缀按 MiB 解释）。
_MIB = 1_048_576


def gbps_to_bytes_per_sec(gbps: float) -> int:
    """Gbps → 字节/秒。8 Gbps = 1e9 字节/秒。"""
    return int(gbps * _GIGA / _BITS_PER_BYTE)


def bytes_per_sec_to_gbps(bytes_per_sec: float) -> float:
    """字节/秒 → Gbps。"""
    return bytes_per_sec * _BITS_PER_BYTE / _GIGA


def format_bwlimit(bytes_per_sec: int) -> str:
    """rclone ``--bwlimit`` 取值：<=0 → "off"（不限速），否则 "<X>M"（MiB/s）。

    🔴 rclone ``--bwlimit`` 纯数字单位是 **KiB/s**，不是 byte/s。本系统内部全用
    byte/s，输出时换算成 MiB 并带 "M" 后缀，让 rclone 按 MiB/s 解释，杜绝 1024 倍歧义。
    保留两位小数，避免大值取整丢失精度。
    """
    if bytes_per_sec <= 0:
        return "off"
    return f"{bytes_per_sec / _MIB:.2f}M"


def _clamp_floor(value: int) -> int:
    """收紧后不低于地板。"""
    return max(value, MIN_BWLIMIT_BYTES)


def additive_adjust(
    *,
    primary_gbps: float,
    target_gbps: float,
    current_bwlimit: int,
    step_bytes: int,
) -> int:
    """慢而稳：纯加性调节（无快速收紧/无乘性减），单调小步收敛不超调。

    去掉乘性减(×0.8)和快速收紧(×0.5)的不对称大幅收紧——它们在控制滞后 >> 控制周期的
    大文件场景下造成输出震荡。改对称小步：
      - primary > 红线×1.1      → current - step（平缓收紧）
      - primary < 红线×0.9      → current + step（平缓放开）
      - 死区 [红线×0.9, 红线×1.1] → 保持（防红线附近抖动，容忍 10% 瞬时过冲）
    step 取小值（≈公平份额比例），牺牲反应速度换单调收敛。
    primary_gbps 由调用方传入（5min 与 1min 两窗结合的代表值）。
    """
    if target_gbps <= 0:
        return 0
    base = current_bwlimit if current_bwlimit > 0 else step_bytes
    if primary_gbps > target_gbps * DEADZONE_UPPER_RATIO:
        return _clamp_floor(base - step_bytes)
    if primary_gbps < target_gbps * DEADZONE_LOWER_RATIO:
        return base + step_bytes
    return base if current_bwlimit > 0 else _clamp_floor(base)


def damp_step(*, old: int, proposed: int) -> int:
    """变化限幅（阻尼）：限制 bwlimit 相对上一轮的单步变化幅度，压震荡。

    - proposed<=0（off）→ 直接放行（关限速要即时生效）。
    - old<=0（冷启动/从 off 恢复）→ 无基数可限幅，放行 proposed。
    - 否则裁剪到 [old×(1-MAX_STEP_RATIO), old×(1+MAX_STEP_RATIO)]。
    """
    if proposed <= 0:
        return proposed
    if old <= 0:
        return proposed
    lo = int(old * (1 - MAX_STEP_RATIO))
    hi = int(old * (1 + MAX_STEP_RATIO))
    return max(lo, min(proposed, hi))


def safety_cap_bytes(*, target_gbps: float, threads: int) -> int:
    """单进程宽松护栏（字节/秒）= 公平份额 × SAFETY_MULTIPLIER。

    公平份额 = 全局目标 ÷ 进程数（旧设计的硬上限）。护栏放宽数倍，让 AIMD 能在
    有效并发不足时把单进程额度抬高到公平份额之上、逼近红线；同时挡住单进程失控
    （如进程数骤降时 current_bw 残留过大）。target<=0 → 0（不限速由上层处理）。
    threads<=0（冷启动无心跳）→ 退化为全局目标，不做除法。
    """
    if target_gbps <= 0:
        return 0
    global_target = int(target_gbps * _GIGA / _BITS_PER_BYTE)
    if threads <= 0:
        return global_target
    return global_target // threads * SAFETY_MULTIPLIER


def aimd_adjust(
    *,
    actual_gbps: float,
    target_gbps: float,
    current_bwlimit: int,
    throttle_rate: float = 0.0,
    spike_gbps: float | None = None,
) -> int:
    """AIMD 控制律：返回新的单进程 bwlimit（字节/秒，0=off）。

    Args:
        actual_gbps: 慢回路——过去 5min 全机群平均吞吐。
        target_gbps: 红线。<=0 视为"不限速"，直接返回 0（off）。
        current_bwlimit: 当前单进程 bwlimit（字节/秒）。
        throttle_rate: 最近窗口 429 率（0~1），超 THROTTLE_OVERRIDE_RATE 强制收紧。
        spike_gbps: 快保护——最近 1min 全机群吞吐。None 关闭快速收紧（向后兼容）。

    决策优先级（高 → 低）：
        1. 红线=off → 0
        2. 快速收紧：spike 超过 红线×FAST_BRAKE_RATIO → ×FAST_BRAKE_FACTOR（收紧幅度最大）
        3. 429 超阈值 → 常规乘性减
        4. 慢回路 AIMD：超红线减 / 死区保持 / 远低于红线加
    """
    # 1) 红线关闭 = 不限速
    if target_gbps <= 0:
        return 0

    # 冷启动：当前 off(0) 但需要限速时，给一个有限基数供乘除。
    base = current_bwlimit if current_bwlimit > 0 else COLD_START_BWLIMIT_BYTES

    # 2) 快速收紧：最近 1min 超过快速收紧阈值 → 立即降至半速（优先级最高的收紧）。
    if spike_gbps is not None and spike_gbps > target_gbps * FAST_BRAKE_RATIO:
        return _clamp_floor(int(base * FAST_BRAKE_FACTOR))

    # 3) 429 风暴：源端已在拒绝，强制常规收紧。
    if throttle_rate > THROTTLE_OVERRIDE_RATE:
        return _clamp_floor(int(base * DECREASE_FACTOR))

    # 4) 慢回路 AIMD
    if actual_gbps > target_gbps:
        # 超红线 → 乘性减
        return _clamp_floor(int(base * DECREASE_FACTOR))
    if actual_gbps < target_gbps * DEADZONE_LOWER_RATIO:
        # 远低于红线 → 加性增（冷启动时从 base 起加）
        return base + INCREASE_STEP_BYTES
    # 死区 [红线×0.9, 红线] → 保持
    return current_bwlimit


def decide_bwlimit(
    *,
    limit_enabled: bool,
    auto_enabled: bool,
    target_gbps: float,
    actual_gbps: float,
    current_bwlimit: int,
    throttle_rate: float = 0.0,
    spike_gbps: float | None = None,
) -> tuple[int, str]:
    """两个独立开关的总决策。返回 (新 bwlimit 字节/秒, 模式名)。

    真值表：
        limit_enabled=False              → (0, "unlimited")   完全不限速
        limit_enabled=True, auto=False   → (current, "manual") 手动固定值
        limit_enabled=True, auto=True    → (aimd(...), "auto")  自动 AIMD
    """
    if not limit_enabled:
        return 0, "unlimited"
    if not auto_enabled:
        return current_bwlimit, "manual"
    new = aimd_adjust(
        actual_gbps=actual_gbps,
        target_gbps=target_gbps,
        current_bwlimit=current_bwlimit,
        throttle_rate=throttle_rate,
        spike_gbps=spike_gbps,
    )
    return new, "auto"


def read_ssm_limit(ssm_client: Any, param_name: str) -> str:
    """从 SSM 读一个限速参数值；任何失败/空值 → fail-safe "off"（不限速）。

    限速是额外约束，控制面/参数故障不该卡死传输——读不到就放开。
    bwlimit（AIMD 自动写）与 tpslimit（手动写）共用此读取语义。
    """
    try:
        resp = ssm_client.get_parameter(Name=param_name)
        value = resp.get("Parameter", {}).get("Value", "")
    except Exception:  # noqa: BLE001 - 任何 SSM 错误都 fail-safe 不限速
        logger.warning("read limit from SSM failed (%s); fail-safe off", param_name)
        return "off"
    if not value:
        return "off"
    return value


def read_bwlimit(ssm_client: Any, param_name: str) -> str:
    """从 SSM 读当前生效 bwlimit（AIMD 控制面写入）；fail-safe "off"。"""
    return read_ssm_limit(ssm_client, param_name)
