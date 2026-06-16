"""queue-mover —— 跨集群 SQS 消息搬运工具（A 集群队列 → B 集群队列）。

用途：多套集群间重新分配负载（如 A 集群排空检修、把堆积导给空闲的 B 集群）。
每套集群一条队列，搬运 = receive(A) → send_batch(B) → delete_batch(A)。

不丢消息契约（与 migration_cli.cmd_replay 同源强化）：
  - 先发后删：只有 send 确认成功（不在 Failed 列表）的消息才从 A 删除；
  - send_message_batch 部分失败 → 失败条不删，留在 A 靠 visibility 自愈重投；
  - 进程中途崩溃 → 已 receive 未删的消息在 MOVER_VISIBILITY_SECONDS 后回 A 队列。
    语义是 at-least-once（极端时序下 B 可能收到重复），worker 端 DDB 按 attempt
    记行、S3 copyto 幂等，重复无害。
  - MessageAttributes 透传（object_size 等路由属性），过滤 AWS. 保留前缀。

用法：
  python3 queue_mover.py \\
      --src-queue https://sqs.<region>.amazonaws.com/<acct>/migration-a-queue \\
      --dst-queue https://sqs.<region>.amazonaws.com/<acct>/migration-b-queue \\
      [--max 100000] [--threads 8] [--region eu-south-2] [--dry-run]

  跨账号/跨 region：--src-region/--dst-region 分开指定（默认都取 --region /
  AWS_REGION）。凭证用当前环境的 profile/实例角色，需同时有 A 队列的
  ReceiveMessage/DeleteMessage 与 B 队列的 SendMessage 权限。
"""
from __future__ import annotations

import argparse
import logging
import os
import sys
import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor

logger = logging.getLogger(__name__)

# receive 时显式覆盖 visibility：搬运是秒级操作，不能继承队列自身的 12h 配置——
# 否则 mover 崩溃后已 receive 的消息被锁 12h 才回源队列。5 分钟足够一批的处理余量。
MOVER_VISIBILITY_SECONDS = 300

# SQS 单批上限（receive 与 batch send/delete 共用）。
_BATCH = 10


def _forwardable_attributes(message: dict) -> dict | None:
    """提取可透传的 MessageAttributes（过滤 AWS. 保留前缀；空则 None）。"""
    attrs = message.get("MessageAttributes")
    if not attrs:
        return None
    forwardable = {k: v for k, v in attrs.items() if not k.startswith("AWS.")}
    return forwardable or None


def _move_one_batch(sqs, *, src_url: str, dst_url: str, want: int, dry_run: bool) -> int:
    """搬一批（≤10 条）：receive → send_batch → 按 send 结果选择性 delete_batch。

    返回成功搬运条数；源队列空返回 0（调用方据此停止）。
    """
    resp = sqs.receive_message(
        QueueUrl=src_url,
        MaxNumberOfMessages=min(want, _BATCH),
        WaitTimeSeconds=1,
        MessageAttributeNames=["All"],
        VisibilityTimeout=MOVER_VISIBILITY_SECONDS,
    )
    messages = resp.get("Messages", [])
    if not messages:
        return 0
    if dry_run:
        for m in messages:
            logger.info("[dry-run] 将搬运: %s", m["Body"][:200])
        return len(messages)

    entries = []
    receipt_by_id: dict[str, str] = {}
    for m in messages:
        mid = m["MessageId"]
        entry: dict = {"Id": mid, "MessageBody": m["Body"]}
        attrs = _forwardable_attributes(m)
        if attrs is not None:
            entry["MessageAttributes"] = attrs
        entries.append(entry)
        receipt_by_id[mid] = m["ReceiptHandle"]

    send_resp = sqs.send_message_batch(QueueUrl=dst_url, Entries=entries)

    # 只删 send 成功的；Failed 条留在源队列（visibility 过期自动重投，不丢）。
    ok_ids = [s["Id"] for s in send_resp.get("Successful", [])]
    for f in send_resp.get("Failed", []):
        logger.warning("send 失败留源队列: id=%s code=%s", f.get("Id"), f.get("Code"))
    if ok_ids:
        sqs.delete_message_batch(
            QueueUrl=src_url,
            Entries=[
                {"Id": mid, "ReceiptHandle": receipt_by_id[mid]} for mid in ok_ids
            ],
        )
    return len(ok_ids)


def move_messages(
    sqs,
    *,
    src_url: str,
    dst_url: str,
    max_messages: int,
    threads: int = 1,
    dry_run: bool = False,
    progress_every: int = 1000,
    on_progress: Callable[[int], None] | None = None,
) -> int:
    """从 src 队列搬最多 max_messages 条消息到 dst 队列，返回实际搬运条数。

    threads>1 时多线程并发搬批（每线程独立 receive，竞争消费天然不重复——
    visibility 保证同一消息不会被两个线程同时拿到）。
    progress_every>0 时每累计搬满该条数触发一次 on_progress(moved 累计值)，
    结束时再触发一次收尾（用于落地进度日志）。progress_every=0 关闭进度回调。
    """
    moved = 0
    claimed = 0  # 已被线程预留的配额（receive 前先占，防多线程同时通过检查超搬）
    last_report = 0  # 上次触发进度回调时的 moved 值
    lock = threading.Lock()

    def _worker() -> None:
        nonlocal moved, claimed, last_report
        while True:
            # 先在锁内预留本轮配额，receive 实际拿到 n ≤ 预留数，差额退回。
            with lock:
                quota = min(_BATCH, max_messages - claimed)
                if quota <= 0:
                    return
                claimed += quota
            n = _move_one_batch(
                sqs, src_url=src_url, dst_url=dst_url, want=quota, dry_run=dry_run
            )
            fire = None
            with lock:
                claimed -= quota - n  # 没用完的配额退回
                moved += n
                if progress_every and on_progress and moved - last_report >= progress_every:
                    last_report = moved
                    fire = moved
            if fire is not None:
                on_progress(fire)  # 锁外回调，不阻塞其它线程
            if n == 0:
                return  # 源队列空（long-poll 1s 后仍无消息）

    if threads <= 1:
        _worker()
    else:
        with ThreadPoolExecutor(max_workers=threads) as pool:
            futures = [pool.submit(_worker) for _ in range(threads)]
            for f in futures:
                f.result()  # 显式收割：线程内异常上抛，不静默
    # 收尾：若有未报告的进度（含 moved==0 的情况按需），补报最终值
    if progress_every and on_progress and moved != last_report:
        on_progress(moved)
    return moved


