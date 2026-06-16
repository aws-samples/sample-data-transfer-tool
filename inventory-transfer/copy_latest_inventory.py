#!/usr/bin/env python3
"""拷贝每个 GCS 桶的最新一次 inventory 报告（manifest + parquet）到 S3

按白名单文件指定的桶列表（每行一个桶名，对应 inventory 根路径下的同名子目录），
找出每个桶最新一次报告，把它的 manifest 文件与全部 parquet 分片拷贝到 S3 指定目录。

工作机制（纯靠 rclone，不读 parquet 内容、不引入云 SDK）：
  1. 读 --bucket-file 白名单拿到桶列表（# 注释与空行忽略；只处理列出的桶，不自动发现）
  2. 对每个桶：rclone lsf --files-only --format sp 列出文件（带大小），
     按 manifest 文件名里的 DATETIME 选最新一次报告
  3. rclone cat 下载该 manifest，从 report_shards_file_names / reportNames 拿权威分片清单
  4. 拷贝集合 = manifest 文件名 + 全部分片 basename（有序去重）
  5. 已传输预检：拷贝集合每个文件都已在目标且大小一致 → 跳过该桶（避免重传）；
     有缺失/不一致 → 继续拷贝（避免漏传；目标文件被删也会自动补传）
  6. rclone copy --files-from 临时清单，精确拷贝这一次报告的全部文件到目标，拷后再校验

"最新"判定：同一桶可能有多个 report config（多 UUID）、每个 config 多次运行。
必须解析每个 manifest 文件名里的 DATETIME（ISO 定宽 YYYY-MM-DDTHH:MM），跨所有 UUID 取最大。
不能按文件名整串排序（会先按 UUID 排而选错）。

认证:
  - GCS / S3 均通过 rclone remote 完成。需在 rclone.conf 中预先配置好源（GCS）与目标（S3）remote。
    生产默认 remote 名：源 gcs / 目标 s3；测试可用 --src-remote gcptest --dst-remote testaws 覆盖。

cron 部署:
  - 设计为可安全地高频重复运行（如每 10 分钟）：内置 flock 非阻塞锁防重叠
    （上一轮未结束时本轮直接退出 0）；已传输的报告自动跳过（不重传），
    目标缺文件自动补传（不漏传）。判断以目标 S3 实际状态为准，无本地状态文件。

用法:
  python3 copy_latest_inventory.py --dry-run          # 默认 remote gcs/s3 + 脚本目录 bucket-list.txt，先预演
  python3 copy_latest_inventory.py                    # 实际执行
  python3 copy_latest_inventory.py --bucket-file /path/buckets.txt
  python3 copy_latest_inventory.py --only-bucket my-bkt --src-remote gcptest --dst-remote testaws
  python3 copy_latest_inventory.py --workers 4 --rclone-flag=--checksum

  # crontab 每 10 分钟:
  # */10 * * * * /usr/bin/python3 /path/copy_latest_inventory.py --workers 4 --log-file /path/inv.log >>/path/cron.log 2>&1
"""
import argparse
import concurrent.futures
import contextlib
import fcntl
import json
import logging
import os
import re
import shlex
import shutil
import subprocess
import tempfile
from datetime import datetime


# ============ 常量配置 ============
RCLONE_BIN = "rclone"

# rclone remote 名（生产约定：源 gcs / 目标 s3；可经 CLI 覆盖）
DEFAULT_SRC_REMOTE = "gcs"
DEFAULT_DST_REMOTE = "s3"

# 路径根（不含 remote: 前缀、不含末尾斜杠）
DEFAULT_SRC_ROOT = (
    "my-gcs-inventory/parquet"  # inventory 报告根：其下每个子目录是一个桶
)
DEFAULT_DST_ROOT = "my-s3-inventory/ops/inventory"  # 目标根：桶名+前缀

# rclone 退出码语义（见 rclone 官方文档）
RCLONE_EXIT_DIR_NOT_FOUND = 3  # directory not found（视为空/不存在，不算致命）

