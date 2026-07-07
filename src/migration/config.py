"""Runtime configuration.

Replaces the old module-level empty-string constants (CLIENT_ID = "" …)
with an env-var-driven, immutable dataclass. Nothing is hardcoded; every
value comes from the environment so the same image runs in any stage.
"""
from __future__ import annotations

import os
from dataclasses import dataclass

# ── Size classification ─────────────────────────────────────────────────────
# 单队列下不再做大小分流入队；此阈值仅用于 worker 内按 size 选 rclone flag
# （大文件优化）与 EMF queue_type 维度（吞吐按大小拆分上报），与队列无关。
LARGE_FILE_THRESHOLD = 100 * 1024 * 1024  # 100 MB

# ── Backpressure (SQS 14-day retention guard) ───────────────────────────────
# Lister keeps the queue's pending backlog between low and high watermarks
# instead of dumping all objects at once (which would let messages expire).
DEFAULT_QUEUE_HIGH_WATERMARK = 5_000_000
DEFAULT_QUEUE_LOW_WATERMARK = 2_000_000

# ── Worker concurrency (SQS in-flight 120k/queue guard) ─────────────────────
# 320 hosts x 256 threads = ~82k in-flight < 120k limit.
DEFAULT_WORKER_THREADS = 256

# ── rclone ──────────────────────────────────────────────────────────────────
RCLONE_BIN = os.environ.get("RCLONE_BIN", "/usr/bin/rclone")
RCLONE_TIMEOUT_SECONDS = int(os.environ.get("RCLONE_TIMEOUT_SECONDS", str(12 * 3600)))

# ── C1: SQS VisibilityTimeout 安全裕量 ───────────────────────────────────────
# rclone timeout 必须显著小于队列 VisibilityTimeout，否则长传跑到接近 timeout
# 时 VisibilityTimeout 到期 → SQS 重投 → 两台 worker 同传一个对象（双写同一
# S3 key）。要求 timeout <= TIMEOUT_VISIBILITY_SAFE_RATIO × visibility，启动时
# 校验，不满足 fail-fast 拒绝启动。
# 默认 visibility 43200(12h) 对齐单队列 CFN 配置；实际值由 systemd unit
# 注入 QUEUE_VISIBILITY_TIMEOUT（须与 CFN 中 MigrationQueue VisibilityTimeout 一致）。
DEFAULT_QUEUE_VISIBILITY_TIMEOUT = 43200
TIMEOUT_VISIBILITY_SAFE_RATIO = 0.7


def validate_timeout_invariant(*, timeout: int, visibility: int) -> None:
    """校验 rclone timeout 相对 SQS VisibilityTimeout 有足够安全裕量。

    不满足抛 ValueError（调用方启动时 fail-fast，拒绝以危险配置上线）。
    要求 timeout <= TIMEOUT_VISIBILITY_SAFE_RATIO × visibility，
    确保即便单次传输跑满 timeout，也能在 VisibilityTimeout 到期前结束并删除/
    保留消息，避免重投导致的双写。
    """
    if visibility <= 0:
        raise ValueError(
            f"QUEUE_VISIBILITY_TIMEOUT 必须为正数，当前={visibility}"
        )
    # 整数运算算上限，避免浮点精度问题：43200*0.7 浮点是 30239.999… 会让恰好
    # 等于 0.7× 的合法配置(30240)被误判越界。用 *7//10 得精确整数 30240。
    safe_ceiling = visibility * 7 // 10
    if timeout > safe_ceiling:
        raise ValueError(
            "RCLONE_TIMEOUT_SECONDS 相对 SQS VisibilityTimeout 无安全裕量，"
            "会导致长传超时被重投、两台 worker 双写同一对象。"
            f"要求 timeout({timeout}) <= {TIMEOUT_VISIBILITY_SAFE_RATIO}"
            f"×VisibilityTimeout({visibility})={safe_ceiling:.0f}。"
            "请下调 RCLONE_TIMEOUT_SECONDS 或上调队列 VisibilityTimeout。"
        )

