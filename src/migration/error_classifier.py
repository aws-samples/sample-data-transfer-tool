"""rclone 退出码 / stderr → 四态 + 低基数 error_class（spec §4.1 / §4.2 / §5.0）。

纯逻辑模块，无副作用：
- classify_state: 退出码 → State 四态（worker 据此决定 SQS/DDB 动作）
- classify_error: stderr → 低基数 error_class 标签（供 DDB 索引 / Athena 聚合）

注意：error_class 必须低基数——绝不回传原始高基数 stderr 文本。
"""
from __future__ import annotations

import re

from migration.models import State

# 退出码 → State 映射（rclone 官方退出码语义）。
# 未列入的码（如 -9 SIGKILL、1 未分类、137 等）一律 UNKNOWN。
#   0          → SUCCESS
#   5          → RETRYABLE（Temporary：网络/429/5xx/中断）
#   2          → FATAL（参数错误，配置 bug，重试无用）
#   3/4        → FATAL（目录/文件不存在）
#   6/7        → FATAL（NoRetry / Fatal 权限封禁）
#   8/9/10     → FATAL（max-transfer / 无文件 / max-duration，保守不重投）
_STATE_BY_EXIT_CODE: dict[int, State] = {
    0: State.SUCCESS,
    2: State.FATAL,
    3: State.FATAL,
    4: State.FATAL,
    5: State.RETRYABLE,
    6: State.FATAL,
    7: State.FATAL,
    8: State.FATAL,
    9: State.FATAL,
    10: State.FATAL,
}

# 标记"目标侧（S3/上传）"的关键词；缺省视为源侧（GCS/下载）。
_DST_MARKER = re.compile(r"\b(?:s3|putobject|upload|bucket|slowdown|dest)\b", re.IGNORECASE)


def _classify_5xx(stderr: str) -> str:
    """5xx 命中后，按是否含目标侧关键词区分 dst_5xx / src_5xx。"""
    return "dst_5xx" if _DST_MARKER.search(stderr) else "src_5xx"


# error_class 模式表（按顺序匹配，先具体后宽泛）。
# 每项：(编译后的正则, 标签或可调用 resolver)。
# 使用 IGNORECASE，正则只匹配语义关键词，不回传原始文本。
_ERROR_PATTERNS: list[tuple[re.Pattern[str], str | None]] = [
    # worker_oom：进程被信号杀死 / OOM（优先级高，避免被其它码淹没）
    (re.compile(r"\b(?:sig)?kill(?:ed)?\b|\bsignal\b|out of memory", re.IGNORECASE), "worker_oom"),
    # integrity_hash：rclone 自带 MD5 校验副产物
    (re.compile(r"hash (?:mismatch|differ)|corrupted on transfer", re.IGNORECASE), "integrity_hash"),
    # src_acl_deny：403 / permissionDenied
    (re.compile(r"\b403\b|permissiondenied|forbidden", re.IGNORECASE), "src_acl_deny"),
    # src_not_found：404 / notFound / doesn't exist（rclone copyto 源缺失 critical 消息）
    (
        re.compile(r"\b404\b|notfound|does not exist|doesn'?t exist", re.IGNORECASE),
        "src_not_found",
    ),
    # src_rate_limit：429
    (re.compile(r"\b429\b|too many requests|ratelimit", re.IGNORECASE), "src_rate_limit"),
    # 5xx：再按源/目标分流（resolver 用 None 占位，下面单独处理）
    (re.compile(r"\b5\d\d\b", re.IGNORECASE), None),
]


# delete 幂等：目标对象已不存在（404 / not found）→ 期望状态已达成，视为成功。
# rclone deletefile 删不存在对象会非零退出（3/4），此处据 stderr 识别 not-found。
_DELETE_NOOP = re.compile(r"\b404\b|not\s*found|does not exist|no such", re.IGNORECASE)

# 瞬时（可重试）错误关键词：限流 / 配额 / 5xx / 网络中断 / 连接重置。
# 根因修复：高并发耗尽源端（GCS InternetEgressBandwidth）配额时返回 429，
# rclone 重试耗尽后以退出码 1（通用错误）退出——而 1 不在 _STATE_BY_EXIT_CODE
# 映射表里 → classify_state(1)=UNKNOWN → 消息既不删也不计数、占满 12h visibility。
# 但 429/配额/5xx 本质是瞬时错误（稍后重试即可成功），应判 RETRYABLE（计数 + 自然重投 +
# maxReceiveCount 后进 DLQ），不是 UNKNOWN（崩溃/未知，不计数）。
# 这里识别 stderr 是否为瞬时错误，供 runner 把退出码 1 的 UNKNOWN 升级为 RETRYABLE。
_TRANSIENT_ERROR = re.compile(
    r"\b429\b|too many requests|rate\s*limit|ratelimit|"
    r"exceeded.{0,40}quota|quota.{0,40}exceeded|egress bandwidth|slow\s*down|"
    r"\b50[0234]\b|service unavailable|"
    r"connection reset|connection refused|timeout|temporarily",
    re.IGNORECASE,
)