# 防 cron 重叠运行的 flock 锁文件（可经 --lock-file 覆盖）
DEFAULT_LOCK_FILE = os.path.join(tempfile.gettempdir(), "copy-latest-inventory.lock")

# 桶白名单文件（可经 --bucket-file 覆盖）。锚定脚本所在目录而非 CWD——
# cron 的 CWD 是 $HOME，相对 CWD 的默认值在 cron 下必然指错。
DEFAULT_BUCKET_FILE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "bucket-list.txt"
)

# manifest 文件名格式：{UUID}_{DATETIME}_manifest.json
#   UUID 为 8-4-4-4-12 十六进制；DATETIME 为 ISO 定宽 'YYYY-MM-DDTHH:MM'
MANIFEST_RE = re.compile(
    r"^(?P<uuid>[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12})_"
    r"(?P<dt>\d{4}-\d{2}-\d{2}T\d{2}:\d{2})_manifest\.json$"
)
MANIFEST_DT_FORMAT = "%Y-%m-%dT%H:%M"

# manifest JSON 里分片清单的两种字段变体（顺序即优先级）
SHARD_LIST_FIELDS = ("report_shards_file_names", "reportNames")


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


# ============ 异常类型 ============
class RcloneError(Exception):
    """rclone 命令以非预期退出码失败。"""


class ManifestError(Exception):
    """manifest 无法解析出有效的分片清单（两种字段都拿不到非空列表）。"""


# ============ rclone 封装 ============
def run_rclone(args, *, capture=True):
    """执行一条 rclone 命令。

    Args:
        args: rclone 子命令及参数列表（不含 'rclone' 本身）。列表传参（非 shell），
              文件名里的冒号/特殊字符天然安全，无注入风险。
        capture: 是否捕获 stdout/stderr。

    Returns:
        subprocess.CompletedProcess
    """
    cmd = [RCLONE_BIN] + list(args)
    logger.debug("执行: %s", " ".join(shlex.quote(c) for c in cmd))
    return subprocess.run(cmd, capture_output=capture, text=True)


def rclone_lsf_with_sizes(remote_path):
    """列举 remote_path 下一层文件，返回 {文件名: 字节数}。

    用 --format sp 输出 'size;name'（size 在前、按第一个 ';' 切分，文件名含 ';' 也安全）。
    目录不存在（exit 3）视为空 {}（白名单桶在源端尚无目录、或目标桶目录在
    首次运行前尚不存在，都是正常情况）；其他非 0 抛 RcloneError。

    Args:
        remote_path: 形如 'gcs:.../{bucket}/' 或 's3:.../{bucket}/'

    Returns:
        dict[str, int]: 文件名 → 字节数
    """
    cp = run_rclone(["lsf", remote_path, "--files-only", "--format", "sp"])
    if cp.returncode == RCLONE_EXIT_DIR_NOT_FOUND:
        logger.warning("路径不存在或为空: %s", remote_path)
        return {}
    if cp.returncode != 0:
        raise RcloneError(
            f"lsf 失败（退出码 {cp.returncode}）: {remote_path}\n{(cp.stderr or '').strip()}"
        )
    sizes = {}
    for line in cp.stdout.splitlines():
        line = line.strip()
        if not line or ";" not in line:
            continue
        size_str, name = line.split(";", 1)
        try:
            sizes[name] = int(size_str)
        except ValueError:
            logger.warning("lsf 输出行 size 非法，忽略: %r", line)
    return sizes


def rclone_cat(remote_file):
    """rclone cat 取单个远端文件内容（用于下载 manifest JSON 文本）。

    Args:
        remote_file: 形如 'gcs:.../{bucket}/{UUID}_{DATETIME}_manifest.json'

    Returns:
        str: 文件内容（stdout）

    Raises:
        RcloneError: 非 0 退出码
    """
    cp = run_rclone(["cat", remote_file])
    if cp.returncode != 0:
        raise RcloneError(
            f"cat 失败（退出码 {cp.returncode}）: {remote_file}\n{(cp.stderr or '').strip()}"
        )
    return cp.stdout


