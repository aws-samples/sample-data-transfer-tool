"""Tests for migration_cli: operational commands for the transfer tool.

Scope (after 主公's narrowing — tool only does transfer):
  - inspect: read a single object's transfer history from DDB (OLTP)
  - replay:  move failed messages from a DLQ back to the main queue
  - active-workers: count live workers from heartbeat table
Pause/resume (ASG ops) and prefix-progress (Athena) are out of scope here.
"""
import json

from migration import migration_cli as cli


# ───────────────────────────── inspect ─────────────────────────────────────
class FakeDDB:
    def __init__(self, items):
        self._items = items
        self.queried = None

    def query(self, **kw):
        self.queried = kw
        return {"Items": self._items}

    def scan(self, **kw):
        return {"Items": self._items}


def test_inspect_returns_attempts(monkeypatch):
    items = [
        {"attempt_timestamp": {"S": "2026-05-20T01:00:00"}, "state": {"S": "FAILED"},
         "source": {"S": "s3src:a/x"}, "error_class": {"S": "src_rate_limit"}},
        {"attempt_timestamp": {"S": "2026-05-20T02:00:00"}, "state": {"S": "SUCCESS"},
         "source": {"S": "s3src:a/x"}},
    ]
    ddb = FakeDDB(items)
    out = cli.cmd_inspect(ddb, "transfer-status", "s3src:a/x")
    assert len(out) == 2
    assert out[0]["state"] == "FAILED"
    assert out[1]["state"] == "SUCCESS"


def test_inspect_unknown_source_empty(monkeypatch):
    ddb = FakeDDB([])
    out = cli.cmd_inspect(ddb, "transfer-status", "s3src:missing")
    assert out == []


# ───────────────────────────── replay ──────────────────────────────────────
class FakeSQS:
    def __init__(self, dlq_batches):
        self._batches = list(dlq_batches)
        self.sent = []
        self.deleted = []
        self.receive_kwargs = []

    def receive_message(self, **kw):
        self.receive_kwargs.append(kw)
        if self._batches:
            return {"Messages": self._batches.pop(0)}
        return {"Messages": []}

    def send_message(self, QueueUrl, MessageBody, MessageAttributes=None):
        self.sent.append((QueueUrl, MessageBody, MessageAttributes))

    def delete_message(self, QueueUrl, ReceiptHandle):
        self.deleted.append(ReceiptHandle)


def _dlq_msg(receipt, source, *, object_size=None):
    msg = {
        "ReceiptHandle": receipt,
        "Body": json.dumps({"source": source, "destination": "s3:b/y"}),
    }
    if object_size is not None:
        msg["MessageAttributes"] = {
            "object_size": {"DataType": "Number", "StringValue": str(object_size)}
        }
    return msg


def test_replay_moves_dlq_to_main():
    sqs = FakeSQS([[_dlq_msg("r1", "s3src:a"), _dlq_msg("r2", "s3src:b")], []])
    n = cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    assert n == 2
    assert len(sqs.sent) == 2          # re-sent to main
    assert sqs.deleted == ["r1", "r2"]  # removed from DLQ after re-send
    assert all(url == "http://main" for url, _, _ in sqs.sent)


def test_replay_respects_max():
    sqs = FakeSQS([[_dlq_msg("r1", "s3src:a"), _dlq_msg("r2", "s3src:b")]])
    n = cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=1)
    assert n == 1
    assert len(sqs.sent) == 1


def test_replay_empty_dlq():
    sqs = FakeSQS([[]])
    n = cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    assert n == 0


def test_replay_requests_message_attributes():
    """HIGH-5: receive 必须带 MessageAttributeNames=['All']，否则属性根本拿不到。"""
    sqs = FakeSQS([[_dlq_msg("r1", "s3src:a", object_size=12345)], []])
    cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    assert sqs.receive_kwargs[0].get("MessageAttributeNames") == ["All"]


def test_replay_preserves_message_attributes():
    """HIGH-5: object_size 等自定义属性必须透传，否则 worker 大文件全走 small 路由。"""
    sqs = FakeSQS([[_dlq_msg("r1", "s3src:big", object_size=999999999)], []])
    cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    _, _, attrs = sqs.sent[0]
    assert attrs is not None
    assert attrs["object_size"]["StringValue"] == "999999999"
    assert attrs["object_size"]["DataType"] == "Number"


