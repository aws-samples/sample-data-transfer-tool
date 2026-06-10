"""Tests for WorkerLoop runtime layer (fake SQS, monkeypatched record/report)."""
import json

import pytest

from migration import worker as worker_mod
from migration.config import Settings
from migration.models import RunResult, State, TransferStats
from migration.worker import WorkerLoop


def _settings(worker_threads=32):
    return Settings(
        aws_region="eu-central-1",
        source_remote="s3src",
        dest_remote="s3",
        queue_url="http://queue",
        dynamodb_table="transfer-status",
        heartbeat_table="heartbeat",
        rclone_config_path="/tmp/rclone.conf",
        worker_threads=worker_threads,
    )


class FakeSQS:
    def __init__(self, batches):
        self._batches = list(batches)
        self.deleted = []
        self.visibility_batches = []  # 记录 change_message_visibility_batch 调用
        self.visibility_changes = []  # 记录单条 change_message_visibility (receipt, timeout)
        self._lock = __import__("threading").Lock()

    def receive_message(self, **kw):
        with self._lock:
            if self._batches:
                return {"Messages": self._batches.pop(0)}
        return {"Messages": []}

    def delete_message(self, QueueUrl, ReceiptHandle):
        with self._lock:
            self.deleted.append(ReceiptHandle)

    def change_message_visibility_batch(self, QueueUrl, Entries):
        with self._lock:
            self.visibility_batches.append(Entries)
        return {"Successful": [{"Id": e["Id"]} for e in Entries], "Failed": []}

    def change_message_visibility(self, QueueUrl, ReceiptHandle, VisibilityTimeout):
        with self._lock:
            self.visibility_changes.append((ReceiptHandle, VisibilityTimeout))


@pytest.fixture(autouse=True)
def _patch_side_effects(monkeypatch):
    """Stub DDB record + monitoring report so the loop touches no real AWS."""
    monkeypatch.setattr(worker_mod.status_store, "record_terminal", lambda *a, **k: None)
    monkeypatch.setattr(worker_mod, "status_store_client", lambda region: object())
    monkeypatch.setattr(worker_mod.monitoring_reporter, "report", lambda *a, **k: None)


def _msg(receipt, source="s3src:a/x", size=1024):
    return {
        "ReceiptHandle": receipt,
        "Body": json.dumps({"source": source, "destination": "s3:b/y"}),
        "MessageAttributes": {"object_size": {"StringValue": str(size)}},
    }


def _ok(*a, **k):
    return RunResult(
        state=State.SUCCESS, exit_code=0,
        stats=TransferStats(bytes=1024, elapsed_seconds=1.0, speed=1024.0),
        cmd_str="rclone",
    )


def _retry(*a, **k):
    return RunResult(state=State.RETRYABLE, exit_code=5, error_class="src_rate_limit", cmd_str="r")


def test_poll_once_success_deletes_and_counts(monkeypatch):
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _ok)
    sqs = FakeSQS([[_msg("r1"), _msg("r2")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    n = loop.poll_once()
    assert n == 2
    assert sqs.deleted == ["r1", "r2"]       # SUCCESS deletes
    assert loop.stats["success"] == 2
    assert loop.stats["total"] == 2


def test_poll_once_retryable_keeps_message(monkeypatch):
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _retry)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert sqs.deleted == []                 # RETRYABLE keeps message
    assert loop.stats["failed"] == 1


def test_poll_once_retryable_requeues_with_backoff(monkeypatch):
    """失败快速重投：RETRYABLE(src_rate_limit) → change_message_visibility(300)，
    不再等 12h 自然过期；ReceiveCount 照常累积，3 次后进 DLQ。"""
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _retry)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert sqs.visibility_changes == [("r1", 300)]   # 限流退避 300s


def test_poll_once_unknown_requeues_immediately(monkeypatch):
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _unknown)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert sqs.visibility_changes == [("r1", 0)]     # 立即重投


