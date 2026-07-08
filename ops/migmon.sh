#!/usr/bin/env bash
# migmon.sh — 迁移集群一体化监控运维 TUI(自包含,纯 aws cli,客户环境直接可跑)。
#
# 融合 sqs-monitor(全集群 SQS/ASG 总览)+ rclone-mon(选集群下钻 SSM 群发)+ 滚动更新:
#   总览表格即主界面 → 方向键选集群行 → 快捷键对该集群下钻/操作。
#
# 交互(默认,需 TTY):
#   ↑/↓ 或 k/j   选集群行
#   p            该集群每台 rclone 进程数(SSM 群发,自动分批 ≤50/批)
#   t            该集群每台在传哪些文件(rclone 完整命令行)
#   R            滚动更新该集群(start-instance-refresh,二次确认护栏)⚠
#   r            重新采集刷新总览
#   q / Esc      退出
#
# 非交互 / 脚本化(无 TTY 或带子命令时):
#   migmon.sh                      # TTY→进 TUI;非 TTY→打印一次总览
#   migmon.sh sqs                  # 只打印总览表格(等价旧 sqs-monitor 单次)
#   migmon.sh sqs 10               # 每 10s 刷新总览(watch 模式)
#   migmon.sh ps   <集群子串>       # 直接查该集群 rclone 进程数
#   migmon.sh top  <集群子串>       # 直接查该集群传输详情
#   migmon.sh refresh <集群子串>    # 滚动更新该集群(仍二次确认)
#   FORMAT=csv migmon.sh sqs       # CSV 时序输出(cron 用,见文件末尾)
#   FORMAT=csv CSV_HEADER=1 migmon.sh sqs
#
# 环境变量:
#   AWS_REGION   默认 eu-south-2
#   PREFIX       队列/集群名前缀,默认 gcs-2-s3-worker
#   PROJECT_TAG  ASG 过滤 tag:Project,默认 gcs-s3-migration
#   SSM_BATCH    SSM 每批实例数,默认 50(硬上限)
#   TIMEOUT      单批 SSM 轮询秒数上限,默认 60
#   PARALLEL     总览采集并发度,默认 8(0/1=串行)
#   REFRESH_MIN_HEALTHY  instance-refresh 最小健康百分比,默认 50
#   REFRESH_WARMUP       新实例预热秒数,默认 180
#
# 权限:sqs:ListQueues/GetQueueAttributes, cloudwatch:GetMetricStatistics,
#       autoscaling:Describe*/StartInstanceRefresh, ec2:DescribeInstances,
#       ssm:SendCommand/GetCommandInvocation/ListCommandInvocations。
set -uo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:${PATH:-}

REGION="${AWS_REGION:-eu-south-2}"
PREFIX="${PREFIX:-gcs-2-s3-worker}"
PROJECT_TAG="${PROJECT_TAG:-gcs-s3-migration}"
SSM_BATCH="${SSM_BATCH:-50}"
TIMEOUT="${TIMEOUT:-60}"
PARALLEL="${PARALLEL:-8}"
FORMAT="${FORMAT:-table}"
REFRESH_MIN_HEALTHY="${REFRESH_MIN_HEALTHY:-50}"
REFRESH_WARMUP="${REFRESH_WARMUP:-180}"

# 颜色(仅 TTY)。
if [ -t 1 ]; then
  C_T=$'\033[1;36m'; C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'
  C_DIM=$'\033[2m'; C_INV=$'\033[7m'; C_0=$'\033[0m'
else C_T=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_INV=""; C_0=""; fi

TMPDIR_MIG="$(mktemp -d "${TMPDIR:-/tmp}/migmon.XXXXXX")"
cleanup() { printf '\e[?25h' 2>/dev/null; rm -rf "$TMPDIR_MIG" 2>/dev/null; }
trap cleanup EXIT INT TERM

# ── 跨平台 UTC 时间戳(GNU date -d @ / BSD date -r) ──────────────────────────
_iso() {  # <epoch>
  date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ
}

