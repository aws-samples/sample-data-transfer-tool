"""Cross-cloud object path validation (spec §4.4).

边界校验**只拦物理上传不了的对象**，不再做命令注入/字符安全防护——后者由
``rclone_runner`` 兜底（argv 数组传参、非 ``shell=True``、``--`` terminator、flag
白名单），且能进 SQS 的消息本就过了 SQS XML 校验，字符层面天然安全。

保留两项：
- ``empty``：无对象 key（``remote:bucket`` 无 ``/key``）= 无效消息。
- ``too_long``：对象 key 超过 GCS/S3 的 1024 字节上限 → rclone 必失败（实测 S3
  HeadObject 400 / create-fs critical）。提前拦成 poison，避免它落 UNKNOWN 卡满
  in-flight、12h×3 才进 DLQ。这是**运维考量**（防卡），不是安全考量。

特殊 key（中文/emoji/``//``/前导斜杠/尾随空格）一律放行：worker 经 S3-compatible
（HMAC）端点访问 GCS，XML API 保留字面 key 可正常传输（已实测 rc=0）。
"""
from __future__ import annotations

_DEFAULT_MAX_BYTES = 1024  # GCS/S3 object key byte limit


def validate_object_path(name: str, max_bytes: int = _DEFAULT_MAX_BYTES) -> tuple[bool, str]:
    """Validate a single object key can physically traverse the pipeline.

    Returns ``(is_safe, reason)``. ``reason == "ok"`` when safe.
    只拦 ``empty`` 与 ``too_long``（见模块 docstring）。
    """
    if not name:
        return False, "empty"

    encoded = name.encode("utf-8")
    if len(encoded) > max_bytes:
        return False, f"too_long_{len(encoded)}"

    return True, "ok"


def build_safe_source(remote: str, parent_path: str, name: str) -> str:
    """Compose an rclone source string ``remote:parent/name``.

    Normalises slashes (no leading/trailing, no doubled) so downstream
    prefix queries stay consistent. The object ``name`` is appended verbatim
    (it has already passed ``validate_object_path``); the ``--`` argv
    terminator in rclone_runner handles leading-dash names.
    """
    remote = remote.rstrip(":")
    parent = parent_path.strip("/")
    path = f"{parent}/{name}" if parent else name
    while "//" in path:
        path = path.replace("//", "/")
    return f"{remote}:{path}"
