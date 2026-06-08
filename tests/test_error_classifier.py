"""错误分类器单元测试（spec §4.1 / §4.2 / §5.0）。

先写测试（RED），覆盖：
- classify_state 全部退出码分支（含未知码归 UNKNOWN）
- classify_error 每个 error_class 模式 + uncategorized 兜底
- exit_code==0 → None
- stderr 含特殊字符/Unicode/空串不崩
"""
from __future__ import annotations

import pytest

from migration.error_classifier import (
    classify_error,
    classify_state,
    is_delete_noop,
    is_transient_error,
)
from migration.models import State


# ---------------------------------------------------------------------------
# is_delete_noop：删除幂等（目标已不存在 = 成功）
# ---------------------------------------------------------------------------
@pytest.mark.unit
@pytest.mark.parametrize(
    "stderr",
    [
        "directory not found",
        "object not found",
        "error 404 NotFound",
        "Failed to deletefile: not found",
    ],
)
def test_is_delete_noop_true_for_not_found(stderr: str) -> None:
    # 删一个已不存在的对象：期望状态已达成 → 视为 noop（幂等成功）
    assert is_delete_noop(stderr) is True


@pytest.mark.unit
@pytest.mark.parametrize(
    "stderr",
    [
        "403 permissionDenied",
        "429 too many requests",
        "500 internal error",
        "",
    ],
)
def test_is_delete_noop_false_for_other_errors(stderr: str) -> None:
    # 权限/限流/5xx 不是 not-found，不算幂等成功
    assert is_delete_noop(stderr) is False


# ---------------------------------------------------------------------------
# is_transient_error：瞬时（可重试）错误识别（根因修复：429 退出码 1 → RETRYABLE）
# ---------------------------------------------------------------------------
@pytest.mark.unit
@pytest.mark.parametrize(
    "stderr",
    [
        'got HTTP response code 429 with body: This workload is drawing too much '
        "egress bandwidth from Cloud Storage and has exceeded the InternetEgressBandwidth quota",
        "429 Too Many Requests",
        "rate limit exceeded",
        "ratelimit hit",
        "googleapi: quota exceeded for egress bandwidth",
        "503 Service Unavailable",
        "500 internal error",
        "502 Bad Gateway",
        "connection reset by peer",
        "connection refused",
        "i/o timeout",
        "SlowDown: please reduce your request rate",
    ],
)
def test_is_transient_error_true(stderr: str) -> None:
    # 限流/配额/5xx/网络中断 = 瞬时错误，重试可成功 → 应升级为 RETRYABLE
    assert is_transient_error(stderr) is True


@pytest.mark.unit
@pytest.mark.parametrize(
    "stderr",
    [
        "",
        "signal: killed",
        "out of memory",
        "403 permissionDenied",
        "404 not found",
        "directory not found",
        "no such file or directory",
    ],
)
def test_is_transient_error_false(stderr: str) -> None:
    # 真正的崩溃/权限/404 不是瞬时错误，不应被误升级为 RETRYABLE（保持原态）
    assert is_transient_error(stderr) is False


# ---------------------------------------------------------------------------
# classify_state：退出码 → 四态
# ---------------------------------------------------------------------------
@pytest.mark.unit
@pytest.mark.parametrize(
    ("exit_code", "expected"),
    [
        (0, State.SUCCESS),
        # FATAL 组：目录/文件不存在、NoRetry、Fatal、参数错误、保守不重投的 8/9/10
        (2, State.FATAL),
        (3, State.FATAL),
        (4, State.FATAL),
        (6, State.FATAL),
        (7, State.FATAL),
        (8, State.FATAL),
        (9, State.FATAL),
        (10, State.FATAL),
        # RETRYABLE 组：仅 5（Temporary）
        (5, State.RETRYABLE),
    ],
)
def test_classify_state_known_codes(exit_code: int, expected: State) -> None:
    assert classify_state(exit_code) is expected


@pytest.mark.unit
@pytest.mark.parametrize("exit_code", [-9, 1, 11, 137, 255, 100, -1])
def test_classify_state_unknown_codes(exit_code: int) -> None:
    """未列入映射的退出码一律归 UNKNOWN（含 SIGKILL/未分类/异常大码）。"""
    assert classify_state(exit_code) is State.UNKNOWN


@pytest.mark.unit
def test_classify_state_returns_state_enum_member() -> None:
    """返回值必须是共享契约里的 State 枚举成员，而非裸字符串。"""
    result = classify_state(5)
    assert isinstance(result, State)


