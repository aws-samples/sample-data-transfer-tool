#!/usr/bin/env python3
"""迁移 Dropbox Advanced/Business 团队下所有成员的文件

通过 Dropbox Business API 遍历团队所有成员，再以 Dropbox-API-Select-User
方式列出每个成员个人空间的文件，组装 rclone 指令发送到 SQS（与 OneDrive 逻辑一致）。
"""
import argparse
import json
import logging
import os
import re
import time
from datetime import datetime, timezone

import boto3
from botocore.config import Config
from dateutil import parser as dateutil_parser
import pathspec
import requests
from requests.exceptions import RequestException
from tenacity import retry, stop_after_attempt, retry_if_exception_type


# ============ 常量配置 ============
# Dropbox Business API 配置
# App 类型必须为 "Dropbox Business API" + "Team member file access"
# 所需 scope: members.read, files.metadata.read, files.content.read
DROPBOX_APP_KEY = ""  # 应用 App key (client_id)
DROPBOX_APP_SECRET = ""  # 应用 App secret (client_secret)
DROPBOX_REFRESH_TOKEN = ""  # 离线获取的长效 refresh token (token_access_type=offline)

# AWS 配置
SQS_QUEUE_URL = ""
AWS_REGION = ""
TARGET_S3_BUCKET = ""
DESTINATION_PREFIX = "n-dropbox"
LOG_S3_PREFIX = "aws-dropbox-migration-logs"

# 文件配置
IGNORE_FILE = ".ignore-dropbox"
MEMBER_WHITELIST_FILE = "memberWhiteList.json"

# Dropbox API 端点
DROPBOX_TOKEN_URL = "https://api.dropboxapi.com/oauth2/token"
DROPBOX_MEMBERS_LIST_URL = "https://api.dropboxapi.com/2/team/members/list"
DROPBOX_MEMBERS_LIST_CONTINUE_URL = (
    "https://api.dropboxapi.com/2/team/members/list/continue"
)
DROPBOX_LIST_FOLDER_URL = "https://api.dropboxapi.com/2/files/list_folder"
DROPBOX_LIST_FOLDER_CONTINUE_URL = (
    "https://api.dropboxapi.com/2/files/list_folder/continue"
)
DROPBOX_GET_CURRENT_ACCOUNT_URL = (
    "https://api.dropboxapi.com/2/users/get_current_account"
)

# 一期只迁个人空间：排除挂载的团队文件夹/共享文件夹，避免跨成员重复迁移
# Team Folder 迁移留作二期（需 Dropbox-API-Path-Root / Dropbox-API-Select-Admin）
INCLUDE_MOUNTED_FOLDERS = False

# 网络请求配置（Dropbox API）
REQUEST_TIMEOUT = (10, 60)  # (connect_timeout, read_timeout)
MAX_RETRIES = 10
RETRY_WAIT_MIN = 1  # 最小等待秒数
RETRY_WAIT_MAX = 32  # 最大等待秒数
RETRYABLE_STATUS_CODES = {429, 500, 502, 503, 504}
REQUEST_DELAY = 0.5  # 每次成功请求后的延迟秒数，避免触发限流

# AWS SDK 重试配置
BOTO_CONFIG = Config(
    retries={"max_attempts": 5, "mode": "adaptive"},  # 自适应重试，含指数退避
    connect_timeout=10,
    read_timeout=60,
)

# AWS 客户端（懒加载）
_sqs_client = None
_s3_client = None


def get_sqs_client():
    """获取 SQS 客户端（懒加载单例）"""
    global _sqs_client
    if _sqs_client is None:
        _sqs_client = boto3.client("sqs", region_name=AWS_REGION, config=BOTO_CONFIG)
    return _sqs_client


def get_s3_client():
    """获取 S3 客户端（懒加载单例）"""
    global _s3_client
    if _s3_client is None:
        _s3_client = boto3.client("s3", region_name=AWS_REGION, config=BOTO_CONFIG)
    return _s3_client


# 运行时常量
TIMESTAMP = datetime.now().strftime("%Y%m%d_%H%M%S")
LOGS_DIR = f"logs/{TIMESTAMP}"


