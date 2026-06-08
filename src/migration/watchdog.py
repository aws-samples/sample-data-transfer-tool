"""systemd watchdog 集成（纯 socket sd_notify，零第三方依赖）。

为什么不用 python-systemd 包：AL2023 默认不带，cfn-init 还得多装一个、多一个
transient 失败点。sd_notify 协议本身极简（往 $NOTIFY_SOCKET 发一行文本），
用标准库 socket 直接实现最稳。

健康判定（心跳上报条件）由 WorkerLoop 提供：consumer 线程存活 + 有进展。
真僵死（所有线程卡住、连空 receive 都不转）→ 停止上报健康 → systemd WatchdogSec
超时 → 杀进程重启（比 Restart=always 更强，能抓"进程假活"）。
"""
from __future__ import annotations

import logging
import os
import socket

logger = logging.getLogger(__name__)


def notify(state: str, *, socket_path: str | None = None) -> bool:
    """向 systemd 发送一条 sd_notify 消息（如 "WATCHDOG=1"、"READY=1"）。

    socket_path 默认取环境变量 NOTIFY_SOCKET（systemd 在 unit 配了
    WatchdogSec/Type=notify 时自动注入）。未设则静默返回 False
    （非 systemd 环境/本地运行，不报错）。

    返回是否成功发送。失败只记 debug、不抛——心跳上报失败不该拖垮 worker。
    """
    path = socket_path if socket_path is not None else os.environ.get("NOTIFY_SOCKET")
    if not path:
        return False
    # 抽象命名空间 socket：systemd 用前导 '@'，转成 '\0'
    addr = "\0" + path[1:] if path.startswith("@") else path
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as sock:
            sock.sendto(state.encode("utf-8"), addr)
        return True
    except OSError as exc:  # pragma: no cover - 依赖真实 systemd socket
        logger.debug("sd_notify 失败（非致命）: %s", exc)
        return False


def is_healthy(
    *,
    alive_consumers: int,
    min_consumers: int,
    progress: int,
    last_progress: int,
) -> bool:
    """watchdog 健康判定（纯函数，便于测试）。

    健康 = 存活 consumer 数达标 **且** 自上次检查以来有进展。

    "进展"= progress 计数器变化。每个 consumer 每轮 receive（无论拿到消息还是
    空响应）都 bump progress——所以：
      - 正常处理消息：progress 涨 ✅
      - 空队列 long-poll 超时返回：progress 照样涨 ✅（不误杀空闲机器）
      - 所有线程卡死/死锁/rclone 永久阻塞：progress 不涨 → 判定不健康 → 触发重启

    min_consumers 通常设为 worker_threads（要求全部存活）或略小留容错余量。
    """
    return alive_consumers >= min_consumers and progress != last_progress