# ── CloudWatch helpers ───────────────────────────────────────────────────────
cw_sum() {  # <metric> <queue-name>  →最近1min Sum
  local m="$1" qn="$2" now start
  now=$(date -u +%s); start=$((now - 300))
  aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace AWS/SQS --metric-name "$m" \
    --dimensions "Name=QueueName,Value=$qn" \
    --start-time "$(_iso "$start")" --end-time "$(_iso "$now")" \
    --period 60 --statistics Sum \
    --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null
}

asg_gbps() {  # <NetworkIn|NetworkOut> <asg>  →Gbps
  local m="$1" asg="$2" now start v
  { [ -z "$asg" ] || [ "$asg" = "None" ]; } && { echo "-"; return; }
  now=$(date -u +%s); start=$((now - 300))
  v=$(aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace AWS/EC2 --metric-name "$m" \
    --dimensions "Name=AutoScalingGroupName,Value=$asg" \
    --start-time "$(_iso "$start")" --end-time "$(_iso "$now")" \
    --period 60 --statistics Sum \
    --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null)
  awk -v v="$v" 'BEGIN{if(v==""||v=="None")print"-";else printf"%.2f", v/60*8/1e9}'
}

_cw_avg() {  # <ns> <metric> <dim-name> <dim-value>
  local ns="$1" m="$2" dn="$3" dv="$4" now start
  now=$(date -u +%s); start=$((now - 300))
  aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace "$ns" --metric-name "$m" \
    --dimensions "Name=$dn,Value=$dv" \
    --start-time "$(_iso "$start")" --end-time "$(_iso "$now")" \
    --period 60 --statistics Average \
    --query "sort_by(Datapoints,&Timestamp)[-1].Average" --output text 2>/dev/null
}

asg_pct() {  # <ns> <metric> <asg>  →整数%,取不到"-"
  local ns="$1" m="$2" asg="$3" v ids id sum=0 n=0 iv
  { [ -z "$asg" ] || [ "$asg" = "None" ]; } && { echo "-"; return; }
  v=$(_cw_avg "$ns" "$m" AutoScalingGroupName "$asg")
  if [ -n "$v" ] && [ "$v" != "None" ]; then awk -v v="$v" 'BEGIN{printf"%.0f",v}'; return; fi
  ids=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" --output text 2>/dev/null)
  for id in $ids; do
    iv=$(_cw_avg "$ns" "$m" InstanceId "$id")
    if [ -n "$iv" ] && [ "$iv" != "None" ]; then
      sum=$(awk -v s="$sum" -v x="$iv" 'BEGIN{print s+x}'); n=$((n+1))
    fi
  done
  [ "$n" -gt 0 ] && awk -v s="$sum" -v n="$n" 'BEGIN{printf"%.0f",s/n}' || echo "-"
}

# ── ASG 缓存(按 tag:Project 一次拉全,本地匹配,零额外 API) ──────────────────
_ASG_CACHE=""
load_asg_cache() {
  _ASG_CACHE=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --filters "Name=tag:Project,Values=$PROJECT_TAG" \
    --query "AutoScalingGroups[].AutoScalingGroupName" --output text 2>/dev/null | tr '\t' '\n')
}
asg_for_cluster() { echo "$_ASG_CACHE" | grep -F "$1" | head -1; }  # <cluster-id>

# ── 千分位 ───────────────────────────────────────────────────────────────────
_commafy() {
  case "$1" in
    ''|'-'|'?'|*[!0-9]*) printf '%s' "$1" ;;
    *) awk -v n="$1" 'BEGIN{s=n;o="";while(length(s)>3){o=","substr(s,length(s)-2)o;s=substr(s,1,length(s)-3)}printf"%s%s",s,o}' ;;
  esac
}