# ============ 日志配置 ============
logger = logging.getLogger(__name__)
logger.setLevel(logging.DEBUG)

formatter = logging.Formatter(
    fmt="%(asctime)s - %(levelname)s - %(message)s", datefmt="%Y-%m-%d %H:%M:%S"
)

# 避免重复添加 handler
if not logger.handlers:
    console_handler = logging.StreamHandler()
    console_handler.setLevel(logging.INFO)
    console_handler.setFormatter(formatter)
    logger.addHandler(console_handler)

# 创建日志目录
os.makedirs(LOGS_DIR, exist_ok=True)


# ============ 自定义异常 ============
class RetryableHTTPError(Exception):
    """可重试的HTTP错误"""

    def __init__(self, status_code, message, retry_after=None):
        self.status_code = status_code
        self.retry_after = retry_after
        super().__init__(f"HTTP {status_code}: {message}")


def custom_wait_strategy(retry_state):
    """自定义等待策略：优先使用 Retry-After，否则使用指数退避"""
    exception = retry_state.outcome.exception()

    # 如果异常是 RetryableHTTPError 且包含 retry_after 值，使用它
    if isinstance(exception, RetryableHTTPError) and exception.retry_after:
        return exception.retry_after

    # 否则使用指数退避：2^(attempt_number - 1)，限制在 RETRY_WAIT_MIN 到 RETRY_WAIT_MAX 之间
    wait_time = min(
        RETRY_WAIT_MAX, max(RETRY_WAIT_MIN, 2 ** (retry_state.attempt_number - 1))
    )
    return wait_time


# ============ 工具函数 ============
def get_file_extension(filename):
    """获取文件的扩展名

    Args:
        filename: 文件名

    Returns:
        str: 文件扩展名（包含点号，如 '.txt'），如果没有扩展名则返回空字符串
    """
    if "." in filename:
        return filename[filename.rfind(".") :]
    return ""


def load_ignore_patterns():
    """加载并解析 .ignore 文件中的过滤规则

    Returns:
        pathspec.PathSpec: 编译后的过滤规则对象，如果文件不存在则返回空规则
    """
    if not os.path.exists(IGNORE_FILE):
        logger.info(f"未找到 {IGNORE_FILE} 文件，将不进行过滤")
        return pathspec.PathSpec.from_lines("gitwildmatch", [])

    try:
        with open(IGNORE_FILE, "r", encoding="utf-8") as f:
            patterns = f.readlines()

        spec = pathspec.PathSpec.from_lines("gitwildmatch", patterns)
        logger.info(f"已加载 {IGNORE_FILE} 过滤规则")
        return spec
    except Exception as e:
        logger.warning(f"加载 {IGNORE_FILE} 失败: {e}，将不进行过滤")
        return pathspec.PathSpec.from_lines("gitwildmatch", [])


def load_member_whitelist():
    """加载成员白名单文件

    Returns:
        set: 白名单邮箱集合（小写），如果文件不存在返回 None
    """
    if not os.path.exists(MEMBER_WHITELIST_FILE):
        logger.info(f"未找到 {MEMBER_WHITELIST_FILE} 文件，将不进行成员过滤")
        return None

    try:
        with open(MEMBER_WHITELIST_FILE, "r", encoding="utf-8") as f:
            whitelist = json.load(f)

        if not isinstance(whitelist, list):
            logger.error(f"{MEMBER_WHITELIST_FILE} 格式错误，应该是字符串数组")
            return None

        # 转换为小写集合，便于不区分大小写匹配
        whitelist_set = {email.lower() for email in whitelist}
        logger.info(
            f"已加载 {MEMBER_WHITELIST_FILE}，白名单成员数: {len(whitelist_set)}"
        )
        return whitelist_set

    except json.JSONDecodeError as e:
        logger.error(f"解析 {MEMBER_WHITELIST_FILE} 失败: {e}")
        return None
    except Exception as e:
        logger.warning(f"加载 {MEMBER_WHITELIST_FILE} 失败: {e}")
        return None


