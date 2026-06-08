#!/usr/bin/env python3
"""S3→S3 高速批量灌数据(本机凭证多线程,不占 worker CPU)。

列源桶指定前缀下的真实对象,组成 SQS 消息(source/destination 均为 s3: remote),
多线程 send_message_batch(每批 10)持续灌入,直到对象列表灌完 N 轮或达到时长。

dest key 拼入唯一 run-id + 轮次,保证目标永不撞 key(避免 rclone 判已存在而 skip,
导致吞吐数据失真偏低)。源和目标可以是同一个桶(用不同前缀)。

用法:
  python3 bench/feed_s3.py --queue-name migration-primary-queue \\
    --src-bucket migration-dest-784682930398-eu-south-2 --src-prefix stress-large/ \\
    --dest-bucket migration-dest-784682930398-eu-south-2 --dest-prefix s3test \\
    --region eu-south-2 --threads 16 --rounds 3
"""
from __future__ import annotations

import argparse
import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import boto3
from botocore.config import Config

_stop = threading.Event()
_sent = [0]
_lock = threading.Lock()
_RUN_ID = str(int(time.time()))


def _sqs_client(region: str):
    cfg = Config(retries={"max_attempts": 5, "mode": "adaptive"}, max_pool_connections=50)
    return boto3.client("sqs", region_name=region, config=cfg)


def list_source_keys(s3, bucket: str, prefix: str) -> list[str]:
    """列源桶前缀下全部对象 key(分页)。"""
    keys: list[str] = []
    paginator = s3.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        keys.extend(o["Key"] for o in page.get("Contents", []))
    return keys


def build_entry(*, src_bucket: str, key: str, dest_bucket: str, dest_prefix: str,
                run_id: str, worker_id: int, seq: int) -> dict:
    """构造单条 S3→S3 copy 的 SQS batch entry(纯函数,易测)。

    source = s3:<src_bucket>/<key>;destination 拼 run_id/worker/seq 保证唯一。
    """
    name = key.rsplit("/", 1)[-1]
    body = {
        "source": f"s3:{src_bucket}/{key}",
        "destination": f"s3:{dest_bucket}/{dest_prefix}/run_{run_id}/w{worker_id}_{seq}/{name}",
    }
    return {"Id": f"w{worker_id}n{seq}", "MessageBody": json.dumps(body)}


def feeder(*, worker_id: int, region: str, queue_url: str, keys: list[str],
           src_bucket: str, dest_bucket: str, dest_prefix: str, rounds: int) -> None:
    """单线程:把分到的 key 组 10 条 batch 发,跑 rounds 轮(或被 stop)。"""
    sqs = _sqs_client(region)
    n = 0
    nkeys = len(keys)
    total = nkeys * rounds
    while not _stop.is_set() and n < total:
        entries = []
        for _ in range(10):
            if n >= total:
                break
            key = keys[n % nkeys]
            entries.append(build_entry(
                src_bucket=src_bucket, key=key, dest_bucket=dest_bucket,
                dest_prefix=dest_prefix, run_id=_RUN_ID, worker_id=worker_id, seq=n,
            ))
            n += 1
        if not entries:
            break
        try:
            sqs.send_message_batch(QueueUrl=queue_url, Entries=entries)
            with _lock:
                _sent[0] += len(entries)
        except Exception as exc:  # noqa: BLE001 - 灌数容错,打印继续
            print(f"[w{worker_id}] batch 失败: {exc}", flush=True)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--queue-name", required=True)
    ap.add_argument("--src-bucket", required=True)
    ap.add_argument("--src-prefix", required=True, help="源对象前缀,如 stress-large/")
    ap.add_argument("--dest-bucket", required=True)
    ap.add_argument("--dest-prefix", default="s3test")
    ap.add_argument("--region", default="eu-south-2")
    ap.add_argument("--threads", type=int, default=16)
    ap.add_argument("--rounds", type=int, default=1, help="每个源对象灌几轮(dest key 含轮次,不撞)")
    args = ap.parse_args()

    s3 = boto3.client("s3", region_name=args.region)
    sqs = _sqs_client(args.region)
    queue_url = sqs.get_queue_url(QueueName=args.queue_name)["QueueUrl"]
    keys = list_source_keys(s3, args.src_bucket, args.src_prefix)
    if not keys:
        print(f"错误: 源 s3:{args.src_bucket}/{args.src_prefix} 没有对象,退出")
        return
    print(f"==== S3->S3 灌数据 threads={args.threads} rounds={args.rounds} "
          f"src=s3:{args.src_bucket}/{args.src_prefix} ({len(keys)} 对象) "
          f"-> {args.queue_name} dest-prefix={args.dest_prefix} ====", flush=True)

    # 把 keys 按线程切片(各线程独立子集,避免重复)
    chunks: list[list[str]] = [keys[i::args.threads] for i in range(args.threads)]
    start = time.time()
    with ThreadPoolExecutor(max_workers=args.threads) as pool:
        futs = [
            pool.submit(feeder, worker_id=w, region=args.region, queue_url=queue_url,
                        keys=chunks[w], src_bucket=args.src_bucket, dest_bucket=args.dest_bucket,
                        dest_prefix=args.dest_prefix, rounds=args.rounds)
            for w in range(args.threads) if chunks[w]
        ]
        # 进度打印
        while any(not f.done() for f in futs):
            time.sleep(10)
            with _lock:
                s = _sent[0]
            el = int(time.time() - start) or 1
            print(f"[+{el}s] sent={s} ({s/el:.0f}/s)", flush=True)
    print(f"==== 灌完 sent={_sent[0]} ====", flush=True)


if __name__ == "__main__":
    main()