# ── 采集单个队列的一行指标(供并行调用,写 tab 分隔到 stdout) ─────────────────
# 输出: cid \t pend \t inflt \t dlq \t recv \t done \t din \t dout \t cpu \t asg
collect_one() {  # <qurl> <all-urls-file>
  local qurl="$1" urls_file="$2"
  local qn cid pend inflt dlq dlqurl recv done asg din dout cpu cand
  qn=$(basename "$qurl"); cid="${qn%-queue}"
  read -r pend inflt <<< "$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$qurl" \
    --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
    --query "[Attributes.ApproximateNumberOfMessages,Attributes.ApproximateNumberOfMessagesNotVisible]" --output text 2>/dev/null)"
  dlq="-"
  for cand in "${qurl}-dlq" "${qurl%-queue}-queue-dlq"; do
    dlqurl=$(grep -F "$(basename "$cand")" "$urls_file" | head -1)
    if [ -n "$dlqurl" ]; then
      dlq=$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$dlqurl" \
        --attribute-names ApproximateNumberOfMessages --query "Attributes.ApproximateNumberOfMessages" --output text 2>/dev/null)
      break
    fi
  done
  recv=$(cw_sum NumberOfMessagesReceived "$qn"); { [ "$recv" = "None" ] || [ -z "$recv" ]; } && recv=0
  done=$(cw_sum NumberOfMessagesDeleted  "$qn"); { [ "$done" = "None" ] || [ -z "$done" ]; } && done=0
  recv=$(awk -v v="$recv" 'BEGIN{printf"%.0f",v/60}')
  done=$(awk -v v="$done" 'BEGIN{printf"%.0f",v/60}')
  asg=$(asg_for_cluster "$cid")
  din=$(asg_gbps NetworkIn  "$asg")
  dout=$(asg_gbps NetworkOut "$asg")
  cpu=$(asg_pct AWS/EC2 CPUUtilization "$asg")
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$cid" "${pend:-?}" "${inflt:-?}" "${dlq:-0}" "$recv" "$done" "$din" "$dout" "$cpu" "${asg:-None}"
}

# ── 采集所有集群(并行),结果按集群名排序写 stdout ──────────────────────────
collect_rows() {
  local urls_file="$TMPDIR_MIG/urls" main_qs qurl idx=0 running=0
  aws sqs list-queues --region "$REGION" --queue-name-prefix "$PREFIX" \
    --query "QueueUrls" --output text 2>/dev/null | tr '\t' '\n' > "$urls_file"
  main_qs=$(grep -v -- '-dlq$' "$urls_file" | grep -v -- '-queue-dlq$')
  [ -z "$main_qs" ] && return 0
  load_asg_cache

  # 并行:每队列一个后台作业写独立文件,控制并发度,最后按序拼接。
  local par="$PARALLEL"; [ "$par" -lt 1 ] 2>/dev/null && par=1
  while read -r qurl; do
    [ -z "$qurl" ] && continue
    idx=$((idx + 1))
    collect_one "$qurl" "$urls_file" > "$TMPDIR_MIG/row.$idx" &
    running=$((running + 1))
    if [ "$running" -ge "$par" ]; then wait -n 2>/dev/null || wait; running=$((running - 1)); fi
  done <<< "$main_qs"
  wait
  # 汇总所有 row.* 并按集群名(第1列)排序,保证顺序稳定。
  cat "$TMPDIR_MIG"/row.* 2>/dev/null | sort -t$'\t' -k1,1
}

# ── 表格列格式 ───────────────────────────────────────────────────────────────
_FMT="│ %1s%-19s %11s %9s %6s %7s %7s %6s %6s %4s │\n"   # 第1列留 1 字符游标位
_BORDER_W=86
_hline() { local mid; mid=$(printf '─%.0s' $(seq 1 $_BORDER_W)); printf '%s%s%s' "$1" "$mid" "$2"; }
_HR="$(_hline '┌' '┐')"; _MR="$(_hline '├' '┤')"; _BR="$(_hline '└' '┘')"

# 渲染表头(不含数据行)。
_print_header() {  # <title-extra>
  # 标题填充宽 = 边框内宽(_BORDER_W) - 两侧空格(2),与数据行右框对齐。
  local tw=$((_BORDER_W - 2))
  echo "${C_T}${_HR}${C_0}"
  printf "${C_T}│ %-${tw}s │${C_0}\n" " migmon  prefix=$PREFIX  region=$REGION  $(date '+%H:%M:%S')  $1"
  echo "${C_T}${_MR}${C_0}"
  printf "${C_T}${_FMT}${C_0}" "" "CLUSTER" "PENDING" "INFLIGHT" "DLQ" "RECV/s" "QPS" "DOWN" "UP" "CPU"
  printf "${C_DIM}${_FMT}${C_0}" "" "(short)" "msgs" "msgs" "msgs" "msg/s" "msg/s" "Gbps" "Gbps" "%"
  echo "${C_T}${_MR}${C_0}"
}

