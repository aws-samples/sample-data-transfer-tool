"""Tests for Settings.from_env."""
import pytest

from migration.config import Settings, validate_timeout_invariant


def _base_env():
    return {
        "AWS_REGION": "eu-central-1",
        "QUEUE_URL": "http://queue",
    }


def test_from_env_minimal_defaults():
    s = Settings.from_env(_base_env())
    assert s.aws_region == "eu-central-1"
    assert s.dest_remote == "s3"
    assert s.source_remote == "s3src"
    assert s.worker_threads == 256
    assert s.queue_url == "http://queue"
    assert s.dynamodb_table == "transfer-message-status-eu-central-1"
    assert s.queue_high_watermark == 5_000_000


def test_from_env_overrides():
    env = _base_env() | {
        "DEST_REMOTE": "s3dst",
        "WORKER_THREADS": "128",
    }
    s = Settings.from_env(env)
    assert s.dest_remote == "s3dst"
    assert s.worker_threads == 128


@pytest.mark.parametrize("missing", ["AWS_REGION", "QUEUE_URL"])
def test_from_env_missing_required_raises(missing):
    env = _base_env()
    del env[missing]
    with pytest.raises(ValueError, match=missing):
        Settings.from_env(env)


def test_from_env_empty_required_raises():
    env = _base_env() | {"QUEUE_URL": ""}
    with pytest.raises(ValueError):
        Settings.from_env(env)


def test_settings_is_frozen():
    from dataclasses import FrozenInstanceError

    s = Settings.from_env(_base_env())
    with pytest.raises(FrozenInstanceError):
        s.aws_region = "us-east-1"  # type: ignore[misc]


# ── C1: timeout < visibility_timeout 不变量 ─────────────────────────────────
def test_from_env_reads_queue_visibility_timeout_default():
    # 默认值给一个合理裕量值（12h 队列 timeout，约定 worker 侧默认 43200）
    s = Settings.from_env(_base_env())
    assert s.queue_visibility_timeout == 43200


def test_from_env_reads_queue_visibility_timeout_override():
    env = _base_env() | {"QUEUE_VISIBILITY_TIMEOUT": "7200"}
    s = Settings.from_env(env)
    assert s.queue_visibility_timeout == 7200


class TestValidateTimeoutInvariant:
    """rclone timeout 必须显著小于 SQS VisibilityTimeout，否则长传到期被
    重投 → 两台机同传同一对象（双写同一 S3 key）。要求 timeout <= 0.7×visibility。"""

    def test_safe_margin_passes(self):
        # 0.7 倍正好在边界，允许
        validate_timeout_invariant(timeout=7000, visibility=10000)

    def test_well_below_passes(self):
        validate_timeout_invariant(timeout=30000, visibility=43200)

    def test_equal_raises(self):
        # timeout == visibility：无裕量，必须拒绝启动
        with pytest.raises(ValueError, match="VisibilityTimeout"):
            validate_timeout_invariant(timeout=43200, visibility=43200)

    def test_above_safe_ratio_raises(self):
        # 超过 0.7 倍但小于 visibility，仍无足够裕量
        with pytest.raises(ValueError):
            validate_timeout_invariant(timeout=9000, visibility=10000)

    def test_greater_than_visibility_raises(self):
        with pytest.raises(ValueError):
            validate_timeout_invariant(timeout=50000, visibility=43200)

    def test_nonpositive_visibility_raises(self):
        with pytest.raises(ValueError):
            validate_timeout_invariant(timeout=10, visibility=0)


def test_from_env_default_timeout_violates_invariant_is_caught():
    """默认 RCLONE_TIMEOUT_SECONDS=12h 且 visibility 默认 43200(12h) → 等值，
    校验必须抛错（这正是 C1 要 fail-fast 拦住的危险配置）。"""
    from migration import config

    with pytest.raises(ValueError):
        validate_timeout_invariant(
            timeout=config.RCLONE_TIMEOUT_SECONDS,
            visibility=43200,
        )