def test_replay_no_attributes_sends_none():
    """无自定义属性的消息：MessageAttributes 不传（None），不构造空 dict。"""
    sqs = FakeSQS([[_dlq_msg("r1", "s3src:a")], []])
    cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    _, _, attrs = sqs.sent[0]
    assert attrs is None


def test_replay_strips_reserved_attributes():
    """AWS 保留属性（AWS. 前缀）不可回传，必须过滤掉。"""
    msg = {
        "ReceiptHandle": "r1",
        "Body": json.dumps({"source": "s3src:a"}),
        "MessageAttributes": {
            "object_size": {"DataType": "Number", "StringValue": "10"},
            "AWS.SomethingReserved": {"DataType": "String", "StringValue": "x"},
        },
    }
    sqs = FakeSQS([[msg], []])
    cli.cmd_replay(sqs, dlq_url="http://dlq", main_url="http://main", max_messages=100)
    _, _, attrs = sqs.sent[0]
    assert "object_size" in attrs
    assert "AWS.SomethingReserved" not in attrs


# ─────────────────────── resolve_dlq_url (HIGH-4) ───────────────────────────
class FakeSQSResolver:
    """mock get_queue_attributes + get_queue_url，验证运行时解析 DLQ URL。"""

    def __init__(self, redrive_policy=None, queue_url_by_name=None):
        self._redrive_policy = redrive_policy
        self._queue_url_by_name = queue_url_by_name or {}
        self.get_attr_kwargs = None
        self.get_url_kwargs = None

    def get_queue_attributes(self, **kw):
        self.get_attr_kwargs = kw
        attrs = {}
        if self._redrive_policy is not None:
            attrs["RedrivePolicy"] = json.dumps(self._redrive_policy)
        return {"Attributes": attrs}

    def get_queue_url(self, **kw):
        self.get_url_kwargs = kw
        return {"QueueUrl": self._queue_url_by_name[kw["QueueName"]]}


def test_resolve_dlq_url_from_redrive_policy():
    """从主队列 RedrivePolicy 的 deadLetterTargetArn 解析出真实 DLQ URL。"""
    dlq_arn = "arn:aws:sqs:eu-central-1:123456789012:my-dlq"
    sqs = FakeSQSResolver(
        redrive_policy={"deadLetterTargetArn": dlq_arn, "maxReceiveCount": 3},
        queue_url_by_name={"my-dlq": "https://sqs.eu-central-1.amazonaws.com/123456789012/my-dlq"},
    )
    url = cli.resolve_dlq_url(sqs, "https://sqs.eu-central-1.amazonaws.com/123456789012/main")
    assert url == "https://sqs.eu-central-1.amazonaws.com/123456789012/my-dlq"
    # 必须显式请求 RedrivePolicy 属性
    assert sqs.get_attr_kwargs["AttributeNames"] == ["RedrivePolicy"]
    # 用 ARN 里的 account + name 反查 URL（不靠字符串拼接）
    assert sqs.get_url_kwargs["QueueName"] == "my-dlq"
    assert sqs.get_url_kwargs["QueueOwnerAWSAccountId"] == "123456789012"


def test_resolve_dlq_url_no_redrive_policy_raises():
    """主队列没配 RedrivePolicy → 明确报错，不静默返回拼接 URL。"""
    sqs = FakeSQSResolver(redrive_policy=None)
    import pytest

    with pytest.raises(ValueError, match="RedrivePolicy"):
        cli.resolve_dlq_url(sqs, "https://sqs.eu-central-1.amazonaws.com/123456789012/main")


# ─────────────────────────── active-workers ────────────────────────────────
def test_active_workers(monkeypatch):
    fake = FakeDDB([])
    monkeypatch.setattr(cli.status_store, "count_active_workers", lambda c, t, *, now_epoch: 42)
    assert cli.cmd_active_workers(fake, "heartbeat", now_epoch=1000) == 42


# ─────────────────────────── arg parser ────────────────────────────────────
def test_build_parser_has_subcommands():
    parser = cli.build_parser()
    args = parser.parse_args(["inspect", "s3src:a/x"])
    assert args.command == "inspect"
    assert args.source == "s3src:a/x"


def test_build_parser_replay_args():
    parser = cli.build_parser()
    args = parser.parse_args(["replay", "--max", "500"])
    assert args.command == "replay"
    assert args.max == 500
