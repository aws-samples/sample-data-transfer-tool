"""Tests for path_safety: cross-cloud object path validation (§4.4)."""
import pytest

from migration.path_safety import build_safe_source, validate_object_path


class TestValidateObjectPath:
    @pytest.mark.parametrize(
        "name",
        [
            "report.pdf",
            "folder/sub/file.bin",
            "数据/文件.txt",  # non-ASCII but valid UTF-8 / SQS XML
            "a" * 500,
            "data:image.bin",  # literal colon is fine inside a path segment
            "café.pdf",  # NFC
            # ── 以下为实测确认 HMAC(S3-compatible)端点可正常传输的特殊 key ──
            "a//b",  # 连续斜杠：native GCS 会规整→404，HMAC 端点保留字面 key 可传
            "slash///leading-triple.txt",  # 多重斜杠同理
            "/leading-slash.png",  # 前导斜杠
            "report .pdf ",  # 尾随空格：S3 可存，按完整 key 精确匹配取数
            "data.",  # 尾随点
            "x\t",  # 尾随 tab（XML 合法）
            "nbsp\xa0space.txt",  # U+00A0 不间断空格（合法 Unicode）
            "a/../b",  # 字面 ".." 段：rclone remote:path 非本地 FS，无穿越语义
            "../etc/passwd",  # 同上，作为对象 key 是合法字面前缀
        ],
    )
    def test_accepts_valid_paths(self, name):
        ok, reason = validate_object_path(name)
        assert ok, reason
        assert reason == "ok"

    def test_rejects_empty(self):
        ok, reason = validate_object_path("")
        assert not ok
        assert reason == "empty"

    @pytest.mark.parametrize(
        "name",
        [
            # 目录型路径（2026-06-10 决策）：尾部 / = 前缀/目录，不是单对象。
            # copyto 会整树复制（实测 220 个对象副作用）、deletefile 会失败。
            # 一律按 poison 拦截：ERROR 日志 + 快速重投 3 次进 DLQ。
            "libs/hive/warehouse/aml.db/aml_item_fea_mid_dev_all/dt=20250724/days=30/",
            "prefix/",
            "a/b/c/",
        ],
    )
    def test_rejects_directory_path(self, name):
        ok, reason = validate_object_path(name)
        assert not ok
        assert reason == "directory_path"

    def test_rejects_too_long_utf8(self):
        # 1025 ASCII bytes > 1024 limit（对象 key 字节数上限）
        ok, reason = validate_object_path("a" * 1025)
        assert not ok
        assert reason.startswith("too_long")

    def test_multibyte_counts_as_bytes_not_chars(self):
        # 400 chars × 3 bytes = 1200 bytes > 1024
        ok, reason = validate_object_path("数" * 400)
        assert not ok
        assert reason.startswith("too_long")

    def test_accepts_exactly_max_bytes(self):
        # 边界：正好 1024 字节应放行
        ok, reason = validate_object_path("a" * 1024)
        assert ok, reason

    def test_custom_max_bytes(self):
        ok, _ = validate_object_path("abcdef", max_bytes=3)
        assert not ok


class TestBuildSafeSource:
    def test_basic(self):
        assert build_safe_source("s3src", "folder/sub", "file.bin") == \
            "s3src:folder/sub/file.bin"

    def test_strips_remote_trailing_colon(self):
        assert build_safe_source("s3src:", "a", "b.txt") == "s3src:a/b.txt"

    def test_empty_parent(self):
        assert build_safe_source("gcs", "", "file.bin") == "gcs:file.bin"

    def test_strips_leading_trailing_slash_on_parent(self):
        assert build_safe_source("gcs", "/a/b/", "c.txt") == "gcs:a/b/c.txt"

    def test_collapses_double_slash(self):
        assert build_safe_source("gcs", "a//b", "c.txt") == "gcs:a/b/c.txt"