def parse_cutoff_date(date_string):
    """Parse and validate cutoff date from command line

    Args:
        date_string: ISO 8601 date string (YYYY-MM-DD or YYYY-MM-DDTHH:MM:SS)

    Returns:
        datetime: UTC datetime object

    Raises:
        ValueError: If date format is invalid
    """
    try:
        dt = datetime.fromisoformat(date_string)

        # If no timezone specified, assume UTC
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        else:
            dt = dt.astimezone(timezone.utc)

        return dt
    except ValueError as e:
        raise ValueError(
            f"Invalid date format: {date_string}. "
            f"Expected ISO 8601 format like '2024-01-01' or '2024-01-15T10:30:00'"
        ) from e


def should_include_item(item, cutoff_date, item_path):
    """Check if item should be included based on server_modified date

    注意：Dropbox 没有"创建时间"字段，使用 server_modified（服务端最后修改时间，
    不受客户端时钟影响，比 client_modified 更可靠）做增量过滤。

    Args:
        item: API response item dict
        cutoff_date: UTC datetime object (None means include all)
        item_path: Relative path for logging

    Returns:
        tuple: (should_include: bool, reason: str)
    """
    if cutoff_date is None:
        return True, "no_filter"

    modified_str = item.get("server_modified")

    if not modified_str:
        logger.warning(
            f"Missing server_modified for: {item_path}, including by default"
        )
        return True, "missing_date"

    try:
        modified_dt = dateutil_parser.isoparse(modified_str)

        if modified_dt >= cutoff_date:
            return True, "after_cutoff"
        else:
            return False, f"modified_{modified_dt.strftime('%Y-%m-%d')}"

    except (ValueError, TypeError) as e:
        logger.error(
            f"Failed to parse server_modified '{modified_str}' for: {item_path}, including by default"
        )
        return True, "parse_error"


@retry(
    stop=stop_after_attempt(MAX_RETRIES),
    wait=custom_wait_strategy,
    retry=retry_if_exception_type((RequestException, RetryableHTTPError)),
    before_sleep=lambda retry_state: logger.warning(
        f"请求失败，URL: {retry_state.args[0]}，{retry_state.outcome.exception()}，"
        f"第{retry_state.attempt_number}次重试，等待{retry_state.next_action.sleep}秒..."
    ),
    reraise=True,
)
def dropbox_post(url, headers, json_body):
    """带重试机制的 POST 请求（Dropbox API 均为 POST）

    Args:
        url: 请求URL
        headers: 请求头
        json_body: JSON 请求体（dict）

    Returns:
        requests.Response: 响应对象

    Raises:
        RetryableHTTPError: 可重试的HTTP错误
        RequestException: 网络请求异常
    """
    response = requests.post(
        url, headers=headers, json=json_body, timeout=REQUEST_TIMEOUT
    )

    if response.status_code in RETRYABLE_STATUS_CODES:
        # 只在限流/错误状态下才检查 Retry-After header
        retry_after_header = response.headers.get("Retry-After")
        retry_after = None
        if retry_after_header:
            try:
                retry_after = float(retry_after_header)
            except (ValueError, TypeError):
                retry_after = None

        # 记录限流相关的响应头信息
        logger.warning(
            f"收到 {response.status_code} 响应 - "
            f"Retry-After: {retry_after if retry_after else 'N/A'} 秒"
        )
        raise RetryableHTTPError(
            response.status_code, response.reason, retry_after=retry_after
        )

    # 成功请求后休眠固定时间，避免触发 Dropbox API 限流
    time.sleep(REQUEST_DELAY)

    return response


# ============ API 函数 ============
def get_access_token():
    """使用 refresh token 获取 Dropbox API 访问令牌

    Returns:
        tuple: (access_token: str, expires_in: int)
    """
    data = {
        "grant_type": "refresh_token",
        "refresh_token": DROPBOX_REFRESH_TOKEN,
        "client_id": DROPBOX_APP_KEY,
        "client_secret": DROPBOX_APP_SECRET,
    }

    try:
        response = requests.post(DROPBOX_TOKEN_URL, data=data, timeout=REQUEST_TIMEOUT)
        response.raise_for_status()
        token_data = response.json()
        return token_data["access_token"], token_data.get("expires_in", 14400)
    except RequestException as e:
        logger.error(f"获取访问令牌失败: {e}")
        raise


