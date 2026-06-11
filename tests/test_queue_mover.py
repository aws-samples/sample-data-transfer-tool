"""Tests for queue_mover: A 集群 SQS → B 集群 SQS 消息搬运工具。

核心契约（不丢消息）：
  - 先发后删：只有 send 成功的消息才从源队列删除
  - send_message_batch 部分失败（Failed entries）→ 失败条不删，靠 visibility 自愈
  - MessageAttributes 透传（过滤 AWS. 保留前缀）
  - receive 用短 VisibilityTimeout（崩溃后消息 5min 回源队列，不被锁 12h）
"""
import json
import threading

from migration import queue_mover


class FakeSQS:
    """可注入批次与失败行为的 SQS fake（线程安全，供 --threads 测试）。"""

    def __init__(self, batches, *, fail_send_ids=None):
        self._batches = list(batches)
        self.sent_batches = []       # [(QueueUrl, Entries)]
        self.deleted_batches = []    # [(QueueUrl, Entries)]
        self.receive_kwargs = []
        self._fail_send_ids = set(fail_send_ids or ())
        self._lock = threading.Lock()

    def receive_message(self, **kw):
        with self._lock:
            self.receive_kwargs.append(kw)
            if self._batches:
                # 真实 SQS 遵守 MaxNumberOfMessages：最多返回 N 条，余下留在队列
                limit = kw.get("MaxNumberOfMessages", 10)
                batch = self._batches[0]
                out, rest = batch[:limit], batch[limit:]
                if rest:
                    self._batches[0] = rest
                else:
                    self._batches.pop(0)
                return {"Messages": out}
        return {"Messages": []}

    def send_message_batch(self, QueueUrl, Entries):
        with self._lock:
            self.sent_batches.append((QueueUrl, Entries))
        ok = [e for e in Entries if e["Id"] not in self._fail_send_ids]
        failed = [
            {"Id": e["Id"], "Code": "InternalError", "SenderFault": False, "Message": "boom"}
            for e in Entries if e["Id"] in self._fail_send_ids
        ]
        return {"Successful": [{"Id": e["Id"]} for e in ok], "Failed": failed}

    def delete_message_batch(self, QueueUrl, Entries):
        with self._lock:
            self.deleted_batches.append((QueueUrl, Entries))
        return {"Successful": [{"Id": e["Id"]} for e in Entries], "Failed": []}


def _msg(receipt, source, *, object_size=None):
    m = {
        "MessageId": f"mid-{receipt}",
        "ReceiptHandle": receipt,
        "Body": json.dumps({"source": source, "destination": "s3:b/y"}),
    }
    if object_size is not None:
        m["MessageAttributes"] = {
            "object_size": {"DataType": "Number", "StringValue": str(object_size)}
        }
    return m


SRC = "https://sqs.eu/123/a-queue"
DST = "https://sqs.eu/123/b-queue"


# ───────────────────────── move_messages 基本语义 ──────────────────────────
def test_moves_all_and_deletes_from_source():
    sqs = FakeSQS([[_msg("r1", "s3:a/1"), _msg("r2", "s3:a/2")]])
    n = queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=100)
    assert n == 2
    # 发到 B
    assert sqs.sent_batches[0][0] == DST
    assert len(sqs.sent_batches[0][1]) == 2
    # 从 A 删（全部 send 成功）
    assert sqs.deleted_batches[0][0] == SRC
    assert {e["ReceiptHandle"] for e in sqs.deleted_batches[0][1]} == {"r1", "r2"}


def test_respects_max_messages():
    sqs = FakeSQS([[_msg(f"r{i}", f"s3:a/{i}") for i in range(10)]])
    n = queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=4)
    assert n == 4


def test_empty_source_returns_zero():
    sqs = FakeSQS([])
    assert queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=10) == 0
    assert sqs.sent_batches == []


