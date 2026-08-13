# Rclone SQS Agent

基于 SQS 和 Auto Scaling 的分布式文件传输系统，用于将 Dropbox Advanced/Business 团队成员的文件迁移到 S3。

## 系统架构

- **SQS 队列**：接收传输任务消息
- **Auto Scaling Group**：自动扩缩容的 EC2 实例
- **DynamoDB**：记录传输状态和重试历史
- **S3**：迁移目标，同时存放迁移日志
- **Secrets Manager**：存储 Dropbox 凭证
- **rclone**：实际执行传输，通过 systemd timer 每 50 分钟自动续期 token

```
sendcmd2sqs-dropbox.py（本地）→ SQS → agent.py（EC2/ASG）→ rclone copyto → S3
                                          ↓
                                      DynamoDB（状态）
```

### Remote 配置

实例上自动维护 2 个固定的 rclone remotes，无需手动配置：

| Remote 名称 | 类型 | 用途 | 示例路径 |
|------------|------|------|----------|
| `s3` | s3 | AWS S3 存储（用 `env_auth`，走实例角色） | `s3:bucket/path/file.txt` |
| `dropbox` | dropbox | Dropbox 团队（团队级 token） | `dropbox:Documents/file.pdf` |

`dropbox` remote 使用**团队级 token**，因此单独访问 `dropbox:` 会报错：

```
This API function operates on a single Dropbox account, but the OAuth 2
access token you provided is for an entire Dropbox Business team.
```

这是预期行为。必须通过 `--dropbox-impersonate <成员邮箱>` 指定以哪个成员的身份访问，`sendcmd2sqs-dropbox.py` 会自动在每条消息里带上这个参数。

### Token 自动刷新

- `rclone-refresh.timer` 每 50 分钟触发一次，用 refresh token 换取新的 access token 并重写 `rclone.conf`
- Dropbox access token 有效期 4 小时，refresh token 长期有效
- 写入 `rclone.conf` 的 token 中包含 refresh_token，rclone 自身也能续期

## 准备工作

### 1. 配置 Dropbox 应用

在 https://www.dropbox.com/developers/apps 创建应用，要求：

- **App 类型**：Scoped access，并启用 **Team member file access**
- **必需 scope**：`members.read`、`files.metadata.read`、`files.content.read`

迁移只需读取 Dropbox，**不要**授予 `files.content.write`。

### 2. 获取 refresh token

必须带 `token_access_type=offline`，否则只能拿到 4 小时后失效的 access token，timer 无法续期。

```bash
# 1) 浏览器打开以下地址（用团队管理员账号登录并授权）
https://www.dropbox.com/oauth2/authorize?client_id=<APP_KEY>&response_type=code&token_access_type=offline

# 2) 用返回的 auth code 换取 refresh token
curl -X POST https://api.dropboxapi.com/oauth2/token \
  -u "<APP_KEY>:<APP_SECRET>" \
  -d grant_type=authorization_code \
  -d code=<AUTH_CODE>
```

响应中的 `refresh_token` 即为部署参数 `DropboxRefreshToken`。可用以下请求验证它能正常续期：

```bash
curl -X POST https://api.dropboxapi.com/oauth2/token \
  -u "<APP_KEY>:<APP_SECRET>" \
  -d grant_type=refresh_token -d refresh_token=<REFRESH_TOKEN>
```

### 3. 准备参数文件

```bash
cp deploy-params-cfn-init.json.template deploy-params-cfn-init.json

# 编辑文件，填入实际的凭证和参数
# 注意：此文件包含敏感信息，不要提交到 Git（.gitignore 已排除）
vim deploy-params-cfn-init.json
```

## 部署步骤

### 方式 1: 使用 CloudFormation CLI（推荐）

```bash
# 部署 Stack
aws cloudformation create-stack \
  --region your-region \
  --stack-name sqs-rclone-agent-cfn \
  --template-body file://deployment-cfn-init.yaml \
  --parameters file://deploy-params-cfn-init.json \
  --capabilities CAPABILITY_NAMED_IAM

# 等待部署完成
aws cloudformation wait stack-create-complete \
  --region your-region \
  --stack-name sqs-rclone-agent-cfn

# 查看输出
aws cloudformation describe-stacks \
  --region your-region \
  --stack-name sqs-rclone-agent-cfn \
  --query 'Stacks[0].Outputs'
```

