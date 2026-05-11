#!/usr/bin/env python3
"""迁移 SharePoint 站点文件"""
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
from tenacity import retry, stop_after_attempt, wait_exponential, retry_if_exception_type


# ============ 常量配置 ============
# Microsoft Graph API 配置
CLIENT_ID = ""
CLIENT_SECRET = ""
TENANT_ID = ""

# AWS 配置
SQS_QUEUE_URL = ""
AWS_REGION = ""
TARGET_S3_BUCKET = ""
DESTINATION_PREFIX = "one-drive-sp"
LOG_S3_PREFIX = "aws-sharepoint-migration-logs"

# 文件配置
SITE_LIST_FILE = "siteList.json"
IGNORE_FILE = ".ignore-sharepoint"
USER_WHITELIST_FILE = "userWhiteList.json"

# 网络请求配置（Microsoft Graph API）
REQUEST_TIMEOUT = (10, 60)  # (connect_timeout, read_timeout)
MAX_RETRIES = 5
RETRY_WAIT_MIN = 1  # 最小等待秒数
RETRY_WAIT_MAX = 30  # 最大等待秒数
RETRYABLE_STATUS_CODES = {429, 500, 502, 503, 504}

# AWS SDK 重试配置
BOTO_CONFIG = Config(
    retries={
        'max_attempts': 5,
        'mode': 'adaptive'  # 自适应重试，含指数退避
    },
    connect_timeout=10,
    read_timeout=60
)

# AWS 客户端（懒加载）
_sqs_client = None
_s3_client = None


def get_sqs_client():
    """获取 SQS 客户端（懒加载单例）"""
    global _sqs_client
    if _sqs_client is None:
        _sqs_client = boto3.client('sqs', region_name=AWS_REGION, config=BOTO_CONFIG)
    return _sqs_client


def get_s3_client():
    """获取 S3 客户端（懒加载单例）"""
    global _s3_client
    if _s3_client is None:
        _s3_client = boto3.client('s3', region_name=AWS_REGION, config=BOTO_CONFIG)
    return _s3_client


# 运行时常量
TIMESTAMP = datetime.now().strftime('%Y%m%d_%H%M%S')
LOGS_DIR = f'logs/{TIMESTAMP}'


# ============ 日志配置 ============
logger = logging.getLogger(__name__)
logger.setLevel(logging.DEBUG)

