"""Tests for lazy boto3 client factories (moto-mocked)."""
from moto import mock_aws

from migration import aws_clients


@mock_aws
def test_clients_are_cached_singletons():
    aws_clients.reset_clients()
    c1 = aws_clients.get_s3_client("eu-central-1")
    c2 = aws_clients.get_s3_client("eu-central-1")
    assert c1 is c2  # same region → cached singleton


@mock_aws
def test_different_services_and_regions_distinct():
    aws_clients.reset_clients()
    s3 = aws_clients.get_s3_client("eu-central-1")
    sqs = aws_clients.get_sqs_client("eu-central-1")
    ddb = aws_clients.get_dynamodb_client("us-east-1")
    assert s3 is not sqs
    assert ddb is not s3


@mock_aws
def test_reset_clears_cache():
    aws_clients.reset_clients()
    c1 = aws_clients.get_s3_client("eu-central-1")
    aws_clients.reset_clients()
    c2 = aws_clients.get_s3_client("eu-central-1")
    assert c1 is not c2


def test_pool_size_scales_with_worker_threads(monkeypatch):
    monkeypatch.setenv("WORKER_THREADS", "512")
    # 512×1.1+16 = 579，必须远大于 botocore 默认 10，否则高线程下连接池打满
    assert aws_clients._pool_size() == int(512 * 1.1) + 16
    assert aws_clients._pool_size() >= 512


def test_pool_size_floor_and_bad_value(monkeypatch):
    monkeypatch.setenv("WORKER_THREADS", "1")
    assert aws_clients._pool_size() == 32  # 1×1.1+16=17 < 32 → 下限兜底
    monkeypatch.setenv("WORKER_THREADS", "not-a-number")
    # 解析失败回退默认 256 → 256×1.1+16 = 297
    assert aws_clients._pool_size() == int(256 * 1.1) + 16


def test_boto_config_has_large_pool():
    # 模块加载时已固化 max_pool_connections，必须 >10（默认值）
    assert aws_clients._BOTO_CONFIG.max_pool_connections > 10


# ── prime_credentials：启动前强制取一次凭证，根治 boot 期 IMDS 抢占失败 ──
class _FakeSession:
    """模拟 botocore session 的凭证获取（None=拿不到，否则返回带 access_key 的对象）。"""
    def __init__(self, creds):
        self._creds = creds
        self.calls = 0

    def get_credentials(self):
        self.calls += 1
        return self._creds


def test_prime_credentials_ok_returns_true(monkeypatch):
    class _Creds:
        access_key = "AKIA..."
    sess = _FakeSession(_Creds())
    monkeypatch.setattr(aws_clients, "_session_factory", lambda: sess)
    assert aws_clients.prime_credentials(attempts=3, base_delay=0.0) is True
    assert sess.calls == 1  # 一次就成功，不重试


def test_prime_credentials_retries_then_succeeds(monkeypatch):
    class _Creds:
        access_key = "AKIA..."
    seq = [None, None, _Creds()]
    calls = {"n": 0}

    class _Sess:
        def get_credentials(self):
            c = seq[calls["n"]]
            calls["n"] += 1
            return c
    monkeypatch.setattr(aws_clients, "_session_factory", lambda: _Sess())
    assert aws_clients.prime_credentials(attempts=5, base_delay=0.0) is True
    assert calls["n"] == 3  # 前两次 None，第三次成功


def test_prime_credentials_exhausts_returns_false(monkeypatch):
    monkeypatch.setattr(aws_clients, "_session_factory", lambda: _FakeSession(None))
    assert aws_clients.prime_credentials(attempts=3, base_delay=0.0) is False


def test_prime_credentials_handles_exception(monkeypatch):
    class _Boom:
        def get_credentials(self):
            raise RuntimeError("IMDS timeout")
    monkeypatch.setattr(aws_clients, "_session_factory", lambda: _Boom())
    # 异常被吞、按失败重试，耗尽返回 False（不抛出，调用方据返回值决定 exit）
    assert aws_clients.prime_credentials(attempts=2, base_delay=0.0) is False
