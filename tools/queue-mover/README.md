# queue-mover — 跨集群 SQS 消息搬运工具

把消息从 **A 集群**的 SQS 队列搬运到 **B 集群**的 SQS 队列，用于多套迁移集群之间重新分配负载：A 集群排空检修、把堆积分流给空闲的 B 集群、跨集群 DLQ 重投等。

> 这是独立运维工具，**不属于 worker 主程序包**（worker 只消费 SQS）。两个等价实现，按部署场景二选一。

```
tools/queue-mover/
├── python/                  # Python 版（boto3，无外部仓库依赖）
│   ├── queue_mover.py       #   多进程绕 GIL，--procs × --threads
│   └── test_queue_mover.py  #   15 个单元测试（注入 fake，不触达 AWS）
├── go/                      # Go 版（aws-sdk-go-v2，编译为单静态二进制）
│   ├── main.go              #   单进程 goroutine 池，--workers
│   ├── main_test.go         #   10 个单元测试（sqsAPI interface + fake）
│   ├── go.mod
│   └── go.sum
└── README.md
```

## 两版怎么选

| 维度 | Python (`python/`) | Go (`go/`) |
|------|---------------------|------------|
| 不丢消息契约 | 完全一致（见下） | 完全一致 |
| 并发模型 | 多进程绕 GIL：`--procs N --threads M` | 单进程 goroutine：`--workers N` |
| 部署依赖 | python3 + boto3 | **无**，单静态二进制 `scp`/`curl` 即跑 |
| 进度日志 | 多进程各报各的，数字交错 | 单一全局计数器，结束自带平均速率 |
| 内存 | 多进程各 ~50MB | 几十 MB |
| 适用 | 跳板机已有 Python 环境、想直接看/改源码 | 想零依赖部署、日志干净、省内存 |

吞吐天花板两版相同 —— 都被 **SQS 单队列服务端限速**封顶（region 内约 1.3 万~2 万条/秒/队列），语言不是瓶颈。要再快只能多队列分片。

## 不丢消息契约（两版严格一致）

1. **先发后删**：只有 `SendMessageBatch` 确认成功（不在 `Failed` 列表）的消息才从 A 删除。
2. **部分失败留源**：批量发送中失败的条目**不删**，留在 A 队列靠 visibility 超时自动重投。
3. **崩溃自愈**：`receive` 显式设 `VisibilityTimeout=300s`（不继承队列自身的 12h），进程中途被杀，已读未删的消息 5 分钟后自动回 A 队列。
4. **at-least-once**：极端时序下 B 可能收到重复消息 —— worker 端 DDB 按 attempt 记行、S3 `copyto` 幂等，重复无害。
5. **属性透传**：`object_size` 等 `MessageAttributes` 原样带到 B（过滤 `AWS.` 保留前缀）。
6. **`--max` 严格不超**：配额在 `receive` 前预留，多进程/多 goroutine 下也精确不超额。

可安全中断/重跑：`Ctrl-C` / `kill` / `ssh` 断连随时可停，最坏是少量已读未删消息 5 分钟后回 A，再跑继续搬，不丢不坏。

## IAM 最小权限

运行身份（堡垒机实例角色）需同时具备：
- 源队列：`sqs:ReceiveMessage` + `sqs:DeleteMessage`
- 目标队列：`sqs:SendMessage`

能跑 `migops.sh dlq-replay` 的现有 ops 角色无需任何变更。

---

## Python 版

### 运行
```bash
cd tools/queue-mover/python

# dry-run 预览（只读取展示，不发送不删除）——先确认方向没写反
AWS_REGION=eu-south-2 python3 queue_mover.py \
    --src-queue <A队列URL> --dst-queue <B队列URL> --max 5 --dry-run

# 正式搬运（2xlarge 跳板机推荐 procs=4 threads=16）
AWS_REGION=eu-south-2 python3 queue_mover.py \
    --src-queue <A队列URL> --dst-queue <B队列URL> \
    --max 1000000 --procs 4 --threads 16 \
    --log-file mover.log --progress-every 50000
```

### 并发档位（按机型 vCPU）
`--procs` ≈ vCPU 的一半（留核给系统和 boto3 网络回调），`--threads` 固定 16。

| 机型 | vCPU | 推荐 | 总并发 |
|------|------|------|--------|
| 2xlarge | 8 | `--procs 4 --threads 16` | 64 |
| 4xlarge | 16 | `--procs 8 --threads 16` | 128 |
| 8xlarge | 32 | `--procs 16 --threads 16` | 256 |

> 单进程 CPU 撞满 ~106%（1 核）说明触到 GIL，加 `--procs`（多进程）而非加 `--threads`。

### 测试
```bash
cd tools/queue-mover/python
python3 -m pytest test_queue_mover.py -q     # 15 个用例，注入 fake，不触达 AWS
```

