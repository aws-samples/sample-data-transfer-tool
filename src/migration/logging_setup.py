"""Worker 节点本地运维日志配置（与 EMF stdout 物理隔离）。

⚠️ 关键约束：stdout 专供纯净 EMF JSON 给 CloudWatch Agent 抽取指标，绝不能
污染。所以运维日志（logging）走**独立的轮转文件** /var/log/migration/worker.log，
不碰 stdout。这样：
  - EMF JSON     → stdout → worker-emf.log（CW Agent tail → 指标）
  - 运维日志     → worker.log（轮转，本地排障，避免 SSH 进每台 journalctl）
  - 严重错误     → 同时保留 stderr → journald（systemd 一眼可见）

轮转防止长时间运行写满磁盘（--disable copy 时本地盘本就紧张）。
"""
from __future__ import annotations

import logging
import os
import sys
from logging.handlers import RotatingFileHandler

# 默认值（可经环境变量覆盖，部署方控制）
DEFAULT_LOG_DIR = "/var/log/migration"
DEFAULT_LOG_FILE = "worker.log"
DEFAULT_MAX_BYTES = 50 * 1024 * 1024  # 单文件 50MB
DEFAULT_BACKUP_COUNT = 5  # 保留 5 个历史，合计 ~300MB 上限
_LOG_FORMAT = "%(asctime)s - %(name)s - %(levelname)s - %(message)s"


def resolve_log_path(env: dict[str, str] | None = None) -> str:
    """解析运维日志文件绝对路径（LOG_DIR/LOG_FILE 可覆盖）。纯函数，便于测试。

    多进程模型下默认按 WORKER_INDEX 拆成 worker-<idx>.log：16 个进程共写同一个
    worker.log 时 RotatingFileHandler 轮转会竞争（rename 期间并发 doRollover 互相
    覆盖、丢日志），正是要靠日志定位问题的场景最不能丢。每进程独立文件根治竞争，
    CW agent 用 worker-*.log 通配统一收集。显式 WORKER_LOG_FILE 优先（尊重覆盖）；
    无 WORKER_INDEX（单进程部署）保持 worker.log 向后兼容。
    """
    e = os.environ if env is None else env
    log_dir = e.get("WORKER_LOG_DIR", DEFAULT_LOG_DIR)
    explicit = e.get("WORKER_LOG_FILE")
    if explicit:
        log_file = explicit
    else:
        # worker-ops-<idx>.log（非 worker-<idx>.log）：避免与 EMF 文件 worker-emf-<idx>.log
        # 被同一个 worker-* 通配同时采集（会把 EMF JSON 误灌进 worker-ops 组）。
        idx = e.get("WORKER_INDEX", "")
        log_file = f"worker-ops-{idx}.log" if idx != "" else DEFAULT_LOG_FILE
    return os.path.join(log_dir, log_file)


def build_handlers(
    *,
    log_path: str,
    max_bytes: int = DEFAULT_MAX_BYTES,
    backup_count: int = DEFAULT_BACKUP_COUNT,
    stderr_level: int = logging.WARNING,
) -> list[logging.Handler]:
    """构造日志 handler 列表（纯函数，不挂到 root，便于单测验证配置）。

    - 轮转文件 handler：全量 INFO+ 写 worker.log（本地排障主力）
    - stderr handler：仅 WARNING+ 转 journald（systemd 快速可见严重问题）

    ⚠️ 不返回任何写 stdout 的 handler——stdout 留给 EMF。
    文件目录创建失败（如本地无权限）时降级为只用 stderr，不让 worker 起不来。
    """
    fmt = logging.Formatter(_LOG_FORMAT)
    handlers: list[logging.Handler] = []

    try:
        os.makedirs(os.path.dirname(log_path), exist_ok=True)
        file_handler = RotatingFileHandler(
            log_path, maxBytes=max_bytes, backupCount=backup_count, encoding="utf-8"
        )
        file_handler.setLevel(logging.INFO)
        file_handler.setFormatter(fmt)
        handlers.append(file_handler)
    except OSError as exc:  # pragma: no cover - 依赖文件系统
        # 降级：拿不到日志目录也不能让 worker 启动失败，至少留 stderr。
        sys.stderr.write(f"worker.log 初始化失败，降级仅用 stderr: {exc}\n")

    stderr_handler = logging.StreamHandler(stream=sys.stderr)
    stderr_handler.setLevel(stderr_level)
    stderr_handler.setFormatter(fmt)
    handlers.append(stderr_handler)
    return handlers


def setup_logging(*, level: int = logging.INFO, env: dict[str, str] | None = None) -> None:
    """配置 root logger：轮转文件 + stderr(WARNING+)。stdout 完全不碰（留给 EMF）。

    幂等：重复调用先清空已有 handler，避免重复输出。worker.main() 启动时调一次。
    """
    root = logging.getLogger()
    root.setLevel(level)
    for h in list(root.handlers):  # 清掉 basicConfig 等遗留 handler
        root.removeHandler(h)
    for h in build_handlers(log_path=resolve_log_path(env)):
        root.addHandler(h)