class TokenManager:
    """Token 管理器，自动处理 token 刷新"""

    def __init__(self, refresh_margin=300):
        """
        Args:
            refresh_margin: 提前刷新时间（秒），默认 5 分钟
        """
        self._token = None
        self._expires_at = 0
        self._refresh_margin = refresh_margin

    def get_headers(self, select_user=None):
        """获取包含有效 token 的请求头，必要时自动刷新

        Args:
            select_user: 团队成员 ID (dbmid:...)，用于以该成员身份访问其文件。
                         team endpoint（如成员列表）不需要此参数。
        """
        current_time = time.time()

        if self._token is None or current_time >= (
            self._expires_at - self._refresh_margin
        ):
            self._refresh_token()

        headers = {
            "Authorization": f"Bearer {self._token}",
            "Content-Type": "application/json",
        }
        if select_user:
            headers["Dropbox-API-Select-User"] = select_user
        return headers

    def _refresh_token(self):
        """刷新 token"""
        logger.info("正在获取/刷新 access token...")
        self._token, expires_in = get_access_token()
        self._expires_at = time.time() + expires_in
        logger.info("Access token 已刷新")


def get_member_home_namespace_id(token_manager, select_user):
    """获取成员个人空间的 namespace id

    本脚本枚举文件时不带 Dropbox-API-Path-Root，拿到的 path_display 是成员个人
    空间视角（如 /migration-test/a.txt）；而 rclone dropbox 后端默认使用
    root_namespace_id（团队空间），同一路径在团队空间视角下不存在，会报
    directory not found。需将此 id 通过 --dropbox-root-namespace 传给 rclone，
    使两侧视角一致。

    Args:
        token_manager: TokenManager 实例
        select_user: 团队成员 ID (dbmid:...)

    Returns:
        str: home_namespace_id；获取失败返回 None
    """
    try:
        headers = token_manager.get_headers(select_user=select_user)
        # 该端点不接受参数，但必须发送 {} 作为 body（发 null 会返回 500）
        response = dropbox_post(DROPBOX_GET_CURRENT_ACCOUNT_URL, headers, {})
    except (RetryableHTTPError, RequestException) as e:
        logger.error(f"获取成员 namespace 失败: {e}, select_user={select_user}")
        return None

    if response.status_code != 200:
        logger.error(
            f"获取成员 namespace 返回非200: {response.status_code}, "
            f"select_user={select_user}, {response.text[:200]}"
        )
        return None

    home_namespace_id = response.json().get("root_info", {}).get("home_namespace_id")
    if not home_namespace_id:
        logger.error(f"响应中缺少 root_info.home_namespace_id, select_user={select_user}")
        return None

    return home_namespace_id


def list_team_members(token_manager):
    """遍历团队所有成员

    调用 /2/team/members/list (+ /continue 分页)，只返回活跃(active)成员。

    Args:
        token_manager: TokenManager 实例

    Returns:
        list: [{team_member_id, email}] 列表
    """
    members = []
    url = DROPBOX_MEMBERS_LIST_URL
    body = {"limit": 1000}

    while True:
        try:
            headers = token_manager.get_headers()
            response = dropbox_post(url, headers, body)
        except (RetryableHTTPError, RequestException) as e:
            logger.error(f"获取团队成员列表失败，已获取 {len(members)} 个成员: {e}")
            break

        if response.status_code != 200:
            logger.error(
                f"获取团队成员列表返回非200: {response.status_code}, {response.text[:200]}"
            )
            break

        data = response.json()
        for m in data.get("members", []):
            profile = m.get("profile", {})
            status = profile.get("status", {}).get(".tag", "")
            if status != "active":
                logger.debug(
                    f"跳过非活跃成员: {profile.get('email')} (status={status})"
                )
                continue
            members.append(
                {
                    "team_member_id": profile.get("team_member_id"),
                    "email": profile.get("email"),
                }
            )

        if not data.get("has_more"):
            break

        url = DROPBOX_MEMBERS_LIST_CONTINUE_URL
        body = {"cursor": data.get("cursor")}
        logger.debug("检测到成员分页，继续获取下一页...")

    logger.info(f"共获取 {len(members)} 个活跃团队成员")
    return members


