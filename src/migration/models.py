"""Shared data contracts across modules (worker, runner, classifier, store).

Defined centrally so parallel modules agree on the same types.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class Op(str, Enum):
    """消息操作类型。delete 用于"迁移后撤销/清理目标对象"。

    - COPY:   rclone copyto，源 → 目标（默认，旧消息无 op 字段即此）
    - DELETE: rclone deletefile，删目标端单个对象（destination）
    """

    COPY = "copy"
    DELETE = "delete"


class State(str, Enum):
    """Four-state transfer outcome (spec §4.1).

    - SUCCESS:   delete SQS message, record SUCCESS, count it
    - RETRYABLE: keep message (let visibility timeout re-deliver), record FAILED
    - FATAL:     delete message (won't fix on retry), record FAILED + fatal-log
    - UNKNOWN:   do NOT delete, do NOT count (crash/SIGKILL/unexpected)
    """

    SUCCESS = "SUCCESS"
    RETRYABLE = "RETRYABLE"
    FATAL = "FATAL"
    UNKNOWN = "UNKNOWN"


@dataclass(frozen=True)
class TransferStats:
    """Parsed from rclone's final --use-json-log stats line."""

    bytes: int = 0
    elapsed_seconds: float = 0.0
    speed: float = 0.0
    errors: int = 0
    transfers: int = 0


@dataclass(frozen=True)
class RunResult:
    """Outcome of one rclone copyto invocation."""

    state: State
    exit_code: int
    stats: TransferStats = field(default_factory=TransferStats)
    error_class: str | None = None
    error_message: str | None = None
    cmd_str: str = ""

    @property
    def success(self) -> bool:
        return self.state is State.SUCCESS


@dataclass(frozen=True)
class TransferMessage:
    """A single SQS work item: one object = one message."""

    source: str
    destination: str
    op: Op = Op.COPY
    rclone_args: tuple[str, ...] = ()

    @classmethod
    def from_body(cls, body: dict) -> TransferMessage:
        # op 缺省 = copy（旧消息天然兼容）；非法值 Op(...) 抛 ValueError → poison。
        op = Op(body.get("op", "copy"))
        # delete 操作 source 可省略（只需 destination）；copy 仍要求 source。
        return cls(
            source=body.get("source", ""),
            destination=body["destination"],
            op=op,
            rclone_args=tuple(body.get("rclone_args", []) or ()),
        )

    def to_body(self) -> dict:
        d: dict = {"source": self.source, "destination": self.destination}
        # COPY 是默认值 → 省略 op，保持旧消息形态。
        if self.op is not Op.COPY:
            d["op"] = self.op.value
        if self.rclone_args:
            d["rclone_args"] = list(self.rclone_args)
        return d