# 渲染一行数据。<cursor-char> <cid> <pend> <inflt> <dlq> <recv> <done> <din> <dout> <cpu>
_print_row() {
  local cur="$1" cid="$2" pend="$3" inflt="$4" dlq="$5" recv="$6" done="$7" din="$8" dout="$9" cpu="${10}"
  local short="${cid#"$PREFIX"-}"
  # 截断超长短名到 19 字符,防撑破右边框(%-19s 不截断,只补齐;用纯 ASCII '~' 省宽度歧义)。
  [ "${#short}" -gt 19 ] && short="${short:0:18}~"
  local dc="$C_0"; { [ "${dlq:-0}" != "-" ] && [ "${dlq:-0}" -gt 0 ]; } 2>/dev/null && dc="$C_ERR"
  local pc="$C_0"; [ "${pend:-0}" -gt 0 ] 2>/dev/null && pc="$C_WARN"
  local cc="$C_OK"; { [ "${cpu:-0}" != "-" ] && [ "${cpu%.*}" -ge 90 ]; } 2>/dev/null && cc="$C_ERR"
  local hl="$C_T"; [ -n "$cur" ] && hl="$C_INV"   # 选中行整行反色
  printf "│ ${hl}%1s%-19s${C_0} ${pc}%11s${C_0} ${C_T}%9s${C_0} ${dc}%6s${C_0} %7s ${C_OK}%7s${C_0} ${C_OK}%6s${C_0} ${C_OK}%6s${C_0} ${cc}%4s${C_0} │\n" \
    "$cur" "$short" "$(_commafy "${pend:-?}")" "$(_commafy "${inflt:-?}")" "$(_commafy "${dlq:-0}")" \
    "$(_commafy "$recv")" "$(_commafy "$done")" "$din" "$dout" "$cpu"
}

# 非交互总览(单次打印,含 TOTAL)。
snapshot() {
  _print_header ""
  local rows; rows=$(collect_rows)
  [ -z "$rows" ] && { printf "│ ${C_ERR}%-$((_BORDER_W-2))s${C_0} │\n" " 没找到前缀为 $PREFIX 的队列"; echo "${C_T}${_BR}${C_0}"; return; }
  local tp=0 ti=0 td=0 tr=0 tq=0
  while IFS=$'\t' read -r cid pend inflt dlq recv done din dout cpu asg; do
    [ -z "$cid" ] && continue
    _print_row "" "$cid" "$pend" "$inflt" "$dlq" "$recv" "$done" "$din" "$dout" "$cpu"
    tp=$((tp+${pend:-0})); ti=$((ti+${inflt:-0}))
    [ "${dlq:-0}" != "-" ] && td=$((td+${dlq:-0}))
    tr=$((tr+recv)); tq=$((tq+done))
  done <<< "$rows"
  echo "${C_T}${_MR}${C_0}"
  printf "${C_T}${_FMT}${C_0}" "" "TOTAL" "$(_commafy "$tp")" "$(_commafy "$ti")" "$(_commafy "$td")" "$(_commafy "$tr")" "$(_commafy "$tq")" "-" "-" "-"
  echo "${C_T}${_BR}${C_0}"
  echo "${C_DIM} PENDING=堆积 INFLIGHT=传输中 DLQ=死信 RECV/s=接收 QPS=完成/秒 DOWN/UP=网卡Gbps${C_0}"
}

# CSV 时序(cron)。
csv_dump() {
  local ts; ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  [ "${CSV_HEADER:-0}" = "1" ] && echo "timestamp,cluster,pending,inflight,dlq,recv_per_s,qps_done,down_gbps,up_gbps,cpu_pct"
  local rows; rows=$(collect_rows)
  [ -z "$rows" ] && { echo "$ts,NO_QUEUE_MATCH($PREFIX),,,,,,,," >&2; return 1; }
  while IFS=$'\t' read -r cid pend inflt dlq recv done din dout cpu asg; do
    [ -z "$cid" ] && continue
    echo "$ts,$cid,$pend,$inflt,$dlq,$recv,$done,$din,$dout,$cpu"
  done <<< "$rows"
}

