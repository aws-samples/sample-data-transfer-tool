"""feed_gcs 纯逻辑单测（RED→GREEN，无 AWS 依赖）。

只测可注入/纯函数部分：凭证加载、消息体构造、size 路由、批次切分。
列桶/发 SQS 的 I/O 经注入，不连真实 AWS。
"""
from __future__ import annotations

import json

import pytest
from feed_gcs import (
    build_message_body,
    build_sqs_entry,
    chunked,
    dest_key_for,
    load_gcs_credentials,
)


# ── load_gcs_credentials ─────────────────────────────────────────────────────
def test_load_gcs_credentials_from_env():
    """凭证从环境变量读，三件套齐全时返回 (ak, sk, endpoint)。"""
    env = {
        "GCS_HMAC_ACCESS_KEY": "AKIAGCS",
        "GCS_HMAC_SECRET": "secret123",
        "GCS_ENDPOINT": "https://storage.googleapis.com",
    }
    creds = load_gcs_credentials(env)
    assert creds.access_key == "AKIAGCS"
    assert creds.secret_key == "secret123"
    assert creds.endpoint == "https://storage.googleapis.com"


def test_load_gcs_credentials_default_endpoint():
    """未给 GCS_ENDPOINT 时回退到 GCS 官方 S3 兼容 endpoint。"""
    env = {"GCS_HMAC_ACCESS_KEY": "AK", "GCS_HMAC_SECRET": "SK"}
    creds = load_gcs_credentials(env)
    assert creds.endpoint == "https://storage.googleapis.com"


def test_load_gcs_credentials_missing_access_key_raises():
    """缺 access key → fail fast，不静默用空凭证。"""
    env = {"GCS_HMAC_SECRET": "SK"}
    with pytest.raises(ValueError, match="GCS_HMAC_ACCESS_KEY"):
        load_gcs_credentials(env)


def test_load_gcs_credentials_missing_secret_raises():
    env = {"GCS_HMAC_ACCESS_KEY": "AK"}
    with pytest.raises(ValueError, match="GCS_HMAC_SECRET"):
        load_gcs_credentials(env)


def test_load_gcs_credentials_empty_string_raises():
    """空字符串视为未配置（防 CFN 占位空值漏网）。"""
    env = {"GCS_HMAC_ACCESS_KEY": "", "GCS_HMAC_SECRET": "SK"}
    with pytest.raises(ValueError, match="GCS_HMAC_ACCESS_KEY"):
        load_gcs_credentials(env)


# ── dest_key_for ─────────────────────────────────────────────────────────────
def test_dest_key_preserves_source_key_under_prefix():
    """真实迁移保留原始 key 路径结构，挂到目标前缀下。"""
    assert dest_key_for("migrated", "data/2024/file.csv") == "migrated/data/2024/file.csv"


def test_dest_key_empty_prefix_keeps_key_as_is():
    """空前缀 → 目标 key 即源 key（不加前导斜杠）。"""
    assert dest_key_for("", "data/file.csv") == "data/file.csv"


def test_dest_key_strips_trailing_slash_on_prefix():
    """前缀末尾斜杠规整，避免出现双斜杠 key。"""
    assert dest_key_for("migrated/", "a/b.txt") == "migrated/a/b.txt"


def test_dest_key_is_idempotent_no_run_id():
    """同源 key 永远映射到同一目标 key（幂等，支持安全重跑/断点续传）。

    这是与压测 feed_local 的本质区别：真实迁移不能掺 run_id 防撞，
    否则重跑会全量重传（rclone 看不到已迁移对象 → 无法 skip）。
    """
    k1 = dest_key_for("migrated", "x/y.bin")
    k2 = dest_key_for("migrated", "x/y.bin")
    assert k1 == k2


# ── build_message_body ───────────────────────────────────────────────────────
def test_build_message_body_gcs_source_s3_dest():
    """source 为 gcs: remote，destination 为 s3: remote，op 缺省=copy 省略。"""
    body = build_message_body(
        src_bucket="gcs-linnjia-test",
        src_key="data/file.csv",
        dest_bucket="datatos3-code",
        dest_prefix="migrated",
    )
    assert body["source"] == "gcs:gcs-linnjia-test/data/file.csv"
    assert body["destination"] == "s3:datatos3-code/migrated/data/file.csv"
    # COPY 是默认值 → 省略 op，与 models.to_body / feed_local 消息形态一致。
    assert "op" not in body


def test_build_message_body_is_json_serializable():
    """消息体必须能 json.dumps（SQS MessageBody 要求）。"""
    body = build_message_body(
        src_bucket="b", src_key="k", dest_bucket="d", dest_prefix="p"
    )
    assert json.loads(json.dumps(body)) == body


def test_build_message_body_no_run_id_contamination():
    """真实迁移消息体不得含 run_id/时间戳等导致目标 key 漂移的字段。"""
    body = build_message_body(
        src_bucket="b", src_key="dir/k.txt", dest_bucket="d", dest_prefix="p"
    )
    assert body["destination"] == "s3:d/p/dir/k.txt"


# ── build_sqs_entry（size 路由用 MessageAttributes.object_size）────────────────
def test_build_sqs_entry_carries_real_object_size():
    """object_size 用真实大小放进 MessageAttributes，供 worker 端 large/small 路由。"""
    entry = build_sqs_entry(
        entry_id="e0",
        body={"source": "gcs:b/k", "destination": "s3:d/p/k"},
        object_size=123456789,
    )
    assert entry["Id"] == "e0"
    attrs = entry["MessageAttributes"]["object_size"]
    assert attrs["DataType"] == "Number"
    assert attrs["StringValue"] == "123456789"
    assert json.loads(entry["MessageBody"])["source"] == "gcs:b/k"


def test_build_sqs_entry_zero_size():
    """0 字节对象也要带 size（不省略），worker 路由按 0 走 small。"""
    entry = build_sqs_entry(entry_id="e1", body={"x": 1}, object_size=0)
    assert entry["MessageAttributes"]["object_size"]["StringValue"] == "0"


# ── chunked（SQS send_message_batch 上限 10 条/批）────────────────────────────
def test_chunked_splits_into_batches_of_ten():
    items = list(range(25))
    batches = list(chunked(items, 10))
    assert [len(b) for b in batches] == [10, 10, 5]


def test_chunked_empty():
    assert list(chunked([], 10)) == []


def test_chunked_exact_multiple():
    batches = list(chunked(list(range(20)), 10))
    assert [len(b) for b in batches] == [10, 10]