def rclone_copy_files_from(
    src_dir, dst_dir, list_path, *, extra_flags=(), dry_run=False
):
    """按清单文件精确拷贝：rclone copy {src} {dst} --files-from {list}。

    清单每行是相对 src_dir 的路径（本场景目录平铺，即纯文件名）。

    Args:
        src_dir: 源目录，形如 'gcs:.../{bucket}/'
        dst_dir: 目标目录，形如 's3:.../{bucket}/'
        list_path: 本地清单文件路径
        extra_flags: 透传给 rclone 的额外参数
        dry_run: True 时加 --dry-run，不实际写目标

    Returns:
        int: rclone 退出码
    """
    args = ["copy", src_dir, dst_dir, "--files-from", list_path]
    args += list(extra_flags)
    if dry_run:
        args.append("--dry-run")
    cp = run_rclone(args)
    # rclone 的进度/INFO/NOTICE 走 stderr，逐行转发到我们的 logger 便于排查
    for line in (cp.stderr or "").splitlines():
        if line.strip():
            logger.info("  rclone> %s", line)
    return cp.returncode


# ============ inventory 报告解析 ============
def parse_manifest_filename(name):
    """从 manifest 文件名解析 (uuid, datetime_str)。不符合命名约定返回 None。"""
    m = MANIFEST_RE.match(name)
    if not m:
        return None
    return m.group("uuid"), m.group("dt")


def find_latest_report(file_names):
    """从某桶目录下的全部文件名中，选 DATETIME 最大的那次 inventory 报告的 manifest。

    同一桶可能有多个 report config（多 UUID），跨 UUID 一起按 DATETIME 比较取全局最新。
    DATETIME 并列（不同 UUID 同时刻）时按 uuid 字符串兜底排序，保证结果确定（不随机）。

    Args:
        file_names: 桶目录下的文件名列表

    Returns:
        tuple (manifest_name, uuid, dt_str) 或 None（没有任何合法 manifest）
    """
    manifests = []  # [(dt_obj, uuid, manifest_name)]
    for name in file_names:
        parsed = parse_manifest_filename(name)
        if parsed is None:
            continue
        uuid, dt_str = parsed
        try:
            dt_obj = datetime.strptime(dt_str, MANIFEST_DT_FORMAT)
        except ValueError:
            logger.warning("manifest 文件名 DATETIME 非法，跳过: %s", name)
            continue
        manifests.append((dt_obj, uuid, name))

    if not manifests:
        return None

    if len(manifests) > 1:
        logger.info(
            "该桶发现 %d 个 manifest，将按 DATETIME 选最新一次报告", len(manifests)
        )

    dt_obj, uuid, manifest_name = max(manifests, key=lambda x: (x[0], x[1]))
    return manifest_name, uuid, dt_obj.strftime(MANIFEST_DT_FORMAT)


def parse_shard_names(manifest_text):
    """从 manifest JSON 文本解析本次报告的全部分片文件名（basename 列表）。

    优先 report_shards_file_names（纯文件名数组），回退 reportNames（gs:// URI 数组）。
    两者各项都取 basename：对纯文件名是无害 no-op，对 gs:// URI 则剥掉路径前缀。

    Args:
        manifest_text: manifest 文件的 JSON 文本

    Returns:
        list[str]: 分片文件名（basename）

    Raises:
        ManifestError: JSON 非法，或两种字段都拿不到非空清单
    """
    try:
        manifest = json.loads(manifest_text)
    except (ValueError, TypeError) as e:
        raise ManifestError(f"manifest 不是合法 JSON: {e}") from e
    if not isinstance(manifest, dict):
        raise ManifestError("manifest 顶层不是 JSON 对象")

    for field in SHARD_LIST_FIELDS:
        raw = manifest.get(field)
        if not raw:
            continue
        if not isinstance(raw, list):
            logger.warning("manifest 字段 %s 不是数组，忽略", field)
            continue
        names = [str(item).rstrip("/").rsplit("/", 1)[-1] for item in raw if item]
        names = [n for n in names if n]  # 剔除空串
        if names:
            return names

    raise ManifestError(
        f"manifest 未含有效分片清单（缺字段 {' / '.join(SHARD_LIST_FIELDS)} 或均为空）"
    )