**参数文件示例** (`deploy-params-cfn-init.json`):
```json
[
  {"ParameterKey": "VPC", "ParameterValue": "vpc-xxxxx"},
  {"ParameterKey": "Subnets", "ParameterValue": "subnet-xxxxx,subnet-yyyyy"},
  {"ParameterKey": "InstanceType", "ParameterValue": "t3.large"},
  {"ParameterKey": "InstanceCount", "ParameterValue": "1"},
  {"ParameterKey": "DropboxAppKey", "ParameterValue": "your-app-key"},
  {"ParameterKey": "DropboxAppSecret", "ParameterValue": "your-app-secret"},
  {"ParameterKey": "DropboxRefreshToken", "ParameterValue": "your-refresh-token"},
  {"ParameterKey": "DestinationBucket", "ParameterValue": "your-bucket-name"}
]
```

**参数约束**：

| 参数 | 必填 | 说明 |
|------|------|------|
| `VPC` / `Subnets` | ✅ | 子网需能访问 Dropbox API 与 AWS API（公有子网或配 NAT） |
| `DestinationBucket` | ✅ | 3-63 字符，符合 S3 桶命名规则。它决定 agent 的 IAM 写入范围 |
| `DropboxAppKey` / `Secret` / `RefreshToken` | ❌ | 留空则只配置 `s3` remote，实例正常启动但无法迁移 Dropbox |
| `InstanceCount` | ❌ | 默认 2，同时作为 ASG 的 Min/Max/Desired |
| `NumWorkers` | ❌ | 每实例 worker 线程数，默认 16 |
| `QueueVisibilityTimeout` | ❌ | 默认 3600 秒（1 小时），最大 43200 |

堆栈名建议不超过 43 字符（IAM RoleName 上限 64，后缀占 `-agent-role-<region>`）。

### 方式 2: 通过 CloudFormation Console 部署

1. 登录 AWS Console，进入 CloudFormation 服务
2. 点击 "Create stack" → "With new resources"
3. 选择 "Upload a template file"，上传 `deployment-cfn-init.yaml`
4. 填写参数：
   - **Stack name**: 堆栈名称（如 `sqs-rclone-agent-cfn`）
   - **VPC**: 选择 VPC
   - **Subnets**: 选择至少 1 个子网
   - **InstanceType**: 实例类型（默认 `t3.medium`）
   - **InstanceCount**: 实例数量（默认 `2`）
   - **DropboxAppKey**: Dropbox 应用 App key
   - **DropboxAppSecret**: Dropbox 应用 App secret
   - **DropboxRefreshToken**: 离线获取的 refresh token
   - **DestinationBucket**: S3 目标桶名（必填）
   - **KeyPairName**: SSH 密钥对（可选）
5. 点击 "Next"，配置堆栈选项（可选）
6. 勾选 "I acknowledge that AWS CloudFormation might create IAM resources"
7. 点击 "Submit" 开始部署

### 部署过程说明

实例启动后由 cfn-init 按 configset 顺序执行：

1. **install_packages**：安装 `python3-pip`、rclone、boto3/requests/pathspec/tenacity
2. **put_files**：写入 `agent.py`、`refresh_rclone_config.py` 及三个 systemd unit
3. **configure_services**：
   - 从 `s3://${DestinationBucket}/user-list/` 同步可选配置（不存在则跳过）
   - 运行 `refresh_rclone_config.py` 生成 `rclone.conf`
   - `systemctl daemon-reload`
4. **start_services**：启用并启动 `rclone-refresh.timer` 与 `rclone-agent`

完成后实例调用 `cfn-signal` 上报结果。部署时间约 5-10 分钟。

如果 cfn-init 失败，`cfn-signal` 会立即上报 FAILURE 并触发回滚，无需等待 `CreationPolicy` 超时。具体错误在实例的 `/var/log/cfn-init.log` 与 `/var/log/cloud-init-output.log`。

### 部署完成

