"""Operational CLI for the transfer tool (spec §5.4, scoped).

Commands (transfer-tool ops only; not data-loading, not infra orchestration):
  inspect <source>     — single object's transfer history (DDB OLTP point query)
  replay [--max N]      — move failed messages from DLQ back to the main queue
  active-workers        — count live workers (heartbeat table)

Out of scope (other teams / layers):
  - filling SQS (upstream team)
  - pause/resume ASG (infra/runbook scripts)
  - prefix progress aggregation (Athena dashboards, §5)
"""
from __future__ import annotations

import argparse
import json
import sys
import time

from . import status_store
from .aws_clients import get_dynamodb_client, get_sqs_client
from .config import Settings


# ──────────────────────────── commands ─────────────────────────────────────
def cmd_inspect(ddb_client, table: str, source: str) -> list[dict]:
    """查单个 source 的所有 attempt 记录（按时间排序）。"""
    return status_store.inspect(ddb_client, table, source)


def resolve_dlq_url(sqs_client, main_queue_url: str) -> str:
    """运行时解析主队列对应的 DLQ URL（HIGH-4，不靠脆弱的 '-dlq' 字符串拼接）。

    读主队列的 RedrivePolicy.deadLetterTargetArn，再用 ARN 中的 account + queue name
    反查真实 DLQ URL。这样不依赖任何命名约定，DLQ 改名也不会崩。
    若主队列未配 RedrivePolicy 则显式报错（不静默回退到拼接）。
    """
    resp = sqs_client.get_queue_attributes(
        QueueUrl=main_queue_url, AttributeNames=["RedrivePolicy"]
    )
    redrive_raw = resp.get("Attributes", {}).get("RedrivePolicy")
    if not redrive_raw:
        raise ValueError(
            f"主队列 {main_queue_url} 未配置 RedrivePolicy，无法解析 DLQ；"
            "请确认 SQS 已绑定死信队列"
        )
    redrive = json.loads(redrive_raw)
    dlq_arn = redrive["deadLetterTargetArn"]
    # ARN 形如 arn:aws:sqs:<region>:<account>:<queue-name>
    parts = dlq_arn.split(":")
    account_id, queue_name = parts[4], parts[5]
    url_resp = sqs_client.get_queue_url(
        QueueName=queue_name, QueueOwnerAWSAccountId=account_id
    )
    return url_resp["QueueUrl"]


def _forwardable_attributes(message: dict) -> dict | None:
    """从 DLQ 消息提取可回传的 MessageAttributes（HIGH-5）。

    过滤掉 AWS 保留属性（'AWS.' 前缀，不允许由发送方设置）。自定义属性如
    object_size 的结构（DataType + StringValue/BinaryValue）可直接用于 send_message。
    无可回传属性时返回 None（不构造空 dict，避免 send_message 报错）。
    """
    attrs = message.get("MessageAttributes")
    if not attrs:
        return None
    forwardable = {k: v for k, v in attrs.items() if not k.startswith("AWS.")}
    return forwardable or None


def cmd_replay(sqs_client, *, dlq_url: str, main_url: str, max_messages: int) -> int:
    """把 DLQ 里的失败消息重投回主队列。

    只从 DLQ 进（不从主队列回滚，防误重放，spec 阶段4）。
    重投成功后才从 DLQ 删除（保证不丢）。返回重投条数。

    HIGH-5：receive 带 MessageAttributeNames=['All'] 拿到自定义属性，
    重投时透传 object_size 等属性，否则 worker 大文件全被当 small 路由。
    """
    moved = 0
    while moved < max_messages:
        resp = sqs_client.receive_message(
            QueueUrl=dlq_url,
            MaxNumberOfMessages=10,
            WaitTimeSeconds=1,
            MessageAttributeNames=["All"],
        )
        messages = resp.get("Messages", [])
        if not messages:
            break
        for m in messages:
            if moved >= max_messages:
                break
            send_kwargs: dict = {"QueueUrl": main_url, "MessageBody": m["Body"]}
            attrs = _forwardable_attributes(m)
            if attrs is not None:
                send_kwargs["MessageAttributes"] = attrs
            sqs_client.send_message(**send_kwargs)
            sqs_client.delete_message(QueueUrl=dlq_url, ReceiptHandle=m["ReceiptHandle"])
            moved += 1
    return moved


def cmd_active_workers(ddb_client, table: str, *, now_epoch: int) -> int:
    """统计活跃 worker 数（heartbeat ttl 未过期）。"""
    return status_store.count_active_workers(ddb_client, table, now_epoch=now_epoch)


# ──────────────────────────── arg parsing ──────────────────────────────────
def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="migration-cli", description="传输工具运营 CLI")
    sub = parser.add_subparsers(dest="command", required=True)

    p_inspect = sub.add_parser("inspect", help="查单个对象的传输历史")
    p_inspect.add_argument("source", help="rclone 源路径，如 s3src:bucket/path/file")

    p_replay = sub.add_parser("replay", help="把 DLQ 失败消息重投回主队列")
    p_replay.add_argument("--max", type=int, default=10000, help="最多重投条数")

    sub.add_parser("active-workers", help="统计活跃 worker 数")

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    settings = Settings.from_env()

    if args.command == "inspect":
        ddb = get_dynamodb_client(settings.aws_region)
        rows = cmd_inspect(ddb, settings.dynamodb_table, args.source)
        print(json.dumps(rows, ensure_ascii=False, indent=2))
        return 0

    if args.command == "replay":
        sqs = get_sqs_client(settings.aws_region)
        main_url = settings.queue_url
        dlq_url = resolve_dlq_url(sqs, main_url)
        n = cmd_replay(sqs, dlq_url=dlq_url, main_url=main_url, max_messages=args.max)
        print(f"重投 {n} 条消息回主队列")
        return 0

    if args.command == "active-workers":
        ddb = get_dynamodb_client(settings.aws_region)
        n = cmd_active_workers(ddb, settings.heartbeat_table, now_epoch=int(time.time()))
        print(f"活跃 worker: {n}")
        return 0

    return 1  # pragma: no cover


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
