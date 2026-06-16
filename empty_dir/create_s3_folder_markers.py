#!/usr/bin/env python3
"""
为 GCS 空目录清单在 S3 侧批量创建 `_$folder$` 0 字节标记对象。

纯 pyarrow 流式读取，不依赖 duckdb。自动探测输入列名：
  - 列 `name`（原始 empty_dir_markers.parquet）：脚本内把末尾 `/` 转为 `_$folder$`。
  - 列 `key` （已转换/去重的 marker_keys.parquet）：直接使用。
put_object 幂等，重复行/重跑均无副作用，无需预先全局去重。

真实运行时逐条写明细日志（--log，默认 transfer.log）:
  时间 <TAB> 状态(OK/FAIL) <TAB> 源(GCS目录) <TAB> 目标(S3 key) [<TAB> 错误]
每个 batch 末尾 flush 并与续传 ckpt 对齐，便于事后逐条查阅。

用法:
  # 1) 干跑：打印前 1000 条 "源 <TAB> 目标" 映射，不写 S3
  python3 create_s3_folder_markers.py --bucket MYBUCKET --dry-run --limit 1000

  # 2) 小批真实创建 1000 个
  python3 create_s3_folder_markers.py --bucket MYBUCKET --limit 1000

  # 3) 全量（支持断点续传，中断后重跑同命令即可）
  python3 create_s3_folder_markers.py --bucket MYBUCKET

  # 4) 全量后抽样核对（head_object 确认存在且 0 字节）
  python3 create_s3_folder_markers.py --bucket MYBUCKET --verify 200

  # 5) 重跑失败清单
  python3 create_s3_folder_markers.py --bucket MYBUCKET --retry-failed
"""
import argparse
import os
import sys
import time
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
import pyarrow.parquet as pq

HERE = os.path.dirname(os.path.abspath(__file__))


def build_s3_client(region, profile, workers):
    session = boto3.Session(profile_name=profile) if profile else boto3.Session()
    cfg = Config(
        retries={"max_attempts": 10, "mode": "adaptive"},
        max_pool_connections=max(workers * 2, 32),
    )
    return session.client("s3", region_name=region, config=cfg)


def put_object_marker(s3, bucket, key):
    """创建单个 0 字节标记对象。成功返回 None，失败返回错误码/描述字符串。"""
    try:
        s3.put_object(Bucket=bucket, Key=key)  # 空 body = 0 字节标记，幂等
        return None
    except ClientError as e:
        return str(e.response.get("Error", {}).get("Code", e))
    except Exception as e:  # noqa: BLE001 - 兜底，单 key 失败不应中断全局
        return repr(e)


def iter_key_batches(input_path, batch_size, limit=None):
    """流式产出 (batch_index, [(src, s3_key), ...])，纯 pyarrow，内存恒定。

    src 为源路径，s3_key 为目标 S3 key。自动探测列名：
      - 列 `name`：原始 GCS 目录路径（以 `/` 结尾），目标 = `<dir>_$folder$`。
      - 列 `key` ：已是最终 S3 key，src 即等于该 key。
    limit 仅取前 N 个。
    """
    pf = pq.ParquetFile(input_path)
    cols = pf.schema_arrow.names
    if "name" in cols:
        col, transform = "name", True
    elif "key" in cols:
        col, transform = "key", False
    else:
        raise SystemExit(f"输入 parquet 既无 'name' 也无 'key' 列，实际列: {cols}")
    produced = 0
    for idx, rb in enumerate(pf.iter_batches(batch_size=batch_size, columns=[col])):
        vals = rb.column(col).to_pylist()
        if transform:
            # 末尾 "/" -> "_$folder$"；防御性处理极少数不以 / 结尾的行
            pairs = [
                (v, (v[:-1] if v.endswith("/") else v) + "_$folder$") for v in vals
            ]
        else:
            pairs = [(v, v) for v in vals]
        if limit is not None:
            remaining = limit - produced
            if remaining <= 0:
                return
            if len(pairs) > remaining:
                pairs = pairs[:remaining]
        produced += len(pairs)
        yield idx, pairs
        if limit is not None and produced >= limit:
            return


