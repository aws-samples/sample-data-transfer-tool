"""构建并执行单次 rclone copyto 命令，解析其 JSON 日志为结构化结果。

设计要点（spec §4.4 / §5.0 / 附录 A）：
- 绝不使用 shell=True，所有参数以 argv list 形式传给 subprocess。
- 来自 SQS 消息的 rclone_args 经白名单过滤（防命令注入）。
- 通过函数参数注入 classify_state / classify_error，使本模块不硬依赖
  并行开发中的 error_classifier；默认 None 时才延迟 import。
"""
from __future__ import annotations

import json
import logging
import os
import shlex
import signal
import subprocess
from collections.abc import Callable

from . import config
from .models import Op, RunResult, State, TransferMessage, TransferStats

logger = logging.getLogger(__name__)

# rclone copyto value 里不允许出现的控制字符（防注入换行/空字节）。
_FORBIDDEN_VALUE_CHARS = ("\x00", "\n", "\r")

# ── flag 模板（提升 build_cmd 可读性；仍以 argv list 拼接，绝不拼成字符串）──
#
# 为什么坚持 list 而非"拼成一个字符串"：
#   字符串方案必须 shell=True，而 source 来自外部 SQS（不可信），对象名含
#   `; rm -rf /`、`$(...)`、空格、引号都会被 shell 解释/执行 → 命令注入 +
#   参数被空格拆断。list + execve 让每个元素就是一个 argv，shell 元字符是
#   字面值，零执行、零转义负担。可读性通过下面的命名模板解决，安全不让步。

# 上传分片：统一交给 rclone 按对象「真实大小」自动决定，不依赖消息预知的 object_size。
# 根因修复：worker 从 SQS 消息读的 object_size 缺失时默认 0 → is_large_object(0)=False →
# 大文件被当小文件、不走 multipart（实测 265MB 文件全走单 PUT，带宽上不去）。
# --s3-upload-cutoff=100M 让 rclone 对 >100MB 的对象自动多分片并发上传、≤100MB 走单 PUT，
# 完全不需要 worker 预知大小，零额外探测调用。chunk-size/concurrency 仅对触发 multipart 的
# 大对象生效，小对象无影响。
_UPLOAD_FLAGS = [
    "--s3-upload-cutoff", "100M",
    "--s3-chunk-size", "64M",
    "--s3-upload-concurrency", "8",
    "--buffer-size", "32M",
    "--max-buffer-memory", "2G",
    # CRC32C 整对象校验和：所有上传默认带，rclone 让 S3 用 CRC32C(FULL_OBJECT)算并
    # 存进目标对象的原生 ChecksumCRC32C 字段（非 ETag/metadata，需 --checksum-mode
    # ENABLED 才读得回）。两段式 --header-upload 写法已端到端验证（x86_64/arm64 均生效）。
    # 需 rclone 支持 CRC32C（自编译版 RcloneS3Key 已含）；官方旧版可能忽略该 header。
    "--header-upload", "x-amz-checksum-algorithm: CRC32C",
    "--header-upload", "x-amz-checksum-type: FULL_OBJECT",
]

# 两版共有：S3 调优 + 编码一致 + 重试 + 结构化日志（不含 --config，运行时注入路径）。
_COMMON_FLAGS = [
    "--s3-no-check-bucket",
    "--s3-disable-checksum",
    "--s3-no-head",
    # --metadata：把源对象的 system + user metadata 照搬到目标（默认关闭，必须显式开）。
    # GCS 源暴露 content-type/cache-control/content-disposition/content-encoding/
    # content-language/mtime + 用户自定义 X-Goog-Meta-*；rclone 把它们写入 S3 对应字段，
    # 用户自定义自动映射到 x-amz-meta-*。content-type 即便不加本 flag 也默认搬运，其余字段
    # 全靠它。metadata 在上传时随 PutObject/multipart header 写入，不受 --s3-no-head 影响
    # （后者只跳过上传后 HEAD 回读校验，不阻止写入；但会导致 rclone 读不回 mtime，仅影响
    # 后续增量同步的 mtime 比对，一次性 copyto 迁移无碍）。
    "--metadata",
    "--s3-encoding", "Slash,InvalidUtf8",
    "--use-mmap",
    "--retries", "3",
    "--low-level-retries", "10",
    "--no-traverse",
    # H3 修复：原 "--stats 0" 是"关闭周期 stats 输出"（非每 0 秒），导致
    # 单文件 copyto 全程无 stats 行 → parse_json_log 走 fallback 只拿 bytes，
    # speed/elapsed 恒为 0。改为有限周期，让 rclone 周期输出含 elapsedTime/
    # speed 的 stats 行，parse_json_log 主分支可解析。30s 对长传开销可忽略。
    "--stats", "30s",
    "--stats-log-level", "NOTICE",
    "--use-json-log",
    "--log-level", "INFO",
]


