#!/usr/bin/env python3
"""灌 delete 消息测试 op=delete 闭环。

列出目标前缀下已有对象，逐个发 op=delete SQS 消息（destination=该对象）。
worker 收到后跑 rclone deletefile 删目标端对象。验证：
- 队列消费完后该前缀对象数归零
- DDB 记录 op=delete 的 SUCCESS attempt
- 重复发(已删)仍 SUCCESS(幂等),不进 DLQ

用法: python3 feed_delete.py --prefix bench/migrated/ --limit 100
"""
from __future__ import annotations

import argparse
import json

import boto3

REGION = "us-east-1"
BUCKET = "datatos3-code"
QUEUE_NAME = "migration-smoke-queue"


def build_delete_entry(*, bucket: str, key: str, batch_start: int, idx: int) -> dict:
    """构造单条 op=delete 的 SQS batch entry(纯函数)。

    只带 op + destination(无 source);object_size 给 "0"(delete 不传输,
    large/small 路由对它无意义)。Id 用 batch_start+idx 保证单批内唯一。
    """
    body = {"op": "delete", "destination": f"s3:{bucket}/{key}"}
    return {
        "Id": f"d{batch_start}_{idx}",
        "MessageBody": json.dumps(body),
        "MessageAttributes": {
            "object_size": {"DataType": "Number", "StringValue": "0"}
        },
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--prefix", default="bench/migrated/")
    ap.add_argument("--limit", type=int, default=100, help="最多删多少个对象")
    args = ap.parse_args()

    s3 = boto3.client("s3", region_name=REGION)
    sqs = boto3.client("sqs", region_name=REGION)
    queue_url = sqs.get_queue_url(QueueName=QUEUE_NAME)["QueueUrl"]

    # 列目标前缀对象（最多 limit 个）。
    keys: list[str] = []
    paginator = s3.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=BUCKET, Prefix=args.prefix):
        for obj in page.get("Contents", []):
            keys.append(obj["Key"])
            if len(keys) >= args.limit:
                break
        if len(keys) >= args.limit:
            break

    print(f"==== 灌 delete 消息: prefix={args.prefix} 对象数={len(keys)} ====")

    sent = 0
    for start in range(0, len(keys), 10):
        chunk = keys[start : start + 10]
        entries = []
        for i, key in enumerate(chunk):
            entries.append(
                build_delete_entry(bucket=BUCKET, key=key, batch_start=start, idx=i)
            )
        sqs.send_message_batch(QueueUrl=queue_url, Entries=entries)
        sent += len(entries)

    print(f"==== 已发送 {sent} 条 delete 消息 ====")


if __name__ == "__main__":
    main()