def test_poll_once_success_does_not_touch_visibility(monkeypatch):
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _ok)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert sqs.visibility_changes == []              # 成功只删，不改 visibility


def test_requeue_failure_falls_back_to_natural_redelivery(monkeypatch):
    """change_message_visibility 失败（节流/receipt 过期）不抛出——
    消息退化为 12h 自然过期重投（原行为），单独计数便于观测。"""
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _retry)
    sqs = FakeSQS([[_msg("r1")]])

    def boom(**kw):
        raise RuntimeError("throttled")

    sqs.change_message_visibility = boom
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()                                  # 不得抛异常
    assert loop.stats["requeue_fail"] == 1
    assert loop.stats["failed"] == 1                  # 计数不受影响


def test_poll_once_empty_returns_zero():
    sqs = FakeSQS([])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    assert loop.poll_once() == 0


def test_handle_one_swallows_errors(monkeypatch):
    def boom(*a, **k):
        raise RuntimeError("kaboom")

    monkeypatch.setattr(worker_mod.rclone_runner, "run", boom)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    # must not raise — single message failure shouldn't kill the thread
    assert loop.poll_once() == 1
    assert sqs.deleted == []  # not deleted → will retry via visibility timeout


def test_size_routing_reads_message_attribute(monkeypatch):
    captured = {}

    def capture_run(msg, config_path, is_large, *, bwlimit="off", tpslimit="off"):
        captured["is_large"] = is_large
        captured["bwlimit"] = bwlimit
        captured["tpslimit"] = tpslimit
        return _ok()

    monkeypatch.setattr(worker_mod.rclone_runner, "run", capture_run)
    sqs = FakeSQS([[_msg("r1", size=200 * 1024 * 1024)]])  # 200MB → large
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert captured["is_large"] is True


def test_handle_one_injects_both_rate_limits(monkeypatch):
    """_handle_one 起 rclone 时同时注入 bwlimit + tpslimit 当前快照。"""
    captured = {}

    def capture_run(msg, config_path, is_large, *, bwlimit="off", tpslimit="off"):
        captured["bwlimit"] = bwlimit
        captured["tpslimit"] = tpslimit
        return _ok()

    monkeypatch.setattr(worker_mod.rclone_runner, "run", capture_run)
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
    with loop._bwlimit_lock:
        loop._bwlimit = "480000000"
        loop._tpslimit = "4000"
    loop._handle_one(_msg("r1"))
    assert captured["bwlimit"] == "480000000"
    assert captured["tpslimit"] == "4000"


def _unknown(*a, **k):
    return RunResult(state=State.UNKNOWN, exit_code=-9, cmd_str="u")


