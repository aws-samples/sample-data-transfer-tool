"""Lazy boto3 client factories with adaptive retry config.

Preserves the original codebase convention: never instantiate
``boto3.client(...)`` directly in business logic — go through these
singletons so retry/timeout config stays consistent.
"""
from __future__ import annotations

import logging
import os
import time

import boto3
from botocore.config import Config

logger = logging.getLogger(__name__)


# botocore 默认连接池仅 10。worker 以 WORKER_THREADS 个线程共享同一个 client
# 高频调 SQS（receive/delete/visibility）；线程数 >> 10 时连接池耗尽，
# 日志反复打印 "Connection pool is full, discarding connection"，请求排队重建 → 吞吐骤降。
# 根因修复：连接池随 worker 线程数放大，留 1.1× 余量给重试抖动，下限 32。
def _pool_size() -> int:
    try:
        threads = int(os.environ.get("WORKER_THREADS", "256"))
    except ValueError:
        threads = 256
    return max(32, int(threads * 1.1) + 16)


_BOTO_CONFIG = Config(
    retries={"max_attempts": 5, "mode": "adaptive"},
    connect_timeout=10,
    read_timeout=60,
    max_pool_connections=_pool_size(),
)

_clients: dict[str, object] = {}


def _client(service: str, region: str):
    key = f"{service}:{region}"
    if key not in _clients:
        _clients[key] = boto3.client(service, region_name=region, config=_BOTO_CONFIG)
    return _clients[key]


def get_sqs_client(region: str):
    return _client("sqs", region)


def get_s3_client(region: str):
    return _client("s3", region)


def get_dynamodb_client(region: str):
    return _client("dynamodb", region)


def get_ssm_client(region: str):
    return _client("ssm", region)


def reset_clients() -> None:
    """Clear cached clients (test helper; lets moto mocks take effect)."""
    _clients.clear()


def _session_factory():
    """返回一个 botocore session（可在测试 monkeypatch）。"""
    return boto3.Session()


def prime_credentials(*, attempts: int = 5, base_delay: float = 1.0) -> bool:
    """启动时强制取一次 IAM 凭证，成功返回 True，耗尽重试仍失败返回 False。

    根因背景：16 个 worker 进程被 systemd 几乎同时拉起，每个进程首个 boto3 client
    同时向 IMDSv2 抢 token（HttpPutResponseHopLimit=2 经一跳），IMDS 单实例并发处理
    能力有限 → 实测约 15% 进程首次取凭证超时失败，且 botocore 会缓存失败态、永久
    NoCredentialsError、该 worker 进程零产出。

    解法：进程起线程**之前**先用退避重试把凭证拿到手（预热）。拿到 → 正常启动；
    彻底拿不到 → 调用方 sys.exit(1)，systemd Restart=always 错峰重启，避开同时抢。
    指数退避（base_delay × 2^i）天然把同时启动的进程在时间轴上摊开，缓解争抢。
    """
    for i in range(attempts):
        try:
            creds = _session_factory().get_credentials()
            if creds is not None and getattr(creds, "access_key", None):
                if i > 0:
                    logger.info("凭证预热成功（第 %d 次尝试）", i + 1)
                return True
            logger.warning("凭证预热：第 %d/%d 次未取到凭证", i + 1, attempts)
        except Exception as exc:  # noqa: BLE001 - 任何取凭证异常都重试，避免进程在无凭证状态下运行
            logger.warning("凭证预热：第 %d/%d 次异常 %s", i + 1, attempts, exc)
        if i < attempts - 1 and base_delay > 0:
            time.sleep(base_delay * (2**i))
    logger.error("凭证预热失败：%d 次尝试均未取到 IAM 凭证，进程将退出待 systemd 重启", attempts)
    return False