# ============ 单桶处理 ============
class BucketResult:
    """单个桶的处理结果。status 取值见下方常量。"""

    COPIED = "COPIED"  # 成功拷贝
    SKIPPED_UP_TO_DATE = "SKIPPED_UP_TO_DATE"  # 最新报告已全部在目标（cron 常态）
    SKIPPED_NO_FILES = "SKIPPED_NO_FILES"  # 桶目录为空
    SKIPPED_NO_MANIFEST = "SKIPPED_NO_MANIFEST"  # 有文件但无合法 manifest
    WARN_INCOMPLETE = "WARN_INCOMPLETE"  # 拷贝后目标仍缺报告成员（部分失败/源缺文件）
    FAILED = "FAILED"  # 失败（manifest/copy 错误等）

    def __init__(self, bucket, status, detail=""):
        self.bucket = bucket
        self.status = status
        self.detail = detail  # 汇总时随 WARN/FAILED 逐桶打印的说明


def missing_members(members, src_sizes, dst_sizes):
    """找出"尚未正确传输到目标"的报告成员。

    成员视为已传输的条件：目标存在同名文件，且与源文件大小一致
    （大小不一致视为未传完/损坏，需要重传）。源里就不存在的成员
    （manifest 声明但源缺失）也会被列入——让 copy 持续尝试并暴露 WARN，
    而非静默当作已完成。

    Args:
        members: 本次报告的全部文件名（manifest + 分片）
        src_sizes: 源目录 {文件名: 字节数}。允许是拷贝前的快照——成员文件
                   写入后内容不变（文件名含 UUID+DATETIME），快照不会过期。
        dst_sizes: 目标目录 {文件名: 字节数}

    Returns:
        list[str]: 缺失/不一致的成员文件名
    """
    return [
        n
        for n in members
        if n not in src_sizes or n not in dst_sizes or src_sizes[n] != dst_sizes[n]
    ]


def split_by_cause(pending, src_sizes):
    """把缺失成员按根因拆分计数：目标缺失/不一致（重跑即愈）vs 源端缺失（重跑不会愈，
    须查源侧/报告生成）。混报会把排障引向错误方向，预检与拷后校验的日志都按此区分。

    Returns:
        tuple: (dst_missing_count, src_absent_count)
    """
    src_absent = sum(1 for n in pending if n not in src_sizes)
    return len(pending) - src_absent, src_absent


