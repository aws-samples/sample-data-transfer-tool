# 运维脚本（在 ops EC2 上运行）

`migops.sh` —— 迁移集群运维一体化脚本。**运维人员无需打开 CloudWatch dashboard，一条命令即可拿到全部信息。**

## 前提

- 在 **ops EC2**（`migration-ops` 栈）上运行：`aws ssm start-session --target <ops 实例 id>`。
- 代码已 clone 到 `/opt/migration`（`cd /opt/migration && sudo git pull` 拉最新）。
- 依赖 aws cli v2 + python3，复用 ops 实例自带的 IAM 权限（无需额外授权）。
- 区域默认 `eu-south-2`，可用 `AWS_REGION=xx` 覆盖。

## 用法

只输**栈名**（即集群名，如 `migration-primary`），所有资源名自动推导。

```bash
cd /opt/migration

# 总览（= 整个 dashboard 一屏：吞吐/CPU内存/队列/Worker/四态/429/限速）
bash ops/migops.sh status migration-primary

# 实时盯盘（每 10s 刷新一次 status）
bash ops/migops.sh watch migration-primary 10

# 单项查询
bash ops/migops.sh throughput migration-primary    # 吞吐 Gbps（下载/上传）
bash ops/migops.sh queue      migration-primary     # 积压 / in-flight / DLQ
bash ops/migops.sh states     migration-primary     # 四态/min（含 dashboard 缺的 FATAL）+ 429 信号
bash ops/migops.sh workers    migration-primary     # 活跃 worker 数 + 实例列表
bash ops/migops.sh ratelimit  migration-primary     # 当前限速（带宽+频率+开关）
bash ops/migops.sh errors     migration-primary 30  # 最近 30 分钟失败的 rclone 原始 cmd+stderr

# 业务查询
bash ops/migops.sh inspect migration-primary 's3:bucket/path/file.bin'   # 单对象传输历史

# 操作
bash ops/migops.sh set-gbps   migration-primary 30      # 调带宽限速目标为 30Gbps（off=关限速）
bash ops/migops.sh set-tps    migration-primary 5000    # 调频率限速总目标（off=不限）
bash ops/migops.sh dlq-replay migration-primary 10000   # DLQ 失败消息重投回主队列
```

## 命令一览

| 命令 | 作用 | 数据源 |
|------|------|--------|
| `status` | 总览（= 整个 dashboard） | CloudWatch + SQS + ASG + DDB + SSM |
| `watch` | 每 N 秒刷新 status，实时盯盘 | 同上 |
| `throughput` | 吞吐 Gbps（NetworkIn=源下载 / NetworkOut=S3上传） | CloudWatch EC2 |
| `queue` | 积压 / in-flight / DLQ 深度 | SQS |
| `states` | 四态 attempts/min + 源端 429 信号（**显式显示 FATAL，补 dashboard 缺的第 4 态**） | CloudWatch EMF |
| `workers` | 活跃 worker 数（心跳）+ ASG 实例列表 | DDB 心跳表 + ASG |
| `cpumem` | CPU% / 内存%（ASG 平均，内存需 CWAgent 已上报） | CloudWatch + CWAgent |
| `ratelimit` | 当前限速全量（带宽/频率/开关） | SSM |
| `inspect` | 单对象传输尝试历史（自动重算分片 PK 精确查） | DDB 状态表 |
| `set-gbps` | 调带宽限速目标 | SSM |
| `set-tps` | 调频率限速总目标 | SSM |
| `errors` | 最近失败的 rclone 原始 cmd+stderr | CloudWatch Logs |
| `dlq-replay` | DLQ 失败消息重投回主队列 | SQS（via migration_cli） |

## 设计要点

- **零学习成本**：一条命令一个动作，只需记栈名。
- **不靠命名拼接**：ASG 名从栈 Output `AgentASGName` 取，DLQ 从主队列 `RedrivePolicy` 反查（改名也不崩）。
- **吞吐换算与 dashboard 一致**：`NetworkIn/Out` 是 60s 周期累计字节，Gbps = `Sum/60×8/1e9`。
- **补 dashboard 短板**：dashboard 的四态 widget 用 SEARCH 表达式，只画有数据的 State；`states` 命令逐态查、显式列出 FATAL=0（健康信号）。