CloudFormation 会自动创建：
- **SQS 队列**：`{StackName}-queue`
- **死信队列**：`{StackName}-queue-dlq`
- **DynamoDB 表**：`{StackName}-transfer-message-status-{Region}`
- **Auto Scaling Group** 和 Launch Template
- **IAM Role**：`{StackName}-agent-role-{Region}`，和安全组
- **Secrets Manager** 密钥：`{StackName}-dropbox-credentials`

## 发送迁移任务

### 使用 sendcmd2sqs-dropbox.py

编辑脚本顶部的常量：

```python
DROPBOX_APP_KEY = ""
DROPBOX_APP_SECRET = ""
DROPBOX_REFRESH_TOKEN = ""

SQS_QUEUE_URL = ""
AWS_REGION = ""
TARGET_S3_BUCKET = ""          # 必须与部署参数 DestinationBucket 一致
DESTINATION_PREFIX = "n-dropbox"
```

> **`TARGET_S3_BUCKET` 必须与 `DestinationBucket` 填成同一个桶。**
> 两者是各自独立的配置，没有一致性校验：`DestinationBucket` 决定 agent 的 IAM
> 能写哪个桶，`TARGET_S3_BUCKET` 决定消息里往哪个桶写。填不一致时堆栈会正常
> 部署、agent 会正常启动、消息会正常投递，然后每个任务在写 S3 时 AccessDenied，
> 重试 3 次后进入 DLQ。

运行：

```bash
# 全量扫描所有活跃团队成员
python3 sendcmd2sqs-dropbox.py

# 只扫描白名单中的成员
python3 sendcmd2sqs-dropbox.py -f

# 增量：只迁移指定时间之后修改的文件（基于 server_modified）
python3 sendcmd2sqs-dropbox.py --modified-after 2024-01-01
python3 sendcmd2sqs-dropbox.py --modified-after 2024-01-15T10:30:00
```

脚本会为每个文件生成如下目标路径，用 Dropbox 的 `file_id` 命名以避免同名冲突：

```
s3://<TARGET_S3_BUCKET>/<DESTINATION_PREFIX>/<成员邮箱>/<file_id><扩展名>
```

文件的 Dropbox `content_hash` 会作为 S3 对象元数据 `x-amz-meta-hash` 一并写入，可用于事后校验。

### 命名空间说明

Dropbox Business 团队有两个命名空间，同一个文件在两者中路径不同：

| 视角 | `hello.txt` 的路径 |
|------|-------------------|
| 成员个人空间（home namespace） | `/Documents/hello.txt` |
| 团队空间（team space） | `/<成员显示名>/Documents/hello.txt` |

脚本枚举文件时不带 `Dropbox-API-Path-Root`，拿到的是**成员个人空间**视角的路径；而 rclone 的 dropbox 后端默认使用 `root_namespace_id`（**团队空间**）。两者不一致会导致每个任务都报 `directory not found`。

因此脚本会为每个成员调用 `/2/users/get_current_account` 取得 `home_namespace_id`，并通过 `--dropbox-root-namespace` 传给 rclone，使两侧视角一致。手动构造消息时必须自行带上这个参数。

### 成员白名单

```bash
cp memberWhiteList.json.template memberWhiteList.json
```

```json
[
  "user1@example.com",
  "user2@example.com"
]
```

配合 `-f` 参数使用，只迁移白名单中的成员。不加 `-f` 时遍历团队所有活跃（`active`）成员。

### 文件过滤

`.ignore-dropbox` 采用 `.gitignore` 语法，默认已排除各类缓存目录、临时文件和 OneNote 文件。匹配基于相对成员个人根目录的路径。

## 消息格式定义

### SQS 消息体结构

发送到 SQS 队列的消息必须是 JSON 格式，包含以下字段：

