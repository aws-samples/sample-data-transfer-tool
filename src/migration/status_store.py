"""DynamoDB 状态层 —— 纯 OLTP（spec §2.3 / §3.1）。

职责边界（严格）：
- 只写**终态**（SUCCESS / RETRYABLE / FATAL / UNKNOWN），不写 PROCESSING。
- 只做 worker 运行时状态机写入 + inspect 点查 + 心跳，**不做 OLAP 聚合**
  （聚合分析走 Firehose → Athena，不在本模块）。

两张表：
- transfer-status：PK source_hash（分片前缀#source）+ SK attempt_timestamp，每次 attempt 一行。
- heartbeat：PK instance_id，带 ttl（DDB TTL 字段）做 worker 存活判定。
"""
from __future__ import annotations

import hashlib

from migration.models import RunResult

# 表名不在此硬编码——单一真相来源是 config.Settings（dynamodb_table / heartbeat_table），
# 由调用方传入。这样 create_tables 建的表与运行时读写的表必然一致。

# 分片数：source 经稳定 hash 后取模打散，避免写入集中到单个热分区。
_SHARD_COUNT = 256
# 心跳存活窗口（秒）：ttl = now_epoch + _HEARTBEAT_TTL_SECONDS。
_HEARTBEAT_TTL_SECONDS = 300


def make_pk(source: str) -> str:
    """生成稳定的分区键：'<分片前缀>#<source>'。

    ⚠️ 必须用 hashlib 而非内置 hash()——内置 hash() 受 PYTHONHASHSEED 影响，
    跨进程不稳定，会导致同一 source 在不同 worker 落到不同 PK，inspect 查不全。
    """
    digest = hashlib.md5(source.encode("utf-8")).hexdigest()  # noqa: S324 (非安全用途，仅用于打散分片)
    shard = int(digest, 16) % _SHARD_COUNT
    return f"{shard}#{source}"