# ---------------------------------------------------------------------------
# classify_error：stderr → 低基数 error_class
# ---------------------------------------------------------------------------
@pytest.mark.unit
def test_classify_error_success_returns_none() -> None:
    """exit_code==0 直接返回 None，不看 stderr。"""
    assert classify_error(0, "anything in stderr should be ignored") is None
    assert classify_error(0, "") is None


@pytest.mark.unit
@pytest.mark.parametrize(
    ("stderr", "expected"),
    [
        # src_acl_deny：403 / permissionDenied
        ("Failed to copy: googleapi: Error 403: permissionDenied", "src_acl_deny"),
        ("HTTP error 403 Forbidden on source", "src_acl_deny"),
        ("error: permissionDenied while reading object", "src_acl_deny"),
        # src_not_found：404 / notFound
        ("googleapi: Error 404: notFound", "src_not_found"),
        ("HTTP 404 object does not exist", "src_not_found"),
        # src_rate_limit：429
        ("googleapi: Error 429: rateLimitExceeded", "src_rate_limit"),
        ("Too Many Requests (429) from source", "src_rate_limit"),
        # integrity_hash：hash mismatch
        ("corrupted on transfer: MD5 hash differ", "integrity_hash"),
        ("ERROR: hash mismatch detected after transfer", "integrity_hash"),
        # worker_oom：signal / killed / out of memory
        ("Killed", "worker_oom"),
        ("process received signal: SIGKILL", "worker_oom"),
        ("fork: Cannot allocate memory: out of memory", "worker_oom"),
    ],
)
def test_classify_error_patterns(stderr: str, expected: str) -> None:
    """每个 error_class 模式至少一条样本命中（exit_code 取一个 FATAL 码）。"""
    assert classify_error(7, stderr) == expected


@pytest.mark.unit
def test_classify_error_src_5xx() -> None:
    """5xx + 源（GCS）相关 → src_5xx。"""
    assert classify_error(5, "googleapi: Error 503: backendError on GCS") == "src_5xx"
    assert classify_error(5, "source GCS returned 500 Internal Server Error") == "src_5xx"


@pytest.mark.unit
def test_classify_error_dst_5xx() -> None:
    """5xx + 目标（S3）相关 → dst_5xx。"""
    assert classify_error(5, "S3 upload failed: 503 SlowDown") == "dst_5xx"
    assert classify_error(5, "PutObject to S3 bucket returned 500") == "dst_5xx"


@pytest.mark.unit
def test_classify_error_arg_error_by_exit_code() -> None:
    """exit_code==2 优先归 arg_error（配置 bug），不依赖 stderr 文本。"""
    assert classify_error(2, "Command line argument parse error") == "arg_error"
    assert classify_error(2, "") == "arg_error"


@pytest.mark.unit
def test_classify_error_uncategorized_fallback() -> None:
    """非 0 退出码但 stderr 不命中任何模式 → uncategorized。"""
    assert classify_error(7, "something totally unexpected happened") == "uncategorized"
    assert classify_error(5, "") == "uncategorized"


@pytest.mark.unit
@pytest.mark.parametrize(
    "stderr",
    [
        "权限不足 403 permissionDenied 中文混合",  # 中文 + 模式
        "emoji 🚀🔥 403 permissionDenied",  # emoji
        "tab\tand\nnewline 403 permissionDenied",  # 控制字符
        "special chars: $^*()[]{}|\\ 403 permissionDenied",  # 正则元字符在文本里
    ],
)
def test_classify_error_special_chars_do_not_crash(stderr: str) -> None:
    """stderr 含特殊字符/Unicode/正则元字符时不崩溃，仍能命中模式。"""
    assert classify_error(7, stderr) == "src_acl_deny"


@pytest.mark.unit
def test_classify_error_special_chars_uncategorized() -> None:
    """纯特殊字符不命中任何模式时安全返回 uncategorized，不抛异常。"""
    assert classify_error(7, "🔥💥\t\n$^*()[]{}|\\") == "uncategorized"


@pytest.mark.unit
def test_classify_error_pattern_priority_acl_before_5xx() -> None:
    """同时出现 403 与 500 时，按模式顺序优先返回更具体的 src_acl_deny。"""
    result = classify_error(7, "403 permissionDenied; later also 500 error")
    assert result == "src_acl_deny"