# ============ 核心业务函数 ============
def send_to_sqs(member_email, entry, home_namespace_id):
    """拼装消息并发送到SQS队列

    Args:
        member_email: 成员邮箱（用于 rclone impersonate 及目标路径）
        entry: Dropbox 文件 entry dict
        home_namespace_id: 成员个人空间 namespace id（见
            get_member_home_namespace_id，用于对齐枚举与 rclone 的路径视角）

    Returns:
        bool: 发送成功返回 True，失败返回 False
    """
    name = entry["name"]
    path_display = entry.get("path_display", "")

    # 去掉前导斜杠，使其成为相对成员个人根目录的路径
    # （rclone dropbox 后端中前导 / 用于访问 Team Folder，此处需避免误判）
    clean_path = path_display.lstrip("/")

    # Dropbox 文件 id 形如 "id:abc123"，去掉前缀；file_id 全局唯一可避免同名冲突
    file_id = entry["id"].replace("id:", "", 1) if entry.get("id") else "N_A"
    extension = get_file_extension(name)

    source = f"dropbox:{clean_path}"
    destination = f"s3:{TARGET_S3_BUCKET}/{DESTINATION_PREFIX}/{member_email}/{file_id}{extension}"
    logger.debug(f"Source: {source}")
    logger.debug(f"Destination: {destination}")

    # 用 --dropbox-impersonate 在固定的 dropbox: remote 上切换成员身份
    # （对应 OneDrive 的 --onedrive-drive-id）
    # --dropbox-root-namespace 让 rclone 使用成员个人空间做根，与上面 clean_path
    # 的视角一致；缺少它 rclone 会用团队空间做根并报 directory not found
    rclone_args = [
        "--dropbox-impersonate",
        f"{member_email}",
        "--dropbox-root-namespace",
        f"{home_namespace_id}",
        "--progress",
    ]

    # Add hash metadata header if available
    content_hash = entry.get("content_hash")
    if content_hash:
        rclone_args.extend(["--header-upload", f"x-amz-meta-hash:{content_hash}"])
        logger.debug(f"Added hash metadata for {name}: {content_hash}")

    message_body = {
        "source": source,
        "destination": destination,
        "rclone_args": rclone_args,
    }

    try:
        response = get_sqs_client().send_message(
            QueueUrl=SQS_QUEUE_URL, MessageBody=json.dumps(message_body)
        )
        logger.info(f"消息已发送到SQS: {response['MessageId']}")
        return True
    except Exception as e:
        logger.error(f"发送SQS消息失败: {e}", exc_info=True)
        return False