# ══════════════════════ SSM 下钻(进程数 / 传输详情) ══════════════════════════
asg_instances() {  # <asg> →InService 实例 id 每行一个
  aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$1" \
    --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$'
}

read_into() {  # read_into ARR < <(cmd)  (bash 3.2 兼容 mapfile)
  local __n="$1" __l; eval "$__n=()"
  while IFS= read -r __l; do [ -n "$__l" ] && eval "$__n+=(\"\$__l\")"; done
}

# 对一批(≤50)实例发 SSM,回收输出。detail=1 打完整命令行,否则只数进程。
run_batch() {  # <detail> <id...>
  local detail="$1"; shift
  local ids=("$@") remote cmd_id waited=0 iid out n params
  [ "${#ids[@]}" -eq 0 ] && return 0
  if [ "$detail" -eq 1 ]; then
    remote='pgrep -af "[r]clone (copyto|copy)" || echo "(no rclone running)"'
  else
    remote='printf "rclone_procs=%s\n" "$(pgrep -c "[r]clone (copyto|copy)" || echo 0)"'
  fi
  params="$TMPDIR_MIG/params.$$"
  python3 - "$remote" >"$params" <<'PY'
import json,sys; print(json.dumps({"commands":[sys.argv[1]]}))
PY
  cmd_id=$(aws ssm send-command --region "$REGION" --document-name "AWS-RunShellScript" \
    --instance-ids "${ids[@]}" --parameters "file://$params" \
    --query 'Command.CommandId' --output text 2>/dev/null)
  rm -f "$params"
  { [ -z "$cmd_id" ] || [ "$cmd_id" = "None" ]; } && { echo "  ${C_ERR}[批发送失败]${C_0} ${ids[*]}"; return 1; }
  while [ "$waited" -lt "$TIMEOUT" ]; do
    local pending
    pending=$(aws ssm list-command-invocations --region "$REGION" --command-id "$cmd_id" \
      --query "length(CommandInvocations[?Status=='Pending'||Status=='InProgress'||Status=='Delayed'])" --output text 2>/dev/null)
    [ "${pending:-0}" = "0" ] && break
    sleep 3; waited=$((waited+3))
  done
  for iid in "${ids[@]}"; do
    out=$(aws ssm get-command-invocation --region "$REGION" --command-id "$cmd_id" --instance-id "$iid" \
      --query 'StandardOutputContent' --output text 2>/dev/null)
    if [ "$detail" -eq 1 ]; then
      echo "${C_DIM}─── $iid ───${C_0}"; printf '%s\n' "${out:-(无输出/超时)}"
    else
      n="${out#*rclone_procs=}"; n="${n%%$'\n'*}"; [[ "$n" =~ ^[0-9]+$ ]] || n="?"
      printf '    %-21s rclone=%s\n' "$iid" "$n"
    fi
  done
}

