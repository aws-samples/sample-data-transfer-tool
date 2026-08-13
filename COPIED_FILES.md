# 已复制的传输工具文件清单

## 核心传输脚本

### 1. sendcmd2sqs-dropbox.py
- **用途**：扫描 Dropbox Advanced/Business 团队成员文件并发送传输任务到 SQS 队列
- **功能**：
  - 调用 `/2/team/members/list` 遍历团队所有活跃成员
  - 用 `Dropbox-API-Select-User` 以成员身份列出其个人空间文件
  - 取每个成员的 `home_namespace_id`，作为 `--dropbox-root-namespace` 传给 rclone
  - 支持 `.ignore-dropbox` 过滤规则、`memberWhiteList.json` 成员白名单
  - 支持 `--modified-after` 增量同步
  - 透传 Dropbox `content_hash` 为 S3 对象元数据 `x-amz-meta-hash`
  - 自动重试机制和限流处理
  - 日志上传到 S3

## 部署模板

### 2. deployment-cfn-init.yaml
- **用途**：AWS CloudFormation 完整部署模板
- **包含的资源**：
  - SQS 队列（主队列和死信队列）
  - DynamoDB 表（传输状态跟踪）
  - EC2 Auto Scaling Group
  - IAM Role 和 Instance Profile
  - Security Group
  - Secrets Manager（Dropbox 凭证）
- **嵌入的 Python 脚本**：
  - refresh_rclone_config.py - Token 刷新脚本（每 50 分钟）
  - agent.py - SQS 消费者和 rclone 传输代理

## 配置模板

### 3. deploy-params-cfn-init.json.template
- **用途**：CloudFormation 部署参数模板
- **包含参数**：VPC、Subnets、InstanceType、InstanceCount、Dropbox 凭证、DestinationBucket

### 4. memberWhiteList.json.template
- **用途**：Dropbox 团队成员白名单模板（配合 `-f` 参数使用）
- **格式**：JSON 字符串数组，元素为成员邮箱

## 配置文件

### 5. .gitignore
- **用途**：Git 版本控制忽略规则
- **排除内容**：凭证文件、日志、临时文件、Python 缓存等

### 6. .ignore-dropbox
- **用途**：文件传输过滤规则（.gitignore 风格），由 sendcmd2sqs-dropbox.py 读取
- **过滤内容**：缓存目录、临时文件、OneNote 文件等

### 7. .semgrep.yml
- **用途**：代码安全扫描配置
- **规则**：抑制合理的 time.sleep() 和 subprocess 警告

## 文档

### 8. README.md
- **用途**：完整的项目文档
- **内容**：
  - 系统架构说明
  - Dropbox 应用准备与 refresh token 获取
  - 部署步骤（CLI 和 Console）
  - 消息格式定义
  - 监控和管理指南
  - 故障排除方法

---

## 架构概览

```
┌──────────────────────────────────────────────────────────────┐
│                    数据传输系统架构                            │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  1. 扫描阶段（本地运行）                                       │
│     ├─ sendcmd2sqs-dropbox.py → 遍历团队成员及其文件          │
│     └─ 发送消息到 SQS 队列                                    │
│                                                              │
│  2. 部署阶段（CloudFormation）                                │
│     └─ deployment-cfn-init.yaml                              │
│        ├─ 创建 SQS 队列                                       │
│        ├─ 创建 DynamoDB 表                                    │
│        ├─ 创建 Auto Scaling Group                            │
│        └─ 部署 agent.py（嵌入式）                             │
│                                                              │
│  3. 传输阶段（EC2 实例自动运行）                               │
│     ├─ agent.py - 多线程消费 SQS 消息                         │
│     ├─ rclone copyto - 执行文件传输                           │
│     └─ refresh_rclone_config.py - 每 50 分钟刷新 token       │
│                                                              │
│  4. 监控阶段                                                  │
│     ├─ DynamoDB - 查看传输状态                                │
│     ├─ CloudWatch Logs - 查看日志                             │
│     └─ SQS DLQ - 查看失败消息                                 │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

## 使用流程

1. **配置凭证**
   - 复制模板文件并填入实际值
   - `cp deploy-params-cfn-init.json.template deploy-params-cfn-init.json`
   - 编辑并填入 Dropbox 凭证和 AWS 资源信息

2. **部署基础设施**
   ```bash
   aws cloudformation create-stack \
     --stack-name sqs-rclone-agent \
     --template-body file://deployment-cfn-init.yaml \
     --parameters file://deploy-params-cfn-init.json \
     --capabilities CAPABILITY_NAMED_IAM
   ```

3. **配置脚本常量**
   - 编辑 `sendcmd2sqs-dropbox.py` 顶部的 `DROPBOX_APP_KEY`、`DROPBOX_APP_SECRET`、
     `DROPBOX_REFRESH_TOKEN`、`SQS_QUEUE_URL`、`AWS_REGION`、`TARGET_S3_BUCKET`
   - `TARGET_S3_BUCKET` 必须与部署参数 `DestinationBucket` 填成同一个桶
   - 如需限定成员范围：`cp memberWhiteList.json.template memberWhiteList.json`

4. **扫描并发送任务**
   ```bash
   # 全量扫描所有团队成员
   python3 sendcmd2sqs-dropbox.py

   # 只扫描白名单成员
   python3 sendcmd2sqs-dropbox.py -f

   # 增量：只迁移指定日期后修改的文件
   python3 sendcmd2sqs-dropbox.py --modified-after 2024-01-01
   ```

5. **监控传输**
   - 查看 DynamoDB 表中的传输状态
   - 查看 CloudWatch Logs
   - 检查 SQS 队列深度

---

**注意**：deployment-cfn-init.yaml 中包含完整的 agent.py 和 refresh_rclone_config.py 代码，部署时会自动安装到 EC2 实例。