def sanitize_rclone_args(args: list[str]) -> list[str]:
    """对来自 SQS 消息的 rclone_args 做白名单过滤。

    - 仅保留 ALLOWED_RCLONE_FLAGS 中的 flag；
    - 属于 RCLONE_FLAGS_WITH_VALUE 的 flag 消费下一个 token 作 value，
      并校验 value 不含 \\0/\\n/\\r、不以 -- 开头，否则连同 flag 丢弃；
    - 非白名单 flag、非 str 元素静默跳过（记 warning）。
    """
    out: list[str] = []
    i = 0
    n = len(args)
    while i < n:
        token = args[i]
        i += 1
        if not isinstance(token, str):
            logger.warning("跳过非字符串 rclone_arg 元素: %r", token)
            continue
        if token not in config.ALLOWED_RCLONE_FLAGS:
            logger.warning("丢弃非白名单 rclone flag: %s", token)
            continue
        if token in config.RCLONE_FLAGS_WITH_VALUE:
            if i >= n:
                logger.warning("白名单 flag 缺少 value，丢弃: %s", token)
                continue
            value = args[i]
            i += 1
            if not _is_safe_value(value):
                logger.warning("flag %s 的 value 不安全，丢弃", token)
                continue
            out.append(token)
            out.append(value)
        else:
            out.append(token)
    return out


def _is_safe_value(value: object) -> bool:
    """校验 flag value：必须是 str、不含控制字符、不以 -- 开头。"""
    if not isinstance(value, str):
        return False
    if any(ch in value for ch in _FORBIDDEN_VALUE_CHARS):
        return False
    if value.startswith("--"):
        return False
    return True


def _validate_endpoint(value: str, label: str) -> None:
    """校验 source/destination：不含 \\0，且具备合法 backend 前缀。"""
    if "\x00" in value:
        raise ValueError(f"{label} 含空字节: {value!r}")
    colon = value.find(":")
    if colon <= 0:
        raise ValueError(f"{label} 缺少 backend 前缀（remote:path）: {value!r}")
    prefix = value[:colon]
    if not prefix.isalnum():
        raise ValueError(f"{label} 的 backend 前缀非字母数字: {value!r}")


def _bwlimit_flags(bwlimit: str) -> list[str]:
    """限速 flag：off/空 → 不拼（rclone 默认不限速）；否则 --bwlimit <值>。

    bwlimit 由控制面下发（字节/秒纯数字或 "off"），不来自不可信消息，但仍只
    接受 ratelimit.format_bwlimit 产出的两种形态，避免误拼非法值。
    """
    if not bwlimit or bwlimit == "off":
        return []
    return ["--bwlimit", bwlimit]


def _tpslimit_flags(tpslimit: str) -> list[str]:
    """请求频率限速 flag：off/空 → 不拼；否则 --tpslimit <值>（次/秒）。

    与 --bwlimit 正交：bwlimit 限字节/秒（防 egress 带宽配额 429），tpslimit 限
    transaction/秒（防后端 QPS rate-limit，如 GCS 读 5000/s、S3 PUT 503）。小文件
    高并发场景才需要。值由 SSM 手动下发（纯整数次/秒或 "off"），不来自不可信消息。
    单进程值——全机群总 TPS = tpslimit × 进程数 × 实例数，下发前自行除算。
    """
    if not tpslimit or tpslimit == "off":
        return []
    return ["--tpslimit", tpslimit]


