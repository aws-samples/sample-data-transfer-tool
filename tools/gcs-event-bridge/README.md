# gcs-event-bridge

跨云**事件桥**：消费 GCP Pub/Sub 的 GCS 对象事件，映射成迁移消息投递到 AWS SQS，
喂给现有的 rclone 迁移 worker 管道。用于**增量同步**——GCS 侧对象一有变更（新增/
metadata 更新），近实时桥接到 S3，无需全量扫描。

```
GCS bucket notification → Pub/Sub topic → subscription
   → gcs-event-bridge（StreamingPull 消费 → 事件映射 → SQS SendMessageBatch → ack）
   → SQS 队列 → rclone 迁移 worker → S3
```

## 事件映射规则

只监控两种 GCS 事件，其余（DELETE/ARCHIVE/INITIALIZE）跳过（ack 丢弃，不投 SQS）：

| GCS 事件 | 含义 | 映射成 | worker 行为 |
|---|---|---|---|
| `OBJECT_FINALIZE` | 对象创建/新版本（**数据变了**） | `copy`（消息不带 `op`） | `rclone copyto`，按 size/mtime 正常同步 |
| `OBJECT_METADATA_UPDATE` | 只改了 metadata（**数据未变**） | `refresh`（消息带 `op:"refresh"`） | `rclone copyto --ignore-times`，**强制重传**刷新 metadata |

> ⚠️ **为什么 METADATA_UPDATE 必须用 refresh 而不是 copy**：源端只改 metadata、数据没变时，
> 普通 copy 会被 rclone 按 size/mtime 判定"nothing to transfer"而 **skip**，metadata 永远
> 刷不到 S3 目标。worker 的 `op=refresh`（`--ignore-times`）强制重传整个对象，是当前
> rclone/GCS→S3 链路下刷新目标 metadata 的唯一正确方式（代价 = 重传全量数据）。

产出的 SQS 消息体对齐 worker 契约（`src/migration/models.py` 的 `TransferMessage`）：
```jsonc
// FINALIZE → copy（省略 op）
{ "source": "gcs:<bucket>/<key>", "destination": "s3:<s3bucket>/<key>" }
// METADATA_UPDATE → refresh
{ "op": "refresh", "source": "gcs:<bucket>/<key>", "destination": "s3:<s3bucket>/<key>" }
```
`object_size`（来自 payload）走 SQS **MessageAttribute**（对齐 worker 大小路由），不进 body。
copy 与 refresh 都携带 size。

## 桶映射（两种写法，每桶二选一）

一条 pipeline 的多个源桶可共用一个 Pub/Sub 订阅，程序按消息的 `bucketId` 查 `bucket_mapping` 分发：

- **写法 A（整桶映射）**：`s3_bucket`（+ 可选 `prefix`）。该桶所有对象 → 一个 S3 桶，保留完整 key。
- **写法 B（前缀路由）**：`prefix_routes` 按对象 key 前缀路由到不同 S3 桶（**最长前缀优先**），
  `strip_prefix:true` 剥掉匹配的前缀段，`default_s3_bucket` 兜底（不配则未命中当未知桶跳过）。

完全没配进 `bucket_mapping` 的源桶 → 跳过 + 计数告警（进度行"未映射桶"），不投 SQS。
配置完整示例见 [`config.example.yaml`](../config.example.yaml)。

## 设计要点

- **多 pipeline 并发故障隔离**：一个进程跑 N 条独立管道（多源），各自 client/凭证/SQS，单条挂不拖垮其它。
- **凭证不内联**：GCP SA JSON key 存 AWS Secrets Manager，配置只放 ARN。
- **先发后 ack（at-least-once）**：投 SQS 成功才 ack Pub/Sub，崩溃时未 ack 消息自动重投（消费侧幂等）。
- **攒批 + send worker 池**：StreamingPull 拉取，应用层攒批 10 条投 SQS（`SendMessageBatch` 上限），
  `send_workers` 个 worker 并发投递（默认 64，是吞吐主杠杆）。
- **fail-fast 配置校验**：启动即校验（region/pipeline/订阅/ARN/队列/桶映射规则 A·B 互斥等），坏配置拒绝启动。
- **project id 解析**：显式 `project_id` 优先；缺省从订阅完整路径 `projects/<proj>/subscriptions/<sub>`
  解析；都拿不到 → fail-fast（提示显式填 `project_id`）。

## 调优（`tuning`，全部可选）

| 字段 | 默认 | 说明 |
|---|---|---|
| `num_goroutines` | 32 | Pub/Sub StreamingPull 拉取并发，对齐核数 |
| `max_outstanding_messages` | 50000 | 未 ack 消息上限（拉取端缓冲） |
| `send_workers` | 64 | SQS 投递并发，**吞吐主杠杆**（实测 64 把单机 QPS 从 ~1万 提到 ~2.8万） |

## 部署前置

**GCP 侧**（每个源桶各做一次）：
```bash
# 1. 建 Pub/Sub topic（多桶可共用）
# 2. GCS 桶配 notification，payload-format 必须 json（否则拿不到 object size）
gcloud storage buckets notifications create gs://<bucket> \
  --topic=<topic> --payload-format=json \
  --event-types=OBJECT_FINALIZE,OBJECT_METADATA_UPDATE
# 3. 建 pull 订阅，授 SA roles/pubsub.subscriber
# 4. SA JSON key 存进 AWS Secrets Manager（配置只放 ARN）
```

**AWS 侧**：运行实例 role 需 `secretsmanager:GetSecretValue`（对应 ARN）+ `sqs:SendMessage`（目标队列）。

## 构建 / 运行

```bash
cd go
go build -o gcs-event-bridge .
./gcs-event-bridge --config /path/to/config.yaml [--log-file /var/log/gcs-event-bridge.log]

# 测试
go test ./...
```

## 可观测

每 30s 打印进度行：pipeline 总计 + **按源桶分类**计数（多桶共用订阅时一桶一行）：
```
pipeline "shared-incr" 进度[总计]: 收 N / 投 N / 跳过 N / 未映射桶 N / 映射错 N / 投递失败 N
pipeline "shared-incr" 进度[桶 eu-abc-dw]: 收 N / 投 N / ...
```
- **收**：Pub/Sub 收到　**投**：成功投递 SQS　**跳过**：非关注事件
- **未映射桶**：源桶不在映射表/前缀无命中且无兜底（跳过 + 告警）
- **映射错**：payload 坏等（nack 重投）　**投递失败**：SQS send 失败（nack，Pub/Sub 重投）

SQS 持续故障时错误日志限流（最多每 5s 一条），失败总量仍由进度行的"投递失败"反映。

> 优雅停机：SIGTERM/SIGINT → 各 pipeline 停止 Receive、发完在途批次再退出（不丢消息）。
