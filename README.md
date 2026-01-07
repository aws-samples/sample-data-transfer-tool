# Rclone SQS Agent

基于 SQS 和 Auto Scaling 的分布式文件传输系统，支持 S3、OneDrive 等多种存储源。

## 系统架构

- **SQS 队列**：接收传输任务消息
- **Auto Scaling Group**：自动扩缩容的 EC2 实例
- **DynamoDB**：记录传输状态和重试历史
- **S3**：存储用户配置和代码
- **Secrets Manager**：存储 OneDrive 凭证
- **用户管理**：从 S3 动态加载用户配置

## 用户管理系统

### 简化配置说明

**重要更新**：系统已简化为固定的 3 个 rclone remotes，无需复杂的用户和站点配置文件。

#### 固定的 Remote 配置

系统自动维护以下 3 个固定的 rclone remotes：

| Remote 名称 | 类型 | Drive 类型 | 用途 | 示例路径 |
|------------|------|-----------|------|----------|
| `s3` | s3 | - | AWS S3 存储 | `s3:bucket/path/file.txt` |
| `onedrive` | onedrive | business | OneDrive 业务版 | `onedrive:Documents/file.pdf` |
| `sharepoint` | onedrive | documentlibrary | SharePoint 文档库 | `sharepoint:Documents/file.pdf` |

#### Token 自动刷新

- **API 调用优化**：每次 token 刷新仅需 1 次 API 调用
- **自动更新**：每 50 分钟自动刷新 OneDrive 和 SharePoint 的访问令牌
- **无需配置**：系统自动处理所有认证和配置

### 用户和站点列表（仅供参考）

以下配置文件仅用于**发送 SQS 消息时的路径参考**，系统不再依赖这些文件进行配置：

#### userList.json（参考用）

```json
[
  {
    "email": "user1@example.onmicrosoft.com",
    "workcode": "USER001"
  },
  {
    "email": "user2@example.onmicrosoft.com", 
    "workcode": "USER002"
  }
]
```

#### siteList.json（参考用）

```json
[
  "jayden-demo",
  "test",
  "private-from-kenty"
]
```

**注意**：这些文件仅用于帮助用户了解可用的用户和站点，发送消息时直接使用固定的 remote 名称即可。

#### Remote 命名规则

系统使用以下固定的 rclone remotes：

- **S3 Remote**：`s3:` - AWS S3 存储
- **OneDrive Remote**：`onedrive:` - OneDrive 业务版
- **SharePoint Remote**：`sharepoint:` - SharePoint 文档库

#### 配置管理流程

```
1. 系统自动维护 3 个固定 remotes
   ↓
2. 每 50 分钟自动刷新 OneDrive 和 SharePoint tokens
   ↓
3. 无需手动配置或上传配置文件
   ↓
4. 直接使用固定 remote 名称发送传输消息
```

#### Fallback 机制

如果 S3 中没有配置文件，系统会：

**OneDrive 用户：**
1. 自动调用 Microsoft Graph API 获取所有用户
2. 生成默认的 workcode (`user1`, `user2`, ...)
3. 配置所有发现的 OneDrive 用户

**SharePoint 站点：**
1. 如果没有 `siteList.json` 文件，跳过 SharePoint 配置
2. 只配置 OneDrive 用户 remotes

## 部署步骤

### 准备工作

1. **准备用户配置**：
   ```bash
   # 创建用户配置文件
   cat > userList.json << EOF
   [
     {
       "email": "your-user@domain.onmicrosoft.com",
       "workcode": "USER001"
     }
   ]
   EOF
   
   # 上传到 S3 (使用实际的bucket名称和部署区域)
   aws s3 cp userList.json s3://your-destination-bucket/user-list/userList.json --region your-deployment-region
   ```

2. **准备参数文件**：
   ```bash
   # 复制模板文件
   cp deploy-params-cfn-init.json.template deploy-params-cfn-init.json
   
   # 编辑文件，填入实际的凭证和参数
   # 注意：此文件包含敏感信息，不要提交到 Git
   vim deploy-params-cfn-init.json
   ```

### 方式 1: 使用 CloudFormation CLI（推荐）