def _build_delete_cmd(msg: TransferMessage, config_path: str) -> list[str]:
    """构建 rclone deletefile 的 argv list（删目标端单个对象）。

    deletefile 只删单个文件（非 delete/purge 递归），单条消息爆炸半径=1 个对象。
    不拼 size/bwlimit/上传调优 flag（删除不传输）；保留重试 + 结构化日志。
    """
    _validate_endpoint(msg.destination, "destination")
    return [
        config.RCLONE_BIN, "deletefile",
        "--config", config_path,
        "--retries", "3",
        "--low-level-retries", "10",
        "--use-json-log",
        "--log-level", "INFO",
        # 经白名单过滤的消息自定义参数（与 copy 一致，防注入）。
        *sanitize_rclone_args(list(msg.rclone_args)),
        "--", msg.destination,
    ]


def build_cmd(
    msg: TransferMessage,
    config_path: str,
    is_large: bool,
    *,
    bwlimit: str = "off",
    tpslimit: str = "off",
) -> list[str]:
    """构建 rclone argv list（flags 在前，positional 在 -- 之后）。

    op=DELETE  → rclone deletefile destination（删目标端单对象，不传输）。
    op=COPY    → rclone copyto source destination（默认）。
    op=REFRESH → rclone copyto source destination --ignore-times（强制重传整对象，
                 刷新源端 metadata-only 变更；复用 copy 的全部 flag 集）。
    bwlimit: 单进程带宽限速（字节/秒纯数字或 "off"=不限速），由 AIMD 控制面下发。
    tpslimit: 单进程请求频率限速（次/秒纯数字或 "off"=不限），由 SSM 手动下发。
    """
    if msg.op is Op.DELETE:
        return _build_delete_cmd(msg, config_path)

    _validate_endpoint(msg.source, "source")
    _validate_endpoint(msg.destination, "destination")

    # is_large 仅用于 EMF QueueType 维度（吞吐按大小拆分上报）；分片不再依赖它，
    # 统一用 _UPLOAD_FLAGS 让 rclone 按真实大小自动 multipart（见 _UPLOAD_FLAGS 注释）。
    _ = is_large

    cmd: list[str] = [
        config.RCLONE_BIN, "copyto",
        "--config", config_path,
        *_UPLOAD_FLAGS,
        *_COMMON_FLAGS,
        # op=refresh：忽略 size/mtime 强制重传，使源端 metadata-only 变更刷到目标
        # （rclone -I/--ignore-times = "transfer all unconditionally"）。仅 REFRESH 拼。
        *(["--ignore-times"] if msg.op is Op.REFRESH else []),
        # 动态限速：仅当控制面下发有限值时才拼（默认 off 不拼）。
        *_bwlimit_flags(bwlimit),
        # 请求频率限速：手动 SSM 下发有限值时才拼（默认 off 不拼）。
        *_tpslimit_flags(tpslimit),
        # 经白名单过滤的消息自定义参数。
        *sanitize_rclone_args(list(msg.rclone_args)),
        # -- 终结符后接 positional，防止 path 以 - 开头被当作 flag。
        "--", msg.source, msg.destination,
    ]
    return cmd


def parse_json_log(stderr: str) -> TransferStats:
    """从 rclone --use-json-log 的 NDJSON 输出解析传输统计。

    优先取含 ``stats`` 字段的行（core/stats，多文件/周期输出时有）。
    单文件 copyto + ``--stats 0`` 场景 rclone **不输出 stats 行**，只有一条
    ``{"msg":"Copied ...","size":N}``——此时 fallback 累加各 "Copied" 行的
    ``size``，保证单文件传输也能记到准确字节数。
    容忍非 JSON 行、空行、含特殊字符的行；全失败返回空 TransferStats。
    """
    last_stats: dict | None = None
    copied_bytes = 0
    copied_count = 0
    for line in stderr.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        if not isinstance(obj, dict):
            continue
        stats = obj.get("stats")
        if isinstance(stats, dict):
            last_stats = stats
            continue
        # fallback：单文件传输完成行带 size 字段。注意大文件是
        # "Multi-thread Copied ..."，小文件是 "Copied ..."，故用 "Copied" in msg。
        msg = obj.get("msg", "")
        if isinstance(msg, str) and "Copied" in msg and "size" in obj:
            try:
                copied_bytes += int(obj["size"])
                copied_count += 1
            except (TypeError, ValueError):
                pass

    if last_stats is not None:
        return TransferStats(
            bytes=int(last_stats.get("bytes", 0) or 0),
            elapsed_seconds=float(last_stats.get("elapsedTime", 0.0) or 0.0),
            speed=float(last_stats.get("speed", 0.0) or 0.0),
            errors=int(last_stats.get("errors", 0) or 0),
            transfers=int(last_stats.get("transfers", 0) or 0),
        )

    # 无 stats 行：用 Copied 行累加（单文件 copyto 的常态）。
    return TransferStats(bytes=copied_bytes, transfers=copied_count)


