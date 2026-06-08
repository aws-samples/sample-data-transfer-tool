"""Tests for worker: SQS consume → transfer → four-state handling (§4.1).

The worker only *consumes* SQS (another team fills the queue). We test the
single-message processing core with injected fakes; no real AWS, no real rclone.
"""
import json

import pytest

from migration.models import RunResult, State, TransferStats
from migration.worker import (
    _split_date_hour,
    is_large_object,
    parse_message_body,
    process_message,
)


# ─────────────────────────── _split_date_hour ──────────────────────────────
class TestSplitDateHour:
    def test_normal(self):
        # Firehose 分区契约：从 ISO 时间戳切出 (date, hour)
        assert _split_date_hour("2026-05-29T14:30:00.123") == ("2026-05-29", "14")

    def test_midnight_hour_zero_padded(self):
        assert _split_date_hour("2026-01-01T00:05:00.000") == ("2026-01-01", "00")

    def test_malformed_falls_back_empty(self):
        # 格式不符不抛错（不丢消息），回退空串
        assert _split_date_hour("not-a-timestamp") == ("", "")
        assert _split_date_hour("") == ("", "")


# ─────────────────────────── parse_message_body ────────────────────────────
class TestParseMessageBody:
    def test_valid(self):
        body = json.dumps({"source": "s3src:a/x", "destination": "s3:b/y"})
        msg = parse_message_body(body)
        assert msg.source == "s3src:a/x"
        assert msg.destination == "s3:b/y"
        assert msg.rclone_args == ()

    def test_with_args(self):
        body = json.dumps(
            {"source": "s3src:a/x", "destination": "s3:b/y", "rclone_args": ["--progress"]}
        )
        msg = parse_message_body(body)
        assert msg.rclone_args == ("--progress",)

    def test_invalid_json_raises(self):
        with pytest.raises(ValueError):
            parse_message_body("{not json")

    def test_missing_required_field_raises(self):
        with pytest.raises(ValueError):
            parse_message_body(json.dumps({"source": "s3src:a"}))

    def test_no_object_key_raises(self):
        # source 只有 remote:bucket、无对象 key → empty → poison
        body = json.dumps({"source": "s3src:bucket-only", "destination": "s3:b/y"})
        with pytest.raises(ValueError):
            parse_message_body(body)

    def test_too_long_key_raises(self):
        # 对象 key 超 1024 字节 → too_long → poison（rclone 物理传不了，提前拦）
        long_key = "a" * 1025
        body = json.dumps(
            {"source": f"gcs:bucket/{long_key}", "destination": f"s3:b/{long_key}"}
        )
        with pytest.raises(ValueError):
            parse_message_body(body)

    def test_special_chars_accepted(self):
        # 中文/emoji/// /前导斜杠/尾随空格 全部放行（HMAC 端点可传，已实测）
        for key in ["数据/文件.txt", "a//b", "/lead.png", "trail .pdf ", "emoji_😀.txt"]:
            body = json.dumps({"source": f"gcs:bucket/{key}", "destination": f"s3:b/{key}"})
            msg = parse_message_body(body)
            assert msg.source == f"gcs:bucket/{key}"

    # ── op=delete 分支：只需 destination ──
    def test_delete_op_without_source_ok(self):
        from migration.models import Op

        body = json.dumps({"op": "delete", "destination": "s3:datatos3-code/mig/x"})
        msg = parse_message_body(body)
        assert msg.op is Op.DELETE
        assert msg.destination == "s3:datatos3-code/mig/x"

    def test_delete_op_missing_destination_raises(self):
        with pytest.raises(ValueError):
            parse_message_body(json.dumps({"op": "delete"}))

    def test_delete_op_too_long_destination_raises(self):
        # delete 目标 key 超长 → too_long → poison
        long_key = "a" * 1025
        with pytest.raises(ValueError):
            parse_message_body(
                json.dumps({"op": "delete", "destination": f"s3:datatos3-code/{long_key}"})
            )

    def test_invalid_op_value_raises(self):
        body = json.dumps({"op": "purge", "destination": "s3:b"})
        with pytest.raises(ValueError):
            parse_message_body(body)