def _build_sqs_client(region: str, threads: int):
    """构建 SQS client（连接池按线程数适配 + adaptive retry）。抽出便于测试注入。"""
    import boto3
    from botocore.config import Config

    return boto3.client(
        "sqs",
        region_name=region,
        config=Config(
            retries={"max_attempts": 10, "mode": "adaptive"},
            max_pool_connections=max(10, threads + 4),
        ),
    )


def _setup_logging(log_file: str | None) -> None:
    """配置日志：始终输出到控制台（stderr），--log-file 时同时落本地盘。

    用 handlers 而非 basicConfig 的单一目标，保证盘+台双写；append 模式不覆盖历史。
    """
    handlers: list[logging.Handler] = [logging.StreamHandler(sys.stderr)]
    if log_file:
        handlers.append(logging.FileHandler(log_file, encoding="utf-8"))
    fmt = logging.Formatter("%(asctime)s %(levelname)s %(message)s")
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    for h in list(root.handlers):  # 清掉已有 handler，避免重复行（多进程/重复调用）
        root.removeHandler(h)
    for h in handlers:
        h.setFormatter(fmt)
        root.addHandler(h)


# ──────────────────────────── CLI ───────────────────────────────────────────
def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="queue-mover", description="跨集群 SQS 消息搬运（A 队列 → B 队列）"
    )
    parser.add_argument("--src-queue", required=True, help="源队列 URL（A 集群）")
    parser.add_argument("--dst-queue", required=True, help="目标队列 URL（B 集群）")
    parser.add_argument("--max", type=int, default=100_000, help="最多搬运条数")
    parser.add_argument("--threads", type=int, default=8, help="每进程并发线程数")
    parser.add_argument(
        "--procs", type=int, default=1,
        help="并发进程数（绕 Python GIL；总并发=procs×threads）。CPU 撞满 1 核时调大",
    )
    parser.add_argument(
        "--region", default=os.environ.get("AWS_REGION", ""), help="AWS region"
    )
    parser.add_argument(
        "--log-file", default=None, help="搬运日志落地路径（同时输出到控制台）"
    )
    parser.add_argument(
        "--progress-every", type=int, default=5000, help="每搬运 N 条打一条进度日志"
    )
    parser.add_argument(
        "--dry-run", action="store_true", help="只 receive 预览，不发送不删除"
    )
    return parser


def _run_single(args, *, max_messages: int) -> int:
    """单进程执行（含进度日志），返回搬运条数。被 main 直接调或被子进程入口调。"""
    sqs = _build_sqs_client(args.region, args.threads)

    def _on_progress(moved: int) -> None:
        logger.info("进度: 已搬运 %d 条", moved)

    return move_messages(
        sqs,
        src_url=args.src_queue,
        dst_url=args.dst_queue,
        max_messages=max_messages,
        threads=args.threads,
        dry_run=args.dry_run,
        progress_every=args.progress_every,
        on_progress=_on_progress,
    )


def _proc_entry(payload: dict) -> int:
    """子进程入口（multiprocessing 要求顶层可 pickle 的函数）。"""
    import argparse as _ap

    _setup_logging(payload["log_file"])
    args = _ap.Namespace(**payload["args"])
    return _run_single(args, max_messages=payload["share"])


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    _setup_logging(args.log_file)

    if args.src_queue == args.dst_queue:
        print("源队列与目标队列不能相同", file=sys.stderr)
        return 2
    if not args.region:
        print("缺 region：--region 或 AWS_REGION", file=sys.stderr)
        return 2
    if args.procs < 1 or args.threads < 1:
        print("--procs / --threads 必须 >= 1", file=sys.stderr)
        return 2

    action = "[dry-run] 可搬运" if args.dry_run else "已搬运"

    if args.procs == 1:
        n = _run_single(args, max_messages=args.max)
        logger.info("%s %d 条消息", action, n)
        return 0

    # 多进程：max 在进程间平分（余数给前几个进程），每进程独立 client/线程池，
    # 各自竞争消费同一源队列（visibility 保证不重复）。绕开单进程 GIL 瓶颈。
    import multiprocessing as mp

    base, extra = divmod(args.max, args.procs)
    shares = [base + (1 if i < extra else 0) for i in range(args.procs)]
    arg_dict = {k: v for k, v in vars(args).items()}
    payloads = [
        {"args": arg_dict, "share": s, "log_file": args.log_file}
        for s in shares if s > 0
    ]
    logger.info("启动 %d 个进程 × %d 线程，总配额 %d", len(payloads), args.threads, args.max)
    with mp.Pool(processes=len(payloads)) as pool:
        results = pool.map(_proc_entry, payloads)
    total = sum(results)
    logger.info("%s %d 条消息（%d 进程合计）", action, total, len(payloads))
    return 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