def test_poll_once_unknown_not_counted_not_deleted(monkeypatch):
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _unknown)
    sqs = FakeSQS([[_msg("r1")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    loop.poll_once()
    assert sqs.deleted == []                  # UNKNOWN keeps message
    assert loop.stats["unknown"] == 1
    assert loop.stats["total"] == 0           # not counted


def test_run_forever_spawns_consumers_and_joins(monkeypatch):
    """run_forever: 起 worker_threads 个 consumer + heartbeat，处理消息，stop 后干净退出。"""
    monkeypatch.setattr(worker_mod.rclone_runner, "run", _ok)
    monkeypatch.setattr(WorkerLoop, "_heartbeat_loop", lambda self, **k: None)
    # 5 条消息后队列空
    sqs = FakeSQS([[_msg(f"r{i}")] for i in range(5)])
    loop = WorkerLoop(_settings(worker_threads=4), "http://large", "i-1", sqs=sqs)

    import threading
    import time as _t
    runner = threading.Thread(target=loop.run_forever, daemon=True)
    runner.start()
    for _ in range(60):
        if len(sqs.deleted) >= 5:
            break
        _t.sleep(0.05)
    loop.running = False
    runner.join(timeout=5)
    assert len(sqs.deleted) == 5
    assert loop.stats["success"] == 5


def test_consume_loop_processes_until_stopped(monkeypatch):
    """竞争消费者：单个 consume loop 自己 receive+处理，running=False 后退出。"""
    import threading

    seen = []
    seen_lock = threading.Lock()

    def rec(msg, config_path, is_large, *, bwlimit="off", tpslimit="off"):
        with seen_lock:
            seen.append(msg.source)
        return _ok()

    monkeypatch.setattr(worker_mod.rclone_runner, "run", rec)
    # 3 条消息后队列空，consume loop 应处理完这 3 条
    msgs = [[_msg(f"r{i}", source=f"s3src:bucket/obj{i}")] for i in range(3)]
    sqs = FakeSQS(msgs)
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)

    # 在后台跑 consume loop，处理完后停
    t = threading.Thread(target=loop._consume_loop, daemon=True)
    t.start()
    # 等它处理完 3 条
    for _ in range(50):
        if len(sqs.deleted) >= 3:
            break
        import time as _t
        _t.sleep(0.05)
    loop.running = False
    t.join(timeout=3)
    assert len(seen) == 3
    assert len(sqs.deleted) == 3
    assert loop.stats["success"] == 3


def test_consume_loop_empty_receive_backs_off():
    """空 receive 不报错、不计数，running=False 即退出。"""
    sqs = FakeSQS([])  # 永远空
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    import threading
    import time as _t
    t = threading.Thread(target=loop._consume_loop, daemon=True)
    t.start()
    _t.sleep(0.1)
    loop.running = False
    t.join(timeout=3)
    assert all(v == 0 for v in loop.stats.values())


def test_consume_loop_swallows_handle_errors(monkeypatch):
    """单条处理异常不拖垮 consume loop（继续下一条）。"""
    calls = {"n": 0}

    def boom_then_ok(msg, config_path, is_large, *, bwlimit="off", tpslimit="off"):
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("boom")
        return _ok()

    monkeypatch.setattr(worker_mod.rclone_runner, "run", boom_then_ok)
    sqs = FakeSQS([[_msg("r1")], [_msg("r2")]])
    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
    import threading
    import time as _t
    t = threading.Thread(target=loop._consume_loop, daemon=True)
    t.start()
    for _ in range(100):
        if calls["n"] >= 2:
            break
        _t.sleep(0.05)
    loop.running = False
    t.join(timeout=3)
    assert calls["n"] >= 2  # 第一条异常后没崩，继续处理了第二条


def test_heartbeat_loop_writes_then_stops(monkeypatch):
    """_heartbeat_loop writes one heartbeat then exits when running flips false."""
    written = {}

    def fake_write(client, table, instance_id, *, now_iso, now_epoch, active_threads):
        written["table"] = table
        written["instance_id"] = instance_id
        written["active_threads"] = active_threads

    monkeypatch.setattr(worker_mod.status_store, "write_heartbeat", fake_write)
    monkeypatch.setattr(worker_mod, "status_store_client", lambda region: object())

    loop = WorkerLoop(_settings(), "http://large", "i-hb", sqs=FakeSQS([]))

    # make sleep flip running false after first write so loop exits
    def stop_sleep(_interval):
        loop.running = False

    monkeypatch.setattr(worker_mod.time, "sleep", stop_sleep)
    loop._heartbeat_loop(interval=0.01)

    assert written["instance_id"] == "i-hb"
    assert written["table"] == "heartbeat"  # from _settings() helper
    assert written["active_threads"] == 32  # _settings() 默认


def test_ratelimit_loop_refreshes_both_bwlimit_and_tpslimit(monkeypatch):
    """_ratelimit_loop 每轮从 SSM 同时刷新 bwlimit + tpslimit 快照。"""
    from migration import aws_clients, ratelimit

    monkeypatch.setattr(aws_clients, "get_ssm_client", lambda region: object())
    # bwlimit param → 480M；tpslimit param → 4000，按参数名区分返回。
    def fake_read(client, param_name):
        if param_name.endswith("/bwlimit"):
            return "480000000"
        if param_name.endswith("/tpslimit"):
            return "4000"
        return "off"

    monkeypatch.setattr(ratelimit, "read_bwlimit", fake_read)
    monkeypatch.setattr(ratelimit, "read_ssm_limit", fake_read)

    loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))

    def stop_sleep(_interval):
        loop.running = False

    monkeypatch.setattr(worker_mod.time, "sleep", stop_sleep)
    loop._ratelimit_loop(interval=0.01)

    with loop._bwlimit_lock:
        assert loop._bwlimit == "480000000"
        assert loop._tpslimit == "4000"