**部署 Stack**:
```bash
# 部署 Stack
aws cloudformation create-stack \
  --region eu-central-1 \
  --stack-name sqs-rclone-agent-cfn \
  --template-body file://deployment-cfn-init.yaml \
  --parameters file://deploy-params-cfn-init.json \
  --capabilities CAPABILITY_NAMED_IAM

# 等待部署完成
aws cloudformation wait stack-create-complete \
  --region eu-central-1 \
  --stack-name sqs-rclone-agent-cfn

# 查看输出
aws cloudformation describe-stacks \
  --region eu-central-1 \
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
  {"ParameterKey": "OneDriveClientId", "ParameterValue": "your-client-id"},
  {"ParameterKey": "OneDriveClientSecret", "ParameterValue": "your-client-secret"},
  {"ParameterKey": "OneDriveTenantId", "ParameterValue": "your-tenant-id"},
  {"ParameterKey": "DestinationBucket", "ParameterValue": "your-bucket-name"}
]
```

### 方式 2: 通过 CloudFormation Console 部署

1. 登录 AWS Console，进入 CloudFormation 服务
2. 点击 "Create stack" → "With new resources"
3. 选择 "Upload a template file"，上传 `deployment-cfn-init.yaml`
4. 填写参数：
   - **Stack name**: 堆栈名称（如 `sqs-rclone-agent-cfn`）
   - **VPC**: 选择 VPC
   - **Subnets**: 选择至少 2 个子网
   - **InstanceType**: 实例类型（默认 `t3.medium`）
   - **InstanceCount**: 实例数量（默认 `1`）
   - **OneDriveClientId**: OneDrive 应用客户端 ID
   - **OneDriveClientSecret**: OneDrive 应用客户端密钥
   - **OneDriveTenantId**: OneDrive 租户 ID
   - **DestinationBucket**: S3 桶名（存储日志）
   - **KeyPairName**: SSH 密钥对（可选）
5. 点击 "Next"，配置堆栈选项（可选）
6. 勾选 "I acknowledge that AWS CloudFormation might create IAM resources"
7. 点击 "Submit" 开始部署

### 部署过程说明

#### 初始化阶段
CloudFormation 在实例启动时会执行以下步骤：

1. **下载用户配置**：
   ```bash
   aws s3 sync s3://${DestinationBucket}/user-list/ /home/ec2-user/agent/ --region ${AWS::Region}
   ```

2. **配置 OneDrive**：
   - 首次运行：读取 `userList.json`，为每个用户配置 OneDrive remote
   - 获取用户 Drive ID 和访问令牌
   - 生成 rclone 配置文件

3. **启动服务**：
   - `rclone-agent`：SQS 消息处理服务
   - `rclone-refresh.timer`：每 50 分钟刷新 OneDrive token

#### Token 刷新优化
- **首次运行**：配置所有 OneDrive remotes（需要 API 调用）
- **后续运行**：仅刷新 access tokens（无额外 API 调用，节省 100%）

### 部署完成

CloudFormation 会自动创建：
- **SQS 队列**：`{StackName}-queue`
- **死信队列**：`{StackName}-queue-dlq`
- **DynamoDB 表**：`transfer-message-status-{Region}`
- **Auto Scaling Group** 和 Launch Template
- **IAM Role** 和安全组
- **Secrets Manager** 密钥（存储 OneDrive 凭证）

部署时间约 10-15 分钟。

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
| `source` | String | ✅ | 源路径，支持 rclone 格式，同时作为 DynamoDB 主键 | `"USER001:Documents/file.pdf"` |
| `destination` | String | ✅ | 目标路径，支持 rclone 格式 | `"s3:bucket/dest/file.pdf"` |
| `rclone_args` | Array | ❌ | 额外的 rclone 命令参数 | `["--progress", "--dry-run"]` |

### 支持的路径格式

#### S3 路径
```json
{
  "source": "s3:your-bucket/source-folder/file.bin",
  "destination": "s3:your-bucket/backup-folder/file.bin"
}
```

#### OneDrive 路径（使用 workcode）
```json
{
  "source": "USER001:Documents/file.pdf",
  "destination": "s3:your-bucket/onedrive-backup/file.pdf"
}
```

#### 混合路径（OneDrive → S3）
```json
{
  "source": "USER002:Shared/report.xlsx",
  "destination": "s3:your-bucket/reports/report.xlsx"
}
```

### 完整消息示例

#### 示例 1: S3 文件传输
```json
{
  "source": "s3:your-bucket/access-logs/test-1mb-1.bin",
  "destination": "s3:your-bucket/test-destination/test-1mb-1.bin"
}
```

