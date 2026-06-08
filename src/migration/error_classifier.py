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
    # src_not_found：404 / notFound
    (re.compile(r"\b404\b|notfound|does not exist", re.IGNORECASE), "src_not_found"),
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