formatter = logging.Formatter(
    fmt='%(asctime)s - %(levelname)s - %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)

console_handler = logging.StreamHandler()
console_handler.setLevel(logging.INFO)
console_handler.setFormatter(formatter)
logger.addHandler(console_handler)

# 创建日志目录
os.makedirs(LOGS_DIR, exist_ok=True)


# ============ 自定义异常 ============
class RetryableHTTPError(Exception):
    """可重试的HTTP错误"""
    def __init__(self, status_code, message):
        self.status_code = status_code
        super().__init__(f"HTTP {status_code}: {message}")


# ============ 工具函数 ============
def get_file_extension(filename):
    """获取文件的扩展名

    Args:
        filename: 文件名

    Returns:
        str: 文件扩展名（包含点号，如 '.txt'），如果没有扩展名则返回空字符串
    """
    if '.' in filename:
        return filename[filename.rfind('.'):]
    return ''


def load_ignore_patterns():
    """加载并解析 .ignore 文件中的过滤规则

    Returns:
        pathspec.PathSpec: 编译后的过滤规则对象，如果文件不存在则返回空规则
    """
    if not os.path.exists(IGNORE_FILE):
        logger.info(f"未找到 {IGNORE_FILE} 文件，将不进行过滤")
        return pathspec.PathSpec.from_lines('gitwildmatch', [])

    try:
        with open(IGNORE_FILE, 'r', encoding='utf-8') as f:
            patterns = f.readlines()

        spec = pathspec.PathSpec.from_lines('gitwildmatch', patterns)
        logger.info(f"已加载 {IGNORE_FILE} 过滤规则")
        return spec
    except Exception as e:
        logger.warning(f"加载 {IGNORE_FILE} 失败: {e}，将不进行过滤")
        return pathspec.PathSpec.from_lines('gitwildmatch', [])


def load_user_whitelist():
    """加载用户白名单文件

    Returns:
        set: 白名单邮箱集合（小写），如果文件不存在返回 None
    """
    if not os.path.exists(USER_WHITELIST_FILE):
        logger.info(f"未找到 {USER_WHITELIST_FILE} 文件，将不进行创建者过滤")
        return None

    try:
        with open(USER_WHITELIST_FILE, 'r', encoding='utf-8') as f:
            whitelist = json.load(f)

        if not isinstance(whitelist, list):
            logger.error(f"{USER_WHITELIST_FILE} 格式错误，应该是字符串数组")
            return None

        # 转换为小写集合，便于不区分大小写匹配
        whitelist_set = {email.lower() for email in whitelist}
        logger.info(f"已加载 {USER_WHITELIST_FILE}，白名单用户数: {len(whitelist_set)}")
        return whitelist_set

    except json.JSONDecodeError as e:
        logger.error(f"解析 {USER_WHITELIST_FILE} 失败: {e}")
        return None
    except Exception as e:
        logger.warning(f"加载 {USER_WHITELIST_FILE} 失败: {e}")
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
    """Check if item should be included based on creation date

    Args:
        item: API response item dict
        cutoff_date: UTC datetime object (None means include all)
        item_path: Relative path for logging

    Returns:
        tuple: (should_include: bool, reason: str)
    """
    if cutoff_date is None:
        return True, "no_filter"

    created_str = item.get('createdDateTime')

    if not created_str:
        logger.warning(f"Missing createdDateTime for: {item_path}, including by default")
        return True, "missing_date"

    try:
        created_dt = dateutil_parser.isoparse(created_str)

        if created_dt >= cutoff_date:
            return True, "after_cutoff"
        else:
            return False, f"created_{created_dt.strftime('%Y-%m-%d')}"

    except (ValueError, TypeError) as e:
        logger.error(f"Failed to parse createdDateTime '{created_str}' for: {item_path}, including by default")
        return True, "parse_error"


@retry(
    stop=stop_after_attempt(MAX_RETRIES),
    wait=wait_exponential(multiplier=1, min=RETRY_WAIT_MIN, max=RETRY_WAIT_MAX),
    retry=retry_if_exception_type((RequestException, RetryableHTTPError)),
    before_sleep=lambda retry_state: logger.warning(
        f"请求失败，{retry_state.outcome.exception()}，"
        f"第{retry_state.attempt_number}次重试，等待{retry_state.next_action.sleep}秒..."
    ),
    reraise=True
)
def request_with_retry(url, headers):
    """带重试机制的GET请求

    Args:
        url: 请求URL
        headers: 请求头

    Returns:
        requests.Response: 响应对象

    Raises:
        RetryableHTTPError: 可重试的HTTP错误
        RequestException: 网络请求异常
    """
    response = requests.get(url, headers=headers, timeout=REQUEST_TIMEOUT)

    if response.status_code in RETRYABLE_STATUS_CODES:
        raise RetryableHTTPError(response.status_code, response.reason)

    return response


# ============ API 函数 ============
def get_access_token():
    """获取 Microsoft Graph API 访问令牌"""
    token_url = f"https://login.microsoftonline.com/{TENANT_ID}/oauth2/v2.0/token"
    data = {
        'client_id': CLIENT_ID,
        'client_secret': CLIENT_SECRET,
        'scope': 'https://graph.microsoft.com/.default',
        'grant_type': 'client_credentials'
    }

    try:
        response = requests.post(token_url, data=data, timeout=REQUEST_TIMEOUT)
        response.raise_for_status()
        token_data = response.json()
        return token_data['access_token']
    except RequestException as e:
        logger.error(f"获取访问令牌失败: {e}")
        raise


class TokenManager:
    """Token 管理器，自动处理 token 刷新"""

    def __init__(self, refresh_margin=300):
        self._token = None
        self._expires_at = 0
        self._refresh_margin = refresh_margin

    def get_headers(self):
        """获取包含有效 token 的请求头，必要时自动刷新"""
        current_time = time.time()
        if self._token is None or current_time >= (self._expires_at - self._refresh_margin):
            self._refresh_token()
        return {'Authorization': f'Bearer {self._token}'}

    def _refresh_token(self):
        """刷新 token"""
        logger.info("正在获取/刷新 access token...")
        self._token = get_access_token()
        self._expires_at = time.time() + 3600
        logger.info("Access token 已刷新")


def list_sites():
    """从 siteList.json 文件读取站点 ID 列表

    Returns:
        list: 站点 ID 列表（字符串数组）
    """
    try:
        with open(SITE_LIST_FILE, 'r', encoding='utf-8') as f:
            sites = json.load(f)

        if not isinstance(sites, list):
            logger.error(f"{SITE_LIST_FILE} 格式错误，应该是字符串数组")
            raise ValueError("站点列表格式错误")

        logger.info(f"从 {SITE_LIST_FILE} 加载了 {len(sites)} 个站点")
        return sites
    except FileNotFoundError:
        logger.error(f"未找到 {SITE_LIST_FILE} 文件")
        raise
    except json.JSONDecodeError as e:
        logger.error(f"解析 {SITE_LIST_FILE} 失败: {e}")
        raise


def get_site_by_id(site_id, token_manager):
    """通过站点 ID 获取站点信息

    Args:
        site_id: 站点 ID（格式如 contoso.sharepoint.com,guid1,guid2）
        token_manager: TokenManager 实例，用于获取有效的认证令牌

    Returns:
        dict: 站点信息，如果找不到返回None
    """
    try:
        url = f"https://graph.microsoft.com/v1.0/sites/{site_id}"

        headers = token_manager.get_headers()
        response = request_with_retry(url, headers)

        if response.status_code == 404:
            logger.error(f"站点不存在: {site_id}")
            return None

        if response.status_code != 200:
            logger.error(f"获取站点失败: {response.status_code}, 站点 ID: {site_id}")
            return None

        site_info = response.json()
        logger.info(f"找到站点: {site_info.get('displayName', 'N/A')}, ID: {site_id}")
        return site_info

    except (RetryableHTTPError, RequestException) as e:
        logger.error(f"获取站点异常 {site_id}: {e}")
        return None


def get_site_drives(site_id, token_manager):
    """获取站点的所有文档库

    Args:
        site_id: 站点ID
        token_manager: TokenManager 实例，用于获取有效的认证令牌

    Returns:
        list: 文档库列表
    """
    url = f"https://graph.microsoft.com/v1.0/sites/{site_id}/drives"
    drives = []

    try:
        while url:
            headers = token_manager.get_headers()
            response = request_with_retry(url, headers)

            if response.status_code != 200:
                logger.warning(f"获取文档库列表失败: {response.status_code}")
                return drives

            data = response.json()
            drives.extend(data.get('value', []))
            url = data.get('@odata.nextLink')

        logger.info(f"站点共有 {len(drives)} 个文档库")
        return drives

    except (RetryableHTTPError, RequestException) as e:
        logger.error(f"获取文档库列表异常: {e}")
        return drives


# ============ 核心业务函数 ============
def send_to_sqs(site_id, drive_id, site_name, drive_name, name, item_id, parent_id, extension, parent_path, file_hash=None):
    """拼装消息并发送到SQS队列

    Args:
        site_id: 完整的站点ID
        drive_id: 文档库ID
        site_name: 站点名称（用于rclone remote）
        drive_name: 文档库名称
        name: 文件名
        item_id: 文件ID
        parent_id: 父目录ID
        extension: 文件扩展名
        parent_path: 父目录路径
        file_hash: 文件哈希值 (quickXorHash)，可选

    Returns:
        bool: 发送成功返回 True，失败返回 False
    """
    # 清理 parent_path，去掉 "/drives/{drive_id}/root:" 前缀
    clean_parent_path = parent_path.replace(f"/drives/{drive_id}/root:", "") if parent_path else ""

    # Source 格式: 站点名称:文档库名称/路径/文件名
    source = f"sharepoint:{clean_parent_path}/{name}"

    # Destination 格式: /one-drive/{站点ID}/{文档库ID}/{父目录ID}/{文件ID}.扩展名
    destination = f"s3:{TARGET_S3_BUCKET}/{DESTINATION_PREFIX}/{site_id}/{drive_id}/{parent_id}/{item_id}{extension}"

    logger.debug(f"Source: {source}")
    logger.debug(f"Destination: {destination}")

    # Build rclone_args conditionally based on file_hash availability
    rclone_args = [
        "--onedrive-drive-id",
        f"{drive_id}",
        "--progress"
    ]

    # Add hash metadata header if available
    if file_hash:
        rclone_args.extend([
            "--header-upload",
            f"x-amz-meta-hash:{file_hash}"
        ])
        logger.debug(f"Added hash metadata for {name}: {file_hash}")

    message_body = {
        "source": source,
        "destination": destination,
        "rclone_args": rclone_args
    }
    logger.debug(f"Message Body: {json.dumps(message_body, ensure_ascii=False)}")

    try:
        response = get_sqs_client().send_message(
            QueueUrl=SQS_QUEUE_URL,
            MessageBody=json.dumps(message_body)
        )
        logger.info(f"消息已发送到SQS: {response['MessageId']}")
        return True
    except Exception as e:
        logger.error(f"发送SQS消息失败: {e}", exc_info=True)
        return False


def process_drive_files(site_id, drive_id, site_name, drive_name,
                        token_manager, ignore_spec, cutoff_date=None, user_whitelist=None,
                        path="root", current_path=""):
    """递归列出文档库中的文件和文件夹

    Args:
        site_id: 完整站点ID
        drive_id: 文档库ID
        site_name: 站点名称（用于rclone）
        drive_name: 文档库名称
        token_manager: TokenManager 实例，用于获取有效的认证令牌
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)
        user_whitelist: 用户邮箱白名单集合 (None = 不过滤)
        path: 当前API路径，默认为root
        current_path: 当前相对路径，用于过滤规则匹配

    Returns:
        tuple: (processed_count, skipped_count)
    """
    file_count = 0
    skipped_count = 0

    try:
        url = f"https://graph.microsoft.com/v1.0/sites/{site_id}/drives/{drive_id}/{path}/children"

        # 处理分页逻辑
        while url:
            try:
                # 每次请求前获取最新的认证头，确保 token 有效
                headers = token_manager.get_headers()
                response = request_with_retry(url, headers)
            except (RetryableHTTPError, RequestException) as e:
                logger.error(f"请求失败，处理中断，已处理 {file_count} 个文件: {e}")
                return file_count

            if response.status_code != 200:
                logger.warning(f"API返回非200状态码，处理中断，已处理 {file_count} 个文件: {response.status_code}, URL: {url}")
                return file_count

            response_data = response.json()
            items = response_data.get('value', [])

            for item in items:
                name = item['name']
                item_relative_path = f"{current_path}/{name}".lstrip('/')

                if 'folder' in item:
                    folder_path_with_slash = item_relative_path + '/'

                    if ignore_spec.match_file(folder_path_with_slash) or ignore_spec.match_file(item_relative_path):
                        logger.info(f"跳过文件夹（已过滤）: {item_relative_path}")
                        continue

                    # NOTE: For folders, we always recurse regardless of creation date
                    # because old folders may contain new files
                    item_id = item['id']
                    subfolder_processed, subfolder_skipped = process_drive_files(
                        site_id, drive_id, site_name, drive_name,
                        token_manager, ignore_spec, cutoff_date, user_whitelist,
                        f"items/{item_id}", item_relative_path
                    )
                    file_count += subfolder_processed
                    skipped_count += subfolder_skipped
                else:
                    # .ignore filtering (existing)
                    if ignore_spec.match_file(item_relative_path):
                        logger.info(f"跳过文件（已过滤）: {item_relative_path}")
                        continue

                    # Date-based filtering
                    should_include, reason = should_include_item(item, cutoff_date, item_relative_path)

                    if not should_include:
                        logger.debug(f"跳过文件（创建时间早于cutoff）: {item_relative_path}, {reason}")
                        skipped_count += 1
                        continue

                    # Creator whitelist filtering
                    if user_whitelist is not None:
                        created_by = item.get('createdBy', {})
                        creator_user = created_by.get('user', {})
                        creator_email = creator_user.get('email', '')

                        if not creator_email:
                            logger.warning(f"跳过文件（无创建者邮箱）: {item_relative_path}")
                            skipped_count += 1
                            continue

                        if creator_email.lower() not in user_whitelist:
                            logger.debug(f"跳过文件（创建者不在白名单）: {item_relative_path}, 创建者: {creator_email}")
                            skipped_count += 1
                            continue

                    extension = get_file_extension(name)
                    item_id = item['id']
                    parent_ref = item.get('parentReference', {})
                    parent_path = parent_ref.get('path', 'N/A')
                    parent_id = parent_ref.get('id', 'N/A')

                    # Extract file hash (quickXorHash) if available
                    file_hash = None
                    if 'file' in item:
                        hashes = item.get('file', {}).get('hashes', {})
                        file_hash = hashes.get('quickXorHash')
                        if file_hash:
                            logger.debug(f"Extracted hash for {name}: {file_hash}")
                        else:
                            logger.debug(f"No hash available for {name}")

                    logger.debug(f"处理文件 - Name: {name}, Extension: {extension}, ID: {item_id}")
                    logger.debug(f"Parent Path: {parent_path}, Parent ID: {parent_id}")
                    logger.debug(f"目标路径: /{DESTINATION_PREFIX}/{site_id}/{drive_id}/{parent_id}/{item_id}{extension}")

                    # 只有发送成功才计数
                    if send_to_sqs(site_id, drive_id, site_name, drive_name, name, item_id, parent_id, extension, parent_path, file_hash):
                        file_count += 1

            url = response_data.get('@odata.nextLink')
            if url:
                logger.debug(f"检测到分页，继续获取下一页数据: {url}")

    except Exception as e:
        logger.error(f"处理文件时发生未预期错误，处理中断，已处理 {file_count} 个文件: {e}", exc_info=True)

    return file_count, skipped_count


def process_site(site_id, token_manager, ignore_spec, cutoff_date=None, user_whitelist=None):
    """处理单个 SharePoint 站点

    Args:
        site_id: 站点 ID（从 siteList.json 读取）
        token_manager: TokenManager 实例，用于获取有效的认证令牌
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)
        user_whitelist: 用户邮箱白名单集合 (None = 不过滤)
    """
    # 使用 site_id 命名日志文件（site_id 已经是安全字符）
    safe_site_id = re.sub(r'[^\w\-_,.]', '_', site_id)
    site_log_file = f'{LOGS_DIR}/{safe_site_id}.log'
    site_file_handler = logging.FileHandler(site_log_file, encoding='utf-8')
    site_file_handler.setLevel(logging.DEBUG)
    site_file_handler.setFormatter(formatter)

    logger.addHandler(site_file_handler)

    try:
        logger.info("=" * 60)
        logger.info(f"站点 ID: {site_id}")
        logger.info("=" * 60)

        # 通过 ID 获取站点信息
        site_info = get_site_by_id(site_id, token_manager)

        if site_info is None:
            logger.error(f"✗ {site_id}: 无法找到站点")
            return

        site_display_name = site_info.get('displayName', 'N/A')
        site_url = site_info.get('webUrl', 'N/A')

        logger.info(f"站点显示名称: {site_display_name}")
        logger.info(f"站点 URL: {site_url}")
        logger.info("=" * 60)

        # 获取站点的所有文档库
        drives = get_site_drives(site_id, token_manager)

        if not drives:
            logger.warning(f"站点 {site_id} 没有文档库")
            return

        logger.info(f"将迁移所有 {len(drives)} 个文档库")

        # 处理每个文档库
        total_files = 0
        total_skipped = 0
        for drive in drives:
            drive_id = drive['id']
            drive_name = drive.get('name', 'Unknown')
            drive_type = drive.get('driveType', 'Unknown')

            logger.info(f"处理文档库: {drive_name} (类型: {drive_type}, ID: {drive_id})")

            file_count, skipped_count = process_drive_files(
                site_id, drive_id, site_display_name, drive_name,
                token_manager, ignore_spec, cutoff_date, user_whitelist
            )

            logger.info(f"文档库 {drive_name} 处理文件数: {file_count}")
            total_files += file_count
            total_skipped += skipped_count

        logger.info(f"站点 {site_display_name} ({site_id}) 处理完成:")
        logger.info(f"  - 已处理文件数: {total_files}")
        if total_skipped > 0:
            logger.info(f"  - 跳过文件数: {total_skipped}")
        logger.info("=" * 60)

    except Exception as e:
        logger.error(f"处理站点 {site_id} 失败: {e}", exc_info=True)
    finally:
        logger.removeHandler(site_file_handler)
        site_file_handler.close()
        upload_single_site_log(site_log_file)


# ============ 日志上传 ============
def upload_single_site_log(log_file):
    """上传单个站点的日志文件到S3

    Args:
        log_file: 日志文件的本地路径
    """
    try:
        if not os.path.exists(log_file):
            logger.warning(f"日志文件不存在: {log_file}")
            return

        log_filename = os.path.basename(log_file)
        s3_key = f'{LOG_S3_PREFIX}/{TIMESTAMP}/{log_filename}'

        with open(log_file, 'rb') as f:
            get_s3_client().upload_fileobj(f, TARGET_S3_BUCKET, s3_key)

        logger.info(f"日志已上传: s3://{TARGET_S3_BUCKET}/{s3_key}")

    except Exception as e:
        logger.error(f"上传日志失败 {log_file}: {e}", exc_info=True)


# ============ 主程序 ============
def main():
    """主函数"""
    # Parse command line arguments
    parser = argparse.ArgumentParser(
        description='迁移 SharePoint 站点文件，支持增量同步',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 迁移所有文件
  python sendcmd2sqs-sharepoint.py

  # 只迁移2024年1月1日之后创建的文件
  python sendcmd2sqs-sharepoint.py --created-after 2024-01-01

  # 只迁移指定时间之后创建的文件
  python sendcmd2sqs-sharepoint.py --created-after 2024-01-15T10:30:00
        """
    )
    parser.add_argument(
        '--created-after',
        type=str,
        default=None,
        metavar='DATE',
        help='只迁移在指定日期之后创建的文件 (ISO 8601格式: YYYY-MM-DD 或 YYYY-MM-DDTHH:MM:SS)'
    )
    parser.add_argument(
        '-f', '--filter',
        action='store_true',
        default=False,
        help=f'启用创建者过滤，只迁移创建者邮箱在 {USER_WHITELIST_FILE} 白名单中的文件'
    )

    args = parser.parse_args()

    # Validate and parse cutoff date if provided
    cutoff_date = None
    if args.created_after:
        try:
            cutoff_date = parse_cutoff_date(args.created_after)
            logger.info(f"=== 增量同步模式: 只迁移 {cutoff_date.strftime('%Y-%m-%d %H:%M:%S UTC')} 之后创建的文件 ===")
        except ValueError as e:
            logger.error(f"日期格式错误: {e}")
            return 1
    else:
        logger.info("=== 全量迁移模式: 迁移所有文件 ===")

    # 加载创建者白名单（如果启用过滤）
    user_whitelist = None
    if args.filter:
        user_whitelist = load_user_whitelist()
        if user_whitelist is None:
            logger.error(f"启用了 --filter 但无法加载 {USER_WHITELIST_FILE}，退出")
            return 1
        logger.info(f"=== 创建者过滤已启用: 只迁移白名单用户创建的文件 ===")

    ignore_spec = load_ignore_patterns()
    token_manager = TokenManager()
    site_ids = list_sites()

    for site_id in site_ids:
        logger.info(f"\n开始处理站点 ID: {site_id}")
        process_site(site_id, token_manager, ignore_spec, cutoff_date, user_whitelist)

    logger.info(f"\n所有站点处理完成，日志已上传到: s3://{TARGET_S3_BUCKET}/{LOG_S3_PREFIX}/{TIMESTAMP}/")
    return 0


if __name__ == '__main__':
    import sys
    sys.exit(main())
