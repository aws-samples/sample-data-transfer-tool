<div align="center">

# GCS / S3 → S3 大规模迁移工具

**面向 100PB 级作业的分布式文件迁移**
SQS 工作队列 · EC2 worker 机群（`rclone`）· 自适应限速 · 纯 CloudWatch 监控

![Python](https://img.shields.io/badge/Python-3.12-3776AB?logo=python&logoColor=white)
![AWS](https://img.shields.io/badge/AWS-CloudFormation-FF9900?logo=amazonaws&logoColor=white)
![rclone](https://img.shields.io/badge/rclone-transfer-3F87C2)
![tests](https://img.shields.io/badge/tests-449%20passing-2EA043)
![coverage](https://img.shields.io/badge/coverage-94%25-2EA043)
![license](https://img.shields.io/badge/license-MIT--0-blue)

</div>

---

## 这是什么

把海量对象从 **GCS 或 S3** 迁到 **AWS S3** 的分布式工具。上游把"待迁移对象"逐个写进 SQS（一对象一条消息），一队 EC2 worker 并发消费、调 `rclone` 完成传输，全程自适应限速、四态可靠重试、CloudWatch 可观测。

```
        ┌─ 一个 CloudFormation 栈（栈名 = 集群身份，如 migration-primary）─────────────┐
        │                                                                          │
 上游 ──▶│  SQS 队列 ──▶ ASG worker 机群（每台 N 进程 × M 线程，跑 rclone）             │──▶ 目标 S3
 feeder │      ▲              │                                                    │
        │      │ 12h 可见性    ▼ 四态：成功删 / 可重试留 / 致命删 / 未知留              │
        │   DLQ（3 次失败）   DynamoDB 状态+心跳 · 日志组 · AIMD 限速 Lambda · Dashboard │
        └──────────────────────────────────────────────────────────────────────────┘
                         ▲                                          ▲
                    GCS（HMAC，S3 兼容端点）                    目标 AWS S3
```

#### 三句话理解架构

1. **工具只消费 SQS**：谁往队列里塞消息（feeder）与本工具解耦；本工具负责把队列里的对象高速、可靠地搬到目标 S3。
2. **一个模板 = 一套完整独立集群**：每个 CloudFormation 栈自带 SQS / DynamoDB / 日志组 / 限速 Lambda / Dashboard / IAM / ASG，**零共享**。栈名即集群身份。
3. **要 N 个隔离集群就部署 N 次**：按业务线 / 数据源 / 优先级分流到多条互不干扰的传输通道，各栈独立观测、扩缩、删除。

| 能力 | 说明 |
|------|------|
| 🎯 **规模** | 100PB 级；单集群可扩至 ~115k 并发 in-flight |
| 🚦 **限速** | 每栈一个 AIMD 闭环，自动收敛到目标带宽、防源端 429 |
| 🔀 **多栈隔离** | 一套代码、一个模板，部署 N 次得 N 个完全隔离的集群 |
| 🛡️ **可靠性** | 四态语义 + 12h 可见性 + 优雅退出 + 进程组 kill 防双写 |
| 📊 **可观测** | EMF 指标 + 每栈一个 Dashboard（无告警、无 SNS） |

---

## 目录

1. [架构与资源构成](#1-架构与资源构成)
2. [并发模型与调优](#2-并发模型与调优)
3. [部署前置：客户网络](#3-部署前置客户网络)
4. [部署前置：GCS 凭证](#4-部署前置gcs-凭证)
5. [部署步骤](#5-部署步骤)
6. [参数说明（18 个）](#6-参数说明18-个)
7. [消息格式与四态语义](#7-消息格式与四态语义)
8. [运维与监控](#8-运维与监控)
9. [自适应限速](#9-自适应限速)
10. [清理资源](#10-清理资源)
11. [开发](#11-开发)

---

## 1. 架构与资源构成

**一个栈 = 一套完整独立的集群。** 所有资源以栈名 `${StackName}` 为前缀（无 cluster 中缀）。要 N 个集群就把这套结构部署 N 次（栈名不同），栈与栈之间零共享。

### 一套集群包含哪些资源

| 资源 | 物理名 | 关键设计 |
|------|--------|---------|
| **SQS 队列 + DLQ** | `${StackName}-queue`（+`-queue-dlq`） | `VisibilityTimeout=43200s（12h）` 容纳 GB 级长传；失败 3 次（`maxReceiveCount`）自动转 DLQ |
| **DynamoDB 状态表** | `${StackName}-transfer-status` | PK = `md5(source)%256#source`（256 分片防热点）+ SK = `attempt_timestamp`；每次尝试一行 |
| **DynamoDB 心跳表** | `${StackName}-heartbeat` | PK = `instance_id#worker_index`；TTL 300s 判活 |
| **日志组 ×2** | `/migration/${StackName}/worker`（EMF）+ `/worker-ops`（运维） | 前者纯 EMF JSON 自动抽指标；后者运维日志 + DIAG 行 + 错误堆栈 |
| **ASG + LaunchTemplate** | `${StackName}-AgentASG` | 网络优化型 EC2，boot 时 `git clone` 代码，systemd 拉起 `WorkerCount` 个 worker 进程 |
| **IAM role + 安全组** | `${StackName}-agent-role` / `-worker-sg` | 写权限按 `TargetBucketPrefix` 划定；SG 纯出站 |
| **限速控制平面** | `${StackName}-ratelimit-controller`（Lambda） | 带宽 AIMD + 频率分摊 + EventBridge（每分钟）+ SSM `/migration/${StackName}/ratelimit/*`（6 参数，详见 §9） |
| **Dashboard** | `${StackName}-dashboard` | 本集群 NIC 吞吐 / 队列 / DLQ / 实例 / CPU / 内存 / 四态分布 |

### 多栈如何隔离

| 维度 | 说明 |
|------|------|
| **集群身份** | = 栈名。栈 `migration-primary` → 资源 `migration-primary-queue`、`/migration/migration-primary/...` |
| **隔离粒度** | 每栈自带全套资源，**栈间零共享**。某栈故障 / 扩缩 / 删除互不影响 |
| **流量路由** | 上游 feeder 把消息投到**目标集群的队列**即决定由哪个集群处理 |
| **唯一可共享项** | 栈外预建的 GCS HMAC secret（多栈可传同一 ARN）；EMF 命名空间 `GcsS3Migration`（429 是源端总体信号，跨栈共用一个 namespace） |

> [!NOTE]
> - **目的地由消息决定**：每条消息的 `destination` 字段决定写到哪个桶/路径；`TargetBucketPrefix` 只用前缀通配划定 IAM 写权限边界。
> - **一套代码服务所有栈**：worker 不感知集群，`QUEUE_URL` / `DYNAMODB_TABLE` / 限速参数路径等全由 systemd 单元注入。多部署一个集群只多一次 `create-stack`，无需改代码。

### 部署配置基线（实测，eu-south-2）

经压测验证的多栈示例：每个栈一次 `create-stack`、一份 params 文件，可按业务异构。

| 栈名 | 实例 | 进程×线程 | 限速目标 | 定位 |
|------|------|----------|---------|------|
| `migration-primary` | `m6in.8xlarge`（x86_64, 32 vCPU） | 16 × 5 | 400 Gbps | 通用主力（甜区配置，见 §2） |
| `migration-standby` | `m6in.8xlarge` | 32 × 8 | 100 Gbps | 小文件优化（进程数≈vCPU 数，最大化绕 GIL） |
| `migration-reserve` | `m6in.8xlarge` | 16 × 5 | 0（不限速） | 备用 |

> **大文件 vs 小文件优化方向相反**：小文件 CPU 受限（rclone 每对象 fork 开销吃满核），靠**进程数≈vCPU 数**绕 GIL；大文件网络受限，少而精的并发 + multipart 即可。
> **取消限速两种等效写法**：`limit-enabled=false`（总开关）或 `target-gbps=0`（控制器输出 `bwlimit=off`）。

---

## 2. 并发模型与调优

每台 EC2 跑 **`WorkerCount` 个独立 worker 进程**（systemd 模板单元 `rclone-agent@0..N-1`），每进程内 **`WorkerThreads` 个竞争消费者线程**。

| 关注点 | 说明 |
|--------|------|
| **为什么多进程** | 单进程几百线程受 Python GIL 限制只吃满 1 核；多进程让每进程独占 GIL，吃满多核。小文件场景进程数取 ≈vCPU 数 |
| **进程隔离** | 心跳表主键用 `instance_id#<WORKER_INDEX>`（`%i` 注入），互不覆盖 |
| **in-flight 硬约束** | `MaxSize × WorkerCount × WorkerThreads ≤ 115000`（SQS 单队列 in-flight 上限 120k 留余量）。调大并发须同步调小 `MaxSize` |
| **限速进程数计数** | 控制器按本栈 ASG `InService × WorkerThreads` 算并发进程数 |

### `WorkerThreads` 调优实测（单台 `m6in.8xlarge`，32 vCPU / 123 GB）

测试条件：S3→S3、单对象 254–358 MB、消息带 `--disable copy` 强制走**下载+上传**（模拟真实跨云负载，数据真正过 worker；同桶同区不加此参数会走 server-side copy，CPU/内存/网卡 ≈ 0，测不出负载）。固定 `WorkerCount=16`，只调 `WorkerThreads`：

| 配置 | 并发 | CPU | load(1min) | 内存 | 吞吐 | 判定 |
|------|------|-----|-----------|------|------|------|
| 16×12 | 192 | 100%（id 0） | 514 | 28 GB | ~1180/min | 过度超配，多出的进程只在排队抢 CPU |
| **16×5** | **80** | 100%（id 0） | **210** | 15 GB | **~1150/min** | **甜区：满血吞吐 + load 健康** |
| 16×4 | 64 | 100%（id 0） | 114 | 10 GB | ~1100/min | 性价比临界，load 更低、吞吐微降 |
| 16×3 | 48 | **id 3.1%** | 58 | 9 GB | ~1095/min | 越过拐点，CPU 喂不满、吞吐下滑 |

**结论：**

- **甜区 = 16×5（80 并发）**：CPU 刚好打满、load 健康、吞吐满血。`192` 过度超配（吞吐不增反平），`48` 已喂不满。
- **真瓶颈是 CPU + 网卡，不是并发数也不是内存**：各档网卡均 ~83 Gbps（双向，撞 `m6in.8xlarge` 50 Gbps 基线突发上限），内存最多用 23%（`m6in` 内存优化机型对此负载偏浪费，计算优化型 `c6in` 更匹配）。`wa=0` 印证 rclone remote→remote 纯内存流式、**不落盘**。
- **`--s3-chunk-size` 调大不能提吞吐**：实测 358 MB 文件 chunk 64M→256M，分片 6→2、并发度下降、单文件耗时 2.3s→6.1s 反而更慢；内存峰值恒 ~430 MB 不随 chunk 变。当前 `64M` 已最优，勿调大。

> 以上是该机型 + 该文件尺寸的实测甜区。换机型 / 文件尺寸分布须重测——小文件取进程数≈vCPU 数，GB 级大文件并发宜少而精。

---

## 3. 部署前置：客户网络

> [!WARNING]
> **本栈不创建任何网络资源。** VPC、子网、路由、NAT/IGW、全部 VPC Endpoint 都须**预先建好**，经 `VpcId` / `WorkerSubnetIds` 传入（多栈可共用）。本栈只创建自己的一个 worker 安全组。不满足下列任一条，worker 会启动失败或产生意外公网流量费。

### VPC

- 必须**已开启 `EnableDnsSupport` + `EnableDnsHostnames`**（Interface Endpoint 私有 DNS 解析的硬前提）。
- 经 `VpcId` 传入，多栈可共用。

### Worker 子网（`WorkerSubnetIds`，建议跨 2+ AZ）

每个子网须同时满足：

- **能出公网下载 GCS**：经 NAT Gateway（私有子网）或 IGW（公有子网）。GCS 走公网 HTTPS。
- **路由表已关联 S3 / DynamoDB Gateway Endpoint**：否则 S3 上传 + DynamoDB 写入走公网，产生可观的 NAT/跨区流量费（100PB 级尤其致命）。

### VPC Endpoint（预先创建，本栈不建）

| 类型 | 服务 | 用途 | 关键配置 |
|------|------|------|---------|
| Interface | `sqs` | worker 收消息 | 启用私有 DNS |
| Interface | `secretsmanager` | 读 GCS HMAC secret | 启用私有 DNS |
| Interface | `logs` | EMF 指标日志上送 | 启用私有 DNS |
| Interface | `monitoring` | CloudWatch 指标 | 启用私有 DNS |
| Gateway | `s3` | 目标 S3 上传 | 关联到 worker 子网路由表 |
| Gateway | `dynamodb` | 状态/心跳写入 | 关联到 worker 子网路由表 |

> SSM（限速参数 + 实例管理）走**公网端点** + worker SG 443 出站，无需 Interface Endpoint。如要求 SSM 走私网，可额外建 `ssm`/`ssmmessages`/`ec2messages`（可选）。

### Endpoint 安全组入站放行

4 个 Interface Endpoint 的安全组须**入站放行 worker 的 TCP 443**：

- **简单（测试）**：放行 worker 子网所在 VPC CIDR（如 `10.20.0.0/16`）的 443。一条规则覆盖所有栈。
- **收紧（生产）**：放行各栈 worker SG 的 443（部署后从 Output `WorkerSecurityGroupId` 取 SG ID 逐条加）。
  > ⚠️ 收紧后，**删某栈前必须先删掉引用其 worker SG 的规则**，否则 worker SG 无法删除，删栈卡在 `DELETE_IN_PROGRESS`。

Gateway Endpoint（S3/DynamoDB）不受安全组约束。worker SG（`${StackName}-worker-sg`）设计为纯出站：入站全关，出站只放 443。

### 固定出站 IP（可选，EIP tag 池）

若源端（GCS）按 **IP 白名单**放行，需要 worker 出站走一组固定的连续公网 IP。做法：客户预先申请一段**连续的 Elastic IP**、打上统一 tag，部署时把 tag 传给本栈，每台 worker boot 时从池里挑一个空闲 EIP 关联（替换 VPC 自动公网 IP），出站即走该 EIP。

| 项 | 说明 |
|----|------|
| **启用条件** | `EipPoolTagKey` + `EipPoolTagValue` **都填**才启用；都空则用 VPC 自动公网 IP（默认，向后兼容） |
| **生效前提** | worker 子网必须**直连 IGW**（公有子网，路由 `0.0.0.0/0 → igw`）。⚠️ 若走 NAT，出站源 IP 是 **NAT 网关的 EIP**，给实例挂 EIP 对出站无效——此时应给 NAT 网关挂固定 EIP，白名单填 NAT 的 IP |
| **池容量** | 池大小必须 **≥ ASG MaxSize**（建议 1.2× 留冗余）。worker 拿不到池 EIP 会 **boot 失败**（cfn-signal 报错、实例不健康），绝不用非白名单 IP 出站 |
| **栈不管 EIP 生命周期** | 客户预建 + 打 tag；本栈只**关联**（`AssociateAddress`），不创建、不释放。实例终止时 EIP 自动解关联回池 |
| **IAM** | 仅启用时给 worker role 加 `ec2:DescribeAddresses` + `ec2:AssociateAddress`（不授 `AllocateAddress`，杜绝孤儿 EIP 泄漏） |

**防乱抢 + 抗 API 风暴**（已实测：2 台并发 + 池零冗余，各拿一个零冲突）：

- **正确性**：用 `--no-allow-reassociation`（AWS 服务端原子操作）——EIP 已被占用时关联**直接失败**，绝不会抢走别人的。100 台同抢一个，只 1 台成功、其余试下一个，**永远不会两台共用**。
- **抗风暴**：一次性拉起大批量（如 100 台）时三层加固——**boot 抖动**（关联前 `sleep 0~60s` 随机错峰，消解 thundering herd）+ **限流退避**（命中 `RequestLimitExceeded` 指数退避，命中"已占用"立即试下一个）+ **每轮重新 describe**（不拿过时快照狂撞）。

> [!NOTE]
> **ASG 扩容不会自动错峰 boot**：设 `DesiredCapacity=N` 会**一次性同时**起 N 台（`MaxBatchSize` 只管 instance refresh，不管扩容；Warm Pool 也是批量转入）。防 EIP 抢占风暴**只能靠 worker 端 boot 抖动**（已内置）。大批量扩容时可额外**分批调 Desired**（如 0→50→100，间隔几分钟）作为保险；若改用动态伸缩策略（按队列积压自动扩），ASG 天然逐步加，抖动 + 退避足够兜底。

---

## 4. 部署前置：GCS 凭证

GCS 通过其 **S3 兼容（XML API）端点** `https://storage.googleapis.com` 访问，认证用 **HMAC 密钥**（对应一个授了 `roles/storage.objectViewer` 的 SA）。rclone.conf 的 `[gcs]` 后端配成 `type=s3, provider=GCS`。

> [!IMPORTANT]
> **为什么用 HMAC 而非原生 `google cloud storage` 后端**：实测原生后端会**规整对象 key 里的斜杠**（连续 `//`、前导 `/` 被折叠），导致这类对象 `object not found`；S3 兼容 XML API **保留字面 key**，能正确迁移含特殊斜杠的对象（已端到端验证）。HMAC 还顺带让 `--metadata` 生效（原生后端不支持），源对象的 Content-Type / 时间戳 / 自定义 metadata 完整搬运。

**凭证流（栈外预建 secret，传 ARN，多栈可共用）：**

1. GCP Cloud Storage → Settings → Interoperability，为授了 `objectViewer` 的 SA 创建 HMAC 密钥（`GOOG...` access key + secret）。
2. 部署前在 Secrets Manager 建好 secret，拿到 ARN：

   ```bash
   aws secretsmanager create-secret --region <region> \
     --name gcs-hmac-shared \
     --secret-string '{"access_key_id":"GOOG...","secret_access_key":"<secret>"}'
   # 输出里的 ARN 即下一步要传的 GcsHmacSecretArn
   ```

3. 把 ARN 作为参数 `GcsHmacSecretArn` 传入栈（多栈可传同一个）。CFN 只引用、不创建、不删除。
4. cfn-init 在每台 worker 上读该 secret，写进 `~/.config/rclone/rclone.conf` 的 `[gcs]` 段。worker role 只被授予对**这一个 secret ARN** 的 `GetSecretValue`。

> 消息里 `source` 用 `gcs:` 前缀（对应 `[gcs]` 后端），如 `gcs:my-bucket/path/file.bin`——feeder 与消息格式零改动。
> ⚠️ 含真实 HMAC 的 `deploy-params-*.json` 必须 gitignore。committed 占位见 `deploy-params-single-cluster.json.template`。

---

## 5. 部署步骤

> 模板 `deployment-cfn-git.yaml` >51KB，Console 必须**上传模板文件**（自动转存 S3），不能直接粘贴。

| 步骤 | 操作 |
|------|------|
| **1. 确认机型可用** | CFN 默认 `m6in.8xlarge`/`x86_64`。换机型须同步改 `CpuArch`（m6in/c6in=x86_64，c7gn/c8gn=arm64）。`aws ec2 describe-instance-type-offerings --filters Name=instance-type,Values=<type>` 验证区域可用 |
| **2. 备好网络** | 按 §3 备 VPC、worker 子网、6 个 VPC Endpoint、Endpoint SG 入站放行。记下 `VpcId` 和子网 ID |
| **3. 预建 GCS secret** | 按 §4 生成 HMAC + `create-secret`，记下 ARN（多栈共用） |
| **4. 打包限速 Lambda** | 见下方命令，拿到 `ArtifactsBucket` + `LambdaS3Key` |
| **5. 创建堆栈** | Console 上传模板 → 填 18 个参数（§6）→ 勾选 IAM 确认 → 提交，等 `CREATE_COMPLETE`（约 10–15 分钟） |
| **6. 部署后** | 见下方"部署后必做" |

### 步骤 4：打包上传限速 Lambda

限速控制器 Lambda 的代码 = 单文件 `src/migration/ratelimit_controller.py`，打成 zip 上传到 `ArtifactsBucket`（多栈可共用）：

```bash
ART_BUCKET=my-ops-artifacts-bucket          # 运维桶（栈外预建）
REGION=eu-south-2

# 1) 打包（zip 内文件名须为 ratelimit_controller.py，匹配 Handler=ratelimit_controller.handler）
cd src/migration && zip -j /tmp/ratelimit-controller.zip ratelimit_controller.py && cd -

# 2) 上传（key 带内容 hash → 改代码 key 变，CFN 才检测到并更新 Lambda）
SHA=$(shasum -a 256 src/migration/ratelimit_controller.py | cut -c1-12)
KEY="migration/ratelimit-controller-${SHA}.zip"
aws s3 cp /tmp/ratelimit-controller.zip "s3://${ART_BUCKET}/${KEY}" --region $REGION

echo "ArtifactsBucket=$ART_BUCKET  LambdaS3Key=$KEY"   # ← 填进 params
```

> 改了控制律重新部署：重跑两步拿新 `LambdaS3Key`，填进 params → `update-stack` 即生效（无需 instance refresh）。
> 可选自编译 rclone：`aws s3 cp ./rclone s3://${ART_BUCKET}/migration/rclone`，把 key `migration/rclone` 填进 `RcloneS3Key`（与 `LambdaS3Key` 同格式、同一个 `ArtifactsBucket`；改它需 instance refresh）。

### 部署后必做

1. **Endpoint SG 收紧模式**：从 Output `WorkerSecurityGroupId` 取 SG ID，在 4 个 Interface Endpoint 的 SG 上各加入站 `TCP 443, Source=<worker SG>`。
2. **目标桶生命周期**：对每个目标桶加 `AbortIncompleteMultipartUpload(DaysAfterInitiation=1)`（否则被 kill 的 rclone 残留的未完成 multipart 持续计费）。命令见 Output `TargetBucketLifecycleReminder`。
3. **更新代码后滚动实例**：CFN 改 LaunchTemplate / cfn-init Metadata（含 `WorkerCount`/`WorkerThreads`/`RcloneS3Key`）**不会重启在跑实例**，须 `aws autoscaling start-instance-refresh --auto-scaling-group-name <Output AgentASGName>`（每栈各滚）。`GitBranch` 须指向**已推送**的分支。而 `RatelimitTargetGbps`（SSM 资源）`update-stack` 即时生效，无需滚动。

### CLI 部署（替代 Console）

```bash
# 模板先上传 S3
aws s3 cp deployment-cfn-git.yaml s3://<bucket>/<key>
# 每个集群各 create-stack 一次（换栈名 + 换 params 文件）
aws cloudformation create-stack --region <region> \
  --stack-name migration-primary \
  --template-url <s3-url> \
  --parameters file://params-primary.json \
  --capabilities CAPABILITY_NAMED_IAM
```

---

## 6. 参数说明（18 个）

模板含 `AWS::CloudFormation::Interface`，Console 按 6 组显示——顺序即"部署准备顺序"。⚠️ = 必填无默认。

**组 1 — 网络（客户提供）**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `VpcId` ⚠️ | `VPC::Id` | — | 客户 VPC（须开 DNS 支持+主机名），多栈可共用 |
| `WorkerSubnetIds` ⚠️ | `List<Subnet::Id>` | — | worker 子网（跨 2+ AZ），多栈可共用 |
| `EipPoolTagKey` | String | `''` | 可选。预建 EIP 池的 tag 键。与 `EipPoolTagValue` **都填**才启用 EIP 关联 |
| `EipPoolTagValue` | String | `''` | 可选。预建 EIP 池的 tag 值。详见 [§3 固定出站 IP](#固定出站-ip可选eip-tag-池) |

**组 2 — 栈外预建资源**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `GcsHmacSecretArn` ⚠️ | String | — | 栈外预建的 GCS HMAC secret ARN。多栈可共用；CFN 只引用 |
| `ArtifactsBucket` ⚠️ | String | — | 运维/制品桶：限速 Lambda zip（必需）+ 可选自编译 rclone。栈外建，可共用 |
| `LambdaS3Key` ⚠️ | String | — | 限速 Lambda zip 在 `ArtifactsBucket` 里的 key（见步骤 4） |

**组 3 — 本集群：目标与限速**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `TargetBucketPrefix` ⚠️ | String | — | 本栈目标桶名前缀（通配）。IAM 写权限 = `arn:aws:s3:::<prefix>*` |
| `RatelimitTargetGbps` | String | `300` | 本栈目标总吞吐（AIMD 收敛目标；`0`=不限速） |
| `RatelimitTpsTarget` | String | `10000` | 全 fleet 总 transaction/秒目标（`off`=不限）。控制器每 60s ÷ 进程数算每进程 `--tpslimit`。可部署后改 SSM 动态调 |

**组 4 — 计算容量**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `InstanceType` | String | `m6in.8xlarge` | 网络优化型，须匹配 `CpuArch` |
| `CpuArch` | String | `x86_64` | m6in/c6in=x86_64，c7gn/c8gn=arm64 |
| `InstanceCount` | Number | `1` | 本栈 ASG DesiredCapacity |
| `WorkerCount` | Number | `16` | 每台 worker 进程数（绕 GIL）。小文件取≈vCPU 数 |
| `WorkerThreads` | Number | `16` | 每进程线程数。硬约束 `MaxSize(449) × WorkerCount × WorkerThreads ≤ 115000`（甜区实测见 §2） |
| `RootVolumeSize` | Number | `100` | 根 EBS（GiB） |

**组 5 — 代码来源**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `GitRepoUrl` | String | `…/sample-data-transfer-tool.git` | cfn-init clone 的仓库 |
| `GitBranch` | String | `release/gcs-to-s3` | clone 的分支/tag/commit（**须已推送**） |
| `RcloneS3Key` | String | `''` | 可选自编译 rclone 在 `ArtifactsBucket` 内的 key（与 `LambdaS3Key` 同格式、同桶，如 `migration/rclone-crc32c`）。空=官网 install.sh。**改它需 instance refresh** |

**组 6 — 可选**

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `KeyPairName` | String | `''` | SSH 密钥对（空=不挂） |
| `ProjectTag` | String | `gcs-s3-migration` | 成本分配 + 批量清理标签 |

---

## 7. 消息格式与四态语义

### 消息格式

发送到 SQS 的消息是 JSON，一对象一条。`source`/`destination` 是 **rclone remote 路径**（`<remote>:<桶>/<对象key>`），remote 名对应 worker `rclone.conf` 的 section：`gcs`（GCS HMAC 端点）、`s3`（目标 S3，`env_auth`）。

```jsonc
// 示例 1 — GCS → S3（最简 copy）
{ "source": "gcs:my-bucket/path/obj.bin",
  "destination": "s3:dest-bucket/migrated/path/obj.bin" }

// 示例 2 — S3 → S3
{ "source": "s3:src-bucket/file.bin",
  "destination": "s3:dest-bucket/copied/file.bin" }

// 示例 3 — 带 CRC32C 校验和（需自编译支持 CRC32C 的 rclone，见 RcloneS3Key）
{ "source": "gcs:my-bucket/obj.bin",
  "destination": "s3:dest-bucket/migrated/obj.bin",
  "rclone_args": ["--header-upload", "x-amz-checksum-algorithm: CRC32C; x-amz-checksum-type: FULL_OBJECT"] }

// 示例 4 — 删除目标对象（迁移后清理）
{ "op": "delete", "destination": "s3:dest-bucket/migrated/obj.bin" }

// 示例 5 — 强制重传刷新 metadata（CDC METADATA_UPDATE；源端只改了 metadata 也重传）
{ "op": "refresh",
  "source": "gcs:my-bucket/path/obj.bin",
  "destination": "s3:dest-bucket/migrated/path/obj.bin" }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `source` | String | copy/refresh ✅ / delete ❌ | rclone 源路径（`gcs:` 或 `s3:`） |
| `destination` | String | ✅ | rclone 目标路径（`s3:`）。桶名须落在 `TargetBucketPrefix` 范围内 |
| `op` | String | ❌ | `copy`（默认可省，`rclone copyto`）/ `delete`（`rclone deletefile`）/ `refresh`（`rclone copyto --ignore-times`，强制重传刷新 metadata，须带 source，代价=重传全量数据） |
| `rclone_args` | Array | ❌ | 额外 rclone 参数（白名单过滤，拒绝含 `\0`/`\n`/`\r` 的值） |

> **发送命令**：
> ```bash
> aws sqs send-message --region eu-south-2 \
>   --queue-url https://sqs.eu-south-2.amazonaws.com/<acct>/migration-primary-queue \
>   --message-body '{"source":"gcs:my-bucket/data/file.bin","destination":"s3:dest-.../data/file.bin"}'
> ```
> 高速批量灌数据见 `bench/feed_s3.py`（S3→S3）/ `bench/feed_gcs.py`（GCS 源）。
> 上传分片由 rclone 按对象真实大小自动决定（`--s3-upload-cutoff 100M`）：>100MB 自动 multipart，≤100MB 单 PUT。

#### 经端到端验证的消息行为

| 消息形态 | 结果 | 说明 |
|----------|------|------|
| 正常 copy（大/小文件） | SUCCESS | 大文件走 multipart |
| `op=delete` 删已存在对象 | SUCCESS | 依赖 `s3:DeleteObject` 权限 |
| `op=delete` 删不存在对象 | SUCCESS | 幂等（not-found 判成功，不进 DLQ） |
| `op=refresh` 刷新已存在对象 | SUCCESS | 强制重传整对象（`--ignore-times`），单测覆盖 |
| `op=refresh` 源对象不存在 | FATAL | 降级 `src_not_found` 进 DLQ（与 copy 同盲点防护，非静默 SUCCESS） |
| 非法 JSON / 缺 `destination` | FATAL | 当 poison：DDB 记 `poison:<md5>`，删消息不卡队列 |
| `rclone_args` 含非白名单 flag / 注入串 | SUCCESS | 非白名单参数（含 `; rm -rf /`）被静默丢弃，命令注入防御生效 |
| copy/refresh 源对象不存在 | FATAL | rclone 对缺失单源退出 0 + "nothing to transfer"，已降级 `src_not_found` 进 DLQ（非静默 SUCCESS）。**上游仍应保证 source 真实存在** |

### 四态语义（SQS 重试，刻意设计）

| 状态 | 含义 | SQS 副作用 | DynamoDB |
|------|------|-----------|----------|
| `SUCCESS` | 成功 | 删消息 | 记终态 + 计数 |
| `RETRYABLE` | 可重试（**429 / 配额 / 5xx / 网络中断 / 超时**） | **不删**，靠 12h 可见性过期自然重投，3 次后转 DLQ | 记 FAILED + 计数 |
| `FATAL` | 不可重试（参数错 / 404 / 权限） | 删消息（重试无益） | 记 FAILED |
| `UNKNOWN` | 真正崩溃/SIGKILL | **不删、不计数** | 记录便于排查 |

> [!IMPORTANT]
> **429/配额/5xx 归 RETRYABLE**：rclone 撞源端限流（如 GCS egress 配额）以退出码 1 退出，本会落 UNKNOWN；分类器据 stderr 识别瞬时错误后**升级 RETRYABLE**，使其被计数、自然重投、超限后进 DLQ。**高 RETRYABLE 速率 = 源端被限流，应调低限速。**
> 不要往失败路径加显式 retry / `ChangeMessageVisibility`，会破坏语义并在 DDB 重复计数。唯一例外是优雅停机（SIGTERM 把在飞消息可见性置 0 让别的 host 立即接管，用于 ASG 滚动）。

---

## 8. 运维与监控

> 填充队列是上游的职责（一对象一条消息）。本工具只消费。

### 运维 CLI

```bash
# env: AWS_REGION, QUEUE_URL, DYNAMODB_TABLE/HEARTBEAT_TABLE。CLI 指向哪个栈的队列/表即操作哪个集群。
QUEUE_URL=https://sqs.<region>.amazonaws.com/<acct>/migration-primary-queue \
  python3 -m migration.migration_cli inspect <source>        # 单对象尝试历史
QUEUE_URL=...migration-primary-queue-dlq \
  python3 -m migration.migration_cli replay --max 10000       # 该栈 DLQ 回主队列
python3 -m migration.migration_cli active-workers             # 活跃 worker 数（按心跳 TTL）

# 实例上 worker 日志（多进程，@<idx> 指定某进程，省略则全部）
sudo journalctl -u 'rclone-agent@*' -f
```

### 日志位置

| 日志 | 位置 | 内容 |
|------|------|------|
| 运维日志 | CW Log Group `/migration/${StackName}/worker-ops` | 运行日志 + 周期 `DIAG` 行（各进程 local_inflight / 四态计数 / delete 成败）+ rclone 失败的原始 cmd+stderr |
| EMF 指标原文 | CW Log Group `/migration/${StackName}/worker` | 纯 EMF JSON，含每条传输 State / ErrorClass |
| 单机本地 | 节点 `/var/log/migration/worker-ops-*.log` + `worker-emf-*.log` | 每进程一份，SSH 本地排障 |

> 跨机查 DIAG：CloudWatch Logs Insights → `/migration/${StackName}/worker-ops` →
> `fields @timestamp, @message | filter @message like /DIAG/ | sort @timestamp desc`

### 监控（纯 Dashboard，无告警无 SNS）

每栈一个 Dashboard `${StackName}-dashboard`（Output `DashboardUrl`）：NIC 吞吐（NetworkIn=GCS 下载 / NetworkOut=S3 上传）、队列积压/in-flight、DLQ、实例数、CPU/内存、四态分布、源端 429 信号。

数据源：`AWS/EC2 NetworkIn/Out`（按本栈 ASG 聚合）、`AWS/SQS`、`AWS/AutoScaling`、`CWAgent mem_used_percent`；四态/错误来自 EMF 命名空间 `GcsS3Migration`。`AttemptCount`（每 attempt 恒 1）配合 `State`/`ErrorClass` 维度看各态条数（`FileCount` 只计 SUCCESS）。

---

## 9. 自适应限速

限速分**两个正交维度**，都由每栈一个 `${StackName}-ratelimit-controller` Lambda（EventBridge 每分钟触发）计算，写入本栈 SSM；worker 每 1h 重读（限速已停用，2026-07-14 从 30s 降频省 SSM 调用），**仅在启动新 rclone 进程时**快照（在跑的传输保持原速率，避免中途变速）。SSM 不可读时 worker 一律退化为不限速（fail-safe，限速故障不阻断传输）。

| 维度 | rclone flag | 限什么 | 防什么 | 计算方式 |
|------|------------|--------|--------|---------|
| **带宽** | `--bwlimit` | 字节/秒 | 打满网卡 / 源端 egress 带宽配额（GCS 429-bandwidth） | **AIMD 闭环**（读 NetworkIn 反馈自动收敛） |
| **请求频率** | `--tpslimit` | transaction/秒 | 后端 QPS rate-limit（GCS 读 ~5000/s、S3 PUT ~3500/s·前缀 → 503） | **总目标 ÷ 进程数**（静态分摊，非反馈） |

### 9.1 带宽限速（`--bwlimit`，AIMD 闭环）

控制器每分钟读本栈 ASG 的 EC2 `NetworkIn`（换算 Gbps）+ EMF 的 `src_rate_limit` RETRYABLE 占比（429 率），按下列**决策优先级**（高 → 低）算出新的每进程 `--bwlimit`：

| 优先级 | 条件 | 动作 | 常量 |
|--------|------|------|------|
| 1 | `target-gbps ≤ 0`（红线关闭） | 输出 `off`（不限速） | — |
| 2 | **快速收紧**：spike（最近 1min）> 红线 × 1.2 | `bwlimit × 0.5`（一步降至半速） | `FAST_BRAKE_RATIO=1.2`, `FAST_BRAKE_FACTOR=0.5` |
| 3 | **429 超阈值**：429 率 > 5% | `bwlimit × 0.8`（常规乘性减） | `THROTTLE_OVERRIDE_RATE=0.05`, `DECREASE_FACTOR=0.8` |
| 4a | 慢回路（5min 均值）> 红线 | `bwlimit × 0.8`（乘性减） | `DECREASE_FACTOR=0.8` |
| 4b | 慢回路 < 红线 × 0.9 | `bwlimit + 50MB/s`（加性增） | `INCREASE_STEP_BYTES=50M` |
| 4c | 死区 [红线 × 0.9, 红线 × 1.1] | 保持不动 | `DEADZONE_LOWER=0.9`, `DEADZONE_UPPER=1.1` |

算出值后再依次经过三道后处理：

| 后处理 | 规则 | 常量 / 原因 |
|--------|------|-----------|
| **阻尼限幅** | 每轮相对上轮变化不超过 ±25% | `MAX_STEP_RATIO=0.25`。bwlimit 仅对新进程生效 + 大文件传输周期长 → 控制响应滞后 2-3min ≫ 控制周期 1min，不限幅会震荡 |
| **安全护栏** | 单进程值 ≤ 公平份额 × 4（公平份额 = 总目标 ÷ 并发进程数） | `SAFETY_MULTIPLIER=4`。宽松护栏防单进程失控，非主控钳制 |
| **地板** | 单进程值 ≥ 10MB/s | `MIN_BWLIMIT_BYTES=10M`。防连续收紧把传输限到停滞 |

其它规则：

- **两个独立开关**（`decide_bwlimit` 真值表）：`limit-enabled=false` → `off`（不限速）；`limit-enabled=true, auto-enabled=false` → 用 SSM 里的固定 `bwlimit`（手动模式）；两者都 `true` → AIMD 自动。
- **冷启动**：当前 `bwlimit=off`(0) 但需要收紧时，用 `COLD_START_BWLIMIT_BYTES=1Gbps` 作基数起算（否则从 0 乘永远是 0）。
- **双时间尺度**：慢回路（5min 均值）抗抖动、平缓收放；快保护（1min spike）应对突增立即收紧。
- **单位换算**：`--bwlimit` 纯数字单位是 **KiB/s** 不是字节/s，故控制器输出带 `M`（MiB）后缀避免 1024× 误差。

### 9.2 请求频率限速（`--tpslimit`，总目标 ÷ 进程数）

`--tpslimit` 无 AIMD 反馈，是**静态分摊**：你填全 fleet 总 TPS 目标，控制器每分钟算每进程值。

- **你设** `ratelimit/tpslimit-target`（全 fleet 总 transaction/秒，默认 `10000`）。
- **控制器算** `ratelimit/tpslimit`（每进程值 = 总目标 ÷ 并发进程数），worker 读这个；**勿手动改**（会被覆盖）。
- **并发进程数 = 实例数 × WorkerCount × WorkerThreads**（如 1 台 16×12 = 192）。
- `tpslimit-target = off` 或无实例（冷启动）→ 每进程 `off`（不限）。

> 默认 `10000 ÷ 192 ≈ 52`/进程，对大文件（每进程 QPS 仅个位数）等于不限，是宽松护栏；小文件高并发时把总 QPS 钳在 GCS/S3 限制下。大文件用不到，小文件场景才需调低。

### 9.3 运维操作

```bash
# 带宽：调目标 / 切手动 / 关限速（其它栈把命名空间换成对应栈名）
aws ssm put-parameter --overwrite --name /migration/migration-primary/ratelimit/target-gbps     --value 500
aws ssm put-parameter --overwrite --name /migration/migration-primary/ratelimit/auto-enabled    --value false
aws ssm put-parameter --overwrite --name /migration/migration-primary/ratelimit/limit-enabled   --value false
# 频率：调全 fleet 总 TPS 目标（控制器下一分钟自动换算每进程值）
aws ssm put-parameter --overwrite --name /migration/migration-primary/ratelimit/tpslimit-target --value 5000
```

| SSM 参数（每栈一套） | 默认 | 维护方 | 作用 |
|----------|------|--------|------|
| `.../ratelimit/limit-enabled` | `true` | 人 | 带宽限速总开关（false=不限速） |
| `.../ratelimit/auto-enabled` | `true` | 人 | 带宽 AIMD 自动开关（false=用固定 bwlimit） |
| `.../ratelimit/target-gbps` | `RatelimitTargetGbps` 参数 | 人 | 本栈带宽目标总吞吐（红线；0=不限速） |
| `.../ratelimit/bwlimit` | `off` | **Lambda** | 控制器算出的当前每进程带宽限速 |
| `.../ratelimit/tpslimit-target` | `RatelimitTpsTarget` 参数（默认 10000） | 人 | 全 fleet 总 transaction/秒目标（off=不限） |
| `.../ratelimit/tpslimit` | `off` | **Lambda** | 控制器算出的当前每进程频率限速（勿手动改） |

> **进程数计数不跨栈**：控制器只算本栈自己 ASG 的 `InService × WorkerCount × WorkerThreads`，多栈互不干扰。
> **EMF 429 信号跨栈共用 `GcsS3Migration` namespace**（无 cluster 维度）——源端 egress 配额是整体资源，所有栈共同消耗源端，用总体 429 信号判断收紧合理。
> **限速是源端 429 的治本手段**：关限速裸跑时大机群可能耗尽源端 egress 配额（RETRYABLE 速率飙升、in-flight 堆积），开限速把请求速率压到配额之下即可消除。
> **控制器与核心库同源**：`ratelimit_controller.py`（Lambda 部署包）的控制律纯函数被 `tests/test_ratelimit_controller.py` 直接单测——生产跑的就是被测代码，无内联分叉。

---

## 10. 清理资源

**一个集群 = 一个栈。** 删 N 套就 `delete-stack` N 次：

```bash
aws cloudformation delete-stack --region <region> --stack-name migration-primary
```

> [!WARNING]
> 若在 Endpoint SG 上加了"放行 worker SG"的规则，**删栈前先删掉该规则**，否则 worker SG 无法删除，删栈卡在 `DELETE_IN_PROGRESS`。

一条 `delete-stack` 删除该栈**全部资源**：ASG / SQS+DLQ / 限速 Lambda+EventBridge / Dashboard / worker SG / IAM role / 本栈 DynamoDB 两表 / 本栈日志组两个 / SSM 限速参数。各栈隔离，删一个不影响其它。

**不随栈删除、需手动清理**：

- **栈外预建的 GCS HMAC secret**（多栈共用）——确认无栈再引用后手动 `delete-secret`。
- **客户网络资源**（VPC / 子网 / VPC Endpoint）——不由本栈创建。

---

## 11. 开发

代码与测试约定见 `CLAUDE.md`。worker 包在 `src/migration/`，测试用 pytest + moto：

```bash
python3 -m pytest                              # 449 测试
ruff check src/ tests/ bench/                  # lint
bash scripts/scan-staged-secrets.sh            # 提交前密钥扫描
```

worker 代码不感知集群（差异全由 systemd 环境变量注入），同一份代码服务所有栈，改 worker 逻辑对全部已部署栈生效（需各栈滚动实例拉取新代码）。

## Security

See [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) for more information.

## License

Licensed under the MIT-0 License. See the LICENSE file.
