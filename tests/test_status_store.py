"""status_store 集成测试（先写，TDD RED）。

DDB 是纯 OLTP：只写终态、点查 inspect、心跳。用 moto 5.x mock_aws 建表→写→读全链路验证。
所有 now 通过参数注入，测试传固定值。
"""
from __future__ import annotations

import pytest
from moto import mock_aws

from migration import aws_clients
from migration.models import RunResult, State, TransferStats
from migration.status_store import (
    count_active_workers,
    create_tables,
    inspect,
    make_pk,
    record_terminal,
    write_heartbeat,
)

REGION = "eu-central-1"

# 测试用表名（生产由 config.Settings 提供）。
STATUS_TABLE = "transfer-message-status-eu-central-1"
HEARTBEAT_TABLE = "worker-heartbeat-eu-central-1"


@pytest.fixture
def ddb():
    """启动 moto mock，reset 客户端缓存，建表，返回 dynamodb client。"""
    with mock_aws():
        aws_clients.reset_clients()
        client = aws_clients.get_dynamodb_client(REGION)
        create_tables(client, STATUS_TABLE, HEARTBEAT_TABLE)
        yield client
        aws_clients.reset_clients()


def _result(
    *,
    state: State = State.SUCCESS,
    exit_code: int = 0,
    bytes_: int = 1024,
    elapsed: float = 2.5,
    error_class: str | None = None,
    error_message: str | None = None,
    cmd_str: str = "rclone copyto onedrive:a.txt s3:bucket/a.txt",
) -> RunResult:
    return RunResult(
        state=state,
        exit_code=exit_code,
        stats=TransferStats(bytes=bytes_, elapsed_seconds=elapsed),
        error_class=error_class,
        error_message=error_message,
        cmd_str=cmd_str,
    )


# ── make_pk ────────────────────────────────────────────────────────────────────


@pytest.mark.unit
def test_make_pk_format():
    """PK 形如 '<0-255>#<source>'。"""
    source = "onedrive:Documents/file.pdf"
    pk = make_pk(source)
    prefix, _, rest = pk.partition("#")
    assert rest == source
    assert 0 <= int(prefix) <= 255


@pytest.mark.unit
def test_make_pk_stable_across_calls():
    """同一 source 多次调用结果完全一致（hashlib 跨进程稳定）。"""
    source = "sharepoint:site/lib/报告.docx"
    assert make_pk(source) == make_pk(source)


@pytest.mark.unit
def test_make_pk_distributes_across_shards():
    """大量不同 source 散落到多个分片前缀（散热有效，非全部同一前缀）。"""
    prefixes = {make_pk(f"onedrive:file-{i}.txt").split("#", 1)[0] for i in range(500)}
    assert len(prefixes) > 50


@pytest.mark.unit
def test_make_pk_unicode_source():
    """非 ASCII source 不报错且稳定。"""
    source = "onedrive:文档/测试 🚀.pdf"
    assert make_pk(source) == make_pk(source)


# ── record_terminal ──────────────────────────────────────────────────────────


@pytest.mark.integration
def test_record_terminal_writes_readable_row(ddb):
    """写一条 SUCCESS 终态，能完整读回。"""
    source = "onedrive:a/b.txt"
    ts = "2026-05-29T10:00:00+00:00"
    record_terminal(
        ddb,
        STATUS_TABLE,
        source,
        ts,
        _result(),
        instance_id="i-abc123",
        now_iso="2026-05-29T10:00:05+00:00",
    )
    rows = inspect(ddb, STATUS_TABLE, source)
    assert len(rows) == 1
    row = rows[0]
    assert row["source"] == source
    assert row["attempt_timestamp"] == ts
    assert row["state"] == "SUCCESS"
    assert row["transferred_bytes"] == 1024
    assert row["elapsed_seconds"] == pytest.approx(2.5)
    assert row["instance_id"] == "i-abc123"
    assert row["rclone_command"] == "rclone copyto onedrive:a.txt s3:bucket/a.txt"
    assert row["updated_at"] == "2026-05-29T10:00:05+00:00"


@pytest.mark.integration
def test_record_terminal_failure_carries_error_fields(ddb):
    """RETRYABLE/FATAL 终态保存 error_class / error_message。"""
    source = "onedrive:fail.bin"
    record_terminal(
        ddb,
        STATUS_TABLE,
        source,
        "2026-05-29T11:00:00+00:00",
        _result(
            state=State.RETRYABLE,
            exit_code=1,
            error_class="rate_limit",
            error_message="429 Too Many Requests",
        ),
        instance_id="i-xyz",
        now_iso="2026-05-29T11:00:01+00:00",
    )
    row = inspect(ddb, STATUS_TABLE, source)[0]
    assert row["state"] == "RETRYABLE"
    assert row["error_class"] == "rate_limit"
    assert row["error_message"] == "429 Too Many Requests"