```json
{
  "source": "source-path",
  "destination": "destination-path",
  "rclone_args": ["--arg1", "value1", "--arg2"]
}
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 | 示例 |
|------|------|------|------|------|
| `source` | String | ✅ | 源路径，rclone 格式，同时作为 DynamoDB 分区键 | `"dropbox:Documents/file.pdf"` |
| `destination` | String | ✅ | 目标路径，rclone 格式 | `"s3:bucket/dest/file.pdf"` |
| `rclone_args` | Array | ❌ | 额外的 rclone 命令参数 | `["--progress", "--dry-run"]` |

### 支持的路径格式

#### S3 到 S3
```json
{
  "source": "s3:your-bucket/source-folder/file.bin",
  "destination": "s3:your-bucket/backup-folder/file.bin"
}
```

#### Dropbox 到 S3
```json
{
  "source": "dropbox:Documents/file.pdf",
  "destination": "s3:your-bucket/n-dropbox/user@example.com/file.pdf",
  "rclone_args": [
    "--dropbox-impersonate", "user@example.com",
    "--dropbox-root-namespace", "1234567890"
  ]
}
```

Dropbox 源**必须**同时带 `--dropbox-impersonate` 和 `--dropbox-root-namespace`，缺任何一个都会失败。

### 完整消息示例

#### 示例 1: S3 文件传输
```json
{
  "source": "s3:your-bucket/access-logs/test-1mb-1.bin",
  "destination": "s3:your-bucket/test-destination/test-1mb-1.bin"
}
```

#### 示例 2: Dropbox 文件传输（脚本生成的实际形态）
```json
{
  "source": "dropbox:Documents/readme.txt",
  "destination": "s3:your-bucket/n-dropbox/user@example.com/G88ux0m3p-gAAAAAAAAAFQ.txt",
  "rclone_args": [
    "--dropbox-impersonate", "user@example.com",
    "--dropbox-root-namespace", "1234567890",
    "--progress",
    "--header-upload", "x-amz-meta-hash:<dropbox content_hash>"
  ]
}
```

#### 示例 3: 带子目录的路径
```json
{
  "source": "dropbox:Projects/2024/report.xlsx",
  "destination": "s3:your-bucket/n-dropbox/user@example.com/report.xlsx",
  "rclone_args": [
    "--dropbox-impersonate", "user@example.com",
    "--dropbox-root-namespace", "1234567890"
  ]
}
```

### 手动发送消息

#### 使用 Python SDK
```python
import boto3
import json

sqs = boto3.client('sqs', region_name='your-region')
queue_url = 'https://sqs.your-region.amazonaws.com/123456789012/your-queue-name'

message = {
    'source': 'dropbox:Documents/file.pdf',
    'destination': 's3:your-bucket/backup/file.pdf',
    'rclone_args': [
        '--dropbox-impersonate', 'user@example.com',
        '--dropbox-root-namespace', '1234567890',
    ],
}

sqs.send_message(
    QueueUrl=queue_url,
    MessageBody=json.dumps(message)
)
```

#### 使用 AWS CLI
```bash
aws sqs send-message \
  --region your-region \
  --queue-url https://sqs.your-region.amazonaws.com/123456789012/your-queue-name \
  --message-body '{
    "source": "s3:your-bucket/test-data/readme.txt",
    "destination": "s3:your-bucket/backup/readme.txt"
  }'
```

### 消息处理流程

```
1. 消息发送到 SQS
   ↓
2. Agent 接收消息 (ReceiveMessage with VisibilityTimeout)
   ↓
3. 解析 JSON Body
   ↓
4. 提取 source, destination, rclone_args
   ↓
5. 执行 rclone copyto
   ↓
6. 成功: DeleteMessage (完成)
   失败: 等待 VisibilityTimeout 过期，自动重试
   ↓
