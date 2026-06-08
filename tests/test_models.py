"""Tests for shared data contracts in models.py."""
import pytest

from migration.models import Op, RunResult, State, TransferMessage, TransferStats


def test_transfer_message_roundtrip_with_args():
    body = {"source": "s3src:a", "destination": "s3:b", "rclone_args": ["--progress"]}
    msg = TransferMessage.from_body(body)
    assert msg.rclone_args == ("--progress",)
    out = msg.to_body()
    assert out["source"] == "s3src:a"
    assert out["rclone_args"] == ["--progress"]


def test_transfer_message_to_body_omits_empty_args():
    msg = TransferMessage(source="s3src:a", destination="s3:b")
    out = msg.to_body()
    assert "rclone_args" not in out


# ── op 字段（copy/delete）──
def test_op_defaults_to_copy_for_legacy_message():
    # 旧消息无 op 字段 → 默认 COPY（向后兼容硬约束）
    msg = TransferMessage.from_body({"source": "s3src:a", "destination": "s3:b"})
    assert msg.op is Op.COPY


def test_op_copy_to_body_omits_op():
    # COPY 是默认值 → 序列化时省略 op（保持旧消息形态，不污染）
    msg = TransferMessage(source="s3src:a", destination="s3:b")
    assert "op" not in msg.to_body()


def test_op_delete_parsed_and_serialized():
    body = {"op": "delete", "destination": "s3:datatos3-code/bench/migrated/x"}
    msg = TransferMessage.from_body(body)
    assert msg.op is Op.DELETE
    # delete 时 source 可空
    assert msg.source == ""
    out = msg.to_body()
    assert out["op"] == "delete"


def test_op_invalid_value_raises():
    # 非法 op 值 → ValueError（调用方按 poison 处理）
    with pytest.raises(ValueError):
        TransferMessage.from_body({"op": "purge", "destination": "s3:b"})


def test_op_enum_is_str():
    assert Op.COPY == "copy"
    assert Op.DELETE.value == "delete"


def test_run_result_success_property():
    assert RunResult(state=State.SUCCESS, exit_code=0).success is True
    assert RunResult(state=State.RETRYABLE, exit_code=5).success is False


def test_transfer_stats_defaults():
    s = TransferStats()
    assert s.bytes == 0
    assert s.errors == 0


def test_state_is_str_enum():
    assert State.SUCCESS == "SUCCESS"
    assert State.SUCCESS.value == "SUCCESS"