# 对某集群(ASG)群发,自动按 SSM_BATCH 分批。detail 控制模式。
drilldown() {  # <asg> <short> <detail>
  local asg="$1" short="$2" detail="$3" ids total=0 bno=0
  read_into IDS < <(asg_instances "$asg")
  echo
  echo "${C_T}===== 集群 $short  (实例 ${#IDS[@]} 台, region=$REGION) =====${C_0}"
  [ "${#IDS[@]}" -eq 0 ] && { echo "  (无 InService 实例)"; return; }
  local start batch
  for ((start=0; start<${#IDS[@]}; start+=SSM_BATCH)); do
    bno=$((bno+1)); batch=("${IDS[@]:start:SSM_BATCH}")
    echo "  ${C_DIM}--- 批 $bno: ${#batch[@]} 台 ---${C_0}"
    if [ "$detail" -eq 1 ]; then
      run_batch 1 "${batch[@]}"
    else
      while IFS= read -r line; do
        echo "$line"
        n="${line##*rclone=}"; [[ "$n" =~ ^[0-9]+$ ]] && total=$((total+n))
      done < <(run_batch 0 "${batch[@]}")
    fi
  done
  [ "$detail" -eq 0 ] && echo "  ${C_T}>>> 合计 rclone 进程: $total (across ${#IDS[@]} 台, $bno 批)${C_0}"
}

# ══════════════════════ 滚动更新(instance-refresh)+ 护栏 ═════════════════════
do_refresh() {  # <asg> <short>
  local asg="$1" short="$2" ninst gitbranch ans rid
  { [ -z "$asg" ] || [ "$asg" = "None" ]; } && { echo "${C_ERR}未找到集群 $short 的 ASG${C_0}"; return 1; }
  ninst=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query "length(AutoScalingGroups[0].Instances)" --output text 2>/dev/null)
  echo
  echo "${C_WARN}⚠  即将滚动更新(instance-refresh):${C_0}"
  echo "     集群 : $short"
  echo "     ASG  : $asg"
  echo "     实例 : ${ninst:-?} 台将逐步替换(MinHealthy=${REFRESH_MIN_HEALTHY}%, Warmup=${REFRESH_WARMUP}s)"
  echo "     后果 : 每台重启并 git clone 拉最新代码,传输中的对象会中断重投(SQS 重投,不丢)。"
  echo
  # 护栏:必须原样键入集群短名确认(比 y/N 更防误触)。
  read -rp "确认请键入集群短名【${short}】(其他任意键取消): " ans
  if [ "$ans" != "$short" ]; then echo "已取消。"; return 0; fi
  rid=$(aws autoscaling start-instance-refresh --region "$REGION" \
    --auto-scaling-group-name "$asg" \
    --preferences "{\"MinHealthyPercentage\":${REFRESH_MIN_HEALTHY},\"InstanceWarmup\":${REFRESH_WARMUP}}" \
    --query 'InstanceRefreshId' --output text 2>/dev/null)
  if [ -n "$rid" ] && [ "$rid" != "None" ]; then
    echo "${C_OK}✓ 已发起滚动更新  InstanceRefreshId=$rid${C_0}"
    echo "  查进度: aws autoscaling describe-instance-refreshes --region $REGION --auto-scaling-group-name $asg --query 'InstanceRefreshes[0].[Status,PercentageComplete]' --output text"
  else
    echo "${C_ERR}✗ 发起失败(权限?已有进行中的 refresh?)${C_0}"
  fi
}

# ══════════════════════ TUI 主循环 ═══════════════════════════════════════════
# 把总览数据读进数组,方向键选行,快捷键下钻/操作。
ROWS_RAW=()
load_rows() { read_into ROWS_RAW < <(collect_rows); }

# 由子串在 ROWS_RAW / ASG 缓存里定位并执行动作(供非交互子命令与 TUI 复用)。
resolve_asg_by_filter() {  # <substr> →打印 "asg\tshort",失败返回1
  load_asg_cache
  local hit; hit=$(echo "$_ASG_CACHE" | grep -F "$1" | head -1)
  [ -z "$hit" ] && return 1
  local short="${hit#"$PREFIX"-}"; short="${short%-AgentASG-*}"
  printf '%s\t%s\n' "$hit" "$short"
}

tui() {
  local cur=0 key key2 n
  printf '\e[?25l'   # 隐藏光标
  load_rows
  n=${#ROWS_RAW[@]}
  [ "$n" -eq 0 ] && { printf '\e[?25h'; echo "没找到前缀为 $PREFIX 的队列。"; return 1; }

  _draw() {
    clear
    _print_header "${C_DIM}[↑↓选 · p进程 · t详情 · R滚动更新 · r刷新 · q退出]${C_0}"
    local i cid pend inflt dlq recv done din dout cpu asg mark
    for ((i=0; i<n; i++)); do
      IFS=$'\t' read -r cid pend inflt dlq recv done din dout cpu asg <<< "${ROWS_RAW[$i]}"
      mark=" "; [ "$i" -eq "$cur" ] && mark="▶"
      _print_row "$mark" "$cid" "$pend" "$inflt" "$dlq" "$recv" "$done" "$din" "$dout" "$cpu"
    done
    echo "${C_T}${_BR}${C_0}"
    echo "${C_DIM} ↑/↓ 或 k/j 选集群 · p 进程数 · t 传输详情 · R 滚动更新⚠ · r 刷新 · q 退出${C_0}"
  }

  # 取当前选中行的 asg/short/cid。
  _sel() {  # 设全局 SEL_CID SEL_ASG SEL_SHORT
    IFS=$'\t' read -r SEL_CID _ _ _ _ _ _ _ _ SEL_ASG <<< "${ROWS_RAW[$cur]}"
    SEL_SHORT="${SEL_CID#"$PREFIX"-}"
  }

  _pause() { printf '\n%s' "${C_DIM}— 按任意键返回总览 —${C_0}"; read -rsn1 </dev/tty; printf '\e[?25l'; }

  _draw
  while true; do
    IFS= read -rsn1 key </dev/tty || break
    case "$key" in
      $'\e') read -rsn2 -t 0.001 key2 </dev/tty
             case "$key2" in
               '[A') ((cur>0)) && cur=$((cur-1)) ;;
               '[B') ((cur<n-1)) && cur=$((cur+1)) ;;
               '') break ;;                       # 单 Esc 退出
             esac; _draw ;;
      k|K) ((cur>0)) && cur=$((cur-1)); _draw ;;
      j|J) ((cur<n-1)) && cur=$((cur+1)); _draw ;;
      p)   _sel; printf '\e[?25h'; clear; drilldown "$SEL_ASG" "$SEL_SHORT" 0; _pause; _draw ;;
      t)   _sel; printf '\e[?25h'; clear; drilldown "$SEL_ASG" "$SEL_SHORT" 1; _pause; _draw ;;
      R)   _sel; printf '\e[?25h'; clear; do_refresh "$SEL_ASG" "$SEL_SHORT"; _pause; _draw ;;
      r)   printf '\e[?25h'; clear; echo "重新采集…"; load_rows; n=${#ROWS_RAW[@]}; printf '\e[?25l'; _draw ;;
      q|Q) break ;;
    esac
  done
  printf '\e[?25h'
}

