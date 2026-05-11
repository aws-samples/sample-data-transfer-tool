#!/usr/bin/env python3
"""迁移所有用户的 OneDrive 文件"""
import argparse
import json
import logging
import os
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
# Microsoft Graph API 配置
CLIENT_ID = ""
CLIENT_SECRET = ""
TENANT_ID = ""

# AWS 配置
SQS_QUEUE_URL = ""
AWS_REGION = ""
TARGET_S3_BUCKET = ""
DESTINATION_PREFIX = "n-one-drive"
LOG_S3_PREFIX = "aws-onedrive-migration-logs"

# 文件配置
USER_LIST_FILE = "userList.json"
IGNORE_FILE = ".ignore-onedrive"

# 网络请求配置（Microsoft Graph API）
REQUEST_TIMEOUT = (10, 60)  # (connect_timeout, read_timeout)
MAX_RETRIES = 10
RETRY_WAIT_MIN = 1  # 最小等待秒数
RETRY_WAIT_MAX = 32  # 最大等待秒数
RETRYABLE_STATUS_CODES = {429, 500, 502, 503, 504}
REQUEST_DELAY = 0.5  # 每次成功请求后的延迟秒数，避免触发限流

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
    wait_time = min(RETRY_WAIT_MAX, max(RETRY_WAIT_MIN, 2 ** (retry_state.attempt_number - 1)))
    return wait_time


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
    wait=custom_wait_strategy,
    retry=retry_if_exception_type((RequestException, RetryableHTTPError)),
    before_sleep=lambda retry_state: logger.warning(
        f"请求失败，URL: {retry_state.args[0]}，{retry_state.outcome.exception()}，"
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
        # 只在限流/错误状态下才检查 Retry-After header
        retry_after_header = response.headers.get('Retry-After')
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
        raise RetryableHTTPError(response.status_code, response.reason, retry_after=retry_after)

    # 成功请求后休眠固定时间，避免触发 Graph API 限流
    time.sleep(REQUEST_DELAY)

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
        """
        Args:
            refresh_margin: 提前刷新时间（秒），默认 5 分钟
        """
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
        self._expires_at = time.time() + 3600  # Microsoft Graph token 有效期 3600 秒
        logger.info("Access token 已刷新")


def list_users():
    """从 userList.json 文件读取用户列表"""
    with open(USER_LIST_FILE, 'r', encoding='utf-8') as f:
        data = json.load(f)
        return data


def get_user_drive(user_id, token_manager):
    """获取用户的 OneDrive 信息

    Args:
        user_id: 用户ID
        token_manager: TokenManager 实例，用于获取有效的认证令牌

    Returns:
        dict: Drive信息，如果获取失败返回None
    """
    url = f"https://graph.microsoft.com/v1.0/users/{user_id}/drive"
    try:
        headers = token_manager.get_headers()
        response = request_with_retry(url, headers)
    except (RetryableHTTPError, RequestException) as e:
        logger.error(f"获取用户Drive失败: {e}")
        return None

    # 处理非重试类状态码（如 401, 403, 404 等，不在 RETRYABLE_STATUS_CODES 中）
    if response.status_code != 200:
        logger.warning(f"获取用户Drive返回非200: {response.status_code}")
        return None

    return response.json()


# ============ 核心业务函数 ============
def send_to_sqs(drive_id, workcode, name, item_id, parent_id, extension, parent_path, file_hash=None):
    """拼装消息并发送到SQS队列

    Args:
        drive_id: Drive ID
        workcode: 用户工号
        name: 文件名
        item_id: 文件ID
        parent_id: 父目录ID
        extension: 文件扩展名
        parent_path: 父目录路径
        file_hash: 文件哈希值 (quickXorHash)，可选

    Returns:
        bool: 发送成功返回 True，失败返回 False
    """
    # 清理 parent_path，去掉 "/drive/root:" 前缀
    clean_parent_path = parent_path.replace("/drive/root:", "") if parent_path else ""

    source = f"onedrive:{clean_parent_path}/{name}"
    destination = f"s3:{TARGET_S3_BUCKET}/{DESTINATION_PREFIX}/{workcode}/{parent_id}/{item_id}{extension}"
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


def process_files(drive_id, user_id, workcode, token_manager, ignore_spec, cutoff_date=None, path="root", current_path=""):
    """递归列出用户 OneDrive 中的文件和文件夹

    Args:
        drive_id: Drive ID
        user_id: 用户ID
        workcode: 用户工号
        token_manager: TokenManager 实例，用于获取有效的认证令牌
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)
        path: 当前API路径，默认为root
        current_path: 当前相对路径，用于过滤规则匹配

    Returns:
        tuple: (processed_count, skipped_count)
    """
    file_count = 0
    skipped_count = 0

    try:
        url = f"https://graph.microsoft.com/v1.0/users/{user_id}/drive/{path}/children"

        # 处理分页逻辑
        while url:
            try:
                # 每次请求前获取最新的认证头，确保 token 有效
                headers = token_manager.get_headers()
                response = request_with_retry(url, headers)
            except (RetryableHTTPError, RequestException) as e:
                logger.error(f"请求失败，处理中断，已处理 {file_count} 个文件: {e}, "
                           f"user_id={user_id}, workcode={workcode}, path={path}, current_path={current_path}")
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
                    subfolder_processed, subfolder_skipped = process_files(
                        drive_id, user_id, workcode, token_manager, ignore_spec,
                        cutoff_date, f"items/{item_id}", item_relative_path
                    )
                    file_count += subfolder_processed 
                    skipped_count += subfolder_skipped

                else:
                    # .ignore filtering (existing)
                    if ignore_spec.match_file(item_relative_path):
                        logger.debug(f"跳过文件（已过滤）: {item_relative_path}")
                        continue

                    # Date-based filtering
                    should_include, reason = should_include_item(item, cutoff_date, item_relative_path)

                    if not should_include:
                        logger.debug(f"跳过文件（创建时间早于cutoff）: {item_relative_path}, {reason}")
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
                    logger.debug(f"目标路径: /{DESTINATION_PREFIX}/{workcode}/{parent_id}/{item_id}{extension}")

                    # 只有发送成功才计数
                    if send_to_sqs(drive_id, workcode, name, item_id, parent_id, extension, parent_path, file_hash):
                        file_count += 1

            url = response_data.get('@odata.nextLink')
            if url:
                logger.debug(f"检测到分页，继续获取下一页数据: {url}")

    except Exception as e:
        logger.error(f"处理文件时发生未预期错误，处理中断，已处理 {file_count} 个文件: {e}", exc_info=True)

    return file_count, skipped_count


def process_user(user, token_manager, ignore_spec, cutoff_date=None):
    """处理单个用户的 OneDrive 文件列表

    Args:
        user: 用户信息字典（包含 email 和 workcode 字段）
        token_manager: TokenManager 实例，用于获取有效的认证令牌
        ignore_spec: pathspec.PathSpec 过滤规则对象
        cutoff_date: UTC datetime object for filtering (None = no filter)
    """
    email = user['email']
    workcode = user['workcode']

    user_log_file = f'{LOGS_DIR}/{workcode}.log'
    user_file_handler = logging.FileHandler(user_log_file, encoding='utf-8')
    user_file_handler.setLevel(logging.DEBUG)
    user_file_handler.setFormatter(formatter)

    logger.addHandler(user_file_handler)

    try:
        user_id = email

        drive_info = get_user_drive(user_id, token_manager)

        if drive_info is None:
            logger.error(f"✗ {workcode} ({email}): 无法获取 Drive")
            return
        drive_id = drive_info['id']

        log_user_header(workcode, email, drive_id)

        total_files, total_skipped = process_files(
            drive_id, user_id, workcode, token_manager, ignore_spec, cutoff_date
        )

        logger.info(f"用户 {workcode} ({email}) 处理完成:")
        logger.info(f"  - 已处理文件数: {total_files}")
        if cutoff_date:
            logger.info(f"  - 跳过文件数: {total_skipped} (创建时间早于 {cutoff_date.strftime('%Y-%m-%d')})")
        logger.info("=" * 60)

    finally:
        logger.removeHandler(user_file_handler)
        user_file_handler.close()
        upload_single_user_log(user_log_file)


# ============ 日志上传 ============
def upload_single_user_log(log_file):
    """上传单个用户的日志文件到S3

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
def log_user_header(display_name, upn, drive_id):
    """记录用户信息的标题"""
    logger.info("=" * 60)
    logger.info(f"用户: {display_name} ({upn})")
    logger.info(f"Drive ID: {drive_id}")
    logger.info("=" * 60)


def main():
    """主函数"""
    # Parse command line arguments
    parser = argparse.ArgumentParser(
        description='迁移所有用户的 OneDrive 文件，支持增量同步',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 迁移所有文件
  python sendcmd2sqs-onedrive.py

  # 只迁移2024年1月1日之后创建的文件
  python sendcmd2sqs-onedrive.py --created-after 2024-01-01

  # 只迁移指定时间之后创建的文件
  python sendcmd2sqs-onedrive.py --created-after 2024-01-15T10:30:00
        """
    )
    parser.add_argument(
        '--created-after',
        type=str,
        default=None,
        metavar='DATE',
        help='只迁移在指定日期之后创建的文件 (ISO 8601格式: YYYY-MM-DD 或 YYYY-MM-DDTHH:MM:SS)'
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

    ignore_spec = load_ignore_patterns()
    token_manager = TokenManager()
    users = list_users()

    for user in users:
        # 传递 token_manager 和 cutoff_date
        process_user(user, token_manager, ignore_spec, cutoff_date)

    logger.info(f"所有用户处理完成，日志已上传到: s3://{TARGET_S3_BUCKET}/{LOG_S3_PREFIX}/{TIMESTAMP}/")
    return 0


if __name__ == '__main__':
    import sys
    sys.exit(main())