@pytest.mark.integration
def test_record_terminal_no_error_fields_when_success(ddb):
    """SUCCESS 时不写 error_class/error_message（避免空属性污染）。"""
    source = "onedrive:ok.txt"
    record_terminal(
        ddb,
        STATUS_TABLE,
        source,
        "2026-05-29T12:00:00+00:00",
        _result(),
        instance_id="i-ok",
        now_iso="2026-05-29T12:00:01+00:00",
    )
    row = inspect(ddb, STATUS_TABLE, source)[0]
    assert "error_class" not in row
    assert "error_message" not in row


@pytest.mark.integration
def test_multiple_attempts_are_separate_rows(ddb):
    """同一 source 多次 attempt 是多行（不同 SK），不是覆盖。"""
    source = "onedrive:retry.dat"
    record_terminal(
        ddb,
        STATUS_TABLE,
        source,
        "2026-05-29T13:00:00+00:00",
        _result(state=State.RETRYABLE, exit_code=1, error_class="network"),
        instance_id="i-1",
        now_iso="2026-05-29T13:00:01+00:00",
    )
    record_terminal(
        ddb,
        STATUS_TABLE,
        source,
        "2026-05-29T14:00:00+00:00",
        _result(),
        instance_id="i-2",
        now_iso="2026-05-29T14:00:01+00:00",
    )
    rows = inspect(ddb, STATUS_TABLE, source)
    assert len(rows) == 2


# ── inspect ────────────────────────────────────────────────────────────────────


@pytest.mark.integration
def test_inspect_returns_attempts_sorted_ascending(ddb):
    """inspect 返回多 attempt 按 attempt_timestamp 升序排序。"""
    source = "onedrive:sorted.txt"
    for ts in ("2026-05-29T03:00:00+00:00", "2026-05-29T01:00:00+00:00", "2026-05-29T02:00:00+00:00"):
        record_terminal(
            ddb, STATUS_TABLE, source, ts, _result(), instance_id="i", now_iso=ts
        )
    rows = inspect(ddb, STATUS_TABLE, source)
    timestamps = [r["attempt_timestamp"] for r in rows]
    assert timestamps == sorted(timestamps)
    assert timestamps[0] == "2026-05-29T01:00:00+00:00"
    assert timestamps[-1] == "2026-05-29T03:00:00+00:00"


@pytest.mark.integration
def test_inspect_unknown_source_returns_empty(ddb):
    """点查不存在的 source 返回空列表，不报错。"""
    assert inspect(ddb, STATUS_TABLE, "onedrive:nope.txt") == []


@pytest.mark.integration
def test_inspect_isolates_by_source(ddb):
    """不同 source 互不串扰（即使散落同分片，SK/source 区分）。"""
    record_terminal(
        ddb, STATUS_TABLE, "onedrive:x.txt", "2026-05-29T10:00:00+00:00",
        _result(), instance_id="i", now_iso="2026-05-29T10:00:00+00:00",
    )
    record_terminal(
        ddb, STATUS_TABLE, "onedrive:y.txt", "2026-05-29T10:00:00+00:00",
        _result(), instance_id="i", now_iso="2026-05-29T10:00:00+00:00",
    )
    assert len(inspect(ddb, STATUS_TABLE, "onedrive:x.txt")) == 1
    assert len(inspect(ddb, STATUS_TABLE, "onedrive:y.txt")) == 1


# ── write_heartbeat ──────────────────────────────────────────────────────────


@pytest.mark.integration
def test_write_heartbeat_sets_ttl_plus_300(ddb):
    """心跳 ttl = now_epoch + 300。"""
    write_heartbeat(
        ddb,
        HEARTBEAT_TABLE,
        "i-worker-1",
        now_iso="2026-05-29T10:00:00+00:00",
        now_epoch=1_700_000_000,
        active_threads=8,
    )
    item = ddb.get_item(
        TableName=HEARTBEAT_TABLE,
        Key={"instance_id": {"S": "i-worker-1"}},
    )["Item"]
    assert item["last_heartbeat"]["S"] == "2026-05-29T10:00:00+00:00"
    assert int(item["ttl"]["N"]) == 1_700_000_000 + 300
    assert int(item["active_threads"]["N"]) == 8