# ══════════════════════ 分发 ═════════════════════════════════════════════════
CMD="${1:-}"
case "$CMD" in
  sqs)
    ARG2="${2:-0}"
    if [ "$FORMAT" = "csv" ]; then csv_dump
    elif [ "$ARG2" -gt 0 ] 2>/dev/null; then
      while true; do clear; snapshot; echo; echo "${C_DIM}每 ${ARG2}s 刷新,Ctrl-C 退出${C_0}"; sleep "$ARG2"; done
    else snapshot; fi
    ;;
  ps|top|refresh)
    FILTER="${2:-}"
    [ -z "$FILTER" ] && { echo "用法: migmon.sh $CMD <集群子串>"; exit 2; }
    if ! RES=$(resolve_asg_by_filter "$FILTER"); then echo "没有匹配 '$FILTER' 的集群。"; exit 1; fi
    IFS=$'\t' read -r A_ASG A_SHORT <<< "$RES"
    case "$CMD" in
      ps)      drilldown "$A_ASG" "$A_SHORT" 0 ;;
      top)     drilldown "$A_ASG" "$A_SHORT" 1 ;;
      refresh) do_refresh "$A_ASG" "$A_SHORT" ;;
    esac
    ;;
  "")
    # 无参:TTY→TUI;非 TTY→打印一次总览(或 CSV)。
    if [ "$FORMAT" = "csv" ]; then csv_dump
    elif [ -t 0 ] && [ -t 1 ]; then tui
    else snapshot; fi
    ;;
  *) echo "未知命令: $CMD (可用: sqs|ps|top|refresh 或无参进 TUI)"; exit 2 ;;
esac

# ── cronjob(CSV 时序,每 5 分钟)────────────────────────────────────────────
#   sudo mkdir -p /var/log/migration
#   FORMAT=csv CSV_HEADER=1 AWS_REGION=eu-south-2 bash migmon.sh sqs > /var/log/migration/sqs-metrics.csv
#   */5 * * * * FORMAT=csv AWS_REGION=eu-south-2 /path/migmon.sh sqs >> /var/log/migration/sqs-metrics.csv 2>> /var/log/migration/sqs-metrics.err