def process_bucket(bucket, args):
    """处理单个桶：找最新报告 → 读 manifest 拿清单 → 已传输预检 → --files-from 精确拷贝。

    为 cron 高频运行设计：拷贝前先比对目标（文件名+大小），最新报告已全部
    在目标则直接跳过（不重传）；有缺失则只走一次 copy 补齐（不漏传，
    目标文件被人删除也会在下一轮自动补传）。判断以目标实际状态为准，无本地状态。

    捕获所有异常，绝不向外抛（单桶失败不中断其他桶），返回带状态的 BucketResult。
    """
    src_dir = f"{args.src_remote}:{args.src_root}/{bucket}/"
    dst_dir = f"{args.dst_remote}:{args.dst_root}/{bucket}/"
    try:
        # 1) 列源文件（带大小，预检要用）
        src_sizes = rclone_lsf_with_sizes(src_dir)
        if not src_sizes:
            return BucketResult(bucket, BucketResult.SKIPPED_NO_FILES, "桶目录为空")

        # 2) 选最新报告的 manifest
        latest = find_latest_report(src_sizes.keys())
        if latest is None:
            return BucketResult(
                bucket, BucketResult.SKIPPED_NO_MANIFEST, "未找到合法 manifest"
            )
        manifest_name, uuid, dt_str = latest
        logger.info(
            "[%s] 选中最新报告: %s（uuid=%s, datetime=%s）",
            bucket,
            manifest_name,
            uuid,
            dt_str,
        )

        # 3) 下载 manifest，解析权威分片清单
        manifest_text = rclone_cat(f"{src_dir}{manifest_name}")
        shard_names = parse_shard_names(
            manifest_text
        )  # 解析不出 → ManifestError → FAILED

        # 4) 拷贝集合 = manifest 本身 + 全部分片，有序去重（manifest 清单通常不含自身）
        members = list(dict.fromkeys([manifest_name] + shard_names))
        logger.info(
            "[%s] 本次报告含 %d 个文件（manifest + %d 分片）",
            bucket,
            len(members),
            len(shard_names),
        )

        # 5) 已传输预检：全部成员已在目标且大小一致 → 跳过，不起 rclone copy（cron 常态路径）
        dst_sizes = rclone_lsf_with_sizes(dst_dir)
        pending = missing_members(members, src_sizes, dst_sizes)
        if not pending:
            logger.info("[%s] 最新报告已全部在目标，跳过", bucket)
            return BucketResult(
                bucket,
                BucketResult.SKIPPED_UP_TO_DATE,
                f"已是最新（{dt_str}，{len(members)} 个文件）",
            )
        # 区分根因再打日志：目标缺失重跑即愈；源缺失（manifest 声明但源没有）重跑不会愈，
        # 须去查源侧/报告生成，混报会把排障引向错误方向。
        absent_src = [n for n in pending if n not in src_sizes]
        logger.info(
            "[%s] 待传 %d/%d 个文件（目标缺失/不一致 %d，源端缺失 %d），开始拷贝",
            bucket,
            len(pending),
            len(members),
            len(pending) - len(absent_src),
            len(absent_src),
        )

        # 6) 写临时清单 → rclone copy --files-from
        list_fd, list_path = tempfile.mkstemp(prefix=f"inv-{bucket}-", suffix=".list")
        try:
            with os.fdopen(list_fd, "w", encoding="utf-8") as f:
                f.write("\n".join(members) + "\n")
            # DEBUG 打印清单内容：临时文件跑完即删，打印出来 copy 命令才可独立复现/核对。
            # 用 isEnabledFor 守卫，避免非 DEBUG 级别下仍白白拼接整份清单字符串（分片多时不划算）。
            if logger.isEnabledFor(logging.DEBUG):
                logger.debug(
                    "[%s] --files-from 清单 %s 内容（%d 项）:\n%s",
                    bucket,
                    list_path,
                    len(members),
                    "\n".join(f"  {name}" for name in members),
                )
            rc = rclone_copy_files_from(
                src_dir,
                dst_dir,
                list_path,
                extra_flags=args.rclone_flag,
                dry_run=args.dry_run,
            )
        finally:
            with contextlib.suppress(OSError):
                os.remove(list_path)

        if rc != 0:
            return BucketResult(bucket, BucketResult.FAILED, f"rclone copy 退出码 {rc}")

        # dry-run 不实际写目标，跳过拷后校验
        if args.dry_run:
            return BucketResult(bucket, BucketResult.COPIED, "dry-run（未实际写入）")

        # 7) 拷后校验：本次报告每个成员都应已在目标且大小一致。
        # 用成员子集比对而非目标文件数全等——目标目录会累积历史报告，全等计数必误报。
        # src_sizes 沿用第 1 步快照：成员文件名含 UUID+DATETIME、写入后内容不变，
        # 无需为校验再列一次源；极端情况（拷贝期间源被改写）产生的假 WARN 下一轮自愈。
        dst_sizes = rclone_lsf_with_sizes(dst_dir)
        still_missing = missing_members(members, src_sizes, dst_sizes)
        if still_missing:
            absent_src = [n for n in still_missing if n not in src_sizes]
            detail = (
                f"拷贝后仍缺 {len(still_missing)}/{len(members)} 个文件"
                f"（目标缺失/不一致 {len(still_missing) - len(absent_src)}，"
                f"源端缺失 {len(absent_src)}）: "
                + ", ".join(still_missing[:5])
                + ("..." if len(still_missing) > 5 else "")
            )
            return BucketResult(bucket, BucketResult.WARN_INCOMPLETE, detail)
        return BucketResult(bucket, BucketResult.COPIED)

    except (RcloneError, ManifestError) as e:
        logger.error("[%s] 处理失败: %s", bucket, e)
        return BucketResult(bucket, BucketResult.FAILED, str(e))
    except Exception as e:  # noqa: BLE001 - 兜底：单桶任何意外都不应中断全局
        logger.error("[%s] 未预期错误: %s", bucket, e, exc_info=True)
        return BucketResult(bucket, BucketResult.FAILED, f"未预期错误: {e}")


