#!/usr/bin/env bash
# migops —— 迁移集群运维一体化脚本（在 ops EC2 上运行，无需看 dashboard 即可拿到全部信息）。
#
# 设计：一条命令一个动作，运维零学习成本。只输「栈名」，所有资源名自动推导：
#   队列 ${STACK}-queue (+ -queue-dlq) · 状态表 ${STACK}-transfer-status · 心跳表 ${STACK}-heartbeat
#   限速 SSM /migration/${STACK}/ratelimit/* · 运维日志组 /migration/${STACK}/worker-ops
#   ASG 从栈 Output AgentASGName 取（不靠拼接）。
#
# 依赖：aws cli v2、python3 + 仓库代码（/opt/migration）。区域默认 eu-south-2，可用 AWS_REGION 覆盖。
#
# 用法：bash ops/migops.sh <命令> <栈名> [参数...]
#   migops status      <栈>                # 总览：吞吐/队列/DLQ/实例/CPU内存/四态/429（= 整个 dashboard 一屏）
#   migops throughput  <栈>                # 实时吞吐 Gbps（NetworkIn=源下载 / NetworkOut=S3上传）
#   migops queue       <栈>                # 队列积压 / in-flight / DLQ 深度
#   migops states      <栈>                # 四态 attempts/min（SUCCESS/RETRYABLE/FATAL/UNKNOWN）+ 429 信号
#   migops workers     <栈>                # 活跃 worker 数 + ASG 实例列表
#   migops inspect     <栈> <source>       # 单个对象的传输尝试历史（查状态表）
#   migops ratelimit   <栈>                # 查当前限速（带宽 + 频率 + 开关）
#   migops set-gbps    <栈> <目标Gbps|off> # 调带宽限速目标
#   migops set-tps     <栈> <总TPS|off>    # 调频率限速总目标
#   migops errors      <栈> [分钟数=30]    # 最近失败的 rclone 原始 cmd+stderr（从 CloudWatch 日志）
#   migops dlq-replay  <栈> [最多条数=10000]# 把 DLQ 失败消息重投回主队列
#   migops watch       <栈> [秒=10]        # 每隔 N 秒刷新 status（实时盯盘）
set -uo pipefail

REGION="${AWS_REGION:-eu-south-2}"
CODE_DIR="${MIGRATION_CODE_DIR:-/opt/migration}"

# ── 颜色（非 TTY 自动关闭，便于重定向到文件）──
if [ -t 1 ]; then C_T=$'\033[1;36m'; C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_0=$'\033[0m'
else C_T=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_0=""; fi

die() { echo "${C_ERR}错误: $*${C_0}" >&2; exit 1; }
hdr() { echo "${C_T}━━ $* ━━${C_0}"; }

# ── 资源名推导 ──────────────────────────────────────────────────────────────
queue_name()  { echo "$1-queue"; }
dlq_name()    { echo "$1-queue-dlq"; }
status_tbl()  { echo "$1-transfer-status"; }
hb_tbl()      { echo "$1-heartbeat"; }
ratelimit()   { echo "/migration/$1/ratelimit/$2"; }
ops_log()     { echo "/migration/$1/worker-ops"; }

# ASG 名从栈 Output 取（缓存到变量，避免重复调用）
asg_name() {
  aws cloudformation describe-stacks --region "$REGION" --stack-name "$1" \
    --query "Stacks[0].Outputs[?OutputKey=='AgentASGName'].OutputValue" --output text 2>/dev/null
}

queue_url() {
  aws sqs get-queue-url --region "$REGION" --queue-name "$(queue_name "$1")" \
    --query QueueUrl --output text 2>/dev/null
}

# 取某指标最近一个数据点（Sum/Average/Maximum）
metric_last() {  # <namespace> <metric> <dim-name> <dim-value> <stat> [period=60]
  local ns="$1" m="$2" dn="$3" dv="$4" stat="$5" period="${6:-60}"
  local now start
  now=$(date -u +%s); start=$((now - 600))
  aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace "$ns" --metric-name "$m" \
    --dimensions "Name=$dn,Value=$dv" \
    --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
    --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
    --period "$period" --statistics "$stat" \
    --query "sort_by(Datapoints,&Timestamp)[-1].$stat" --output text 2>/dev/null
}

# 设 python 运行所需 env（供 migration_cli）
py_env() {  # <stack>
  export PYTHONPATH="$CODE_DIR/src"
  export AWS_REGION="$REGION"
  export QUEUE_URL="$(queue_url "$1")"
  export DYNAMODB_TABLE="$(status_tbl "$1")"
  export HEARTBEAT_TABLE="$(hb_tbl "$1")"
}

