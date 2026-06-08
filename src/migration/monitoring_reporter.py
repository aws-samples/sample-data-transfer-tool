"""监控：EMF 实时聚合层（低基数）→ CloudWatch。

🔴 Cardinality 红线
    EMF 维度白名单只允许 {QueueType, ErrorClass, InstanceId, State, Op}。
    source / source_path 绝不能进 EMF 维度——几十亿基数会让 CloudWatch
    自定义指标成本激增（百万美元/月）。高基数明细字段不进任何指标维度。

输出路径
    EMF: build_emf -> json.dumps -> print 到 stdout，由 CloudWatch agent 采集
    （CloudWatch Logs 在摄入端自动抽取 GcsS3Migration 命名空间指标）。
    明细层（Firehose/Athena）已移除——监控改为纯 CloudWatch，不再双写。
"""
from __future__ import annotations

import json
from collections.abc import Callable
from typing import Any

# EMF 指标命名空间
EMF_NAMESPACE = "GcsS3Migration"

# 🔴 EMF 维度白名单——唯一允许进入指标维度的键
# State 是低基数（SUCCESS/RETRYABLE/FATAL/UNKNOWN 共 4 值），加入维度让
# FileCount/TransferredBytes 可按状态过滤，避免"已完成"把失败和重试也算进去。
# Op 低基数（copy/delete 共 2 值），加入维度让 dashboard 区分传输 vs 删除操作。
ALLOWED_EMF_DIMENSIONS = frozenset({"QueueType", "ErrorClass", "InstanceId", "State", "Op"})

# EMF 指标定义：name -> unit
# FileCount 只在 SUCCESS 计 1（= 已完成文件数）；AttemptCount 每条 attempt 恒计 1，
# 配合 State 维度可在 Dashboard 看到各态（SUCCESS/RETRYABLE/FATAL/UNKNOWN）的真实条数——
# FileCount 在非 SUCCESS 恒为 0，无法反映 RETRYABLE/UNKNOWN，故另设 AttemptCount。
_EMF_METRICS: tuple[tuple[str, str], ...] = (
    ("TransferredBytes", "Bytes"),
    ("TransferDuration", "Seconds"),
    ("TransferSpeed", "Bytes/Second"),
    ("FileCount", "Count"),
    ("AttemptCount", "Count"),
)


def assert_no_high_cardinality_dimension(dimensions: list[str]) -> None:
    """守卫：拒绝任何不在白名单内的维度。

    任何不在 ALLOWED_EMF_DIMENSIONS 的维度（尤其 source/source_path）都会
    抛 ValueError，从根上挡住高基数维度进入 EMF。
    """
    offenders = [d for d in dimensions if d not in ALLOWED_EMF_DIMENSIONS]
    if offenders:
        raise ValueError(
            "高基数维度被守卫拦截，禁止进入 EMF 维度: "
            f"{offenders}（白名单仅 {sorted(ALLOWED_EMF_DIMENSIONS)}）"
        )


def build_emf(event: dict[str, Any], *, timestamp_ms: int) -> dict[str, Any]:
    """构造 EMF JSON 文档（dict 形式，调用方负责 json.dumps）。

    维度严格限定为 [["QueueType"],["ErrorClass"],["InstanceId"]]，并在写入前
    经守卫函数校验，杜绝 source 之类高基数键混入。

    ⚠️ ``_aws.Timestamp``（毫秒 epoch）是 EMF 规范**必填**字段：CloudWatch Logs
    据此识别该日志事件为 EMF 并自动抽取指标。缺失则日志进了 Log Group 也**不会**
    生成任何指标（线上实测发现：namespace 一直为空）。timestamp_ms 由调用方注入
    （保持纯函数，不在内部调 time）。
    """
    dimension_names = ["QueueType", "ErrorClass", "InstanceId", "State", "Op"]
    assert_no_high_cardinality_dimension(dimension_names)

    metrics_meta = [{"Name": name, "Unit": unit} for name, unit in _EMF_METRICS]

    # FileCount 只在 SUCCESS 时计 1，其余状态计 0——这样"已完成文件数"=SUM(FileCount)
    # 天然只统计成功，不被失败/重试 attempt 重复计入（按 state 维度也可单独看各态）。
    is_success = event["state"] == "SUCCESS"
    return {
        "_aws": {
            "Timestamp": timestamp_ms,
            "CloudWatchMetrics": [
                {
                    "Namespace": EMF_NAMESPACE,
                    "Dimensions": [[name] for name in dimension_names],
                    "Metrics": metrics_meta,
                }
            ],
        },
        # 维度值（低基数）
        "QueueType": event["queue_type"],
        "ErrorClass": event["error_class"],
        "InstanceId": event["instance_id"],
        "State": event["state"],
        # op 缺省 copy（旧 event 无 op 字段时兼容）。
        "Op": event.get("op", "copy"),
        # 指标值
        "TransferredBytes": event["bytes"],
        "TransferDuration": event["elapsed"],
        "TransferSpeed": event["speed"],
        "FileCount": 1 if is_success else 0,
        # 每条 attempt 恒 1：按 State 维度聚合即各态条数；按 ErrorClass 维度即各错误类型条数。
        "AttemptCount": 1,
    }


def _now_ms() -> int:
    """当前毫秒 epoch（抽出便于测试 monkeypatch）。"""
    import time

    return int(time.time() * 1000)


def emit_emf(
    event: dict[str, Any],
    *,
    out: Callable[[str], Any] = print,
    now_ms: Callable[[], int] = _now_ms,
) -> None:
    """构造 EMF 并以单行 JSON 输出（out / now_ms 可注入，便于测试捕获）。"""
    out(json.dumps(build_emf(event, timestamp_ms=now_ms())))


def report(
    event: dict[str, Any],
    *,
    out: Callable[[str], Any] = print,
) -> None:
    """监控上报入口：EMF → stdout → CloudWatch（纯 CloudWatch，无明细双写）。"""
    emit_emf(event, out=out)
