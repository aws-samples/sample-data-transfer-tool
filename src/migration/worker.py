"""SQS-consuming transfer worker (spec §3.1 执行层 / §4.1 四态).

This tool only *consumes* SQS — another team fills the queue. The worker:
  1. receives a message ({source, destination, rclone_args})
  2. validates the path (defensive: messages come from outside)
  3. runs rclone copyto (size-routed large/small command template)
  4. four-state handling: SUCCESS/RETRYABLE/FATAL/UNKNOWN
  5. records terminal state to DDB (OLTP) + reports monitoring (EMF + Firehose)

The single-message core (``process_message``) is pure and fully injectable so
it can be tested without real AWS or rclone. The runtime layer
(``WorkerLoop``) wires the real clients and a thread pool.
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import signal
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime, timezone

from . import config, logging_setup, monitoring_reporter, rclone_runner, status_store, watchdog
from .config import LARGE_FILE_THRESHOLD, Settings
from .models import RunResult, State, TransferMessage
from .path_safety import validate_object_path

logger = logging.getLogger(__name__)


# ───────────────────────────── pure helpers ────────────────────────────────
def is_large_object(object_size: int) -> bool:
    """对象 >= 阈值走大文件队列/命令模板。"""
    return object_size >= LARGE_FILE_THRESHOLD


def _object_key(remote_path: str) -> str:
    """从 rclone 路径 ``remote:bucket/key`` 取**对象 key**（桶名之后）用于安全校验。

    too_long 上限（1024 字节）是 GCS/S3 对**对象 key**的限制（不含桶名），且目标端
    key 已含目标前缀 → 这里取到的就是"目标完整对象 key"，与落点字节数一致。
    无 key 段（只有 ``remote:bucket``）时返回空串（validate 判 empty，本就是无效消息）。
    """
    path = remote_path.split(":", 1)[1] if ":" in remote_path else remote_path
    return path.split("/", 1)[1] if "/" in path else ""


def parse_message_body(body: str) -> TransferMessage:
    """解析并校验 SQS 消息体。

    消息来自外部（上游灌数据方），必须在边界校验：
    - JSON 合法、含 source/destination
    - source/destination 的 path 部分通过 path_safety（防 \\0/CRLF/注入）

    校验失败抛 ValueError（调用方按 poison 处理）。
    """
    try:
        data = json.loads(body)
    except (json.JSONDecodeError, TypeError) as exc:
        raise ValueError(f"invalid json: {exc}") from exc

    if not isinstance(data, dict):
        raise ValueError("message body must be a JSON object")

    # op 缺省 = copy；非法值在 TransferMessage.from_body 里 Op(...) 抛 ValueError。
    op = data.get("op", "copy")
    # copy 需 source+destination；delete 只删目标端，仅需 destination（source 可空）。
    required = ("destination",) if op == "delete" else ("source", "destination")
    for field in required:
        if field not in data:
            raise ValueError(f"missing required field: {field}")

    # 只校验实际存在的端点路径（delete 无 source 时不校验 source）。
    for field in ("source", "destination"):
        if field in data and data[field]:
            ok, reason = validate_object_path(_object_key(data[field]))
            if not ok:
                raise ValueError(f"unsafe {field}: {reason}")

    return TransferMessage.from_body(data)


# ─────────────────────────── single-message core ───────────────────────────
@dataclass(frozen=True)
class ProcessOutcome:
    """process_message 的结果：传输状态 + 是否计入统计。"""

    state: State
    counted: bool


def _utc_now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3]


def _split_date_hour(now_iso: str) -> tuple[str, str]:
    """从 ISO 时间戳切出 (date, hour) 给 Firehose 分区。

    now_iso 形如 "2026-05-29T14:30:00.123" → ("2026-05-29", "14")。
    格式不符合预期时回退空串（Firehose 分区会落到默认值，不至于抛错丢消息）。
    """
    if len(now_iso) >= 13 and now_iso[10] == "T":
        return now_iso[:10], now_iso[11:13]
    return "", ""


def process_message(
    *,
    body: str,
    object_size: int,
    instance_id: str,
    config_path: str,
    region: str,
    status_table: str,
    now_iso: str,
    run_fn: Callable[..., RunResult],
    delete_fn: Callable[[], None],
    record_fn: Callable[..., None],
    report_fn: Callable[..., None],
) -> ProcessOutcome:
    """处理单条消息，返回四态结果。所有副作用经注入函数，便于测试。

    四态语义（spec §4.1）：
      SUCCESS   → 删消息 + 记终态 + 上报 + 计数
      RETRYABLE → 不删（靠 visibility timeout 重投）+ 记 FAILED + 上报 + 计数
      FATAL     → 不删（记 FAILED + 上报 + 计数）；靠自然重投 3 次后 SQS 转 DLQ
      UNKNOWN   → 不删 + 不计数（崩溃/SIGKILL/未预期）

    删除原则：**只有 SUCCESS 删消息**。其余处理不了的（RETRYABLE/FATAL/poison/
    UNKNOWN）一律保留，靠 12h visibility 自然超时重投，maxReceiveCount=3 后由 SQS
    转入 DLQ —— 绝不直接删除，保证"处理不了的消息最终都在 DLQ 留底、可 replay"。
    """
    queue_type = "large" if is_large_object(object_size) else "small"

    # 边界解析：脏消息（无法解析/不安全）按 poison 处理。
    # 先落一条 FATAL 终态（用 body 摘要作可追溯 source），**不删消息**——
    # 靠 12h visibility 自然重投，maxReceiveCount=3 后 SQS 自动转入 DLQ。
    # 原则：处理不了的消息绝不直接删除，最终都落 DLQ 以便排查/replay（消息不丢）。
    try:
        msg = parse_message_body(body)
    except ValueError as exc:
        poison_source = f"poison:{hashlib.md5(body.encode('utf-8', 'replace')).hexdigest()}"  # noqa: S324
        logger.error("poison message: %s (source=%s) -> 留队列待 DLQ", exc, poison_source)
        poison_result = RunResult(
            state=State.FATAL,
            exit_code=-1,
            error_class="poison_message",
            error_message=str(exc),
        )
        try:
            record_fn(
                source=poison_source,
                attempt_timestamp=now_iso,
                result=poison_result,
                instance_id=instance_id,
            )
        except Exception:  # noqa: BLE001 - 记录失败不影响"不删、走 DLQ"
            logger.exception("failed to record poison terminal")
        return ProcessOutcome(state=State.FATAL, counted=False)

    result = run_fn(msg, config_path, is_large_object(object_size))

    # rclone 出错（非 SUCCESS）→ 把原始命令 + stderr 打 ERROR 日志，经 worker.log
    # → CloudWatch worker-ops 落地，无需登录每台机看 journalctl 即可定位失败根因。
    # SUCCESS 不打（百万级成功会写满日志组）；stderr 可能很长，截断到 4KB 避免单条日志过大。
    if result.state is not State.SUCCESS:
        stderr_text = (result.error_message or "")[:4096]
        logger.error(
            "rclone 失败 state=%s exit=%s error_class=%s source=%s\ncmd: %s\nstderr: %s",
            result.state.value, result.exit_code, result.error_class or "none",
            msg.source, result.cmd_str, stderr_text,
        )

    # 终态记录（UNKNOWN 也记，便于排查；但不计数、不删消息）。
    record_fn(
        source=msg.source,
        attempt_timestamp=now_iso,
        result=result,
        instance_id=instance_id,
    )

    # 监控上报（event 含 source 进明细层；EMF 维度只用低基数字段）。
    # date/hour 供 Firehose 动态分区（Athena 按小时聚合的前提）；event_time 留行级时间。
    # now_iso 形如 "2026-05-29T14:30:00.123"：[:10]=date, [11:13]=hour。
    partition_date, partition_hour = _split_date_hour(now_iso)
    report_fn(
        {
            "source": msg.source,
            "destination": msg.destination,
            "queue_type": queue_type,
            "op": msg.op.value,
            "error_class": result.error_class or "none",
            "instance_id": instance_id,
            "bytes": result.stats.bytes,
            "elapsed": result.stats.elapsed_seconds,
            "speed": result.stats.speed,
            "state": result.state.value,
            "event_time": now_iso,
            "date": partition_date,
            "hour": partition_hour,
        }
    )

    if result.state is State.SUCCESS:
        delete_fn()
        return ProcessOutcome(state=State.SUCCESS, counted=True)
    if result.state is State.FATAL:
        # 不删：靠自然重投 3 次后 SQS 转 DLQ（处理不了的消息留底、可 replay，不直接丢）。
        return ProcessOutcome(state=State.FATAL, counted=True)
    if result.state is State.RETRYABLE:
        return ProcessOutcome(state=State.RETRYABLE, counted=True)  # 不删，自然重投
    # UNKNOWN：不删、不计数。
    return ProcessOutcome(state=State.UNKNOWN, counted=False)


# ───────────────────────────── runtime layer ───────────────────────────────
class WorkerLoop:
    """多线程 SQS 消费循环，串联 runner / status_store / monitoring。

    并发模型：一个 ThreadPoolExecutor（settings.worker_threads 个 worker 线程）
    真正并行处理消息。每轮 receive 一批（≤10）提交线程池，并控制"在途任务数"
    不超过线程数，使 SQS in-flight ≈ 全 fleet 并发数。

    in-flight 约束：单机线程数受 settings.worker_threads 限制
    （320 台 × 256 = 82k < SQS in-flight 120k/队列上限，spec §6.0）。
    """

    def __init__(self, settings: Settings, queue_url: str, instance_id: str, *, sqs):
        self.settings = settings
        self.queue_url = queue_url
        self.instance_id = instance_id
        self.sqs = sqs
        self.running = True
        # 诊断细分计数：四态各自独立 + delete 调用成败。failed 保留为
        # retryable+fatal 之和（向后兼容旧引用），但分别细分便于定位 in-flight 暴涨根因。
        self.stats = {
            "success": 0,
            "retryable": 0,
            "fatal": 0,
            "failed": 0,
            "unknown": 0,
            "delete_ok": 0,
            "delete_fail": 0,
            "swallowed": 0,
            "total": 0,
        }
        self._lock = threading.Lock()
        # H1: 在途 receipt 集合。SIGTERM 优雅退出时对这些消息立即重置
        # VisibilityTimeout=0，让它们马上重投而非卡满 12h。
        self._inflight: set[str] = set()
        self._inflight_lock = threading.Lock()
        # Watchdog：每个 consumer 每轮 receive（含空响应）bump 一次 progress。
        # watchdog 线程据"progress 是否在涨 + consumer 是否存活"向 systemd 上报健康——
        # 真僵死（所有线程卡住、progress 不涨）则停止上报 → systemd 超时重启进程。
        self._progress = 0
        self._progress_lock = threading.Lock()
        self._alive_consumers = 0  # 当前存活的 consumer 线程数（进 loop +1，退出 -1）
        # 动态限速：控制面下发的当前单进程 bwlimit（字节/秒纯数字或 "off"）+
        # 手动 SSM 下发的 tpslimit（次/秒纯数字或 "off"）。_ratelimit_loop 周期从
        # SSM 刷新两者，_handle_one 起新 rclone 时一并快照注入。同一把锁保护两值的
        # 跨线程读写；默认 "off" 即未读到时不限（fail-safe）。
        self._bwlimit = "off"
        self._tpslimit = "off"
        self._bwlimit_lock = threading.Lock()

    def _record(self, *, source, attempt_timestamp, result, instance_id):
        client = status_store_client(self.settings.aws_region)
        status_store.record_terminal(
            client,
            self.settings.dynamodb_table,
            source,
            attempt_timestamp,
            result,
            instance_id,
            now_iso=attempt_timestamp,
        )

    def _report(self, event):
        monitoring_reporter.report(event)

    def _handle_one(self, msg: dict) -> None:
        receipt = msg["ReceiptHandle"]
        size = int(
            msg.get("MessageAttributes", {})
            .get("object_size", {})
            .get("StringValue", 0)
            or 0
        )
        now_iso = _utc_now_iso()
        # H1: 处理前登记在途，finally 移除——shutdown 据此对未完成消息重置
        # VisibilityTimeout=0；已删/已终态的消息出 finally 即移除，不会被误重置。
        with self._inflight_lock:
            self._inflight.add(receipt)
        # 起新 rclone 进程时快照当前限速值（仅新进程生效；在跑进程保持原速）。
        with self._bwlimit_lock:
            bwlimit = self._bwlimit
            tpslimit = self._tpslimit
        try:
            outcome = process_message(
                body=msg["Body"],
                object_size=size,
                instance_id=self.instance_id,
                config_path=self.settings.rclone_config_path,
                region=self.settings.aws_region,
                status_table=self.settings.dynamodb_table,
                now_iso=now_iso,
                run_fn=lambda m, cfg, large: rclone_runner.run(
                    m, cfg, large, bwlimit=bwlimit, tpslimit=tpslimit
                ),
                delete_fn=lambda: self._delete_message(receipt),
                record_fn=self._record,
                report_fn=self._report,
            )
            self._tally(outcome)
        finally:
            with self._inflight_lock:
                self._inflight.discard(receipt)

    def _delete_message(self, receipt: str) -> None:
        """删消息并计数成败。

        诊断关键：若 SUCCESS/FATAL 调到这里但 SQS delete 抛异常（节流/网络），
        消息不会被删 → 占满 12h VisibilityTimeout，形成无法回收的 in-flight 残留。这里把
        delete 失败单独计数 + WARNING，区分"逻辑没走到 delete"与"delete 调用失败"。
        异常继续上抛（保持原契约：process_message 的 delete_fn 失败由上层 _safe_handle 处理）。
        """
        try:
            self.sqs.delete_message(QueueUrl=self.queue_url, ReceiptHandle=receipt)
        except Exception:
            with self._lock:
                self.stats["delete_fail"] += 1
            logger.warning("delete_message 失败（消息将占用 visibility timeout 直到超时后重投）", exc_info=True)
            raise
        with self._lock:
            self.stats["delete_ok"] += 1

    def _tally(self, outcome: ProcessOutcome) -> None:
        with self._lock:
            if not outcome.counted:
                self.stats["unknown"] += 1
                return
            self.stats["total"] += 1
            if outcome.state is State.SUCCESS:
                self.stats["success"] += 1
            elif outcome.state is State.RETRYABLE:
                self.stats["retryable"] += 1
                self.stats["failed"] += 1
            else:  # FATAL
                self.stats["fatal"] += 1
                self.stats["failed"] += 1

    def poll_once(self) -> int:
        """拉取一批消息（≤10）并同步处理，返回处理条数。

        单次驱动，主要供测试。生产路径用 run_forever → 多个 _consume_loop
        竞争消费者并发拉取（每线程各自 receive，无单点分发瓶颈）。
        """
        messages = self._receive_batch(max_messages=10)
        for m in messages:
            self._safe_handle(m)
        return len(messages)

    def _receive_batch(self, max_messages: int) -> list[dict]:
        """从 SQS 拉一批消息。

        不覆盖 VisibilityTimeout —— 使用队列自身配置（large-queue=12h 支持大
        文件长传）。硬编码会让 >timeout 的传输中途被重投 → 重复传输 + 提前进 DLQ。
        """
        resp = self.sqs.receive_message(
            QueueUrl=self.queue_url,
            MaxNumberOfMessages=max_messages,
            WaitTimeSeconds=20,
            MessageAttributeNames=["All"],
        )
        return resp.get("Messages", [])

    def _safe_handle(self, msg: dict) -> None:
        """处理单条消息；任何异常不拖垮调用线程（消息靠 visibility timeout 重投）。"""
        try:
            self._handle_one(msg)
        except Exception:  # noqa: BLE001
            # 诊断：吞掉的异常多半是 record_fn(DDB put 节流) 在 delete 之前抛出——
            # 传输可能已成功但消息没删 → 占满 12h visibility，形成无法回收的 in-flight 残留。单独计数。
            with self._lock:
                self.stats["swallowed"] = self.stats.get("swallowed", 0) + 1
            logger.exception("unhandled error processing message; leaving for retry")

    def _bump_progress(self) -> None:
        """progress +1。每个 consumer 每轮 receive（含空响应）调一次，
        作为 watchdog "有进展"判定依据——空队列 long-poll 返回也算进展
        （证明线程活着且 SQS 可达），不会误杀空闲机器。"""
        with self._progress_lock:
            self._progress += 1

    def get_progress(self) -> int:
        with self._progress_lock:
            return self._progress

    def _consume_loop(self) -> None:
        """竞争消费者线程：自己 receive(1) + 处理，循环到 running=False。

        N 个该 loop 并行跑（= worker_threads），各自独立拉取——receive 吞吐随
        线程数线性扩展，无 MainThread 单点分发瓶颈；in-flight 天然 = 正在处理
        的线程数 ≤ worker_threads。
        """
        with self._lock:
            self._alive_consumers += 1
        try:
            while self.running:
                try:
                    messages = self._receive_batch(max_messages=1)
                    # 每轮都 bump：拿到消息或空响应都算"活着在干活"（watchdog 用）。
                    self._bump_progress()
                    if not messages:
                        continue  # long-poll 已含等待，空轮询直接继续
                    self._safe_handle(messages[0])
                except Exception:  # noqa: BLE001 - receive 出错也不退出线程
                    logger.exception("consume loop error; backing off")
                    time.sleep(5)
        finally:
            with self._lock:
                self._alive_consumers -= 1

    def _watchdog_loop(self, *, interval: float, min_consumers: int) -> None:
        """向 systemd 上报健康：仅当"consumer 存活 + progress 有进展"时发 WATCHDOG=1。

        真僵死（所有 consumer 卡住、progress 不涨）→ 停止上报 → systemd WatchdogSec
        超时 → 杀进程重启（在途消息靠 SQS VisibilityTimeout 自然重投，不丢）。
        非 systemd 环境（无 NOTIFY_SOCKET）notify 静默返回 False，loop 空转无害。
        """
        last_progress = self.get_progress()
        while self.running:
            time.sleep(interval)
            with self._lock:
                alive = self._alive_consumers
            progress = self.get_progress()
            if watchdog.is_healthy(
                alive_consumers=alive,
                min_consumers=min_consumers,
                progress=progress,
                last_progress=last_progress,
            ):
                watchdog.notify("WATCHDOG=1")
            else:
                logger.error(
                    "watchdog 停止上报健康：alive=%d/%d progress=%d(上次%d)——进程疑似停滞，等待 systemd 重启",
                    alive, min_consumers, progress, last_progress,
                )
            last_progress = progress

    def run_forever(self) -> None:
        """生产主循环：起 worker_threads 个竞争消费者 + 后台心跳 + watchdog，跑到 running=False。"""
        hb = threading.Thread(target=self._heartbeat_loop, daemon=True, name="heartbeat")
        hb.start()
        rl = threading.Thread(target=self._ratelimit_loop, daemon=True, name="ratelimit")
        rl.start()
        consumers = [
            threading.Thread(target=self._consume_loop, daemon=True, name=f"consumer-{i}")
            for i in range(self.settings.worker_threads)
        ]
        for c in consumers:
            c.start()
        logger.info("started %d competing consumers", len(consumers))

        # systemd 就绪信号 + 启动 watchdog 健康上报线程（仅当配了 NOTIFY_SOCKET 才真生效）。
        watchdog.notify("READY=1")
        wd_interval, wd_min = self._watchdog_params(len(consumers))
        wd = threading.Thread(
            target=self._watchdog_loop,
            kwargs={"interval": wd_interval, "min_consumers": wd_min},
            daemon=True,
            name="watchdog",
        )
        wd.start()

        # 主线程等待停止信号；consumer/heartbeat/watchdog 各自循环检查 running。
        while self.running:
            time.sleep(1)
        for c in consumers:
            c.join(timeout=30)

    def _watchdog_params(self, consumer_count: int) -> tuple[float, int]:
        """从 systemd 注入的 WATCHDOG_USEC 推算心跳上报间隔（取 1/2，留充足余量），
        无则默认 60s。min_consumers 要求至少一半 consumer 存活（留容错余量，
        避免个别线程偶发退出就误判整机僵死）。"""
        usec = os.environ.get("WATCHDOG_USEC")
        interval = (int(usec) / 1_000_000 / 2) if usec and usec.isdigit() else 60.0
        min_consumers = max(1, consumer_count // 2)
        return interval, min_consumers

    def shutdown(self) -> None:
        """优雅退出（SIGTERM/SIGINT 调用）：停止拉取 + 在途消息立即重投。

        H1：daemon consumer 被直接杀死会让在途消息既没删也没退回，卡满
        VisibilityTimeout（12h）才重投——一次滚动更新数万消息全卡。这里：
          1. running=False 让 consumer 处理完当前条后停拉新消息；
          2. 对所有在途 receipt 批量 change_message_visibility(VisibilityTimeout=0)，
             让 SQS 立即重投给其它存活 worker，而非等 12h。
        batch API（10条/批）失败只 log、不阻断退出——必须保证进程能停。
        幂等：清空 _inflight 后再次调用不重复发 batch。
        """
        self.running = False
        with self._inflight_lock:
            receipts = list(self._inflight)
            self._inflight.clear()
        if not receipts:
            return
        logger.info("shutdown: 重置 %d 条在途消息 VisibilityTimeout=0 立即重投", len(receipts))
        self._reset_visibility(receipts)

    def _reset_visibility(self, receipts: list[str]) -> None:
        """对一批 receipt 分块（≤10）调 change_message_visibility_batch=0。

        SQS batch API 上限 10 条/次；任一批失败只 log，继续处理其余批，
        不抛异常——退出路径不能被卡住。
        """
        for start in range(0, len(receipts), 10):
            chunk = receipts[start : start + 10]
            entries = [
                {"Id": str(i), "ReceiptHandle": r, "VisibilityTimeout": 0}
                for i, r in enumerate(chunk)
            ]
            try:
                self.sqs.change_message_visibility_batch(
                    QueueUrl=self.queue_url, Entries=entries
                )
            except Exception:  # noqa: BLE001 - 退出路径，失败不阻断
                logger.exception("shutdown: 重置可见性失败（这批 %d 条将等 timeout 重投）", len(chunk))

    def _heartbeat_loop(self, *, interval: float = 60.0) -> None:
        """后台心跳：每 interval 秒写一次 heartbeat（存活判定 + active-workers 统计依据）。"""
        while self.running:
            try:
                client = status_store_client(self.settings.aws_region)
                with self._lock:
                    active = self.stats["total"]
                status_store.write_heartbeat(
                    client,
                    self.settings.heartbeat_table,
                    self.instance_id,
                    now_iso=_utc_now_iso(),
                    now_epoch=_utc_now_epoch(),
                    active_threads=self.settings.worker_threads,
                )
                logger.debug("heartbeat written (processed so far: %d)", active)
                self._log_diag()
            except Exception:  # noqa: BLE001 - 心跳失败不影响传输
                logger.exception("heartbeat write failed")
            time.sleep(interval)

    def _log_diag(self) -> None:
        """每个心跳周期打印本进程诊断快照（INFO → worker.log → CloudWatch）。

        判决 in-flight 暴涨根因的核心证据：
          - local_inflight：本进程当前真正持有（已 receive 未结案）的消息数。
            竞争消费者模型下应恒 ≤ worker_threads；若全 fleet 各进程的 local_inflight
            之和 << SQS 报的 NotVisible，则 SQS 数是估算失真/孤儿，不是 worker 多领。
          - retryable/fatal/unknown：非 SUCCESS 的消息按设计不删（retryable/unknown）
            或删（fatal）。retryable/unknown 偏高 → 12h visibility 累积无法回收的 in-flight 残留。
          - delete_fail：SUCCESS/FATAL 走到 delete 但 SQS 调用失败 → 也会占满 12h visibility。
        """
        with self._inflight_lock:
            local_inflight = len(self._inflight)
        with self._lock:
            s = dict(self.stats)
        logger.info(
            "DIAG inst=%s local_inflight=%d threads=%d | "
            "total=%d success=%d retryable=%d fatal=%d unknown=%d | "
            "delete_ok=%d delete_fail=%d swallowed=%d",
            self.instance_id, local_inflight, self.settings.worker_threads,
            s["total"], s["success"], s["retryable"], s["fatal"], s["unknown"],
            s["delete_ok"], s["delete_fail"], s["swallowed"],
        )

    def _ratelimit_loop(self, *, interval: float = 30.0) -> None:
        """后台限速刷新：每 interval 秒从 SSM 读 bwlimit + tpslimit，更新本地快照。

        worker 端极简——bwlimit 由控制器 Lambda（AIMD）写、worker 只读；tpslimit
        由运维手动写 SSM、worker 同周期读（30s < 1min，改 SSM 后约半分钟内被各
        节点取到新值）。读失败 read_ssm_limit 已 fail-safe 返回 "off"（不限），不卡传输。
        仅影响之后新起的 rclone 进程；在跑进程保持原速（层次 1）。
        """
        from . import aws_clients, ratelimit

        while self.running:
            try:
                client = aws_clients.get_ssm_client(self.settings.aws_region)
                bw = ratelimit.read_bwlimit(
                    client, self.settings.ratelimit_bwlimit_param
                )
                tps = ratelimit.read_ssm_limit(
                    client, self.settings.ratelimit_tpslimit_param
                )
                with self._bwlimit_lock:
                    bw_changed = bw != self._bwlimit
                    tps_changed = tps != self._tpslimit
                    self._bwlimit = bw
                    self._tpslimit = tps
                if bw_changed:
                    logger.info("bwlimit updated from control plane: %s", bw)
                if tps_changed:
                    logger.info("tpslimit updated from SSM: %s", tps)
            except Exception:  # noqa: BLE001 - 限速刷新失败不影响传输
                logger.exception("ratelimit refresh failed")
            time.sleep(interval)


def status_store_client(region: str):
    """间接层：方便测试 monkeypatch DDB client。"""
    from .aws_clients import get_dynamodb_client

    return get_dynamodb_client(region)


def _utc_now_epoch() -> int:
    return int(datetime.now(timezone.utc).timestamp())


def _ec2_instance_id() -> str:
    """取 EC2 实例 ID（IMDSv2）；失败回退 hostname。"""
    try:  # pragma: no cover - 依赖 EC2 元数据，单测不覆盖
        import urllib.request

        token_req = urllib.request.Request(
            "http://169.254.169.254/latest/api/token", method="PUT"
        )
        token_req.add_header("X-aws-ec2-metadata-token-ttl-seconds", "21600")
        token = urllib.request.urlopen(token_req, timeout=2).read().decode("utf-8")
        id_req = urllib.request.Request(
            "http://169.254.169.254/latest/meta-data/instance-id"
        )
        id_req.add_header("X-aws-ec2-metadata-token", token)
        return urllib.request.urlopen(id_req, timeout=2).read().decode("utf-8")
    except Exception:  # noqa: BLE001
        import socket

        return socket.gethostname()


def resolve_instance_id(env: dict[str, str] | None = None) -> str:
    """worker 实例标识 = EC2 instance-id，多进程时追加 #<WORKER_INDEX> 后缀。

    B1 多进程模型：同一 EC2 上 systemd 起 N 个 worker 进程绕开 GIL。心跳表 PK 是
    instance_id，N 个进程必须各自唯一，否则互相覆盖同一条心跳、且限速控制器据
    heartbeat 统计的总线程数会少算 N 倍。systemd 模板单元用 %i 注入 WORKER_INDEX，
    单进程部署不设此变量则退化为纯 instance-id（向后兼容）。
    """
    e = os.environ if env is None else env
    base = _ec2_instance_id()
    idx = e.get("WORKER_INDEX", "")
    return f"{base}#{idx}" if idx else base


def _install_signal_handlers(loop: WorkerLoop) -> None:
    """注册 SIGTERM/SIGINT → loop.shutdown()（H1 优雅退出）。

    systemd stop / ASG 实例刷新 / spot 回收都发 SIGTERM；Ctrl-C 发 SIGINT。
    handler 仅置位 + 重置在途可见性，不做重活——配合 systemd TimeoutStopSec
    给足收尾窗口。
    """
    def _handler(signum, _frame):
        logger.info("收到信号 %s，开始优雅退出", signum)
        loop.shutdown()

    signal.signal(signal.SIGTERM, _handler)
    signal.signal(signal.SIGINT, _handler)


def main(argv: list[str] | None = None) -> int:  # pragma: no cover - 进程入口
    """worker 进程入口（systemd ExecStart 调用）。

    消费单一迁移队列（大小文件统一入队）；起一个 WorkerLoop 跑到底。
    """
    # 运维日志走独立轮转文件 /var/log/migration/worker.log（不碰 stdout——stdout
    # 专供 EMF JSON 给 CW Agent）；WARNING+ 另转 stderr→journald。见 logging_setup。
    logging_setup.setup_logging(level=logging.INFO)
    settings = Settings.from_env()
    # C1: 启动即 fail-fast 校验 rclone timeout 相对 VisibilityTimeout 有安全裕量，
    # 否则长传超时被重投会导致两台 worker 双写同一对象。配置危险则拒绝启动。
    config.validate_timeout_invariant(
        timeout=config.RCLONE_TIMEOUT_SECONDS,
        visibility=settings.queue_visibility_timeout,
    )
    queue_url = settings.queue_url
    instance_id = resolve_instance_id()

    from .aws_clients import get_sqs_client, prime_credentials

    # 起消费线程前先把 IAM 凭证取到手：16 进程同时启动会令 IMDS 过载，约 15% 进程
    # 首次取凭证失败并缓存失败态 → 永久 NoCredentialsError 零产出。预热失败则退出，
    # 由 systemd Restart=always 错峰重启避开同时抢（退避也把启动摊开）。
    if not prime_credentials():
        logger.error("IAM 凭证预热失败，退出（systemd 将重启本进程）")
        return 1

    sqs = get_sqs_client(settings.aws_region)
    logger.info(
        "starting worker: queue=%s instance=%s threads=%d",
        queue_url, instance_id, settings.worker_threads,
    )
    loop = WorkerLoop(settings, queue_url, instance_id, sqs=sqs)
    _install_signal_handlers(loop)
    loop.run_forever()
    return 0


if __name__ == "__main__":  # pragma: no cover
    import sys

    sys.exit(main())