### 参数
| 参数 | 默认 | 说明 |
|------|------|------|
| `--src-queue` / `--dst-queue` | 必填 | 源/目标队列完整 URL，相同会被拒绝 |
| `--max` | 100000 | 本次最多搬运条数（每次启动独立计数；多进程时平分） |
| `--procs` | 1 | 并发进程数（绕 GIL） |
| `--threads` | 8 | 每进程并发线程数 |
| `--log-file` | — | 日志落盘路径（控制台同时输出，append 不覆盖） |
| `--progress-every` | 5000 | 每搬运 N 条打一条进度日志 |
| `--region` | `$AWS_REGION` | AWS region |
| `--dry-run` | 关 | 只预览不发送不删除 |

---

## Go 版

### 构建
```bash
cd tools/queue-mover/go

# x86_64 Linux（Intel/AMD 机型）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o queue-mover .

# ARM64 Linux（Graviton 机型 —— 报 "exec format error" 时用这个）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o queue-mover .
```
> 用 `uname -m` 确认堡垒机架构：`x86_64` → amd64，`aarch64` → arm64。

### 运行
```bash
# dry-run 预览
./queue-mover --src-queue <A队列URL> --dst-queue <B队列URL> \
    --region eu-south-2 --max 5 --dry-run

# 正式搬运（region 内堡垒机推荐 workers=256）
./queue-mover --src-queue <A队列URL> --dst-queue <B队列URL> \
    --region eu-south-2 --max 1000000 --workers 256 \
    --log-file mover.log --progress-every 50000
```

`--workers` 直接 256 起步（goroutine 不吃核，2x/4x/8x 机器都扛得住）。加 workers 速率不再涨 = 已撞 SQS 服务端限速，属正常。

### 测试
```bash
cd tools/queue-mover/go
go test ./...     # 10 个用例，sqsAPI interface 注入 fake
go vet ./...
```

### 参数
| 参数 | 默认 | 说明 |
|------|------|------|
| `--src-queue` / `--dst-queue` | 必填 | 源/目标队列完整 URL，相同会被拒绝 |
| `--max` | 100000 | 本次最多搬运条数（每次启动独立计数） |
| `--workers` | 64 | 并发 goroutine 数（= 总并发管道数） |
| `--log-file` | — | 日志落盘路径（控制台同时输出，append 不覆盖） |
| `--progress-every` | 5000 | 每搬运 N 条打一条进度日志 |
| `--region` | `$AWS_REGION` | AWS region |
| `--dry-run` | 关 | 只预览不发送不删除 |

---

## 查询队列地址

```bash
# 按集群名前缀列出全部队列地址（含主队列与 DLQ）
aws sqs list-queues --region eu-south-2 \
    --queue-name-prefix gcs-2-s3-worker --query QueueUrls --output text | tr '\t' '\n'

# 已知队列名直接查地址（主队列名 = <栈名>-queue）
aws sqs get-queue-url --region eu-south-2 \
    --queue-name <栈名>-queue --query QueueUrl --output text
```

## 搬运前后守恒校验

```bash
# A 减少数应等于 B 增加数
aws sqs get-queue-attributes --region eu-south-2 --queue-url <A队列> \
    --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
    --query Attributes --output text
aws sqs get-queue-attributes --region eu-south-2 --queue-url <B队列> \
    --attribute-names ApproximateNumberOfMessages --query Attributes --output text
```

## 性能参考（2026-06-11 实测）

| 版本 | 位置 | 并发 | 吞吐 |
|------|------|------|------|
| Go | 跨洋办公网 | 64 goroutine | 273 条/秒 |
| Go | 跨洋办公网 | 256 goroutine | 1,409 条/秒 |
| Python | region 内跳板机 | 6 进程 × 16 线程 | ~13,500 条/秒 |
| Go | region 内跳板机（推算） | 256 goroutine | ~1.3 万~2 万 条/秒 |

瓶颈是到 SQS 的网络往返（每 10 条消息 3 次串行 API）+ SQS 服务端限速 —— **务必在与队列同 region 的堡垒机上运行**。计费：每条消息 ≈ 3 个 SQS 请求 ≈ $1.2/百万条。

## 注意事项

- 工具不校验队列与集群的对应关系，src/dst 写反会把 B 的积压搬进 A —— 执行前先 `--dry-run` 看 body 里的 source 前缀确认方向。
- 消息原样投递，不改写任何字段；两集群需配置兼容（相同 rclone remote 名、目标桶 IAM 覆盖）。
- 批量恒为 10 条/批（SQS API 硬上限），并发杠杆只有 `--procs`/`--threads`（Python）或 `--workers`（Go）。
- 千万级以上重分配的更优解：上游 feeder 直接改投 B 队列，本工具只搬存量。