# 零传输假成功：源不存在 + 目标端也无同名文件时，rclone 把单对象 copyto 退化为
# 父目录空同步 → 输出 "There was nothing to transfer" 并 exit 0。历史上被记
# SUCCESS(bytes=0) 直接删消息——永不重试、无告警、无 DLQ 留底（数据完整性盲点，
# 2026-06-10 测试栈实测确认）。runner 据此把 exit 0 + transfers==0 + 此消息的
# COPY 结果降级 FATAL。合法零传输不受影响：目录消息已被 directory_path poison
# 拦在 rclone 之前；目标已有相同文件的重跑走 "Unchanged skipping" 不是本消息。
_NOTHING_TO_TRANSFER = re.compile(r"there was nothing to transfer", re.IGNORECASE)


def is_nothing_to_transfer(stderr: str) -> bool:
    """stderr 是否含 rclone 的"无可传输"消息（源不存在的 exit-0 假成功标记）。"""
    return bool(_NOTHING_TO_TRANSFER.search(stderr or ""))


# 源对象不存在：rclone copyto 源缺失时走 cmd/cmd.go 的 generic critical 路径，
# 以退出码 1 退出（不是 3/4），消息为 "Source doesn't exist or is a directory and
# destination is a file"。1 不在 _STATE_BY_EXIT_CODE → UNKNOWN → 不删不计数、
# 空占 12h visibility × maxReceiveCount 次（36h）才进 DLQ。但源不存在是确定性
# 终态（上游清单超前于实际数据等），重试永远不会成功，应升级 FATAL（删消息 +
# 计数 + DDB 记 src_not_found）。要求 "source" 紧邻 "doesn't exist"，避免误伤
# 其它 not-found 文案（如退出码 3/4 的 directory not found 已有自己的 FATAL 路径）。
_SOURCE_MISSING = re.compile(r"source\s+(?:doesn'?t|does\s+not)\s+exist", re.IGNORECASE)


def is_source_missing(stderr: str) -> bool:
    """stderr 是否表示"源对象不存在"（确定性终态，重试无用）。

    供 runner 判定：rclone 退出码 1（通用错误）+ stderr 命中源缺失时，
    把本应归 UNKNOWN 的结果升级为 FATAL（删消息 + 计数，不再空转重投）。
    """
    return bool(_SOURCE_MISSING.search(stderr or ""))


def is_delete_noop(stderr: str) -> bool:
    """delete 操作的 stderr 是否表示"目标已不存在"（幂等成功，非真失败）。

    仅供 op=DELETE 路径用：not-found 时把状态强制为 SUCCESS，避免重投进 DLQ。
    """
    return bool(_DELETE_NOOP.search(stderr or ""))


def is_transient_error(stderr: str) -> bool:
    """stderr 是否为瞬时（可重试）错误：429 / 配额 / 5xx / 网络中断 / timeout。

    供 runner 判定：rclone 退出码 1（通用错误）+ stderr 命中瞬时错误时，
    把本应归 UNKNOWN 的结果升级为 RETRYABLE（限流类错误重试可成功，应计数）。
    """
    return bool(_TRANSIENT_ERROR.search(stderr or ""))


# 失败快速重投（2026-06-10 决策）：所有失败态不再等 12h visibility 自然过期，
# 处理完立即 change_message_visibility 丢回队列，ReceiveCount 照常累积、
# maxReceiveCount=3 后进 DLQ 留底（可 replay）。延迟按 error_class 分级：
# - 限流类必须给源端恢复窗口——0 秒连打 3 次必然 3 连败，会把"等几分钟就能
#   成功"的消息假性打进 DLQ；
# - 5xx 给短退避（后端抖动通常秒级恢复）；
# - 确定性终态（not_found/acl/arg/poison）与崩溃类重试本就无望或需人工介入，
#   0 秒立即重投快速烧满 3 次进 DLQ，不浪费 in-flight 槽位。
_RETRY_DELAY_BY_ERROR_CLASS: dict[str, int] = {
    "src_rate_limit": 300,
    "src_5xx": 60,
    "dst_5xx": 60,
}


def retry_delay_seconds(error_class: str | None) -> int:
    """失败快速重投的 VisibilityTimeout 秒数（按 error_class 分级退避）。"""
    if error_class is None:
        return 0
    return _RETRY_DELAY_BY_ERROR_CLASS.get(error_class, 0)


def classify_state(exit_code: int) -> State:
    """rclone 退出码 → State 四态。未知码归 State.UNKNOWN。"""
    return _STATE_BY_EXIT_CODE.get(exit_code, State.UNKNOWN)


def classify_error(exit_code: int, stderr: str) -> str | None:
    """stderr → 低基数 error_class 标签。

    - exit_code==0：返回 None（成功无错误）
    - exit_code==2：返回 "arg_error"（参数错误，不依赖 stderr）
    - 按 _ERROR_PATTERNS 顺序匹配 stderr，命中即返回对应标签
    - 5xx 命中再按源/目标关键词分流为 src_5xx / dst_5xx
    - 都不命中但 exit_code!=0：返回 "uncategorized"
    """
    if exit_code == 0:
        return None
    if exit_code == 2:
        return "arg_error"

    text = stderr or ""
    for pattern, label in _ERROR_PATTERNS:
        if pattern.search(text):
            return _classify_5xx(text) if label is None else label

    return "uncategorized"
