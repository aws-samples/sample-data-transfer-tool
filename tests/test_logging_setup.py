"""logging_setup 单元测试：运维日志配置与 stdout 物理隔离。"""
from __future__ import annotations

import logging
import sys

import pytest

from migration import logging_setup as ls


@pytest.mark.unit
def test_resolve_log_path_default():
    assert ls.resolve_log_path({}) == "/var/log/migration/worker.log"


@pytest.mark.unit
def test_resolve_log_path_env_override():
    env = {"WORKER_LOG_DIR": "/tmp/x", "WORKER_LOG_FILE": "a.log"}
    assert ls.resolve_log_path(env) == "/tmp/x/a.log"


@pytest.mark.unit
def test_resolve_log_path_per_worker_index():
    # 多进程模型：每进程独立日志文件 worker-<idx>.log，避免 16 进程共写一个文件
    # 时 RotatingFileHandler 轮转竞争丢日志（CW agent 用 worker-*.log 通配收集）。
    assert ls.resolve_log_path({"WORKER_INDEX": "0"}) == "/var/log/migration/worker-ops-0.log"
    assert ls.resolve_log_path({"WORKER_INDEX": "7"}) == "/var/log/migration/worker-ops-7.log"


@pytest.mark.unit
def test_resolve_log_path_no_index_keeps_plain_name():
    # 单进程部署（无 WORKER_INDEX）保持 worker.log，向后兼容。
    assert ls.resolve_log_path({}) == "/var/log/migration/worker.log"


@pytest.mark.unit
def test_resolve_log_path_explicit_file_overrides_index():
    # 显式指定 WORKER_LOG_FILE 时尊重它，不再按 index 改名。
    env = {"WORKER_INDEX": "3", "WORKER_LOG_FILE": "custom.log"}
    assert ls.resolve_log_path(env) == "/var/log/migration/custom.log"


@pytest.mark.unit
def test_build_handlers_writes_to_file_and_stderr(tmp_path):
    log_path = str(tmp_path / "worker.log")
    handlers = ls.build_handlers(log_path=log_path)
    kinds = {type(h).__name__ for h in handlers}
    assert "RotatingFileHandler" in kinds
    assert "StreamHandler" in kinds


@pytest.mark.unit
def test_no_handler_writes_to_stdout(tmp_path):
    # 🔴 关键：stdout 留给 EMF，任何 handler 都不能写 stdout
    handlers = ls.build_handlers(log_path=str(tmp_path / "worker.log"))
    for h in handlers:
        stream = getattr(h, "stream", None)
        assert stream is not sys.stdout, "运维日志 handler 不得写 stdout（污染 EMF）"


@pytest.mark.unit
def test_stderr_handler_only_warning_and_above(tmp_path):
    handlers = ls.build_handlers(log_path=str(tmp_path / "worker.log"))
    stderr_h = [h for h in handlers if getattr(h, "stream", None) is sys.stderr]
    assert stderr_h, "应有一个 stderr handler"
    assert stderr_h[0].level == logging.WARNING


@pytest.mark.unit
def test_file_handler_rotation_configured(tmp_path):
    handlers = ls.build_handlers(
        log_path=str(tmp_path / "worker.log"), max_bytes=1234, backup_count=3
    )
    fh = [h for h in handlers if type(h).__name__ == "RotatingFileHandler"][0]
    assert fh.maxBytes == 1234
    assert fh.backupCount == 3


@pytest.mark.unit
def test_file_handler_level_is_warning(tmp_path):
    """文件 handler 级别为 WARNING（收 WARNING/ERROR,过滤 INFO/DIAG）。"""
    handlers = ls.build_handlers(log_path=str(tmp_path / "worker.log"))
    fh = [h for h in handlers if type(h).__name__ == "RotatingFileHandler"][0]
    assert fh.level == logging.WARNING


@pytest.mark.unit
def test_log_file_keeps_warning_error_drops_info(tmp_path, monkeypatch):
    """经完整 setup_logging 走 logger 路径：WARNING/ERROR 落文件,INFO 不落。

    用真实 logger.info/warning/error（走 logger 级别判定），而非 handler.handle()
    （handle 不查 handler.level,只跑 filter——验证级别过滤必须走 logger）。
    """
    monkeypatch.setenv("WORKER_LOG_DIR", str(tmp_path))
    monkeypatch.delenv("WORKER_INDEX", raising=False)   # 用默认 worker.log
    ls.setup_logging()
    log = logging.getLogger("leveltest")
    log.info("info-line-DIAG")
    log.warning("warn-line")
    log.error("error-line\nstderr: 429")   # 多行 ERROR
    for h in logging.getLogger().handlers:
        h.flush()
    content = (tmp_path / "worker.log").read_text()
    assert "error-line" in content and "stderr: 429" in content  # ERROR 含多行续行完整保留
    assert "warn-line" in content                                 # WARNING 保留
    assert "info-line-DIAG" not in content                        # INFO 过滤


@pytest.mark.unit
def test_setup_logging_idempotent(tmp_path, monkeypatch):
    # 重复调用不堆叠 handler（清旧再加）
    monkeypatch.setenv("WORKER_LOG_DIR", str(tmp_path))
    ls.setup_logging()
    n1 = len(logging.getLogger().handlers)
    ls.setup_logging()
    n2 = len(logging.getLogger().handlers)
    assert n1 == n2
    # 清理
    for h in list(logging.getLogger().handlers):
        logging.getLogger().removeHandler(h)