def test_poison_message_records_fatal_and_keeps_for_dlq(monkeypatch):
    """Malformed body: record a FATAL terminal (traceable) but KEEP message →
    快速重投烧满 3 次 ReceiveCount 后 SQS 转 DLQ（不直接删，留底可 replay）。"""
    recorded = {}

    def capture_record(*, source, attempt_timestamp, result, instance_id, message_body=None):
        recorded["source"] = source
        recorded["state"] = result.state
        recorded["error_class"] = result.error_class
        recorded["message_body"] = message_body

    monkeypatch.setattr(worker_mod.status_store, "record_terminal",
                        lambda *a, **k: capture_record(**k) if False else None)
    # use process_message directly for clarity
    from migration.models import State
    from migration.worker import process_message
    spy = {"deleted": False, "requeues": []}
    out = process_message(
        body="{bad json",
        object_size=1024,
        instance_id="i-1",
        config_path="/tmp/c",
        region="r",
        status_table="t",
        now_iso="2026-05-20T00:00:00",
        run_fn=lambda *a, **k: _ok(),
        delete_fn=lambda: spy.update(deleted=True),
        record_fn=lambda **k: capture_record(**k),
        report_fn=lambda *a, **k: None,
        requeue_fn=lambda delay: spy["requeues"].append(delay),
    )
    assert out.state is State.FATAL
    assert out.counted is False
    assert spy["deleted"] is False                      # poison 不删 → 快速烧向 DLQ
    assert spy["requeues"] == [0]                       # 立即重投
    assert recorded["state"] is State.FATAL             # 但先记一条终态
    assert recorded["source"].startswith("poison:")     # traceable source
    assert recorded["error_class"] == "poison_message"
    assert recorded["message_body"] == "{bad json"      # 完整原始消息体进 DDB


# ── H1: 优雅退出（in-flight 集合 + shutdown 重置 visibility）─────────────────


class _BlockingSQS(FakeSQS):
    """receive 返回一条消息后阻塞，便于测试在途 receipt 被记录。"""

    def __init__(self, msgs, gate):
        super().__init__([])
        self._msgs = list(msgs)
        self._gate = gate

    def receive_message(self, **kw):
        with self._lock:
            if self._msgs:
                return {"Messages": [self._msgs.pop(0)]}
        return {"Messages": []}


class TestInflightTracking:
    def test_handle_one_tracks_then_discards_receipt(self, monkeypatch):
        # 处理过程中 receipt 在 in-flight 集合，处理完移除
        seen_inflight = {}

        def run_capturing(msg, config_path, is_large, *, bwlimit="off", tpslimit="off"):
            seen_inflight["during"] = set(loop._inflight)
            return _ok()

        monkeypatch.setattr(worker_mod.rclone_runner, "run", run_capturing)
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
        loop._handle_one(_msg("r1"))
        assert "r1" in seen_inflight["during"]   # 处理时在途
        assert loop._inflight == set()           # 处理完移除

    def test_inflight_discarded_even_on_error(self, monkeypatch):
        def boom(*a, **k):
            raise RuntimeError("x")

        monkeypatch.setattr(worker_mod.rclone_runner, "run", boom)
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
        loop._safe_handle(_msg("r1"))
        # 即便处理抛错，in-flight 也要清掉（finally），否则 shutdown 会重复重置
        assert loop._inflight == set()