def read_ckpt(path):
    try:
        with open(path) as f:
            return int(f.read().strip())
    except (FileNotFoundError, ValueError):
        return -1


def write_ckpt(path, batch_idx):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(str(batch_idx))
    os.replace(tmp, path)  # 原子替换，避免半写


def put_markers(args):
    s3 = build_s3_client(args.region, args.profile, args.workers)
    bucket = args.bucket

    done_batch = read_ckpt(args.ckpt)
    if done_batch >= 0:
        print(
            f"[续传] 检测到进度 ckpt，已完成 batch <= {done_batch}，将跳过。",
            flush=True,
        )

    failed_f = open(args.failed, "a", buffering=1)  # 行缓冲，随时落盘
    # 逐条明细日志，格式: 时间 <TAB> 状态(OK/FAIL) <TAB> 源 <TAB> 目标 [<TAB> 错误]
    log_f = open(args.log, "a")  # 块缓冲，每个 batch 末尾 flush，与 ckpt 对齐

    ok_total = 0
    fail_total = 0
    started = time.time()

    def put_one(src, key):
        return (src, key, put_object_marker(s3, bucket, key))

    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        for idx, pairs in iter_key_batches(args.input, args.batch_size, args.limit):
            if idx <= done_batch:
                continue
            futures = [pool.submit(put_one, src, key) for src, key in pairs]
            for fut in as_completed(futures):
                src, key, err = fut.result()
                ts = time.strftime("%Y-%m-%d %H:%M:%S")
                if err is None:
                    ok_total += 1
                    log_f.write(f"{ts}\tOK\t{src}\t{key}\n")
                else:
                    fail_total += 1
                    log_f.write(f"{ts}\tFAIL\t{src}\t{key}\t{err}\n")
                    failed_f.write(key + "\n")
            log_f.flush()  # batch 边界落盘，与 ckpt 对齐（崩溃最多丢当前 batch，续传幂等重做）
            write_ckpt(args.ckpt, idx)  # 该 batch 完成（失败已落 failed_keys）
            rate = (ok_total + fail_total) / max(time.time() - started, 1e-6)
            print(
                f"[batch {idx}] 累计 成功={ok_total:,} 失败={fail_total:,} "
                f"速率={rate:,.0f}/s",
                flush=True,
            )

    failed_f.close()
    log_f.close()
    elapsed = time.time() - started
    print("=" * 60, flush=True)
    print(
        f"完成。成功={ok_total:,} 失败={fail_total:,} 耗时={elapsed:,.1f}s", flush=True
    )
    print(f"逐条明细日志: {args.log}", flush=True)
    if fail_total:
        print(f"失败 key 已写入: {args.failed}（可用 --retry-failed 重跑）", flush=True)
    return fail_total


def dry_run(args):
    n = 0
    print(
        "--- dry-run（未写 S3）。每行：源(GCS目录) <TAB> 目标(S3 key) ---",
        file=sys.stderr,
    )
    for _, pairs in iter_key_batches(args.input, args.batch_size, args.limit):
        for src, key in pairs:
            print(f"{src}\t{key}")
            n += 1
    print(f"--- dry-run: 共 {n:,} 条映射（未写 S3）---", file=sys.stderr)


def retry_failed(args):
    """读取 failed_keys.txt 去重后重跑，成功的从失败列表移除。"""
    if not os.path.exists(args.failed):
        print(f"无失败文件 {args.failed}，无需重跑。")
        return 0
    with open(args.failed) as f:
        keys = sorted({line.rstrip("\n") for line in f if line.strip()})
    if not keys:
        print("失败文件为空，无需重跑。")
        return 0
    print(f"[retry] 重跑 {len(keys):,} 个失败 key …", flush=True)
    s3 = build_s3_client(args.region, args.profile, args.workers)
    still_failed = []
    lock = threading.Lock()

    def put_one(key):
        err = put_object_marker(s3, args.bucket, key)
        if err is not None:
            with lock:
                still_failed.append(key)
        return err

    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        list(pool.map(put_one, keys))

    with open(args.failed, "w") as f:
        for k in still_failed:
            f.write(k + "\n")
    print(f"[retry] 完成。仍失败={len(still_failed):,}", flush=True)
    return len(still_failed)