# ─────────────────────────── is_large_object ───────────────────────────────
class TestIsLargeObject:
    def test_large(self):
        assert is_large_object(200 * 1024 * 1024) is True

    def test_small(self):
        assert is_large_object(1024) is False

    def test_boundary(self):
        assert is_large_object(100 * 1024 * 1024) is True  # >= threshold


# ─────────────────────────── process_message ───────────────────────────────
def _fake_run_result(state, error_class=None, error_message=None):
    return RunResult(
        state=state,
        exit_code=0 if state is State.SUCCESS else 5,
        stats=TransferStats(bytes=1024, elapsed_seconds=1.0, speed=1024.0),
        error_class=error_class,
        error_message=error_message,
        cmd_str="rclone copyto -- s3src:a s3:b",
    )


class _Spy:
    """Collects side effects so tests can assert what the worker did."""

    def __init__(self):
        self.deleted = False
        self.recorded = None
        self.reported = None

    def delete(self):
        self.deleted = True

    def record(self, **kw):
        self.recorded = kw

    def report(self, event, **kw):
        self.reported = event


def _process(state, spy, *, is_large=False, error_class=None, error_message=None):
    body = json.dumps({"source": "s3src:a/x", "destination": "s3:b/y"})

    def fake_run(msg, config_path, is_large_):
        return _fake_run_result(state, error_class, error_message)

    return process_message(
        body=body,
        object_size=1024,
        instance_id="i-test",
        config_path="/tmp/rclone.conf",
        region="eu-central-1",
        status_table="transfer-status",
        now_iso="2026-05-20T00:00:00",
        run_fn=fake_run,
        delete_fn=spy.delete,
        record_fn=spy.record,
        report_fn=spy.report,
    )


class TestProcessMessageFourStates:
    def test_success_deletes_and_records_and_counts(self):
        spy = _Spy()
        result = _process(State.SUCCESS, spy)
        assert result.state is State.SUCCESS
        assert spy.deleted is True            # SUCCESS → delete message
        assert spy.recorded is not None        # recorded terminal state
        assert spy.reported is not None        # monitoring reported
        assert result.counted is True

    def test_reported_event_carries_partition_fields(self):
        # 明细层契约：event 必须含 date/hour/event_time，否则 Firehose 分区提取失败、
        # Parquet 落不进正确分区（线上踩坑：monitoring/events/ 一直空）。
        spy = _Spy()
        _process(State.SUCCESS, spy)
        assert spy.reported["date"] == "2026-05-20"   # now_iso[:10]
        assert spy.reported["hour"] == "00"            # now_iso[11:13]
        assert spy.reported["event_time"] == "2026-05-20T00:00:00"

    def test_retryable_keeps_message(self):
        spy = _Spy()
        result = _process(State.RETRYABLE, spy, error_class="src_rate_limit")
        assert spy.deleted is False            # RETRYABLE → keep (visibility re-deliver)
        assert spy.recorded is not None        # still record FAILED
        assert result.counted is True

    def test_fatal_keeps_message_and_records(self):
        spy = _Spy()
        result = _process(State.FATAL, spy, error_class="src_not_found")
        # FATAL 不删：靠自然重投 3 次后 SQS 转 DLQ（处理不了的留底、可 replay）
        assert spy.deleted is False
        assert spy.recorded is not None
        assert result.counted is True

    def test_unknown_does_not_delete_and_does_not_count(self):
        spy = _Spy()
        result = _process(State.UNKNOWN, spy)
        assert spy.deleted is False            # UNKNOWN → keep, no count (critical fix)
        assert result.counted is False

    def test_report_event_has_no_high_cardinality_source_in_dimensions(self):
        # The monitoring event may carry source in the body (detail layer),
        # but the worker must pass queue_type/error_class/instance_id for EMF dims.
        spy = _Spy()
        _process(State.SUCCESS, spy)
        ev = spy.reported
        assert ev["instance_id"] == "i-test"
        assert "queue_type" in ev


