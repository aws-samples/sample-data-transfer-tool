#!/usr/bin/env bash
# ddb-inspect —— 按源文件地址查 DDB 里该对象的全部传输尝试历史(自包含)。
#
# DDB transfer-status 表主键 = source_hash(md5(source)%256#source)+ attempt_timestamp。
# 直接 query(走主键,毫秒级,不 scan)。PK 的 md5 分片前缀由本脚本算出(与
# status_store.make_pk 完全一致:int(md5_hex,16)%256 == md5 末两位十六进制转十进制)。
#
# 用法:
#   ops/ddb-inspect.sh "s3:bucket/path/to/object"
#   TABLE=gcs-2-s3-worker-m6in-pool1-transfer-status AWS_PROFILE=xxx AWS_REGION=eu-south-2 \
#     ops/ddb-inspect.sh "gcs:eu-abc-dw/libs/.../000508_0"
#   ops/ddb-inspect.sh "s3:bucket/x" raw     # 第二参 raw=输出原始 JSON(默认表格摘要)
#   ops/ddb-inspect.sh "s3:bucket/x" full    # full=不截断 error_message / rclone_command
set -uo pipefail

SRC="${1:-}"
FORMAT="${2:-table}"   # table=人读摘要(截断);full=不截断;raw=完整 JSON
REGION="${AWS_REGION:-eu-south-2}"
TABLE="${TABLE:-transfer-message-status-${REGION}}"
PROFILE_ARG=""
[ -n "${AWS_PROFILE:-}" ] && PROFILE_ARG="--profile ${AWS_PROFILE}"

if [ -z "$SRC" ]; then
  echo "用法: $0 \"<source>\" [table|full|raw]" >&2
  echo "  例: $0 \"gcs:eu-abc-dw/libs/hive/warehouse/dw.db/.../000508_0\"" >&2
  echo "  环境变量: TABLE(默认 transfer-message-status-\$AWS_REGION) / AWS_PROFILE / AWS_REGION" >&2
  exit 2
fi

command -v python3 >/dev/null 2>&1 || { echo "需要 python3(算分片 PK + 格式化)" >&2; exit 3; }

# ── 算 source_hash 主键(与 status_store.make_pk 同源)──
PK="$(python3 -c 'import sys,hashlib; s=sys.argv[1]; print(f"{int(hashlib.md5(s.encode()).hexdigest(),16)%256}#{s}")' "$SRC")"
SHARD="${PK%%#*}"
echo "源对象 : $SRC"
echo "分片PK : $PK   (shard=$SHARD)"
echo "表     : $TABLE   region=$REGION"
echo "────────────────────────────────────────────────────────────────────"

# ── query 主键(SK 升序 = 尝试时间顺序),结果落临时文件(避免 stdin 与 heredoc 冲突)──
RESP_FILE="$(mktemp -t ddb-inspect.XXXXXX)"
trap 'rm -f "$RESP_FILE"' EXIT

# shellcheck disable=SC2086
aws dynamodb query $PROFILE_ARG --region "$REGION" \
  --table-name "$TABLE" \
  --key-condition-expression "source_hash = :pk" \
  --expression-attribute-values "{\":pk\":{\"S\":\"$PK\"}}" \
  --scan-index-forward \
  --output json > "$RESP_FILE" 2> "${RESP_FILE}.err"

if [ -s "${RESP_FILE}.err" ]; then
  ERR="$(cat "${RESP_FILE}.err")"; rm -f "${RESP_FILE}.err"
  if echo "$ERR" | grep -q "ResourceNotFoundException"; then
    echo "❌ 表不存在: $TABLE(检查 TABLE/AWS_REGION,或栈是否已删)" >&2
  elif echo "$ERR" | grep -qE "AccessDenied|UnrecognizedClient|ExpiredToken|InvalidClientTokenId"; then
    echo "❌ 权限/凭证问题: $(echo "$ERR" | tail -1)" >&2
  else
    echo "❌ 查询失败: $(echo "$ERR" | tail -1)" >&2
  fi
  exit 1
fi
rm -f "${RESP_FILE}.err"

if [ "$FORMAT" = "raw" ]; then
  cat "$RESP_FILE"
  exit 0
fi

# ── 格式化:Python 从文件参数读(stdin 不被占用),按 attempt 一行展开 ──
TRUNC=1; [ "$FORMAT" = "full" ] && TRUNC=0
python3 - "$RESP_FILE" "$TRUNC" <<'PY'
import sys, json

with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
trunc = sys.argv[2] == "1"

def g(item, k, typ="S"):
    v = item.get(k)
    return v.get(typ) if v else "-"

items = data.get("Items", [])
if not items:
    print("⚠ 没查到该 source 的任何记录。可能原因:")
    print("  - 该对象从未被传输(没进过队列/没被 worker 处理)")
    print("  - source 字符串与消息里的不完全一致(大小写/前缀/末尾斜杠都敏感)")
    print("  - 查错了表/region")
    sys.exit(0)

# 统计各状态次数
from collections import Counter
states = Counter(g(it, "state") for it in items)
print(f"共 {len(items)} 次尝试  |  " + "  ".join(f"{k}={v}" for k, v in states.items()))
print()

def clip(s, n):
    if s is None:
        return None
    s = s.replace("\n", " \\n ")
    return s if not trunc else (s[:n] + (" …(truncated, 用 full 看全)" if len(s) > n else ""))

for i, it in enumerate(items, 1):
    print(f"#{i}  [{g(it,'attempt_timestamp')}]  state={g(it,'state')}  error_class={g(it,'error_class')}")
    print(f"     instance={g(it,'instance_id')}  bytes={g(it,'transferred_bytes','N')}  elapsed={g(it,'elapsed_seconds','N')}s")
    em = it.get("error_message", {}).get("S")
    if em:
        print(f"     error_message: {clip(em, 400)}")
    cmd = it.get("rclone_command", {}).get("S")
    if cmd:
        print(f"     rclone_command: {clip(cmd, 500)}")
    print()
PY