# ── 命令实现 ────────────────────────────────────────────────────────────────

cmd_throughput() {  # <stack>
  local asg; asg=$(asg_name "$1"); [ -z "$asg" ] && die "找不到栈 $1 的 ASG（栈是否存在？）"
  local ni no nig nog
  ni=$(metric_last AWS/EC2 NetworkIn  AutoScalingGroupName "$asg" Sum)
  no=$(metric_last AWS/EC2 NetworkOut AutoScalingGroupName "$asg" Sum)
  # NetworkIn/Out 是 60s 周期累计字节 → Gbps = Sum/60*8/1e9（与 dashboard 公式一致）
  nig=$(awk -v v="$ni" 'BEGIN{if(v==""||v=="None")print"  -";else printf"%.2f",v/60*8/1e9}')
  nog=$(awk -v v="$no" 'BEGIN{if(v==""||v=="None")print"  -";else printf"%.2f",v/60*8/1e9}')
  printf "  下载(NetworkIn)  %s${C_DIM}Gbps  ← 从源(GCS/S3)拉取${C_0}\n" "$nig"
  printf "  上传(NetworkOut) %s${C_DIM}Gbps  → 写入目标 S3${C_0}\n" "$nog"
}

cmd_queue() {  # <stack>
  local qurl; qurl=$(queue_url "$1"); [ -z "$qurl" ] && die "找不到栈 $1 的队列"
  local attrs pending inflight
  attrs=$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$qurl" \
    --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
    --query Attributes --output json)
  pending=$(echo "$attrs" | python3 -c "import sys,json;print(json.load(sys.stdin)['ApproximateNumberOfMessages'])")
  inflight=$(echo "$attrs" | python3 -c "import sys,json;print(json.load(sys.stdin)['ApproximateNumberOfMessagesNotVisible'])")
  local dlqurl dlq=0
  dlqurl=$(aws sqs get-queue-url --region "$REGION" --queue-name "$(dlq_name "$1")" --query QueueUrl --output text 2>/dev/null)
  [ -n "$dlqurl" ] && dlq=$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$dlqurl" \
    --attribute-names ApproximateNumberOfMessages --query Attributes.ApproximateNumberOfMessages --output text 2>/dev/null)
  printf "  待处理(积压)   %s\n" "$pending"
  printf "  in-flight     %s ${C_DIM}（正在处理；上限 115k/队列）${C_0}\n" "$inflight"
  if [ "${dlq:-0}" -gt 0 ] 2>/dev/null; then
    printf "  ${C_ERR}DLQ 死信      %s（有失败消息，可 migops dlq-replay 重投）${C_0}\n" "$dlq"
  else
    printf "  DLQ 死信      %s\n" "${dlq:-0}"
  fi
}

cmd_states() {  # <stack>
  hdr "四态 attempts/min（最近窗口，按 State）"
  local now start
  now=$(date -u +%s); start=$((now - 300))
  # EMF AttemptCount 按 State 维度。SEARCH 不便用 cli，逐态查（FATAL 没数据=0，正好补上 dashboard 缺的第4态）
  for st in SUCCESS RETRYABLE FATAL UNKNOWN; do
    local v
    v=$(aws cloudwatch get-metric-statistics --region "$REGION" \
      --namespace GcsS3Migration --metric-name AttemptCount \
      --dimensions "Name=State,Value=$st" \
      --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
      --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
      --period 60 --statistics Sum \
      --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null)
    [ "$v" = "None" ] || [ -z "$v" ] && v=0
    local color="$C_0"
    [ "$st" = "RETRYABLE" ] && [ "${v%.*}" -gt 0 ] 2>/dev/null && color="$C_WARN"
    [ "$st" = "FATAL" ] && [ "${v%.*}" -gt 0 ] 2>/dev/null && color="$C_ERR"
    printf "  ${color}%-10s %s${C_0}\n" "$st" "$v"
  done
  echo "  ${C_DIM}（FATAL 长期为 0 是健康信号——无不可重试的致命失败）${C_0}"
  hdr "源端限流信号：RETRYABLE by ErrorClass（src_rate_limit = 源端 429）"
  local r
  r=$(aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace GcsS3Migration --metric-name AttemptCount \
    --dimensions Name=State,Value=RETRYABLE Name=ErrorClass,Value=src_rate_limit \
    --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
    --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
    --period 60 --statistics Sum \
    --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null)
  [ "$r" = "None" ] || [ -z "$r" ] && r=0
  if [ "${r%.*}" -gt 0 ] 2>/dev/null; then
    printf "  ${C_WARN}429/min  %s  ← 源端在限流，建议调低限速（migops set-gbps）${C_0}\n" "$r"
  else
    printf "  429/min  %s ${C_DIM}（无源端限流）${C_0}\n" "$r"
  fi
}