7. 3 次失败后自动进入死信队列
```

### 重试机制

系统使用标准的 SQS 重试机制：
- **VisibilityTimeout**: 由 `QueueVisibilityTimeout` 参数控制，默认 3600 秒（最大 43200）
- **MaxReceiveCount**: 3 次（最大重试次数）
- **自动重试**: 失败消息自动重新可见
- **死信队列**: 3 次失败后自动转移

传输失败时 agent **不会**删除消息也不会改变可见性，而是让其自然超时以递增 `ReceiveCount`。因此单个任务的重试间隔即为 VisibilityTimeout。

### 错误处理

如果消息 JSON 解析失败，或缺少 `source`/`destination`，Agent 会记录错误并**删除**该消息，避免无效消息反复占用 worker。传输本身失败则保留消息等待重试。

## 监控和管理

### 查看传输状态

在 DynamoDB Console 中打开 `{StackName}-transfer-message-status-{Region}` 表，可以查看：
- **传输状态**：PROCESSING / SUCCESS / FAILED
- **时间信息**：开始时间、完成时间、耗时
- **传输统计**：字节数、文件数
- **重试历史**：每次重试都会创建新记录，通过 `attempt_timestamp` 区分
- **执行命令**：完整的 rclone 命令记录
- **实例 ID**：执行该任务的 EC2 实例

#### DynamoDB 表结构

表使用复合主键支持重试跟踪：
- **Partition Key**: `source` (源路径)
- **Sort Key**: `attempt_timestamp` (尝试时间戳)
- **GSI**: `status-timestamp-index`，可按状态查询

每次传输尝试会创建一条记录，同一次尝试的 PROCESSING 和最终状态（SUCCESS/FAILED）共用同一个 `attempt_timestamp`。

### 传输命令

系统使用 `rclone copyto` 命令进行单文件传输：

```bash
rclone copyto source destination \
  --s3-no-check-bucket \
  --stats 1m \
  --retries 3 \
  --low-level-retries 10 \
  --log-level INFO
```

消息中的 `rclone_args` 会追加到上面的固定参数之后。

#### 参数说明

| 参数 | 含义 | 作用 |
|------|------|------|
| `copyto` | 单文件复制命令 | 将源文件复制到目标位置，如果目标是目录则自动使用源文件名 |
| `--s3-no-check-bucket` | 跳过 S3 桶存在性检查 | 减少 API 调用，提高传输速度，适用于已知存在的桶 |
| `--stats 1m` | 每分钟显示统计信息 | 输出传输进度，用于解析传输字节数、文件数和耗时 |
| `--retries 3` | rclone 层面重试 3 次 | 处理临时网络问题和服务端错误的重试机制 |
| `--low-level-retries 10` | 底层操作重试 10 次 | HTTP/网络层面的重试，处理连接超时等底层问题 |
| `--log-level INFO` | 信息级别日志 | 输出详细的传输信息和错误详情，用于结果解析和调试 |

#### Dropbox 相关参数

| 参数 | 作用 |
|------|------|
| `--dropbox-impersonate <email>` | 以该团队成员的身份访问其个人空间（设置 `Dropbox-API-Select-User`） |
| `--dropbox-root-namespace <id>` | 指定以成员个人空间为根，而非默认的团队空间 |

**注意**: 避免使用与现有参数冲突的选项（如 `--verbose` 与 `--log-level` 冲突）。

### 成员管理操作

成员列表在每次运行 `sendcmd2sqs-dropbox.py` 时通过 `/2/team/members/list` 实时获取，无需维护配置文件。

- **限定迁移范围**：编辑 `memberWhiteList.json` 并使用 `-f` 参数
- **新增成员**：直接重新运行脚本即可，新成员会被自动发现
- **轮换凭证**：更新 Secrets Manager 中的 `{StackName}-dropbox-credentials`，然后在实例上执行
  `sudo systemctl start rclone-refresh.service` 立即生效（否则最多等 50 分钟）

> 通过 `update-stack` 修改 Dropbox 参数只会更新 Secret，**不会**触发 ASG 滚动更新
> （Launch Template 引用的是 Secret 的 ARN，ARN 不变）。实例上的 `rclone.conf`
> 需要手动触发刷新或等 timer。

### 调整实例数量

`InstanceCount` 同时作为 ASG 的 MinSize/MaxSize/DesiredCapacity。修改实例数应通过 `update-stack` 更新该参数，直接在 EC2 Console 改 Desired capacity 会被后续堆栈更新覆盖。

### 性能监控

#### 队列监控
```bash
# 检查队列状态
aws sqs get-queue-attributes \
  --queue-url your-queue-url \
  --attribute-names All
```

#### 传输统计
```bash
# 查询 DynamoDB 传输记录
aws dynamodb scan \
  --table-name your-stack-transfer-message-status-your-region \
  --filter-expression "#status = :status" \
  --expression-attribute-names '{"#status": "status"}' \
  --expression-attribute-values '{":status": {"S": "SUCCESS"}}'