class TestProcessMessageRcloneErrorLogging:
    """rclone 出错时把原始 cmd + stderr 打到 logger（→ worker.log → CloudWatch worker-ops），
    便于不进每台机就能定位失败原因。SUCCESS 不打（避免百万级成功刷屏）。"""

    def test_retryable_logs_cmd_and_stderr(self, caplog):
        import logging as _logging
        spy = _Spy()
        with caplog.at_level(_logging.ERROR, logger="migration.worker"):
            _process(
                State.RETRYABLE, spy,
                error_class="src_rate_limit",
                error_message="2026/06/05 ERROR: 429 Too Many Requests\nquota exceeded",
            )
        text = caplog.text
        # 原始 rclone 命令必须出现（定位用哪条命令失败）
        assert "rclone copyto -- s3src:a s3:b" in text
        # stderr 原文必须出现（定位失败根因）
        assert "429 Too Many Requests" in text
        # 错误分类也带上
        assert "src_rate_limit" in text

    def test_fatal_logs_cmd_and_stderr(self, caplog):
        import logging as _logging
        spy = _Spy()
        with caplog.at_level(_logging.ERROR, logger="migration.worker"):
            _process(
                State.FATAL, spy,
                error_class="src_not_found",
                error_message="directory not found",
            )
        assert "rclone copyto -- s3src:a s3:b" in caplog.text
        assert "directory not found" in caplog.text

    def test_unknown_logs_cmd_and_stderr(self, caplog):
        import logging as _logging
        spy = _Spy()
        with caplog.at_level(_logging.ERROR, logger="migration.worker"):
            _process(
                State.UNKNOWN, spy,
                error_message="signal: killed",
            )
        assert "rclone copyto -- s3src:a s3:b" in caplog.text
        assert "signal: killed" in caplog.text

    def test_success_does_not_log_error(self, caplog):
        import logging as _logging
        spy = _Spy()
        with caplog.at_level(_logging.ERROR, logger="migration.worker"):
            _process(State.SUCCESS, spy)
        # 成功不打 ERROR（百万级成功不能刷屏 CloudWatch）
        assert "rclone copyto" not in caplog.text


class TestProcessMessageBadBody:
    def test_malformed_body_does_not_delete(self):
        """Unparseable/invalid body → poison：记一条 FATAL 终态(可追溯)，但**不删**，
        靠自然重投 3 次后由 SQS 转入 DLQ。处理不了的消息绝不直接删，保证可 replay。"""
        spy = _Spy()
        result = process_message(
            body="{bad json",
            object_size=1024,
            instance_id="i-test",
            config_path="/tmp/c",
            region="r",
            status_table="t",
            now_iso="2026-05-20T00:00:00",
            run_fn=lambda *a, **k: _fake_run_result(State.SUCCESS),
            delete_fn=spy.delete,
            record_fn=spy.record,
            report_fn=spy.report,
        )
        assert result.state is State.FATAL  # malformed = poison, classify FATAL
        assert spy.deleted is False         # 不删 → 留队列待 DLQ
        assert spy.recorded is not None     # 但先记一条终态(含 poison: source 可查)
        assert result.counted is False


class TestResolveInstanceId:
    """B1 多进程：instance_id 加 WORKER_INDEX 后缀，保证同机 N 进程心跳唯一。"""

    def test_single_process_no_suffix(self, monkeypatch):
        # 不设 WORKER_INDEX → 纯 instance-id（向后兼容单进程部署）
        from migration import worker
        monkeypatch.setattr(worker, "_ec2_instance_id", lambda: "i-abc123")
        assert worker.resolve_instance_id({}) == "i-abc123"

    def test_multiprocess_appends_index(self, monkeypatch):
        from migration import worker
        monkeypatch.setattr(worker, "_ec2_instance_id", lambda: "i-abc123")
        assert worker.resolve_instance_id({"WORKER_INDEX": "0"}) == "i-abc123#0"
        assert worker.resolve_instance_id({"WORKER_INDEX": "15"}) == "i-abc123#15"

    def test_distinct_per_process(self, monkeypatch):
        # 同机不同 index → 不同 instance_id（心跳不互相覆盖）
        from migration import worker
        monkeypatch.setattr(worker, "_ec2_instance_id", lambda: "i-xyz")
        ids = {worker.resolve_instance_id({"WORKER_INDEX": str(i)}) for i in range(16)}
        assert len(ids) == 16