cmd_workers() {  # <stack>
  local asg; asg=$(asg_name "$1")
  py_env "$1"
  local active; active=$(python3 -m migration.migration_cli active-workers 2>/dev/null | grep -oE '[0-9]+' | head -1)
  printf "  活跃 worker（心跳 TTL 未过期）: ${C_OK}%s${C_0}\n" "${active:-?}"
  if [ -n "$asg" ]; then
    local inst des
    inst=$(metric_last AWS/AutoScaling GroupInServiceInstances AutoScalingGroupName "$asg" Average)
    des=$(metric_last AWS/AutoScaling GroupDesiredCapacity AutoScalingGroupName "$asg" Average)
    printf "  ASG 实例: InService=%s / Desired=%s\n" "${inst%.*}" "${des%.*}"
    hdr "实例列表"
    aws autoscaling describe-auto-scaling-groups --region "$REGION" --auto-scaling-group-names "$asg" \
      --query "AutoScalingGroups[0].Instances[].[InstanceId,LifecycleState,HealthStatus]" --output text 2>/dev/null \
      | sed 's/^/    /'
  fi
}

cmd_cpumem() {  # <stack>
  local asg; asg=$(asg_name "$1"); [ -z "$asg" ] && return
  local cpu mem
  cpu=$(metric_last AWS/EC2 CPUUtilization AutoScalingGroupName "$asg" Average)
  # 内存：CWAgent mem_used_percent 优先按 ASG 聚合维度查；该维度缺数据时（新实例/
  # 聚合维度未生成）回退到逐个 InService 实例的 InstanceId 维度取平均。
  mem=$(metric_last CWAgent mem_used_percent AutoScalingGroupName "$asg" Average)
  if [ -z "$mem" ] || [ "$mem" = "None" ]; then
    local ids sum=0 n=0
    ids=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" --auto-scaling-group-names "$asg" \
      --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" --output text 2>/dev/null)
    for id in $ids; do
      local v; v=$(metric_last CWAgent mem_used_percent InstanceId "$id" Average)
      if [ -n "$v" ] && [ "$v" != "None" ]; then sum=$(awk -v s="$sum" -v v="$v" 'BEGIN{print s+v}'); n=$((n+1)); fi
    done
    [ "$n" -gt 0 ] && mem=$(awk -v s="$sum" -v n="$n" 'BEGIN{printf"%.2f",s/n}') || mem="None"
  fi
  printf "  CPU %s%%   内存 %s%% ${C_DIM}（ASG 平均；内存需 CWAgent 已上报）${C_0}\n" \
    "$(awk -v v="$cpu" 'BEGIN{if(v==""||v=="None")print"-";else printf"%.0f",v}')" \
    "$(awk -v v="$mem" 'BEGIN{if(v==""||v=="None")print"待采集";else printf"%.0f",v}')"
}

cmd_ratelimit() {  # <stack>
  for p in limit-enabled auto-enabled target-gbps bwlimit tpslimit-target tpslimit; do
    local v; v=$(aws ssm get-parameter --region "$REGION" --name "$(ratelimit "$1" "$p")" --query Parameter.Value --output text 2>/dev/null)
    printf "  %-16s %s\n" "$p" "${v:-（未设）}"
  done
  echo "  ${C_DIM}target-gbps/tpslimit-target 由你设；bwlimit/tpslimit 由限速 Lambda 每分钟自动算${C_0}"
}

cmd_set_gbps() {  # <stack> <gbps|off>
  local val="$2"
  if [ "$val" = "off" ]; then
    aws ssm put-parameter --region "$REGION" --overwrite --name "$(ratelimit "$1" limit-enabled)" --value false --type String >/dev/null
    echo "${C_OK}已关闭带宽限速（limit-enabled=false）${C_0}"
  else
    aws ssm put-parameter --region "$REGION" --overwrite --name "$(ratelimit "$1" target-gbps)" --value "$val" --type String >/dev/null
    aws ssm put-parameter --region "$REGION" --overwrite --name "$(ratelimit "$1" limit-enabled)" --value true --type String >/dev/null
    aws ssm put-parameter --region "$REGION" --overwrite --name "$(ratelimit "$1" auto-enabled)" --value true --type String >/dev/null
    echo "${C_OK}带宽限速目标 = ${val} Gbps（AIMD 自动模式，下一分钟生效）${C_0}"
  fi
}