def create_tables(client, status_table: str, heartbeat_table: str) -> None:
    """建两张表 —— **仅供测试 mock 与本地开发**。

    生产环境的表由 CloudFormation 声明式创建（deployment-cfn-git.yaml 的
    AWS::DynamoDB::Table），worker 启动不建表。此函数保留是为了 moto 测试和
    本地起 DynamoDB Local 时一键建表，表名由调用方传入（来自 config.Settings），
    与 CFN 中的表名保持一致。
    status_table：复合主键；heartbeat_table：单主键 + TTL 属性 ttl。
    """
    client.create_table(
        TableName=status_table,
        AttributeDefinitions=[
            {"AttributeName": "source_hash", "AttributeType": "S"},
            {"AttributeName": "attempt_timestamp", "AttributeType": "S"},
        ],
        KeySchema=[
            {"AttributeName": "source_hash", "KeyType": "HASH"},
            {"AttributeName": "attempt_timestamp", "KeyType": "RANGE"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )
    client.create_table(
        TableName=heartbeat_table,
        AttributeDefinitions=[
            {"AttributeName": "instance_id", "AttributeType": "S"},
        ],
        KeySchema=[
            {"AttributeName": "instance_id", "KeyType": "HASH"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )


# message_body 截断上限：SQS body 可达 256KB，DDB 单 item 上限 400KB（与
# error_message/rclone_command 同行），截断防 ValidationException 整行写失败。
# 16KB 足够覆盖正常消息（source+destination+rclone_args 通常 <1KB）。
_MAX_MESSAGE_BODY_BYTES = 16 * 1024


def record_terminal(
    client,
    table: str,
    source: str,
    attempt_timestamp: str,
    result: RunResult,
    instance_id: str,
    *,
    now_iso: str,
    message_body: str | None = None,
) -> None:
    """写一条终态记录。

    每次 attempt 用各自的 attempt_timestamp 作 SK，故为独立行（不覆盖历史尝试）。
    now_iso 作参数注入（不在函数内调 datetime.now，便于测试且适配受限环境）。
    message_body：原始 SQS 消息体（2026-06-10 决策：出错消息 DDB 记录完整消息体，
    便于直接定位/replay）。SUCCESS 路径不传（百万级成功行不重复存 body）。
    """
    item: dict = {
        "source_hash": {"S": make_pk(source)},
        "attempt_timestamp": {"S": attempt_timestamp},
        "source": {"S": source},
        "state": {"S": result.state.value},
        "transferred_bytes": {"N": str(result.stats.bytes)},
        "elapsed_seconds": {"N": str(result.stats.elapsed_seconds)},
        "instance_id": {"S": instance_id},
        "rclone_command": {"S": result.cmd_str},
        "updated_at": {"S": now_iso},
    }
    # 仅在有值时写错误字段，避免成功记录里塞空属性。
    if result.error_class is not None:
        item["error_class"] = {"S": result.error_class}
    if result.error_message is not None:
        item["error_message"] = {"S": result.error_message}
    if message_body is not None:
        item["message_body"] = {"S": message_body[:_MAX_MESSAGE_BODY_BYTES]}

    client.put_item(TableName=table, Item=item)


def write_heartbeat(
    client,
    table: str,
    instance_id: str,
    *,
    now_iso: str,
    now_epoch: int,
    active_threads: int,
) -> None:
    """写/覆盖 worker 心跳；ttl = now_epoch + 300（DDB 自动过期清理 + 存活判定依据）。"""
    client.put_item(
        TableName=table,
        Item={
            "instance_id": {"S": instance_id},
            "last_heartbeat": {"S": now_iso},
            "ttl": {"N": str(now_epoch + _HEARTBEAT_TTL_SECONDS)},
            "active_threads": {"N": str(active_threads)},
        },
    )


def inspect(client, table: str, source: str) -> list[dict]:
    """按 make_pk(source) query，返回该 source 全部 attempt（按 attempt_timestamp 升序）。

    供 migration-cli inspect 点查单个文件的尝试历史。
    """
    resp = client.query(
        TableName=table,
        KeyConditionExpression="source_hash = :pk",
        ExpressionAttributeValues={":pk": {"S": make_pk(source)}},
        ScanIndexForward=True,  # SK 升序
    )
    return [_deserialize(item) for item in resp.get("Items", [])]


def count_active_workers(client, table: str, *, now_epoch: int) -> int:
    """scan heartbeat 表，统计 ttl > now_epoch 的活跃 worker 数（供监控）。

    严格大于：ttl == now_epoch 视为已过期。DDB TTL 删除有最长 48h 延迟，故服务端
    FilterExpression 主动按 ttl 过滤（HIGH-6），过期残留行不传回客户端，省带宽。
    'ttl' 是 DDB 保留字，用 #ttl 别名规避。
    """
    count = 0
    kwargs: dict = {
        "TableName": table,
        "FilterExpression": "#ttl > :now",
        "ExpressionAttributeNames": {"#ttl": "ttl"},
        "ExpressionAttributeValues": {":now": {"N": str(now_epoch)}},
    }
    while True:
        resp = client.scan(**kwargs)
        # 服务端已按 ttl > now 过滤，返回的即活跃行，直接计数。
        count += len(resp.get("Items", []))
        last_key = resp.get("LastEvaluatedKey")
        if not last_key:
            break
        kwargs["ExclusiveStartKey"] = last_key
    return count


def _deserialize(item: dict) -> dict:
    """把 DDB 低层属性映射拍平成普通 dict；N 还原为 int/float。"""
    out: dict = {}
    for key, value in item.items():
        if "S" in value:
            out[key] = value["S"]
        elif "N" in value:
            num = value["N"]
            out[key] = float(num) if ("." in num or "e" in num.lower()) else int(num)
        else:  # pragma: no cover - 本表不使用其它属性类型
            out[key] = next(iter(value.values()))
    return out
