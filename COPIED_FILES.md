# 已复制的传输工具文件清单

## 核心传输脚本

### 1. sendcmd2sqs-onedrive.py (16.5 KB)
- **用途**：扫描 OneDrive 用户文件并发送传输任务到 SQS 队列
- **功能**：
  - 从 userList.json 读取用户列表
  - 调用 Microsoft Graph API 列出用户文件
  - 支持 .ignore 文件过滤规则
  - 自动重试机制和限流处理
  - 日志上传到 S3

### 2. sendcmd2sqs-sharepoint.py (18.9 KB)
- **用途**：扫描 SharePoint 站点文件并发送传输任务到 SQS 队列
- **功能**：
  - 从 siteList.json 读取站点列表
  - 搜索并获取 SharePoint 站点信息
  - 遍历所有文档库的文件
  - 支持 .ignore 文件过滤规则
  - 自动重试和日志记录

### 3. query_drive_id.py (2.4 KB)
- **用途**：查询 SharePoint 站点的 Drive ID
- **功能**：
  - 获取指定站点的 Drive ID
  - 用于调试和配置验证

## 部署模板

### 4. deployment-cfn-init.yaml (44.6 KB)
- **用途**：AWS CloudFormation 完整部署模板
- **包含的资源**：
  - SQS 队列（主队列和死信队列）
  - DynamoDB 表（传输状态跟踪）
  - EC2 Auto Scaling Group
  - IAM Role 和 Instance Profile
  - Security Group
  - Secrets Manager（OneDrive 凭证）
- **嵌入的 Python 脚本**：
  - refresh_rclone_config.py - Token 刷新脚本（每 50 分钟）
  - agent.py - SQS 消费者和 rclone 传输代理

## 配置模板

### 5. deploy-params-cfn-init.json.template (691 B)
- **用途**：CloudFormation 部署参数模板
- **包含参数**：VPC、Subnets、InstanceType、OneDrive 凭证、S3 Bucket 等

### 6. userList.json.template (165 B)
- **用途**：OneDrive 用户列表模板
- **格式**：JSON 数组，包含 email 和 workcode 字段

### 7. siteList.json.template (43 B)
- **用途**：SharePoint 站点列表模板
- **格式**：JSON 字符串数组

## 配置文件

### 8. .gitignore (348 B)
- **用途**：Git 版本控制忽略规则
- **排除内容**：凭证文件、日志、临时文件、Python 缓存等

### 9. .ignore (686 B)
- **用途**：文件传输过滤规则（.gitignore 风格）
- **过滤内容**：缓存目录、临时文件、OneNote 文件等

### 10. .semgrep.yml (1.2 KB)
- **用途**：代码安全扫描配置
- **规则**：抑制合理的 time.sleep() 和 subprocess 警告

## 文档

### 11. README.md (17.0 KB)
- **用途**：完整的项目文档
- **内容**：
  - 系统架构说明
  - 部署步骤（CLI 和 Console）
  - 消息格式定义
  - 监控和管理指南
  - 故障排除方法

---

## 文件总计
- **Python 脚本**：3 个（核心传输逻辑）
- **CloudFormation 模板**：1 个（包含嵌入的 agent.py 和 refresh 脚本）
- **配置模板**：3 个
- **配置文件**：3 个
- **文档**：1 个

## 架构概览

```
┌──────────────────────────────────────────────────────────────┐
│                    数据传输系统架构                            │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  1. 扫描阶段（本地运行）                                       │
│     ├─ sendcmd2sqs-onedrive.py  → 扫描 OneDrive             │
│     ├─ sendcmd2sqs-sharepoint.py → 扫描 SharePoint          │
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
   - 编辑并填入 OneDrive 凭证和 AWS 资源信息

2. **部署基础设施**
   ```bash
   aws cloudformation create-stack \
     --stack-name sqs-rclone-agent \
     --template-body file://deployment-cfn-init.yaml \
     --parameters file://deploy-params-cfn-init.json \
     --capabilities CAPABILITY_NAMED_IAM
   ```

3. **配置用户和站点**
   - 编辑 userList.json（OneDrive 用户）
   - 编辑 siteList.json（SharePoint 站点）
   - 上传到 S3: `aws s3 cp userList.json s3://your-bucket/user-list/`

4. **扫描并发送任务**
   ```bash
   # OneDrive 扫描
   python3 sendcmd2sqs-onedrive.py
   
   # SharePoint 扫描
   python3 sendcmd2sqs-sharepoint.py
   ```

5. **监控传输**
   - 查看 DynamoDB 表中的传输状态
   - 查看 CloudWatch Logs
   - 检查 SQS 队列深度

---

**注意**：deployment-cfn-init.yaml 中包含完整的 agent.py 和 refresh_rclone_config.py 代码，部署时会自动安装到 EC2 实例。