cmd_set_tps() {  # <stack> <tps|off>
  aws ssm put-parameter --region "$REGION" --overwrite --name "$(ratelimit "$1" tpslimit-target)" --value "$2" --type String >/dev/null
  echo "${C_OK}频率限速总目标 = $2（控制器下一分钟自动换算每进程值）${C_0}"
}

cmd_inspect() {  # <stack> <source>
  py_env "$1"
  python3 -m migration.migration_cli inspect "$2"
}

cmd_errors() {  # <stack> [minutes=30]
  local mins="${2:-30}" lg start
  lg=$(ops_log "$1")
  start=$(( ( $(date -u +%s) - mins*60 ) * 1000 ))
  hdr "最近 ${mins} 分钟失败的 rclone 命令 + stderr（CloudWatch ${lg}）"
  aws logs filter-log-events --region "$REGION" --log-group-name "$lg" \
    --start-time "$start" --filter-pattern '"rclone 失败"' \
    --query "events[].message" --output text 2>/dev/null | sed 's/^/  /' | head -100
  echo "  ${C_DIM}（无输出 = 该时间窗无失败记录）${C_0}"
}

cmd_dlq_replay() {  # <stack> [max=10000]
  py_env "$1"
  export QUEUE_URL="$(aws sqs get-queue-url --region "$REGION" --queue-name "$(dlq_name "$1")" --query QueueUrl --output text 2>/dev/null)"
  # migration_cli replay 用 QUEUE_URL 解析 DLQ→main，这里直接调它（它内部从主队列 RedrivePolicy 反查，
  # 故需把 QUEUE_URL 设回主队列；replay 自己 resolve DLQ）。
  export QUEUE_URL="$(queue_url "$1")"
  python3 -m migration.migration_cli replay --max "${2:-10000}"
}

cmd_status() {  # <stack> —— 总览（= 整个 dashboard 一屏）
  echo "${C_T}╔══ 集群 $1 实时总览  $(date '+%Y-%m-%d %H:%M:%S')  region=$REGION ══╗${C_0}"
  hdr "吞吐";       cmd_throughput "$1"
  hdr "CPU / 内存"; cmd_cpumem "$1"
  hdr "队列";       cmd_queue "$1"
  hdr "Worker";     cmd_workers "$1"
  cmd_states "$1"
  hdr "限速";       cmd_ratelimit "$1"
}

cmd_watch() {  # <stack> [interval=10]
  local iv="${2:-10}"
  while true; do clear; cmd_status "$1"; echo; echo "${C_DIM}每 ${iv}s 刷新，Ctrl-C 退出${C_0}"; sleep "$iv"; done
}

# ── 入口 ────────────────────────────────────────────────────────────────────
usage() {
  # 只打印文件头部的注释块（到第一个非注释行为止），自动随注释增减。
  awk 'NR==1{next} /^#/{sub(/^# ?/,"");print;next} {exit}' "$0"
  exit "${1:-0}"
}

[ $# -lt 1 ] && usage 1
CMD="$1"; shift
case "$CMD" in
  -h|--help|help) usage 0 ;;
esac
[ $# -lt 1 ] && die "缺少栈名。用法见: bash $0 help"
STACK="$1"; shift

case "$CMD" in
  status)      cmd_status     "$STACK" ;;
  throughput)  hdr "吞吐"; cmd_throughput "$STACK" ;;
  queue)       hdr "队列"; cmd_queue "$STACK" ;;
  states)      cmd_states     "$STACK" ;;
  workers)     hdr "Worker"; cmd_workers "$STACK" ;;
  cpumem)      hdr "CPU/内存"; cmd_cpumem "$STACK" ;;
  inspect)     [ $# -lt 1 ] && die "用法: migops inspect <栈> <source>"; cmd_inspect "$STACK" "$1" ;;
  ratelimit)   hdr "限速"; cmd_ratelimit "$STACK" ;;
  set-gbps)    [ $# -lt 1 ] && die "用法: migops set-gbps <栈> <Gbps|off>"; cmd_set_gbps "$STACK" "$1" ;;
  set-tps)     [ $# -lt 1 ] && die "用法: migops set-tps <栈> <总TPS|off>"; cmd_set_tps "$STACK" "$1" ;;
  errors)      cmd_errors     "$STACK" "${1:-30}" ;;
  dlq-replay)  cmd_dlq_replay "$STACK" "${1:-10000}" ;;
  watch)       cmd_watch      "$STACK" "${1:-10}" ;;
  *)           die "未知命令: $CMD（用法见: bash $0 help）" ;;
esac