#### 示例 2: OneDrive 文件传输
```json
{
  "source": "onedrive:Documents/readme.txt",
  "destination": "s3:your-bucket/backup/readme.txt"
}
```

#### 示例 3: SharePoint 文件传输
```json
{
  "source": "sharepoint:Documents/report.pdf",
  "destination": "s3:your-bucket/sharepoint-backup/report.pdf"
}
```

#### 示例 4: 大文件传输
```json
{
  "source": "onedrive:test-data/test-500mb.bin",
  "destination": "s3:your-bucket/large-files/test-500mb.bin"
}
```

#### 示例 5: 使用额外参数的传输
```json
{
  "source": "sharepoint:Documents/file.pdf",
  "destination": "s3:your-bucket/backup/file.pdf",
  "rclone_args": ["--progress", "--verbose"]
}
```

### 消息发送方式

#### 方式 1: 使用 Python SDK
```python
import boto3
import json

sqs = boto3.client('sqs', region_name='your-region')
queue_url = 'https://sqs.your-region.amazonaws.com/123456789012/your-queue-name'

message = {
    'source': 'USER001:Documents/file.pdf',
    'destination': 's3:your-bucket/backup/file.pdf'
}

sqs.send_message(
    QueueUrl=queue_url,
    MessageBody=json.dumps(message)
)
```

#### 方式 2: 使用 AWS CLI
```bash
aws sqs send-message \
  --region your-region \
  --queue-url https://sqs.your-region.amazonaws.com/123456789012/your-queue-name \
  --message-body '{
    "source": "USER001:test-data/readme.txt",
    "destination": "s3:your-bucket/backup/readme.txt"
  }'
```

#### 方式 3: 批量发送
```python
# 批量发送多个传输任务
import boto3
import json

sqs = boto3.client('sqs', region_name='your-region')
queue_url = 'your-queue-url'

messages = [
    {'source': 'USER001:folder1/file1.txt', 'destination': 's3:bucket/backup/file1.txt'},
    {'source': 'USER002:folder2/file2.pdf', 'destination': 's3:bucket/backup/file2.pdf'},
]

for message in messages:
    sqs.send_message(
        QueueUrl=queue_url,
        MessageBody=json.dumps(message)
    )
```

### 消息处理流程

```
1. 消息发送到 SQS
   ↓
2. Agent 接收消息 (ReceiveMessage with VisibilityTimeout)
   ↓
3. 解析 JSON Body
   ↓
4. 提取 source, destination
   ↓
5. 执行 rclone copyto
   ↓
6. 成功: DeleteMessage (完成)
   失败: 等待VisibilityTimeout过期，自动重试
   ↓
7. 3次失败后自动进入死信队列
```

### 重试机制

系统使用标准的 SQS 重试机制：
- **VisibilityTimeout**: 12小时（最大值，确保长时间传输不被中断）
- **MaxReceiveCount**: 3次（最大重试次数）
- **自动重试**: 失败消息自动重新可见
- **死信队列**: 3次失败后自动转移

### 错误处理

如果消息格式不正确，Agent 会：
1. 记录错误日志：`Failed to parse message: {error}`
2. 跳过该消息
3. 继续处理下一条消息

**注意**：格式错误的消息不会被删除，会在 VisibilityTimeout 后重新可见，最终进入 DLQ。

## 监控和管理

### 查看传输状态

在 DynamoDB Console 中打开 `transfer-message-status-{Region}` 表，可以查看：
- **传输状态**：PROCESSING / SUCCESS / FAILED
- **时间信息**：开始时间、完成时间、耗时
- **传输统计**：字节数、文件数
- **重试历史**：每次重试都会创建新记录，通过 `attempt_timestamp` 区分
- **执行命令**：完整的 rclone 命令记录

#### DynamoDB 表结构

表使用复合主键支持重试跟踪：
- **Partition Key**: `source` (源路径)
- **Sort Key**: `attempt_timestamp` (尝试时间戳)

每次传输尝试会创建一条记录，同一次尝试的 PROCESSING 和最终状态（SUCCESS/FAILED）会更新同一条记录。

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

#### 参数说明