def process_member_files(member, token_manager, ignore_spec, cutoff_date=None):
    """列出单个成员个人空间中的所有文件

    使用 recursive=true 一次性遍历整棵目录树（无需手动递归子文件夹）。

    Args:
        member: 成员信息 dict（含 team_member_id 和 email）
        token_manager: TokenManager 实例
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)

    Returns:
        tuple: (processed_count, skipped_count)
    """
    select_user = member["team_member_id"]
    member_email = member["email"]

    # rclone 需要成员个人空间的 namespace id 才能解析下面枚举出的路径
    home_namespace_id = get_member_home_namespace_id(token_manager, select_user)
    if not home_namespace_id:
        logger.error(
            f"无法获取 {member_email} 的 home_namespace_id，跳过该成员"
            f"（继续投递会导致 rclone 报 directory not found）"
        )
        return 0, 0
    logger.info(f"成员 {member_email} home_namespace_id={home_namespace_id}")

    file_count = 0
    skipped_count = 0

    url = DROPBOX_LIST_FOLDER_URL
    body = {
        "path": "",  # 成员个人空间根目录
        "recursive": True,
        "include_deleted": False,
        "include_media_info": False,
        "include_mounted_folders": INCLUDE_MOUNTED_FOLDERS,
        "limit": 2000,
    }

    try:
        while True:
            try:
                headers = token_manager.get_headers(select_user=select_user)
                response = dropbox_post(url, headers, body)
            except (RetryableHTTPError, RequestException) as e:
                logger.error(
                    f"请求失败，处理中断，已处理 {file_count} 个文件: {e}, "
                    f"member={member_email}"
                )
                break

            if response.status_code != 200:
                logger.warning(
                    f"API返回非200状态码，处理中断，已处理 {file_count} 个文件: "
                    f"{response.status_code}, member={member_email}, {response.text[:200]}"
                )
                break

            response_data = response.json()
            entries = response_data.get("entries", [])

            for entry in entries:
                # recursive=true 已展开整棵树，只处理文件，跳过 folder / deleted
                if entry.get(".tag") != "file":
                    continue

                name = entry["name"]
                path_display = entry.get("path_display", "")
                item_relative_path = path_display.lstrip("/")

                # .ignore 过滤（基于相对路径）
                if ignore_spec.match_file(item_relative_path):
                    logger.debug(f"跳过文件（已过滤）: {item_relative_path}")
                    continue

                # 基于 server_modified 的增量过滤
                should_include, reason = should_include_item(
                    entry, cutoff_date, item_relative_path
                )
                if not should_include:
                    logger.debug(
                        f"跳过文件（修改时间早于cutoff）: {item_relative_path}, {reason}"
                    )
                    skipped_count += 1
                    continue

                logger.debug(
                    f"处理文件 - Name: {name}, Path: {path_display}, ID: {entry.get('id')}"
                )

                # 只有发送成功才计数
                if send_to_sqs(member_email, entry, home_namespace_id):
                    file_count += 1

            if not response_data.get("has_more"):
                break

            url = DROPBOX_LIST_FOLDER_CONTINUE_URL
            body = {"cursor": response_data.get("cursor")}
            logger.debug(f"检测到分页，继续获取下一页数据: member={member_email}")

    except Exception as e:
        logger.error(
            f"处理文件时发生未预期错误，处理中断，已处理 {file_count} 个文件: {e}",
            exc_info=True,
        )

    return file_count, skipped_count


def process_member(member, token_manager, ignore_spec, cutoff_date=None):
    """处理单个团队成员的文件列表

    Args:
        member: 成员信息 dict（含 team_member_id 和 email）
        token_manager: TokenManager 实例
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)
    """
    email = member["email"]
    team_member_id = member["team_member_id"]

    # 使用清理后的 email 命名日志文件
    safe_email = re.sub(r"[^\w\-_.@]", "_", email)
    member_log_file = f"{LOGS_DIR}/{safe_email}.log"
    member_file_handler = logging.FileHandler(member_log_file, encoding="utf-8")
    member_file_handler.setLevel(logging.DEBUG)
    member_file_handler.setFormatter(formatter)

    logger.addHandler(member_file_handler)

    try:
        log_member_header(email, team_member_id)

        total_files, total_skipped = process_member_files(
            member, token_manager, ignore_spec, cutoff_date
        )

        logger.info(f"成员 {email} 处理完成:")
        logger.info(f"  - 已处理文件数: {total_files}")
        if cutoff_date:
            logger.info(
                f"  - 跳过文件数: {total_skipped} (修改时间早于 {cutoff_date.strftime('%Y-%m-%d')})"
            )
        logger.info("=" * 60)

    finally:
        logger.removeHandler(member_file_handler)
        member_file_handler.close()
        upload_single_member_log(member_log_file)


# ============ 日志上传 ============
def upload_single_member_log(log_file):
    """上传单个成员的日志文件到S3

    Args:
        log_file: 日志文件的本地路径
    """
    try:
        if not os.path.exists(log_file):
            logger.warning(f"日志文件不存在: {log_file}")
            return

        log_filename = os.path.basename(log_file)
        s3_key = f"{LOG_S3_PREFIX}/{TIMESTAMP}/{log_filename}"

        with open(log_file, "rb") as f:
            get_s3_client().upload_fileobj(f, TARGET_S3_BUCKET, s3_key)

        logger.info(f"日志已上传: s3://{TARGET_S3_BUCKET}/{s3_key}")

    except Exception as e:
        logger.error(f"上传日志失败 {log_file}: {e}", exc_info=True)


# ============ 主程序 ============
def log_member_header(email, team_member_id):
    """记录成员信息的标题"""
    logger.info("=" * 60)
    logger.info(f"成员: {email}")
    logger.info(f"Team Member ID: {team_member_id}")
    logger.info("=" * 60)