# ── H2: 进程组感知的默认 runner ──────────────────────────────────────────────
#
# 为什么不直接用 subprocess.run(timeout=)：
#   subprocess.run 超时只 kill 主进程（rclone 本体）；rclone 的 multipart
#   分片上传子操作（独立子进程）不一定随之收尾，会变孤儿继续写 S3——与
#   VisibilityTimeout 重投后的新 worker 双写同一对象。SIGTERM 杀 Python 时同理。
# 解法：start_new_session=True 让 rclone 自成进程组（pgid=pid）；超时/异常时
#   os.killpg 对整个进程组发 SIGKILL，确保所有分片子进程一并终止。
def _default_runner(
    cmd: list[str],
    *,
    capture_output: bool = True,
    check: bool = False,  # noqa: ARG001 - 与 subprocess.run 签名对齐；本函数永不 raise on returncode
    timeout: float | None = None,
    popen_factory: Callable[..., object] = subprocess.Popen,
) -> subprocess.CompletedProcess:
    """以独立进程组启动 rclone，超时杀整组，返回 CompletedProcess（兼容契约）。

    popen_factory 可注入用于测试（默认 subprocess.Popen）。
    超时时先 killpg 整个进程组再重抛 TimeoutExpired，交由 run() 归类 UNKNOWN。
    """
    stdout_dst = subprocess.PIPE if capture_output else None
    stderr_dst = subprocess.PIPE if capture_output else None
    # start_new_session=True → setsid → 子进程成为新 session/进程组组长，
    # 其 fork 出的分片子进程都落在同一进程组，便于整组 kill。
    proc = popen_factory(
        cmd,
        stdout=stdout_dst,
        stderr=stderr_dst,
        start_new_session=True,
    )
    try:
        stdout, stderr = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill_process_group(proc)
        # 收尾读取已缓冲输出，避免管道残留；不再设超时（组已被 SIGKILL）。
        try:
            proc.communicate()
        except Exception:  # noqa: BLE001 - 收尾尽力而为，不掩盖原始超时
            logger.warning("超时收尾 communicate 失败（进程组已被 kill）")
        raise
    return subprocess.CompletedProcess(
        args=cmd,
        returncode=proc.returncode,
        stdout=stdout or b"",
        stderr=stderr or b"",
    )


def _kill_process_group(proc) -> None:
    """对子进程所在进程组发 SIGKILL，杀掉 rclone 及其全部分片子进程。

    取不到 pgid（进程已退出）或权限问题只 log，不掩盖调用方要重抛的超时。
    """
    try:
        pgid = os.getpgid(proc.pid)
        os.killpg(pgid, signal.SIGKILL)
        logger.warning("rclone 超时，已 SIGKILL 进程组 pgid=%s", pgid)
    except (ProcessLookupError, PermissionError, OSError) as exc:
        logger.warning("kill 进程组失败（可能已退出）: %s", exc)


def _resolve_classifiers(
    classify_state: Callable[[int], State] | None,
    classify_error: Callable[[int, str], str | None] | None,
) -> tuple[Callable[[int], State], Callable[[int, str], str | None]]:
    """默认 None 时延迟 import error_classifier（避免硬依赖未完成模块）。"""
    if classify_state is not None and classify_error is not None:
        return classify_state, classify_error
    from . import error_classifier  # 延迟导入

    return (
        classify_state or error_classifier.classify_state,
        classify_error or error_classifier.classify_error,
    )