# ============ 桶白名单 ============
def load_bucket_whitelist(path):
    """读取桶白名单文件，返回桶名列表（保留文件行序）。

    格式：每行一个桶名；# 注释行与空行忽略；首行剥 UTF-8 BOM；行内首尾空白 strip；
    重复桶名告警并去重（保留首次出现）。与 ../gcs-sqs-go 的桶映射文件行级约定一致。

    Args:
        path: 白名单文件路径

    Returns:
        list[str]: 桶名列表（可能为空——由调用方决定是否致命）

    Raises:
        OSError: 文件不存在/不可读
    """
    buckets = []
    seen = set()
    with open(path, "r", encoding="utf-8") as f:
        for line_no, raw in enumerate(f, 1):
            if line_no == 1:
                raw = raw.lstrip("\ufeff")  # 剥 UTF-8 BOM（Windows 记事本保存会加）
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line in seen:
                logger.warning("白名单第 %d 行桶名重复，忽略: %s", line_no, line)
                continue
            seen.add(line)
            buckets.append(line)
    return buckets


def list_buckets(args):
    """从白名单文件取桶列表；--only-bucket 在白名单基础上再过滤（测试单桶用）。"""
    buckets = load_bucket_whitelist(args.bucket_file)
    logger.info("白名单 %s: %d 个桶", args.bucket_file, len(buckets))
    if args.only_bucket:
        wanted = set(args.only_bucket)
        missing = wanted - set(buckets)
        if missing:
            logger.warning("以下指定桶不在白名单中: %s", ", ".join(sorted(missing)))
        buckets = [b for b in buckets if b in wanted]
    return buckets


def check_remotes_configured(*remotes):
    """校验 rclone remote 是否已配置（rclone.conf 或 RCLONE_CONFIG_* 环境变量）。

    Returns:
        list[str]: 未配置的 remote 名（空列表 = 全部已配置）

    Raises:
        RcloneError: listremotes 本身执行失败
    """
    cp = run_rclone(["listremotes"])
    if cp.returncode != 0:
        raise RcloneError(
            f"listremotes 失败（退出码 {cp.returncode}）:\n{(cp.stderr or '').strip()}"
        )
    configured = {line.rstrip(":") for line in cp.stdout.splitlines() if line.strip()}
    return [r for r in remotes if r not in configured]