def validate_credentials():
    """校验 Dropbox 凭证是否已配置

    Returns:
        list: 未配置（为空）的常量名列表，全部已配置则返回空列表
    """
    return [
        name
        for name, value in (
            ("DROPBOX_APP_KEY", DROPBOX_APP_KEY),
            ("DROPBOX_APP_SECRET", DROPBOX_APP_SECRET),
            ("DROPBOX_REFRESH_TOKEN", DROPBOX_REFRESH_TOKEN),
        )
        if not value
    ]


def main():
    """主函数"""
    # Parse command line arguments
    parser = argparse.ArgumentParser(
        description="迁移 Dropbox Advanced/Business 团队所有成员的文件，支持增量同步",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 迁移所有成员的所有文件
  python sendcmd2sqs-dropbox.py

  # 只迁移2024年1月1日之后修改的文件
  python sendcmd2sqs-dropbox.py --modified-after 2024-01-01

  # 只迁移指定时间之后修改的文件
  python sendcmd2sqs-dropbox.py --modified-after 2024-01-15T10:30:00

  # 启用成员白名单过滤，只迁移 memberWhiteList.json 中列出的成员
  python sendcmd2sqs-dropbox.py -f
        """,
    )
    parser.add_argument(
        "--modified-after",
        type=str,
        default=None,
        metavar="DATE",
        help="只迁移在指定日期之后修改的文件 (ISO 8601格式: YYYY-MM-DD 或 YYYY-MM-DDTHH:MM:SS)",
    )
    parser.add_argument(
        "-f",
        "--filter",
        action="store_true",
        default=False,
        help=f"启用成员过滤，只迁移邮箱在 {MEMBER_WHITELIST_FILE} 白名单中的成员的文件",
    )

    args = parser.parse_args()

    # 启动前校验凭证，避免拿空凭证去换 token 得到难懂的 400 错误
    missing = validate_credentials()
    if missing:
        logger.error(
            "缺少 Dropbox 凭证配置: %s。请在脚本顶部填写这些常量后再运行。",
            ", ".join(missing),
        )
        logger.error(
            "获取方式: 在 https://www.dropbox.com/developers/apps 的 App "
            "(Scoped access + Team member file access) Settings 页查看 App key/secret，"
            "并以 token_access_type=offline 走 OAuth 授权码流程换取 refresh token。"
        )
        return 1

    # Validate and parse cutoff date if provided
    cutoff_date = None
    if args.modified_after:
        try:
            cutoff_date = parse_cutoff_date(args.modified_after)
            logger.info(
                f"=== 增量同步模式: 只迁移 {cutoff_date.strftime('%Y-%m-%d %H:%M:%S UTC')} 之后修改的文件 ==="
            )
        except ValueError as e:
            logger.error(f"日期格式错误: {e}")
            return 1
    else:
        logger.info("=== 全量迁移模式: 迁移所有文件 ===")

    # 加载成员白名单（如果启用过滤）
    member_whitelist = None
    if args.filter:
        member_whitelist = load_member_whitelist()
        if member_whitelist is None:
            logger.error(f"启用了 --filter 但无法加载 {MEMBER_WHITELIST_FILE}，退出")
            return 1
        logger.info(f"=== 成员过滤已启用: 只迁移白名单成员的文件 ===")

    ignore_spec = load_ignore_patterns()
    token_manager = TokenManager()

    members = list_team_members(token_manager)
    if not members:
        logger.error("未获取到任何团队成员，退出")
        return 1

    # 应用成员白名单过滤
    if member_whitelist is not None:
        before = len(members)
        members = [
            m for m in members if (m.get("email") or "").lower() in member_whitelist
        ]
        logger.info(f"成员白名单过滤: {before} -> {len(members)} 个成员")
        if not members:
            logger.error("白名单过滤后无可处理成员，退出")
            return 1

    for member in members:
        process_member(member, token_manager, ignore_spec, cutoff_date)

    logger.info(
        f"所有成员处理完成，日志已上传到: s3://{TARGET_S3_BUCKET}/{LOG_S3_PREFIX}/{TIMESTAMP}/"
    )
    return 0


if __name__ == "__main__":
    import sys

    sys.exit(main())