| 参数 | 含义 | 作用 |
|------|------|------|
| `copyto` | 单文件复制命令 | 将源文件复制到目标位置，如果目标是目录则自动使用源文件名 |
| `--s3-no-check-bucket` | 跳过S3桶存在性检查 | 减少API调用，提高传输速度，适用于已知存在的桶 |
| `--stats 1m` | 每分钟显示统计信息 | 输出传输进度，用于解析传输字节数、文件数和耗时 |
| `--retries 3` | rclone层面重试3次 | 处理临时网络问题和服务端错误的重试机制 |
| `--low-level-retries 10` | 底层操作重试10次 | HTTP/网络层面的重试，处理连接超时等底层问题 |
| `--log-level INFO` | 信息级别日志 | 输出详细的传输信息和错误详情，用于结果解析和调试 |

#### 自定义参数

用户可以通过消息中的 `rclone_args` 字段添加额外参数：

```json
{
  "source": "USER001:Documents/file.pdf",
  "destination": "s3:your-bucket/backup/file.pdf",
  "rclone_args": ["--progress", "--dry-run"]
}
```

**注意**: 避免使用与现有参数冲突的选项（如 `--verbose` 与 `--log-level` 冲突）。

### 用户管理操作

#### 添加新用户
1. 编辑 `userList.json` 添加用户信息
2. 上传到 S3：
   ```bash
   aws s3 cp userList.json s3://${DestinationBucket}/user-list/userList.json --region ${AWS::Region}
   ```
3. 重启实例或等待下次 token 刷新（50分钟）

#### 修改用户 workcode
1. 在 `userList.json` 中修改 workcode
2. 重新上传到 S3
3. 重启实例应用更改

#### 删除用户
1. 从 `userList.json` 中移除用户条目
2. 重新上传到 S3
3. 重启实例清理配置

### 调整实例数量

在 EC2 Console 的 Auto Scaling Groups 中：
1. 找到 `{StackName}-AgentASG-xxx`
2. 点击 "Edit" 修改 Desired capacity
3. 系统会自动启动或停止实例

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
  --table-name transfer-message-status-your-region \
  --filter-expression "#status = :status" \
  --expression-attribute-names '{"#status": "status"}' \
  --expression-attribute-values '{":status": {"S": "SUCCESS"}}'
```

### 服务管理

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
# 手动触发 rclone-refresh（刷新 OneDrive tokens）
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

# 查看服务执行历史（最近20条）
sudo journalctl -u rclone-refresh.service --no-pager -n 20

# 实时跟踪服务日志
sudo journalctl -u rclone-refresh.service -f

# 查看最近10分钟的执行记录
sudo journalctl -u rclone-refresh.service --since "10 minutes ago"

# 查看今天的所有执行记录
sudo journalctl -u rclone-refresh.service --since today

# 检查配置文件是否更新
ls -la /root/.config/rclone/rclone.conf
ls -la /home/ec2-user/.config/rclone/rclone.conf

# 查看配置文件中的 token 信息
sudo cat /root/.config/rclone/rclone.conf | grep -A 5 -B 5 "token"

# 手动触发 token 刷新测试
sudo systemctl start rclone-refresh.service
```

#### 服务配置验证

```bash
# 检查 rclone 配置
rclone listremotes

# 测试 OneDrive 连接
rclone lsd USER001: --max-depth 1
```

### 故障排除

#### 本地连接方式
```bash
# 连接到实例
ssh ec2-user@<instance-ip>

# 查看 agent 服务日志
sudo journalctl -u rclone-agent -f

# 查看 token 刷新日志
sudo journalctl -u rclone-refresh -f

# 查看 rclone 配置
rclone listremotes

# 测试 OneDrive 连接
rclone lsd USER001: --max-depth 1
```

#### 常见问题
1. **OneDrive 认证失败**：检查 Secrets Manager 中的凭证
2. **文件路径不存在**：验证 OneDrive 中的文件路径
3. **权限问题**：确保 IAM 角色有足够权限
4. **网络连接**：检查安全组和网络配置

### 清理资源

在 CloudFormation Console 中删除堆栈即可清理所有资源：

```bash
aws cloudformation delete-stack \
  --region your-region \
  --stack-name sqs-rclone-agent-cfn
```

**注意**：删除堆栈会清理所有相关资源，包括 DynamoDB 表中的传输记录。

## Security

See [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) for more information.

## License

This library is licensed under the MIT-0 License. See the LICENSE file.