# ============ 主程序 ============
def build_arg_parser():
    parser = argparse.ArgumentParser(
        description="拷贝每个被审计桶的最新一次 GCS Storage Insights inventory 报告到 S3",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 默认（生产 remote gcs/s3），先 dry-run 预演
  python3 copy_latest_inventory.py --dry-run

  # 实际执行
  python3 copy_latest_inventory.py

  # 用测试 remote 做小范围验证（只处理一个桶）
  python3 copy_latest_inventory.py \\
      --src-remote gcptest --dst-remote testaws \\
      --only-bucket my-bucket --dry-run -v

  # 自定义根路径与目标
  python3 copy_latest_inventory.py \\
      --src-root my-gcs-inventory/parquet \\
      --dst-root my-s3-inventory/ops/inventory

  # 4 个桶并发 + 用校验和判断跳过（跨云 mtime 不可靠）
  python3 copy_latest_inventory.py --workers 4 --rclone-flag=--checksum

  # crontab 每 10 分钟运行（内置 flock 防重叠、已传输自动跳过，可安全高频重跑）:
  # */10 * * * * /usr/bin/python3 /path/copy_latest_inventory.py --workers 4 --log-file /path/inv.log >>/path/cron.log 2>&1
        """,
    )
    parser.add_argument(
        "--src-remote",
        default=DEFAULT_SRC_REMOTE,
        help=f"GCS 源 rclone remote 名（默认 {DEFAULT_SRC_REMOTE}；测试用 gcptest）",
    )
    parser.add_argument(
        "--dst-remote",
        default=DEFAULT_DST_REMOTE,
        help=f"S3 目标 rclone remote 名（默认 {DEFAULT_DST_REMOTE}；测试用 testaws）",
    )
    parser.add_argument(
        "--src-root",
        default=DEFAULT_SRC_ROOT,
        metavar="PATH",
        help=f"inventory 报告根路径（不含 remote:，不含末尾斜杠；默认 {DEFAULT_SRC_ROOT}）",
    )
    parser.add_argument(
        "--dst-root",
        default=DEFAULT_DST_ROOT,
        metavar="PATH",
        help=f"目标根路径 桶名+前缀（不含 remote:，不含末尾斜杠；默认 {DEFAULT_DST_ROOT}）",
    )
    parser.add_argument(
        "--bucket-file",
        default=DEFAULT_BUCKET_FILE,
        metavar="PATH",
        help=f"桶白名单文件：每行一个桶名，# 注释与空行忽略（默认 {DEFAULT_BUCKET_FILE}）",
    )
    parser.add_argument(
        "--only-bucket",
        action="append",
        metavar="BUCKET",
        default=[],
        help="只处理白名单中的指定桶（可重复指定多次，测试用）；不给则处理白名单全部桶",
    )
    parser.add_argument(
        "--workers",
        type=int,
        default=1,
        metavar="N",
        help="并发处理的桶数量（默认 1=串行；每个 worker 一条 rclone 进程）",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="预演：rclone copy 加 --dry-run，不实际写 S3",
    )
    parser.add_argument(
        "--rclone-flag",
        action="append",
        default=[],
        metavar="FLAG",
        help="透传给 rclone copy 的额外参数（可重复），如 --rclone-flag=--checksum",
    )
    parser.add_argument(
        "--log-file",
        default=None,
        metavar="PATH",
        help="额外写入的日志文件路径（默认仅 console）",
    )
    parser.add_argument(
        "--lock-file",
        default=DEFAULT_LOCK_FILE,
        metavar="PATH",
        help=f"flock 锁文件路径，防 cron 重叠运行（默认 {DEFAULT_LOCK_FILE}）",
    )
    parser.add_argument(
        "-v",
        "--verbose",
        action="store_true",
        help="console 输出 DEBUG 级别（含每条 rclone 命令行）",
    )
    return parser


def main():
    args = build_arg_parser().parse_args()

    if args.verbose:
        console_handler.setLevel(logging.DEBUG)

    # 可选文件日志。须在 flock 之前配好：锁冲突的"本轮跳过"也要进主日志，
    # 否则排障时"某时间窗为何无运行记录"的答案只在 cron 重定向里，日志脑裂。
    # 多进程 append 同一日志文件是安全的（O_APPEND）。
    file_handler = None
    if args.log_file:
        file_handler = logging.FileHandler(args.log_file, encoding="utf-8")
        file_handler.setLevel(logging.DEBUG)
        file_handler.setFormatter(formatter)
        logger.addHandler(file_handler)

    try:
        # flock 非阻塞防重叠：cron 每 10 分钟触发、跨云拷贝可能超 10 分钟，重叠时本轮直接退出。
        # 锁句柄保持引用至进程结束，退出（含异常/被 kill）时内核自动释放，无残留锁问题。
        lock_fp = open(args.lock_file, "w")  # noqa: SIM115 - 锁文件须保持打开至进程退出
        try:
            fcntl.flock(lock_fp, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            logger.info("另一实例正在运行（锁 %s），本轮跳过", args.lock_file)
            return 0  # cron 场景下重叠不算错误；file_handler 由 finally 统一清理

        # 规整根路径：去掉末尾斜杠，避免拼接出 '//'
        args.src_root = args.src_root.strip("/")
        args.dst_root = args.dst_root.strip("/")

        # 预检：rclone 是否可用
        if shutil.which(RCLONE_BIN) is None:
            logger.error("未找到 rclone 可执行文件，请先安装并配置 rclone remote")
            return 1

        # 预检：源/目标 remote 是否已配置（白名单模式下没有"根路径列举"兜底暴露配置错，
        # 必须主动校验，否则错误 remote 名会让每个桶都报一遍 lsf 失败）
        try:
            missing_remotes = check_remotes_configured(args.src_remote, args.dst_remote)
        except RcloneError as e:
            logger.error("rclone listremotes 失败: %s", e)
            return 1
        if missing_remotes:
            logger.error(
                "rclone remote 未配置: %s（检查 rclone.conf 或 RCLONE_CONFIG_* 环境变量）",
                ", ".join(missing_remotes),
            )
            return 1

        logger.info("=== 拷贝最新 inventory 报告 ===")
        logger.info(
            "源: %s:%s/  →  目标: %s:%s/",
            args.src_remote,
            args.src_root,
            args.dst_remote,
            args.dst_root,
        )
        if args.dry_run:
            logger.info("*** DRY-RUN 模式：只解析+组装清单，不实际写 S3 ***")

        # 读桶白名单
        try:
            buckets = list_buckets(args)
        except OSError as e:
            logger.error("读取桶白名单失败 %s: %s", args.bucket_file, e)
            return 1

        if not buckets:
            logger.warning("桶白名单为空，无可处理的桶: %s", args.bucket_file)
            return 1

        logger.info("共 %d 个桶待处理", len(buckets))

        # 处理（按 --workers 并发；rclone 子进程阻塞时释放 GIL，线程足够）
        results = []
        if args.workers <= 1:
            for i, bucket in enumerate(buckets, 1):
                logger.info("[%d/%d] 处理桶: %s", i, len(buckets), bucket)
                results.append(process_bucket(bucket, args))
        else:
            with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as ex:
                future_map = {ex.submit(process_bucket, b, args): b for b in buckets}
                done = 0
                for future in concurrent.futures.as_completed(future_map):
                    done += 1
                    logger.info(
                        "[%d/%d] 完成桶: %s", done, len(buckets), future_map[future]
                    )
                    results.append(future.result())

        # 汇总
        by_status = {}
        for r in results:
            by_status.setdefault(r.status, []).append(r)

        logger.info("%s", "=" * 60)
        logger.info("全部处理完成:")
        logger.info("  - 总桶数: %d", len(results))
        logger.info(
            "  - 成功拷贝(COPIED): %d", len(by_status.get(BucketResult.COPIED, []))
        )
        logger.info(
            "  - 已最新跳过(SKIPPED_UP_TO_DATE): %d",
            len(by_status.get(BucketResult.SKIPPED_UP_TO_DATE, [])),
        )
        logger.info(
            "  - 跳过-空桶(SKIPPED_NO_FILES): %d",
            len(by_status.get(BucketResult.SKIPPED_NO_FILES, [])),
        )
        logger.info(
            "  - 跳过-无manifest(SKIPPED_NO_MANIFEST): %d",
            len(by_status.get(BucketResult.SKIPPED_NO_MANIFEST, [])),
        )
        warns = by_status.get(BucketResult.WARN_INCOMPLETE, [])
        fails = by_status.get(BucketResult.FAILED, [])
        if warns:
            logger.warning("  - 成员缺失(WARN_INCOMPLETE): %d", len(warns))
            for r in warns:
                logger.warning("      %s: %s", r.bucket, r.detail)
        if fails:
            logger.error("  - 失败(FAILED): %d", len(fails))
            for r in fails:
                logger.error("      %s: %s", r.bucket, r.detail)
        logger.info("%s", "=" * 60)

        return 2 if fails else 0

    finally:
        if file_handler is not None:
            logger.removeHandler(file_handler)
            file_handler.close()


if __name__ == "__main__":
    import sys

    sys.exit(main())