def run(
    msg: TransferMessage,
    config_path: str,
    is_large: bool,
    *,
    bwlimit: str = "off",
    tpslimit: str = "off",
    runner: Callable[..., object] = _default_runner,
    classify_state: Callable[[int], State] | None = None,
    classify_error: Callable[[int, str], str | None] | None = None,
) -> RunResult:
    """执行单次 rclone copyto，返回结构化 RunResult。

    bwlimit: 单进程带宽限速（字节/秒纯数字或 "off"），由 worker 从控制面读取后传入。
    tpslimit: 单进程请求频率限速（次/秒纯数字或 "off"），由 worker 从 SSM 读取后传入。
    runner 可注入用于测试；超时/系统错误归类为 UNKNOWN（不可计数）。
    """
    try:
        cmd = build_cmd(msg, config_path, is_large, bwlimit=bwlimit, tpslimit=tpslimit)
    except (ValueError, OSError) as exc:
        # build_cmd 校验失败（如缺前缀、空字节）。
        return RunResult(
            state=State.UNKNOWN,
            exit_code=-1,
            error_class="subprocess_error",
            error_message=str(exc),
            cmd_str="",
        )

    cmd_str = shlex.join(cmd)

    try:
        completed = runner(
            cmd,
            capture_output=True,
            check=False,
            timeout=config.RCLONE_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as exc:
        return RunResult(
            state=State.UNKNOWN,
            exit_code=-1,
            error_class="rclone_timeout",
            error_message=str(exc),
            cmd_str=cmd_str,
        )
    except (OSError, ValueError) as exc:
        return RunResult(
            state=State.UNKNOWN,
            exit_code=-1,
            error_class="subprocess_error",
            error_message=str(exc),
            cmd_str=cmd_str,
        )

    returncode = completed.returncode  # type: ignore[attr-defined]
    stderr_bytes = completed.stderr or b""  # type: ignore[attr-defined]
    stderr = stderr_bytes.decode("utf-8", errors="backslashreplace")

    state_fn, error_fn = _resolve_classifiers(classify_state, classify_error)
    state = state_fn(returncode)
    error_class = error_fn(returncode, stderr)
    stats = parse_json_log(stderr)

    # 根因修复：源端触发配额限制（GCS 429 / egress bandwidth）时 rclone 以退出码 1 退出，
    # classify_state(1)=UNKNOWN → 既不删也不计数、占满 12h visibility（实测 20 台时
    # in-flight 涨到数万的真因）。但 429/5xx/网络中断本质是瞬时错误，应判 RETRYABLE
    # （计数 + 自然重投 + maxReceiveCount 后进 DLQ）。仅在 UNKNOWN 且 stderr 命中瞬时
    # 错误时升级；真正的崩溃/SIGKILL（stderr 不含这些关键词）仍保持 UNKNOWN。
    from . import error_classifier

    if state is State.UNKNOWN:
        if error_classifier.is_source_missing(stderr):
            # 源不存在（copyto 走 generic critical 路径退出码 1）= 确定性终态，
            # 重试永远不会成功 → FATAL（删消息 + 计数），不空占 visibility 重投。
            state = State.FATAL
        elif error_classifier.is_transient_error(stderr):
            state = State.RETRYABLE

    # 零传输假成功降级（2026-06-10）：源不存在 + 目标端也无同名文件时 rclone 把
    # 单对象 copyto 退化为父目录空同步 → "There was nothing to transfer" + exit 0。
    # 真实传输 transfers>=1，空同步 transfers==0 → 降级 FATAL + src_not_found
    # （DDB 记 body，走快速重投烧进 DLQ 留底），不再静默记 SUCCESS 删消息。
    if (
        state is State.SUCCESS
        and msg.op in (Op.COPY, Op.REFRESH)
        and stats.transfers == 0
        and error_classifier.is_nothing_to_transfer(stderr)
    ):
        state = State.FATAL
        error_class = "src_not_found"

    # delete 幂等：目标已不存在（404/not found）→ 期望状态已达成，强制 SUCCESS，
    # 避免对象不存在被当 FATAL 反复重投进 DLQ。仅 op=DELETE 路径生效。
    if msg.op is Op.DELETE and returncode != 0:
        if error_classifier.is_delete_noop(stderr):
            state = State.SUCCESS
            error_class = None

    return RunResult(
        state=state,
        exit_code=returncode,
        stats=stats,
        error_class=error_class,
        error_message=stderr or None,
        cmd_str=cmd_str,
    )