@pytest.mark.integration
def test_write_heartbeat_overwrites_same_instance(ddb):
    """同一 instance_id 心跳是覆盖更新，不累积行。"""
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-dup",
        now_iso="2026-05-29T10:00:00+00:00", now_epoch=1_700_000_000, active_threads=1,
    )
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-dup",
        now_iso="2026-05-29T10:05:00+00:00", now_epoch=1_700_000_300, active_threads=4,
    )
    item = ddb.get_item(
        TableName=HEARTBEAT_TABLE, Key={"instance_id": {"S": "i-dup"}}
    )["Item"]
    assert item["last_heartbeat"]["S"] == "2026-05-29T10:05:00+00:00"
    assert int(item["active_threads"]["N"]) == 4


# ── count_active_workers ─────────────────────────────────────────────────────


@pytest.mark.integration
def test_count_active_workers_filters_expired(ddb):
    """只数 ttl > now_epoch 的活跃 worker，过期的不计。"""
    now = 1_700_000_000
    # 活跃：ttl 在未来
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-alive-1",
        now_iso="t", now_epoch=now, active_threads=1,
    )
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-alive-2",
        now_iso="t", now_epoch=now - 100, active_threads=1,  # ttl=now-100+300=now+200 仍活
    )
    # 过期：ttl <= now（now_epoch 远在过去）
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-dead",
        now_iso="t", now_epoch=now - 1000, active_threads=1,  # ttl=now-700 已过期
    )
    assert count_active_workers(ddb, HEARTBEAT_TABLE, now_epoch=now) == 2


@pytest.mark.integration
def test_count_active_workers_empty_table(ddb):
    """空 heartbeat 表返回 0。"""
    assert count_active_workers(ddb, HEARTBEAT_TABLE, now_epoch=1_700_000_000) == 0


@pytest.mark.integration
def test_count_active_workers_paginates(ddb, monkeypatch):
    """scan 分页：注入一次 LastEvaluatedKey，确保翻页逻辑被走到且不重复计数。"""
    now = 1_700_000_000
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-page-1", now_iso="t", now_epoch=now, active_threads=1
    )
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-page-2", now_iso="t", now_epoch=now, active_threads=1
    )

    real_scan = ddb.scan
    calls = {"n": 0}

    def fake_scan(**kwargs):
        resp = real_scan(**kwargs)
        # 首次调用：截成单条 + 伪造 LastEvaluatedKey 强制翻页
        if calls["n"] == 0 and "ExclusiveStartKey" not in kwargs:
            calls["n"] += 1
            first = resp["Items"][0]
            return {
                "Items": [first],
                "LastEvaluatedKey": {"instance_id": first["instance_id"]},
            }
        # 第二页：返回另一条（按 instance_id 区分），不重复首条
        first_id = "i-page-1"
        rest = [i for i in resp["Items"] if i["instance_id"]["S"] != first_id]
        return {"Items": rest}

    monkeypatch.setattr(ddb, "scan", fake_scan)
    assert count_active_workers(ddb, HEARTBEAT_TABLE, now_epoch=now) == 2


@pytest.mark.integration
def test_count_active_workers_uses_server_side_filter(ddb, monkeypatch):
    """HIGH-6: scan 必须带 FilterExpression='ttl > :now'，服务端过滤过期行，减少传输。"""
    now = 1_700_000_000
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-x", now_iso="t", now_epoch=now, active_threads=1
    )
    real_scan = ddb.scan
    seen = []

    def spy_scan(**kwargs):
        seen.append(kwargs)
        return real_scan(**kwargs)

    monkeypatch.setattr(ddb, "scan", spy_scan)
    count_active_workers(ddb, HEARTBEAT_TABLE, now_epoch=now)
    assert seen, "scan 未被调用"
    kw = seen[0]
    assert "ttl" in kw["FilterExpression"]
    assert kw["ExpressionAttributeValues"][":now"] == {"N": str(now)}


@pytest.mark.integration
def test_count_active_workers_boundary_equal_ttl_not_active(ddb):
    """ttl == now_epoch 视为已过期（边界：严格大于才算活跃）。"""
    now = 1_700_000_000
    # ttl 恰好等于 now：now_epoch_at_write + 300 == now → now_epoch_at_write = now-300
    write_heartbeat(
        ddb, HEARTBEAT_TABLE, "i-edge",
        now_iso="t", now_epoch=now - 300, active_threads=1,
    )
    assert count_active_workers(ddb, HEARTBEAT_TABLE, now_epoch=now) == 0