class TestShutdown:
    def test_shutdown_sets_running_false(self):
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
        loop.shutdown()
        assert loop.running is False

    def test_shutdown_resets_inflight_visibility_to_zero(self):
        sqs = FakeSQS([])
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
        # 模拟有 3 条在途
        with loop._inflight_lock:
            loop._inflight.update({"r1", "r2", "r3"})
        loop.shutdown()
        # 全部进了一个 batch（<=10），VisibilityTimeout=0 立即重投
        assert len(sqs.visibility_batches) == 1
        entries = sqs.visibility_batches[0]
        assert {e["ReceiptHandle"] for e in entries} == {"r1", "r2", "r3"}
        assert all(e["VisibilityTimeout"] == 0 for e in entries)

    def test_shutdown_batches_in_chunks_of_ten(self):
        sqs = FakeSQS([])
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
        receipts = {f"r{i}" for i in range(23)}
        with loop._inflight_lock:
            loop._inflight.update(receipts)
        loop.shutdown()
        # 23 条 → 3 个 batch（10/10/3），SQS batch API 上限 10
        assert len(sqs.visibility_batches) == 3
        sizes = sorted(len(b) for b in sqs.visibility_batches)
        assert sizes == [3, 10, 10]
        all_receipts = {e["ReceiptHandle"] for b in sqs.visibility_batches for e in b}
        assert all_receipts == receipts

    def test_shutdown_empty_inflight_no_calls(self):
        sqs = FakeSQS([])
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
        loop.shutdown()
        assert sqs.visibility_batches == []  # 空集合不调 API

    def test_shutdown_swallows_batch_api_errors(self):
        class ExplodingSQS(FakeSQS):
            def change_message_visibility_batch(self, QueueUrl, Entries):
                raise RuntimeError("api down")

        sqs = ExplodingSQS([])
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
        with loop._inflight_lock:
            loop._inflight.add("r1")
        # batch API 失败只 log，不阻断退出（仍要把 running 设 False）
        loop.shutdown()
        assert loop.running is False

    def test_shutdown_idempotent(self):
        sqs = FakeSQS([])
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=sqs)
        with loop._inflight_lock:
            loop._inflight.add("r1")
        loop.shutdown()
        loop.shutdown()  # 第二次：in-flight 已清空，不应再发 batch
        assert len(sqs.visibility_batches) == 1


class TestSignalHandlers:
    def test_install_registers_sigterm_and_sigint(self, monkeypatch):
        import signal as _sig

        registered = {}

        def fake_signal(signum, handler):
            registered[signum] = handler

        monkeypatch.setattr(worker_mod.signal, "signal", fake_signal)
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
        worker_mod._install_signal_handlers(loop)
        assert _sig.SIGTERM in registered
        assert _sig.SIGINT in registered

    def test_handler_calls_shutdown(self, monkeypatch):
        import signal as _sig

        registered = {}
        monkeypatch.setattr(
            worker_mod.signal, "signal",
            lambda signum, handler: registered.__setitem__(signum, handler),
        )
        called = {"n": 0}
        loop = WorkerLoop(_settings(), "http://large", "i-1", sqs=FakeSQS([]))
        monkeypatch.setattr(loop, "shutdown", lambda: called.__setitem__("n", called["n"] + 1))
        worker_mod._install_signal_handlers(loop)
        # 模拟收到 SIGTERM
        registered[_sig.SIGTERM](_sig.SIGTERM, None)
        assert called["n"] == 1
        assert loop.running is True  # shutdown 被 mock，未真正置位（验证 handler 调了 shutdown）