```

### 服务管理

实例已带 SSM Agent 并附加 `AmazonSSMManagedInstanceCore`，推荐用 Session Manager 登录，无需开放 SSH。

#### 服务状态查询

```bash
# 查询 rclone-agent 服务状态
sudo systemctl status rclone-agent.service

# 查询 rclone-refresh 服务状态
sudo systemctl status rclone-refresh.service
sudo systemctl status rclone-refresh.timer
```

#### 服务日志查询

```bash
# 查看 rclone-agent 服务日志
sudo journalctl -u rclone-agent.service --no-pager -n 20

# 查看 rclone-refresh 服务日志
sudo journalctl -u rclone-refresh.service --no-pager -n 10

# 实时跟踪日志
sudo journalctl -u rclone-agent -f
sudo journalctl -u rclone-refresh -f
```

#### 手动触发服务

```bash
# 手动触发 rclone-refresh（刷新 Dropbox token 并重写 rclone.conf）
sudo systemctl start rclone-refresh.service

# 重启 rclone-agent 服务
sudo systemctl restart rclone-agent.service

# 停止/启动服务
sudo systemctl stop rclone-agent.service
sudo systemctl start rclone-agent.service
```

#### rclone-refresh 执行监控

```bash
# 查看 timer 状态和下次执行时间
sudo systemctl list-timers rclone-refresh.timer

# 查看服务执行历史（最近 20 条）
sudo journalctl -u rclone-refresh.service --no-pager -n 20

# 查看最近 10 分钟的执行记录
sudo journalctl -u rclone-refresh.service --since "10 minutes ago"

# 检查配置文件是否更新
ls -la /root/.config/rclone/rclone.conf
ls -la /home/ec2-user/.config/rclone/rclone.conf
```

#### 服务配置验证

```bash
# 检查 rclone 配置（应输出 s3: 和 dropbox:）
rclone listremotes

# 测试 Dropbox 连接（必须带 impersonate 和 root-namespace）
rclone lsd --dropbox-impersonate user@example.com \
  --dropbox-root-namespace 1234567890 dropbox:

# 测试 S3 连接
rclone lsd s3:your-bucket
```

### 故障排除

```bash
# 通过 Session Manager 连接实例
aws ssm start-session --target <instance-id> --region your-region

# 查看 cfn-init 执行结果（部署阶段问题）
sudo grep -E 'Command .* (succeeded|failed)|BUILD FAILED' /var/log/cfn-init.log
sudo tail -50 /var/log/cloud-init-output.log

# 查看 agent 服务日志
sudo journalctl -u rclone-agent -f

# 查看 token 刷新日志
sudo journalctl -u rclone-refresh -f
```

#### 常见问题

| 现象 | 原因与排查方向 |
|------|---------------|
| 堆栈卡在 CREATE_IN_PROGRESS 后超时 | 实例无法出网（检查子网路由/NAT），或 cfn-signal 未能到达 |
| `rclone listremotes` 只有 `s3:` | Dropbox 凭证为空或无效，查看 `journalctl -u rclone-refresh` |
| 所有任务报 `directory not found` | 消息缺少 `--dropbox-root-namespace`，见「命名空间说明」 |
| 所有任务报 AccessDenied（写 S3 时） | `TARGET_S3_BUCKET` 与 `DestinationBucket` 不一致 |
| `dropbox:` 报 "token is for an entire team" | 缺少 `--dropbox-impersonate`，属预期行为 |
| Dropbox API 返回 `missing_scope` | 应用未启用 Team member file access 或 scope 不全 |
| 消息进入 DLQ | 查 DynamoDB 中该 `source` 的 `error_message` 字段 |

### 清理资源

在 CloudFormation Console 中删除堆栈即可清理所有资源：

```bash
aws cloudformation delete-stack \
  --region your-region \
  --stack-name sqs-rclone-agent-cfn
```

**注意**：删除堆栈会清理所有相关资源，包括 DynamoDB 表中的传输记录。Secrets Manager 密钥默认有 7 天恢复窗口。已迁移到 S3 的文件不受影响。

## Security

See [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) for more information.

## License

This library is licensed under the MIT-0 License. See the LICENSE file.
