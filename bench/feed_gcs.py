#!/usr/bin/env python3
"""真实 GCS→S3 迁移灌数据:列 GCS 桶对象 → 生成 SQS 消息(gcs: source)。

与压测 feed_local.py 的本质区别:
- source 是真实 `gcs:<bucket>/<key>`(rclone.conf 的 [gcs] remote),非 s3: 中转
- destination key 保留源 key 结构挂到目标前缀下,**幂等**(同源永远映射同一目标),
  不掺 run_id —— 真实迁移要支持安全重跑/断点续传(rclone 据已迁移对象 skip)
- 凭证从环境变量读(GCS_HMAC_ACCESS_KEY/SECRET/ENDPOINT),绝不硬编码

I/O(列桶 / 发 SQS)经注入,纯逻辑可无 AWS 单测。

用法:
  export GCS_HMAC_ACCESS_KEY=... GCS_HMAC_SECRET=...
  python3 feed_gcs.py --src-bucket gcs-linnjia-test \\
      --dest-bucket datatos3-code --dest-prefix migrated --queue-name migration-smoke-queue
"""
from __future__ import annotations

import argparse
import json
import os
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from typing import Any

# GCS S3 兼容官方 endpoint(未显式配置时回退)。
_DEFAULT_GCS_ENDPOINT = "https://storage.googleapis.com"
# SQS send_message_batch 上限 10 条/批。
_SQS_BATCH_MAX = 10


@dataclass(frozen=True)
class GcsCredentials:
    """GCS HMAC 凭证(S3 兼容互操作)。"""

    access_key: str
    secret_key: str
    endpoint: str


def load_gcs_credentials(env: dict[str, str]) -> GcsCredentials:
    """从环境变量读 GCS HMAC 凭证;缺失/空值 fail fast(不静默用空凭证)。

    endpoint 未配置时回退官方 S3 兼容 endpoint。
    """
    access_key = env.get("GCS_HMAC_ACCESS_KEY", "")
    if not access_key:
        raise ValueError("GCS_HMAC_ACCESS_KEY 未配置或为空")
    secret_key = env.get("GCS_HMAC_SECRET", "")
    if not secret_key:
        raise ValueError("GCS_HMAC_SECRET 未配置或为空")
    endpoint = env.get("GCS_ENDPOINT") or _DEFAULT_GCS_ENDPOINT
    return GcsCredentials(access_key=access_key, secret_key=secret_key, endpoint=endpoint)


def dest_key_for(prefix: str, src_key: str) -> str:
    """目标 key = 前缀 + 源 key(保留路径结构,幂等)。

    前缀末尾斜杠规整,空前缀直接返回源 key(无前导斜杠)。
    """
    p = prefix.rstrip("/")
    if not p:
        return src_key
    return f"{p}/{src_key}"


def build_message_body(
    *, src_bucket: str, src_key: str, dest_bucket: str, dest_prefix: str
) -> dict[str, Any]:
    """构造迁移消息体:gcs: source + s3: destination,op 缺省 copy 省略。

    与 models.TransferMessage.to_body / feed_local 消息形态一致(copy 不写 op)。
    """
    return {
        "source": f"gcs:{src_bucket}/{src_key}",
        "destination": f"s3:{dest_bucket}/{dest_key_for(dest_prefix, src_key)}",
    }


def build_sqs_entry(*, entry_id: str, body: dict[str, Any], object_size: int) -> dict[str, Any]:
    """构造 SQS batch entry,真实 object_size 进 MessageAttributes 供 worker 大小路由。"""
    return {
        "Id": entry_id,
        "MessageBody": json.dumps(body),
        "MessageAttributes": {
            "object_size": {"DataType": "Number", "StringValue": str(int(object_size))}
        },
    }


def chunked(items: list[Any], size: int) -> Iterator[list[Any]]:
    """按 size 切分(SQS 批量上限);空列表产出空迭代。"""
    for start in range(0, len(items), size):
        yield items[start : start + size]


# ── I/O 层(运行时用,单测经注入不触达真实 AWS)──────────────────────────────
def list_gcs_objects(s3_client: Any, bucket: str, prefix: str = "") -> list[tuple[str, int]]:
    """列 GCS 桶对象 (key, size)。s3_client 是指向 GCS endpoint 的 boto3 s3 client。"""
    objs: list[tuple[str, int]] = []
    paginator = s3_client.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        objs.extend((o["Key"], o["Size"]) for o in page.get("Contents", []))
    return objs


def feed(
    *,
    objects: Iterable[tuple[str, int]],
    src_bucket: str,
    dest_bucket: str,
    dest_prefix: str,
    queue_url: str,
    send_batch: Any,
) -> int:
    """把对象清单组成消息批量发 SQS;send_batch(queue_url, entries) 经注入。返回发送条数。"""
    obj_list = list(objects)
    sent = 0
    for idx, batch in enumerate(chunked(obj_list, _SQS_BATCH_MAX)):
        entries = []
        for j, (key, size) in enumerate(batch):
            body = build_message_body(
                src_bucket=src_bucket, src_key=key,
                dest_bucket=dest_bucket, dest_prefix=dest_prefix,
            )
            entries.append(build_sqs_entry(entry_id=f"m{idx}_{j}", body=body, object_size=size))
        send_batch(queue_url, entries)
        sent += len(entries)
    return sent


def main() -> None:
    import boto3
    from botocore.config import Config

    ap = argparse.ArgumentParser()
    ap.add_argument("--src-bucket", default="gcs-linnjia-test")
    ap.add_argument("--src-prefix", default="")
    ap.add_argument("--dest-bucket", default="datatos3-code")
    ap.add_argument("--dest-prefix", default="migrated")
    ap.add_argument("--queue-name", default="migration-smoke-queue")
    ap.add_argument("--region", default="us-east-1")
    args = ap.parse_args()

    creds = load_gcs_credentials(dict(os.environ))
    cfg = Config(retries={"max_attempts": 5, "mode": "adaptive"})
    # 指向 GCS endpoint 的 s3 client 列源对象(HMAC 即 S3 兼容凭证)。
    gcs = boto3.client(
        "s3", region_name=args.region, endpoint_url=creds.endpoint,
        aws_access_key_id=creds.access_key, aws_secret_access_key=creds.secret_key, config=cfg,
    )
    sqs = boto3.client("sqs", region_name=args.region, config=cfg)
    queue_url = sqs.get_queue_url(QueueName=args.queue_name)["QueueUrl"]

    objs = list_gcs_objects(gcs, args.src_bucket, args.src_prefix)
    print(f"==== GCS→S3 灌数据 src=gcs:{args.src_bucket}/{args.src_prefix} "
          f"对象数={len(objs)} dest=s3:{args.dest_bucket}/{args.dest_prefix} ====")

    def _send(qurl: str, entries: list[dict]) -> None:
        sqs.send_message_batch(QueueUrl=qurl, Entries=entries)

    sent = feed(
        objects=objs, src_bucket=args.src_bucket, dest_bucket=args.dest_bucket,
        dest_prefix=args.dest_prefix, queue_url=queue_url, send_batch=_send,
    )
    print(f"==== 已发送 {sent} 条迁移消息 ====")


if __name__ == "__main__":
    main()