def verify(args):
    """抽样 head_object 核对存在性与 0 字节。"""
    s3 = build_s3_client(args.region, args.profile, args.workers)
    sample = []
    for _, pairs in iter_key_batches(args.input, args.batch_size, None):
        sample.extend(key for _, key in pairs)
        if len(sample) >= args.verify * 50:  # 取足够池子再均匀抽样
            break
    if not sample:
        print("输入为空。")
        return 1
    step = max(len(sample) // args.verify, 1)
    picks = sample[::step][: args.verify]
    ok = bad = missing = 0
    for k in picks:
        try:
            r = s3.head_object(Bucket=args.bucket, Key=k)
            if r["ContentLength"] == 0:
                ok += 1
            else:
                bad += 1
                print(f"[非0字节 {r['ContentLength']}] {k}")
        except ClientError as e:
            if e.response["Error"]["Code"] in ("404", "NoSuchKey"):
                missing += 1
                print(f"[缺失] {k}")
            else:
                raise
    print("=" * 60)
    print(f"核对 {len(picks)} 个: 0字节存在={ok} 非0字节={bad} 缺失={missing}")
    return 0 if (bad == 0 and missing == 0) else 1


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--bucket", required=True, help="目标 S3 bucket 名")
    ap.add_argument(
        "--input",
        default=os.path.join(HERE, "empty_dir_markers.parquet"),
        help="输入 parquet。默认原始 empty_dir_markers.parquet（列 name，脚本内转换）；"
        "也可传已去重的 marker_keys.parquet（列 key，省去重复 put）。",
    )
    ap.add_argument("--workers", type=int, default=128, help="并发线程数（默认 128）")
    ap.add_argument(
        "--batch-size",
        type=int,
        default=10000,
        help="每批 key 数（默认 1 万，决定续传粒度）",
    )
    ap.add_argument(
        "--region",
        default=os.environ.get("AWS_REGION"),
        help="AWS region（默认读 AWS_REGION）",
    )
    ap.add_argument("--profile", default=None, help="AWS profile（默认用默认凭证链）")
    ap.add_argument(
        "--ckpt", default=os.path.join(HERE, "progress.ckpt"), help="断点续传进度文件"
    )
    ap.add_argument(
        "--failed", default=os.path.join(HERE, "failed_keys.txt"), help="失败 key 记录"
    )
    ap.add_argument(
        "--log",
        default=os.path.join(HERE, "transfer.log"),
        help="逐条明细日志（时间<TAB>状态<TAB>源<TAB>目标[<TAB>错误]），追加写入",
    )
    ap.add_argument(
        "--limit", type=int, default=None, help="仅处理前 N 个 key（小批验证用）"
    )
    ap.add_argument(
        "--dry-run", action="store_true", help="只打印 源<TAB>目标 映射，不写 S3"
    )
    ap.add_argument(
        "--verify",
        type=int,
        default=0,
        metavar="N",
        help="抽样 N 个 key 做 head_object 核对",
    )
    ap.add_argument(
        "--retry-failed", action="store_true", help="重跑 failed_keys.txt 中的 key"
    )
    args = ap.parse_args()

    if not os.path.exists(args.input) and not args.retry_failed:
        sys.exit(f"输入文件不存在: {args.input}（请先运行阶段一 DuckDB 生成）")

    if args.dry_run:
        dry_run(args)
    elif args.verify:
        sys.exit(verify(args))
    elif args.retry_failed:
        sys.exit(1 if retry_failed(args) else 0)
    else:
        sys.exit(1 if put_markers(args) else 0)


if __name__ == "__main__":
    main()
