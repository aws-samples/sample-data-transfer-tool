"""monitoring_reporter 单元测试。

纯 CloudWatch 监控：EMF 实时聚合层（低基数）→ stdout → CloudWatch。
最重要的红线：source/source_path 绝不能进 EMF 维度（cardinality 爆炸）。
"""
from __future__ import annotations

import json

import pytest

from migration import monitoring_reporter as mr


def _event(**overrides):
    """构造一个典型 transfer 事件。"""
    base = {
        "queue_type": "large",
        "error_class": "none",
        "instance_id": "i-0abc123",
        "bytes": 1048576,
        "elapsed": 2.0,
        "speed": 524288.0,
        "state": "SUCCESS",
        "source": "gcs:bucket/Documents/file.pdf",
    }
    base.update(overrides)
    return base


# --- ALLOWED_EMF_DIMENSIONS 白名单常量 ---


@pytest.mark.unit
def test_allowed_dimensions_is_exact_whitelist():
    # 低基数维度白名单：QueueType/ErrorClass/InstanceId/State/Op（Op=copy|delete）。
    assert mr.ALLOWED_EMF_DIMENSIONS == frozenset(
        {"QueueType", "ErrorClass", "InstanceId", "State", "Op"}
    )


# --- assert_no_high_cardinality_dimension 守卫 ---


@pytest.mark.unit
def test_guard_passes_for_whitelisted_dimensions():
    # 不抛异常即通过
    mr.assert_no_high_cardinality_dimension(["QueueType", "ErrorClass", "InstanceId"])


@pytest.mark.unit
def test_guard_rejects_source_dimension():
    # 🔴 红线：source 进维度必须被拦截
    with pytest.raises(ValueError, match="source"):
        mr.assert_no_high_cardinality_dimension(["QueueType", "source"])


@pytest.mark.unit
def test_guard_rejects_source_path_dimension():
    with pytest.raises(ValueError, match="source_path"):
        mr.assert_no_high_cardinality_dimension(["source_path"])


@pytest.mark.unit
def test_guard_rejects_any_unknown_dimension():
    with pytest.raises(ValueError):
        mr.assert_no_high_cardinality_dimension(["RandomHighCardKey"])


# --- build_emf ---


@pytest.mark.unit
def test_build_emf_dimensions_are_only_whitelist():
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    metrics_meta = emf["_aws"]["CloudWatchMetrics"][0]
    dims = metrics_meta["Dimensions"]
    assert dims == [["QueueType"], ["ErrorClass"], ["InstanceId"], ["State"], ["Op"]]
    # 展平后每个维度名都在白名单内
    flat = {d for group in dims for d in group}
    assert flat == mr.ALLOWED_EMF_DIMENSIONS


@pytest.mark.unit
def test_build_emf_op_dimension_value():
    # op 缺省 copy；显式 delete 时进 Op 维度。delete 事件 bytes=0 不污染吞吐。
    assert mr.build_emf(_event(), timestamp_ms=1)["Op"] == "copy"
    emf_del = mr.build_emf(_event(op="delete", bytes=0), timestamp_ms=1)
    assert emf_del["Op"] == "delete"
    assert emf_del["TransferredBytes"] == 0


@pytest.mark.unit
def test_build_emf_filecount_one_on_success_zero_otherwise():
    # 已完成文件数 = SUM(FileCount)，只统计 SUCCESS，不被失败/重试 attempt 重复计入
    assert mr.build_emf(_event(state="SUCCESS"), timestamp_ms=1)["FileCount"] == 1
    assert mr.build_emf(_event(state="RETRYABLE"), timestamp_ms=1)["FileCount"] == 0
    assert mr.build_emf(_event(state="FATAL"), timestamp_ms=1)["FileCount"] == 0
    assert mr.build_emf(_event(state="UNKNOWN"), timestamp_ms=1)["FileCount"] == 0


@pytest.mark.unit
def test_build_emf_includes_state_dimension_value():
    emf = mr.build_emf(_event(state="RETRYABLE"), timestamp_ms=1)
    assert emf["State"] == "RETRYABLE"


