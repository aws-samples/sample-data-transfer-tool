# go-worker —— 基于 rclone rcd 的 GCS/S3 → S3 迁移消费者（Go）

大规模（100PB 级）文件迁移工具的**传输侧消费者**。从 SQS 拉消息（一对象一消息
`{source, destination, op}`），驱动 rclone 完成 GCS/S3 → S3 传输，结果记 DynamoDB +
CloudWatch EMF。本工程**只消费 SQS**——填队列由上游系统负责。

## 为什么是 rcd + Go

传统做法对每条消息 fork 一个 `rclone` 进程。小文件（~100KB）下单条 ~200-400ms 里 <5%
是真实传输，95% 是**进程冷启动 + 两条全新 TLS 握手 + backend 初始化**——单机吞吐被钉在 ~50 QPS。

go-worker 用一个常驻 `rclone rcd` 守护进程承载所有传输，worker 通过 `operations/copyfile`
rc 调用提交。**连接池在 rcd 进程生命周期内复用**（实测：连续拷贝复用同一条到 S3 的 TLS
连接），消灭每文件 fork+握手。Go 单进程多 goroutine 取代多进程（无 GIL），单机内存十几 MB。

## 架构

```
systemd ──┬─ rclone-rcd.service   常驻 rcd（127.0.0.1:5572，全套 rclone flag + 全局限速）
          └─ go-worker.service    Go 单进程（Type=notify，依赖 rcd ready）

go-worker 进程内：
  RECEIVER_GOROUTINES 个 receiver ─批量拉 SQS→ channel(背压) ─→ WORKER_GOROUTINES 个 worker
     每个 worker：parse → 同步 operations/copyfile（阻塞到完成）→ 四态副作用
  + heartbeat(30s 写 DDB) + ratelimit(60s 读 SSM→设 rcd 全局) + watchdog(sd_notify) goroutine
```

**同步调用，不轮询**：worker goroutine 同步阻塞在 `copyfile` HTTP 调用上，请求携带带
`RCLONE_TIMEOUT` deadline 的 ctx。无 `_async`、无 job 轮询。

## 并发控制

| 旋钮 | 控制 | 在哪设 |
|---|---|---|
| `WORKER_GOROUTINES`（=CFN `WorkerThreads`）| 同时**提交/处理**多少传输 | go-worker env |
| rcd `--transfers`（=CFN `WorkerThreads`）| rcd 同时**执行**多少传输 | rcd 启动 flag |
| `--bwlimit` / `--tpslimit` | 带宽 / 每秒事务数（**rcd 全局**） | 控制器 Lambda 写 SSM，worker 经 rc 设 rcd 全局 |

⚠️ `WORKER_GOROUTINES` 必须 = `--transfers`（提交=执行对齐，否则 job 在 rcd 内排队）。
CFN 用单一参数 `WorkerThreads` 同时驱动两者。in-flight 约束 `MaxSize(449) × WorkerThreads ≤ 115000`。
N 从几十起步压测，看 GCS/S3 429 拐点找甜点。

> 限速是 **rcd 全局**（每台 EC2 一个 rcd，core/bwlimit + options/set 涵盖该机所有传输），
> 不是每传输一份。控制器 Lambda 按 **实例数** 分摊 fleet 目标（rcd 模式），不除以进程数。

## 防双写（HTTP 断开即中止）

单次传输最长 = `RCLONE_TIMEOUT_SECONDS`，**启动 fail-fast 校验 `RCLONE_TIMEOUT ≤ 0.7×VISIBILITY`**。
超时或停机 → ctx 取消 → HTTP 断开 → **rcd 的传输 context 随之取消，daemon 中止传输**（实测：
断开后 core/stats 字节冻结）。这是 killpg 的等价替身，无需 job/stop。

## 四态语义（only SUCCESS deletes）

| 状态 | SQS 动作 | 计数 |
|---|---|---|
| SUCCESS | 删消息 | ✓ |
| RETRYABLE | 改 visibility 分级退避重投（src_rate_limit=300s / 5xx=60s） | ✓ |
| FATAL | 立即重投(0)，烧满 3 次进 DLQ | ✓ |
| UNKNOWN | 立即重投(0) | ✗ |