def test_partial_send_failure_keeps_failed_on_source():
    """B 端部分失败：失败条**不删**（靠 visibility 回源队列），成功条正常删。"""
    msgs = [_msg("r1", "s3:a/1"), _msg("r2", "s3:a/2"), _msg("r3", "s3:a/3")]
    # Entry Id = MessageId
    sqs = FakeSQS([msgs], fail_send_ids={"mid-r2"})
    n = queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=10)
    assert n == 2  # 只算成功搬运的
    deleted = {e["ReceiptHandle"] for b in sqs.deleted_batches for e in b[1]}
    assert deleted == {"r1", "r3"}  # r2 留在源队列


def test_preserves_message_attributes():
    sqs = FakeSQS([[_msg("r1", "s3:a/big", object_size=200 * 1024 * 1024)]])
    queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=10)
    entry = sqs.sent_batches[0][1][0]
    assert entry["MessageAttributes"]["object_size"]["StringValue"] == str(200 * 1024 * 1024)


def test_strips_aws_reserved_attributes():
    m = _msg("r1", "s3:a/x")
    m["MessageAttributes"] = {
        "AWS.Trace": {"DataType": "String", "StringValue": "x"},
        "object_size": {"DataType": "Number", "StringValue": "1"},
    }
    sqs = FakeSQS([[m]])
    queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=10)
    entry = sqs.sent_batches[0][1][0]
    assert "AWS.Trace" not in entry.get("MessageAttributes", {})
    assert "object_size" in entry["MessageAttributes"]


def test_receive_uses_short_visibility_and_all_attributes():
    """崩溃自愈：receive 显式短 visibility（不能继承队列的 12h）；属性全量拉取。"""
    sqs = FakeSQS([[_msg("r1", "s3:a/1")]])
    queue_mover.move_messages(sqs, src_url=SRC, dst_url=DST, max_messages=10)
    kw = sqs.receive_kwargs[0]
    assert kw["VisibilityTimeout"] == queue_mover.MOVER_VISIBILITY_SECONDS
    assert kw["MessageAttributeNames"] == ["All"]
    assert kw["QueueUrl"] == SRC


def test_dry_run_neither_sends_nor_deletes():
    sqs = FakeSQS([[_msg("r1", "s3:a/1"), _msg("r2", "s3:a/2")]])
    n = queue_mover.move_messages(
        sqs, src_url=SRC, dst_url=DST, max_messages=10, dry_run=True
    )
    assert n == 2  # 统计到的可搬运条数
    assert sqs.sent_batches == []
    assert sqs.deleted_batches == []


def test_threads_never_exceed_max():
    """实测踩坑（2026-06-11 真实队列）：2 线程 --max 2 搬了 4 条。
    配额必须 receive 前预留，多线程严格不超 max。"""
    batches = [[_msg(f"r{b}-{i}", f"s3:a/{b}/{i}") for i in range(10)] for b in range(5)]
    sqs = FakeSQS(batches)
    n = queue_mover.move_messages(
        sqs, src_url=SRC, dst_url=DST, max_messages=2, threads=4
    )
    assert n == 2
    assert sum(len(b[1]) for b in sqs.sent_batches) == 2


def test_threads_move_concurrently_without_loss():
    """多线程：100 条分 10 批，全部恰好搬一次（计数准确、无重复删除）。"""
    batches = [[_msg(f"r{b}-{i}", f"s3:a/{b}/{i}") for i in range(10)] for b in range(10)]
    sqs = FakeSQS(batches)
    n = queue_mover.move_messages(
        sqs, src_url=SRC, dst_url=DST, max_messages=1000, threads=4
    )
    assert n == 100
    sent = [e["Id"] for b in sqs.sent_batches for e in b[1]]
    assert len(sent) == 100 and len(set(sent)) == 100
    deleted = [e["ReceiptHandle"] for b in sqs.deleted_batches for e in b[1]]
    assert len(deleted) == 100 and len(set(deleted)) == 100


# ───────────────────────────── CLI 入口 ────────────────────────────────────
def test_main_requires_different_queues(capsys):
    rc = queue_mover.main(["--src-queue", SRC, "--dst-queue", SRC])
    assert rc == 2
    assert "不能相同" in capsys.readouterr().err
