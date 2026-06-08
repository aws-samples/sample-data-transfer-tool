#!/usr/bin/env python3
"""本地高速批量灌数据(用本机凭证,不占 worker CPU)。

并发线程 × SQS send_message_batch(每批 10 条),比逐条 bash 快几个数量级。
每条消息 source/dest 为 s3: remote,dest key 唯一(线程号+序号),--disable copy
强制走 EC2 中转,模拟 GCS->S3 跨云负载。

用法: python3 feed_local.py --threads 16 --duration 600
"""
from __future__ import annotations

import argparse
import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import boto3
from botocore.config import Config

REGION = "us-east-1"
BUCKET = "datatos3-code"
QUEUE_NAME = "migration-smoke-queue"
SRC_PREFIX = "bench/source/lg_"

_stop = threading.Event()
_sent = [0]  # 原子计数(GIL 下 += 安全用锁兜底)
_lock = threading.Lock()
# 每次运行的唯一 run-id,拼进 destination key,保证跨 feeder 重启绝不撞 key
# (避免目标已存在 → rclone 判定无需传输 skip → 网卡空转、吞吐数据失真偏低)。
_RUN_ID = str(int(time.time()))


def _session_client():
    """每线程独立 client(boto3 client 线程安全,但连接池按需扩大)。"""
    cfg = Config(retries={"max_attempts": 5, "mode": "adaptive"}, max_pool_connections=50)
    return boto3.client("sqs", region_name=REGION, config=cfg)


def parse_large_files(resp: dict) -> list[tuple[str, int]]:
    """从 list_objects_v2 响应解析 (key, size) 列表(纯函数,易测)。"""
    return [(o["Key"], o["Size"]) for o in resp.get("Contents", [])]


def list_large_files(s3) -> list[tuple[str, int]]:
    """列源桶大文件 (key, size)。"""
    resp = s3.list_objects_v2(Bucket=BUCKET, Prefix=SRC_PREFIX)
    return parse_large_files(resp)


def build_copy_entry(
    *, bucket: str, key: str, size: int, run_id: str, worker_id: int, seq: int
) -> dict:
    """构造单条 copy 的 SQS batch entry(纯函数)。

    destination 拼入 run_id/worker_id/seq 保证全局唯一 → 目标必不存在 →
    rclone 不会 skip,真实占用网卡,bench 吞吐数据才可信。
    """
    name = key.rsplit("/", 1)[-1]
    body = {
        "source": f"s3:{bucket}/{key}",
        "destination": f"s3:{bucket}/bench/run_{run_id}/w{worker_id}_{seq}_{name}",
    }
    return {
        "Id": f"w{worker_id}n{seq}",
        "MessageBody": json.dumps(body),
        "MessageAttributes": {
            "object_size": {"DataType": "Number", "StringValue": str(size)}
        },
    }


def feeder(worker_id: int, queue_url: str, files: list[tuple[str, int]]) -> None:
    """单线程循环:组 10 条 batch 持续发,直到 stop。"""
    sqs = _session_client()
    n = 0
    nfiles = len(files)
    while not _stop.is_set():
        entries = []
        for _ in range(10):
            key, size = files[n % nfiles]
            entries.append(
                build_copy_entry(
                    bucket=BUCKET, key=key, size=size,
                    run_id=_RUN_ID, worker_id=worker_id, seq=n,
                )
            )
            n += 1
        try:
            sqs.send_message_batch(QueueUrl=queue_url, Entries=entries)
            with _lock:
                _sent[0] += len(entries)
        except Exception as exc:  # noqa: BLE001 - 灌数容错,打印继续
            print(f"[w{worker_id}] batch 失败: {exc}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--threads", type=int, default=16)
    ap.add_argument("--duration", type=int, default=600)
    args = ap.parse_args()

    s3 = boto3.client("s3", region_name=REGION)
    sqs = boto3.client("sqs", region_name=REGION)
    queue_url = sqs.get_queue_url(QueueName=QUEUE_NAME)["QueueUrl"]
    files = list_large_files(s3)
    print(f"==== 批量灌数据 threads={args.threads} duration={args.duration}s "
          f"files={len(files)} queue={queue_url} ====")

    start = time.time()
    with ThreadPoolExecutor(max_workers=args.threads) as pool:
        for w in range(args.threads):
            pool.submit(feeder, w, queue_url, files)

        # 主线程周期打印吞吐 + 队列深度
        while time.time() - start < args.duration:
            time.sleep(10)
            el = int(time.time() - start)
            with _lock:
                sent = _sent[0]
            attrs = sqs.get_queue_attributes(
                QueueUrl=queue_url,
                AttributeNames=["ApproximateNumberOfMessages",
                                "ApproximateNumberOfMessagesNotVisible"],
            )["Attributes"]
            rate = sent / el if el else 0
            print(f"[+{el}s] 已发送={sent} ({rate:.0f} msg/s) | "
                  f"queue visible={attrs['ApproximateNumberOfMessages']} "
                  f"inflight={attrs['ApproximateNumberOfMessagesNotVisible']}")
        _stop.set()

    print(f"==== 结束,总发送={_sent[0]} 条 ====")


if __name__ == "__main__":
    main()