@pytest.mark.unit
def test_build_emf_namespace_is_correct():
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    assert emf["_aws"]["CloudWatchMetrics"][0]["Namespace"] == "GcsS3Migration"


@pytest.mark.unit
def test_build_emf_declares_metrics_with_units():
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    metrics = emf["_aws"]["CloudWatchMetrics"][0]["Metrics"]
    by_name = {m["Name"]: m["Unit"] for m in metrics}
    assert by_name == {
        "TransferredBytes": "Bytes",
        "TransferDuration": "Seconds",
        "TransferSpeed": "Bytes/Second",
        "FileCount": "Count",
        "AttemptCount": "Count",
    }


@pytest.mark.unit
def test_attempt_count_is_one_for_every_state():
    """AttemptCount 每条 attempt 恒 1（各态都 1），供 Dashboard 按 State 维度看各态条数。

    对比 FileCount：FileCount 仅 SUCCESS=1，无法反映 RETRYABLE/UNKNOWN（线上 429 风暴
    时这些态在 Dashboard 不可见的根因）。AttemptCount 补足这个可观测性缺口。"""
    for state in ("SUCCESS", "RETRYABLE", "FATAL", "UNKNOWN"):
        assert mr.build_emf(_event(state=state), timestamp_ms=1)["AttemptCount"] == 1


@pytest.mark.unit
def test_build_emf_includes_aws_timestamp():
    """EMF 规范必填：_aws.Timestamp（毫秒）。缺失则 CloudWatch 不抽取指标。
    回归 2026-05-29 线上 bug（指标 namespace 一直为空）。"""
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    assert emf["_aws"]["Timestamp"] == 1700000000000


@pytest.mark.unit
def test_emit_emf_injects_timestamp():
    """emit_emf 用注入的 now_ms 填 _aws.Timestamp，单行 JSON 输出。"""
    import json as _json

    captured = []
    mr.emit_emf(_event(), out=captured.append, now_ms=lambda: 1699999999999)
    doc = _json.loads(captured[0])
    assert doc["_aws"]["Timestamp"] == 1699999999999


@pytest.mark.unit
def test_build_emf_sets_dimension_values_and_metric_values():
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    assert emf["QueueType"] == "large"
    assert emf["ErrorClass"] == "none"
    assert emf["InstanceId"] == "i-0abc123"
    assert emf["State"] == "SUCCESS"
    assert emf["TransferredBytes"] == 1048576
    assert emf["TransferDuration"] == 2.0
    assert emf["TransferSpeed"] == 524288.0
    assert emf["FileCount"] == 1


@pytest.mark.unit
def test_build_emf_never_includes_source_anywhere():
    # 🔴 即使 event 含 source，EMF 输出体里也不能出现 source 键
    emf = mr.build_emf(_event(), timestamp_ms=1700000000000)
    assert "source" not in emf
    assert "source_path" not in emf


# --- emit_emf ---


@pytest.mark.unit
def test_emit_emf_outputs_valid_json_with_correct_namespace():
    captured = []
    mr.emit_emf(_event(), out=captured.append)
    assert len(captured) == 1
    parsed = json.loads(captured[0])
    assert parsed["_aws"]["CloudWatchMetrics"][0]["Namespace"] == "GcsS3Migration"


@pytest.mark.unit
def test_emit_emf_output_has_no_source():
    captured = []
    mr.emit_emf(_event(), out=captured.append)
    assert "secret-file.pdf" not in captured[0]


# --- report 入口（纯 EMF，无 Firehose 明细层）---


@pytest.mark.unit
def test_report_emits_emf_only():
    captured = []
    mr.report(_event(), out=captured.append)
    # EMF 写了一次到 stdout
    assert len(captured) == 1
    parsed = json.loads(captured[0])
    assert parsed["_aws"]["CloudWatchMetrics"][0]["Namespace"] == "GcsS3Migration"


@pytest.mark.unit
def test_report_no_firehose_attr():
    # 明细层已移除：report_detail / get_firehose_client 不应再存在于模块
    assert not hasattr(mr, "report_detail")
    assert not hasattr(mr, "get_firehose_client")