`op`：`copy`（默认）/ `delete`（删目标对象，幂等）/ `refresh`（注入 `IgnoreTimes` 强制重传，
刷新源端 metadata-only 变更，CDC METADATA_UPDATE 用）。

## 目录结构

```
main.go                       入口（IMDS 取 instance-id、signal、拉起 app.Run）
internal/
  app/        配置(env) + 组装运行（Run/heartbeat/ratelimit/watchdog 接线）
  worker/     消费循环(consume) + 四态决策(process) + rcd 执行(runner)
  rcd/        rclone rcd HTTP 客户端（copyfile/deletefile/限速/stats）
  message/    SQS 消息解析 + endpoint 拆分
  model/      四态 + 传输契约
  classify/   错误文本 → error_class + 四态
  status/     DDB 终态记录 + 心跳 + make_pk（与 Python 字节级对拍）
  emf/        CloudWatch EMF（维度白名单，stdout）
  obslog/     分级日志（WARNING+ 落 worker-ops 文件 → CW）
  watchdog/   systemd sd_notify（READY=1 / WATCHDOG=1）
  ratelimit/  60s 读 SSM → 设 rcd 全局限速
lambda/
  ratelimit_controller.py   AIMD 限速控制器（Lambda，rcd 模式每-EC2 分摊）
deploy/       systemd unit（rclone-rcd / go-worker）+ env 模板
deployment-cfn-git.yaml      单集群 CloudFormation 模板（cfn-init 下载 release 二进制）
ops/          运维 CLI（migops.sh）
```

## 构建

```bash
go build ./...
go test ./...                 # 全部单测（make_pk 与 Python 字节级对拍 + 防双写命门）
# 交叉编译部署目标（必须匹配实例 CpuArch）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o go-worker-linux-amd64 .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o go-worker-linux-arm64 .
```

## 部署

CFN 模板 `deployment-cfn-git.yaml`（>51200 字节，须 `--template-url` 经 S3 部署或 console 上传）。
cfn-init 在 boot 时：① 按 `CpuArch` 从 GitHub release 下 `go-worker-linux-<arch>` 二进制；
② 从 `ArtifactsBucket` 下 rclone 二进制；③ 起 rclone-rcd + go-worker 两个 systemd unit。

**限速 Lambda 部署包**：zip `lambda/ratelimit_controller.py`（自包含，仅标准库）上传到
`ArtifactsBucket/<LambdaS3Key>`。默认 `LambdaS3Key` 指向 go 集群专用包
`ops/ratelimit-controller-goworker.zip`。

关键参数（详见模板 `Metadata.Interface`）：`InstanceType`（默认 c8in.8xlarge）、`CpuArch`、
`WorkerThreads`（并发=rcd --transfers）、`GoWorkerReleaseTag`、`RcloneS3Key`（须匹配 CpuArch）、
`RatelimitTargetGbps` / `RatelimitTpsTarget`（fleet 目标，控制器按实例数分摊）。

## 与原 Python worker 的语义对齐

- `make_pk = "<md5(source)%256>#<source>"`（`internal/status/pk_test.go` 锁 golden 值，字节级对拍）
- EMF 维度白名单 `{QueueType,ErrorClass,InstanceId,State,Op}`，stdout JSON → CW 抽取
- DDB 终态：source_hash/attempt_timestamp/started_at/state/transferred_bytes/elapsed_seconds/
  speed_bps/error_class/error_message/message_body（失败存 body）
- 分级日志：WARNING+ → worker-ops 文件 → CW worker-ops 组；EMF → worker-emf 文件 → CW worker 组
- 修复了 Python 的 `worker_oom` 误判（`\bsignal\b` 把优雅 SIGTERM 误判 OOM、掩盖 429）：
  `internal/classify` 收窄信号正则、429 优先，`TestWorkerOOMMisjudgmentFixed` 用真实 stderr 锁定