# rclone flags only allowed to arrive via an SQS message's rclone_args.
ALLOWED_RCLONE_FLAGS = frozenset(
    {
        "--s3-storage-class",
        "--s3-server-side-encryption",
        "--header-upload",
        "--progress",
        # --disable <feature>：关闭指定可选特性。压测用 "--disable copy" 强制
        # S3→S3 走下载+上传（模拟真实跨云负载），而非 server-side copy（数据不过
        # worker、测不出 CPU/内存）。value 经 _is_safe_value 校验（不含控制字符、
        # 不以 -- 开头），如 "copy" / "copy,move" 均安全。
        "--disable",
    }
)
# Flags from the allowlist that consume a following value token.
RCLONE_FLAGS_WITH_VALUE = frozenset(
    {
        "--s3-storage-class",
        "--s3-server-side-encryption",
        "--header-upload",
        "--disable",
    }
)


@dataclass(frozen=True)
class Settings:
    """Immutable runtime settings resolved from environment variables."""

    aws_region: str
    source_remote: str  # rclone remote name for the source, e.g. "gcs" or "s3src"
    dest_remote: str  # rclone remote name for the destination, e.g. "s3"
    queue_url: str  # 单一迁移队列（大小文件统一入队，VisibilityTimeout 固定 12h）
    dynamodb_table: str
    heartbeat_table: str
    rclone_config_path: str
    # 动态限速控制面下发的当前生效 bwlimit（字节/秒纯数字或 "off"）SSM 参数名。
    # 控制器 Lambda 写、worker 周期读；读不到时 worker 退化为不限速。
    ratelimit_bwlimit_param: str = "/migration/ratelimit/bwlimit"
    # 请求频率限速 tpslimit（次/秒纯数字或 "off"）SSM 参数名。手动维护（不接
    # AIMD），worker 与 bwlimit 同周期刷新；读不到退化为不限。
    ratelimit_tpslimit_param: str = "/migration/ratelimit/tpslimit"
    worker_threads: int = DEFAULT_WORKER_THREADS
    # EMF 指标总开关。默认 False（关闭）——200 台 / 百亿级文件全量迁移下 CloudWatch
    # Logs 摄入费 + 自定义指标费达数千美元/月，默认关掉直接归零，无需改 CFN。
    # 需观测时显式注入 METRICS_ENABLED=true 打开；关闭不影响 DDB 四态终态与本地日志。
    metrics_enabled: bool = False
    queue_high_watermark: int = DEFAULT_QUEUE_HIGH_WATERMARK
    queue_low_watermark: int = DEFAULT_QUEUE_LOW_WATERMARK
    # 队列 VisibilityTimeout（秒），用于 C1 安全裕量校验 + H1 优雅退出语义。
    queue_visibility_timeout: int = DEFAULT_QUEUE_VISIBILITY_TIMEOUT

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> Settings:
        e = os.environ if env is None else env

        def req(key: str) -> str:
            val = e.get(key, "")
            if not val:
                raise ValueError(f"required env var missing or empty: {key}")
            return val

        region = req("AWS_REGION")
        return cls(
            aws_region=region,
            source_remote=e.get("SOURCE_REMOTE", "s3src"),
            dest_remote=e.get("DEST_REMOTE", "s3"),
            queue_url=req("QUEUE_URL"),
            dynamodb_table=e.get("DYNAMODB_TABLE", f"transfer-message-status-{region}"),
            heartbeat_table=e.get("HEARTBEAT_TABLE", f"worker-heartbeat-{region}"),
            rclone_config_path=e.get(
                "RCLONE_CONFIG", "/root/.config/rclone/rclone.conf"
            ),
            ratelimit_bwlimit_param=e.get(
                "RATELIMIT_BWLIMIT_PARAM", "/migration/ratelimit/bwlimit"
            ),
            ratelimit_tpslimit_param=e.get(
                "RATELIMIT_TPSLIMIT_PARAM", "/migration/ratelimit/tpslimit"
            ),
            worker_threads=int(e.get("WORKER_THREADS", str(DEFAULT_WORKER_THREADS))),
            metrics_enabled=e.get("METRICS_ENABLED", "false").strip().lower()
            in ("true", "1", "yes", "on"),
            queue_high_watermark=int(
                e.get("QUEUE_HIGH_WATERMARK", str(DEFAULT_QUEUE_HIGH_WATERMARK))
            ),
            queue_low_watermark=int(
                e.get("QUEUE_LOW_WATERMARK", str(DEFAULT_QUEUE_LOW_WATERMARK))
            ),
            queue_visibility_timeout=int(
                e.get(
                    "QUEUE_VISIBILITY_TIMEOUT", str(DEFAULT_QUEUE_VISIBILITY_TIMEOUT)
                )
            ),
        )
