#!/usr/bin/env bash
# sqs-monitor —— 迁移集群 SQS 基础监控（自包含，纯 aws cli，客户环境直接可跑）。
#
# 显示每条主队列：堆积(待处理) / in-flight(在处理) / DLQ 深度 / 接收QPS / 完成QPS(=成功传输/秒)。
# 自动发现匹配前缀的所有主队列（排除 -dlq），多套集群一屏对比。
#
# QPS 数据源：CloudWatch AWS/SQS（标准指标，不依赖 EMF/worker 代码）：
#   NumberOfMessagesReceived  = 接收速率（worker 拉取量）
#   NumberOfMessagesDeleted   = 完成速率（成功传输后删消息 = 成功对象/秒）
#   注意：CloudWatch SQS 指标按分钟聚合，有 ~1-2 分钟延迟（堆积/in-flight 是实时的）。
#
# 用法：
#   bash sqs-monitor.sh                     # 默认前缀 gcs-2-s3-worker，单次输出（彩色表格）
#   bash sqs-monitor.sh gcs-2-s3-worker     # 指定队列名前缀
#   bash sqs-monitor.sh gcs-2-s3-worker 10  # 每 10 秒刷新（watch 模式，Ctrl-C 退出）
#
#   # cron/日志模式：每行一个集群、带 UTC 时间戳的 CSV，append 进文件即时序数据
#   FORMAT=csv bash sqs-monitor.sh                      # 单次 CSV（无表头）
#   FORMAT=csv CSV_HEADER=1 bash sqs-monitor.sh         # 带表头（首次建文件用）
#   见文件末尾「cronjob 配置」注释块。
set -uo pipefail

# cron 的 PATH 极简（只有 /usr/bin:/bin），aws cli 常在 /usr/local/bin → 找不到。
# 显式补全 PATH，让本脚本在 cron 非交互环境下也能找到 aws/date/awk。
export PATH=/usr/local/bin:/usr/bin:/bin:${PATH:-}

REGION="${AWS_REGION:-eu-south-2}"
PREFIX="${1:-gcs-2-s3-worker}"
INTERVAL="${2:-0}"          # 0 = 单次；>0 = 每 N 秒刷新
FORMAT="${FORMAT:-table}"   # table=彩色表格(交互)；csv=每行一集群带时间戳(cron/日志)
# ASG 按 tag:Project 精确点查（4 个集群 stack 默认都用 gcs-s3-migration）。
# 账号里有 50+ 个 EKS NodeGroup ASG，靠这个 tag 过滤只捞本工具的 ASG、不分页全量。
PROJECT_TAG="${PROJECT_TAG:-gcs-s3-migration}"

if [ -t 1 ]; then C_T=$'\033[1;36m'; C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_0=$'\033[0m'
else C_T=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_0=""; fi

# CloudWatch SQS 某指标最近 1 分钟的 Sum（用于算每秒速率）
cw_sum() {  # <metric> <queue-name>
  local m="$1" qn="$2" now start
  now=$(date -u +%s); start=$((now - 300))
  aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace AWS/SQS --metric-name "$m" \
    --dimensions "Name=QueueName,Value=$qn" \
    --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
    --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
    --period 60 --statistics Sum \
    --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null
}

# ASG 名缓存：{cluster-id}<TAB>{asg-physical-name} 每行一条（collect_rows 填充一次）。
# 根因修复：原 asg_for_cluster 每集群都调一次 describe-auto-scaling-groups，账号里
# 50+ 个 EKS NodeGroup ASG 时该调用分页返回全量，高负载期偶发限流 → query 取 [0]
# 得 None → DOWN/UP/CPU 三列集体变 '-'。改为按集群 tag 精确点查本工具的 ASG，
# 不拉全量，且整轮只调一次。
_ASG_NAMES_CACHE=""

# 只查本工具的 ASG（按 tag:Project=$PROJECT_TAG 过滤），不返回账号里的 EKS 等 ASG。
# CFN 模板给每个 ASG 打了 Project tag（ProjectTag 参数，默认 gcs-s3-migration）。
load_asg_cache() {
  _ASG_NAMES_CACHE=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --filters "Name=tag:Project,Values=$PROJECT_TAG" \
    --query "AutoScalingGroups[].AutoScalingGroupName" --output text 2>/dev/null | tr '\t' '\n')
}

# 由队列名推集群标识（去掉 -queue 后缀），在缓存里本地匹配 ASG 物理名（零额外 API）。
# 匹配规则：ASG 名包含集群标识即认定属于该集群（兼容 <id>-AgentASG-xxx 这类命名）。
asg_for_cluster() {  # <cluster-id>
  local cid="$1"
  echo "$_ASG_NAMES_CACHE" | grep -F "$cid" | head -1
}

# ASG 的 EC2 NetworkIn/Out 最近 1 分钟 Sum → Gbps（period-accumulated bytes ÷ 60 × 8 ÷ 1e9）。
asg_gbps() {  # <metric NetworkIn|NetworkOut> <asg-name>
  local m="$1" asg="$2" now start v
  [ -z "$asg" ] || [ "$asg" = "None" ] && { echo "-"; return; }
  now=$(date -u +%s); start=$((now - 300))
  v=$(aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace AWS/EC2 --metric-name "$m" \
    --dimensions "Name=AutoScalingGroupName,Value=$asg" \
    --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
    --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
    --period 60 --statistics Sum \
    --query "sort_by(Datapoints,&Timestamp)[-1].Sum" --output text 2>/dev/null)
  awk -v v="$v" 'BEGIN{if(v==""||v=="None")print"-";else printf"%.2f", v/60*8/1e9}'
}

# 单维度查 Average %（内部 helper）。维度名/值由调用方给。
_cw_avg() {  # <namespace> <metric> <dim-name> <dim-value>
  local ns="$1" m="$2" dn="$3" dv="$4" now start
  now=$(date -u +%s); start=$((now - 300))
  aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace "$ns" --metric-name "$m" \
    --dimensions "Name=$dn,Value=$dv" \
    --start-time "$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$start" +%Y-%m-%dT%H:%M:%SZ)" \
    --end-time "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$now" +%Y-%m-%dT%H:%M:%SZ)" \
    --period 60 --statistics Average \
    --query "sort_by(Datapoints,&Timestamp)[-1].Average" --output text 2>/dev/null
}

# ASG 的 CPU/内存利用率（Average %）。优先按 AutoScalingGroupName 聚合维度查；
# 该维度无数据时（CWAgent 常只按 InstanceId 上报，没开 ASG 聚合），回退遍历 ASG
# 的 InService 实例、逐个按 InstanceId 维度取值再求平均。取不到显示 "-"。
asg_pct() {  # <namespace> <metric> <asg-name>
  local ns="$1" m="$2" asg="$3" v
  [ -z "$asg" ] || [ "$asg" = "None" ] && { echo "-"; return; }
  # 1) 先试 ASG 聚合维度
  v=$(_cw_avg "$ns" "$m" AutoScalingGroupName "$asg")
  if [ -n "$v" ] && [ "$v" != "None" ]; then
    awk -v v="$v" 'BEGIN{printf "%.0f", v}'; return
  fi
  # 2) 回退：遍历 InService 实例的 InstanceId 维度求平均
  local ids id sum=0 n=0 iv
  ids=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" --output text 2>/dev/null)
  for id in $ids; do
    iv=$(_cw_avg "$ns" "$m" InstanceId "$id")
    if [ -n "$iv" ] && [ "$iv" != "None" ]; then
      sum=$(awk -v s="$sum" -v x="$iv" 'BEGIN{print s+x}'); n=$((n+1))
    fi
  done
  [ "$n" -gt 0 ] && awk -v s="$sum" -v n="$n" 'BEGIN{printf "%.0f", s/n}' || echo "-"
}

# 表格列宽（全 ASCII 列头，避免中文宽度错位）。
# 列: CLUSTER(20) PENDING(12) INFLIGHT(9) DLQ(8) RECV/s(7) QPS(7) DOWN(7) UP(7) CPU(5)
# 内容总宽 = 20+12+9+8+7+7+7+7+5 + 9个分隔空格 = 91；+ 左右各 "│ "/" │" 边框。
_FMT="│ %-20s %12s %9s %8s %7s %7s %7s %7s %5s │\n"
# 边框按内容宽度自动生成（93 = 1空格+91+1空格），避免手数横线数错位。
_BORDER_W=93
_hline() {  # <左角> <右角>
  local mid; mid=$(printf '─%.0s' $(seq 1 $_BORDER_W))
  printf '%s%s%s' "$1" "$mid" "$2"
}
_HR="$(_hline '┌' '┐')"
_MR="$(_hline '├' '┤')"
_BR="$(_hline '└' '┘')"

# 大整数加千分位逗号（229785904 → 229,785,904），一眼看清量级。非数字/"-"/"?" 原样返回。
# 用 awk 实现（不依赖 GNU sed 的 \B/\> 方言，BSD/macOS 与 Linux 行为一致）。
_commafy() {  # <value>
  case "$1" in
    ''|'-'|'?'|*[!0-9]*) printf '%s' "$1" ;;
    *) awk -v n="$1" 'BEGIN{
         s=n; out=""; while(length(s)>3){ out=","substr(s,length(s)-2)out; s=substr(s,1,length(s)-3) }
         printf "%s%s", s, out
       }' ;;
  esac
}

# 采集所有集群的指标，每集群输出一行（制表符分隔的原始值，无颜色/格式）：
#   cid  pend  inflt  dlq  recv  done  din  dout  cpu
# table 和 csv 两个渲染器共用本函数，避免重复 40 行 AWS 调用。
# 找不到队列时不输出任何行（调用方据空判断报错）。
collect_rows() {
  local urls main_qs
  urls=$(aws sqs list-queues --region "$REGION" --queue-name-prefix "$PREFIX" --query "QueueUrls" --output text 2>/dev/null | tr '\t' '\n')
  main_qs=$(echo "$urls" | grep -v -- '-dlq$' | grep -v -- '-queue-dlq$')
  [ -z "$main_qs" ] && return 0

  # 整轮一次性按 tag 拉本工具 ASG 进缓存，后续 asg_for_cluster 本地匹配，零额外 API。
  load_asg_cache

  while read -r qurl; do
    [ -z "$qurl" ] && continue
    local qn cid pend inflt dlq dlqurl recv done asg din dout cpu
    qn=$(basename "$qurl")
    cid="${qn%-queue}"   # 集群标识 = 队列名去掉 -queue 后缀（ASG 匹配 / CSV 用全名）
    # 实时深度
    read -r pend inflt <<< "$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$qurl" \
      --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
      --query "[Attributes.ApproximateNumberOfMessages,Attributes.ApproximateNumberOfMessagesNotVisible]" --output text 2>/dev/null)"
    # DLQ 深度（主队列名换 -dlq 后缀，两种命名都试）
    dlq="-"
    for cand in "${qurl}-dlq" "${qurl%-queue}-queue-dlq"; do
      dlqurl=$(echo "$urls" | grep -F "$(basename "$cand")" | head -1)
      if [ -n "$dlqurl" ]; then
        dlq=$(aws sqs get-queue-attributes --region "$REGION" --queue-url "$dlqurl" \
          --attribute-names ApproximateNumberOfMessages --query "Attributes.ApproximateNumberOfMessages" --output text 2>/dev/null)
        break
      fi
    done
    # QPS（CloudWatch 最近1min Sum / 60）
    recv=$(cw_sum NumberOfMessagesReceived "$qn"); [ "$recv" = "None" ] || [ -z "$recv" ] && recv=0
    done=$(cw_sum NumberOfMessagesDeleted  "$qn"); [ "$done" = "None" ] || [ -z "$done" ] && done=0
    recv=$(awk -v v="$recv" 'BEGIN{printf "%.0f", v/60}')
    done=$(awk -v v="$done" 'BEGIN{printf "%.0f", v/60}')
    # ASG 网络速率（NetworkIn=源下载 / NetworkOut=S3上传）+ CPU 利用率
    asg=$(asg_for_cluster "$cid")
    din=$(asg_gbps NetworkIn  "$asg")
    dout=$(asg_gbps NetworkOut "$asg")
    cpu=$(asg_pct AWS/EC2 CPUUtilization "$asg")      # EC2 原生 CPU%（一定有）

    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$cid" "${pend:-?}" "${inflt:-?}" "${dlq:-0}" "$recv" "$done" "$din" "$dout" "$cpu"
  done <<< "$main_qs"
}

# 交互式彩色表格渲染（默认）。
snapshot() {
  echo "${C_T}${_HR}${C_0}"
  printf "${C_T}│ %-${_BORDER_W}s │${C_0}\n" " SQS + ASG Monitor    prefix=$PREFIX    region=$REGION    $(date '+%Y-%m-%d %H:%M:%S')"
  echo "${C_T}${_MR}${C_0}"
  # 表头两行：第一行字段名，第二行单位（DOWN/UP 单位 Gbps、CPU 单位 %）。
  printf "${C_T}${_FMT}${C_0}" "CLUSTER" "PENDING" "INFLIGHT" "DLQ" "RECV/s" "QPS" "DOWN" "UP" "CPU"
  printf "${C_DIM}${_FMT}${C_0}" "(short)" "msgs" "msgs" "msgs" "msg/s" "msg/s" "Gbps" "Gbps" "%"
  echo "${C_T}${_MR}${C_0}"

  local rows; rows=$(collect_rows)
  [ -z "$rows" ] && { printf "│ %-${_BORDER_W}s │\n" " ${C_ERR}没找到前缀为 $PREFIX 的队列${C_0}"; echo "${C_T}${_BR}${C_0}"; return; }

  local total_pend=0 total_inflt=0 total_dlq=0 total_recv=0 total_done=0
  while IFS=$'\t' read -r cid pend inflt dlq recv done din dout cpu; do
    [ -z "$cid" ] && continue
    # 显示名去掉公共前缀（gcs-2-s3-worker-c8in-big-pool2 → c8in-big-pool2），省列宽、聚焦差异。
    local short="${cid#"$PREFIX"-}"
    # 颜色：DLQ>0 红、CPU>=90 红其余绿、堆积>0 黄
    local dc="$C_0"; [ "${dlq:-0}" != "-" ] && [ "${dlq:-0}" -gt 0 ] 2>/dev/null && dc="$C_ERR"
    local pc="$C_0"; [ "${pend:-0}" -gt 0 ] 2>/dev/null && pc="$C_WARN"
    local cc="$C_OK"; [ "${cpu:-0}" != "-" ] && [ "${cpu%.*}" -ge 90 ] 2>/dev/null && cc="$C_ERR"
    printf "│ ${C_T}%-20s${C_0} ${pc}%12s${C_0} ${C_T}%9s${C_0} ${dc}%8s${C_0} %7s ${C_OK}%7s${C_0} ${C_OK}%7s${C_0} ${C_OK}%7s${C_0} ${cc}%5s${C_0} │\n" \
      "$short" "$(_commafy "${pend:-?}")" "$(_commafy "${inflt:-?}")" "$(_commafy "${dlq:-0}")" \
      "$(_commafy "$recv")" "$(_commafy "$done")" "$din" "$dout" "$cpu"
    total_pend=$((total_pend + ${pend:-0}))
    total_inflt=$((total_inflt + ${inflt:-0}))
    [ "${dlq:-0}" != "-" ] && total_dlq=$((total_dlq + ${dlq:-0}))
    total_recv=$((total_recv + recv))
    total_done=$((total_done + done))
  done <<< "$rows"

  echo "${C_T}${_MR}${C_0}"
  printf "${C_T}${_FMT}${C_0}" "TOTAL" "$(_commafy "$total_pend")" "$(_commafy "$total_inflt")" \
    "$(_commafy "$total_dlq")" "$(_commafy "$total_recv")" "$(_commafy "$total_done")" "-" "-" "-"
  echo "${C_T}${_BR}${C_0}"
  echo "${C_DIM} PENDING=待处理堆积  INFLIGHT=传输中  DLQ=死信  RECV/s=接收速率  QPS=完成/秒(≈成功对象/秒)  DOWN/UP=网卡下载/上传 Gbps${C_0}"
  echo "${C_DIM} 堆积/INFLIGHT/DLQ 近实时；RECV/QPS/网卡/CPU 来自 CloudWatch(~1-2min 延迟,1 分钟均值)${C_0}"
}

# CSV 渲染（cron/日志模式）：每集群一行，首列 UTC 时间戳。无颜色、无边框，append 即时序。
# CSV_HEADER=1 时先输出一行表头（建文件时用一次即可）。
csv_dump() {
  local ts; ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  [ "${CSV_HEADER:-0}" = "1" ] && echo "timestamp,cluster,pending,inflight,dlq,recv_per_s,qps_done,down_gbps,up_gbps,cpu_pct"
  local rows; rows=$(collect_rows)
  if [ -z "$rows" ]; then
    echo "$ts,NO_QUEUE_MATCH($PREFIX),,,,,,,," >&2
    return 1
  fi
  while IFS=$'\t' read -r cid pend inflt dlq recv done din dout cpu; do
    [ -z "$cid" ] && continue
    echo "$ts,$cid,$pend,$inflt,$dlq,$recv,$done,$din,$dout,$cpu"
  done <<< "$rows"
}

# ── 分发 ──────────────────────────────────────────────────────────────────────
if [ "$FORMAT" = "csv" ]; then
  csv_dump                                  # 单次 CSV（cron 每次触发跑一次）
elif [ "$INTERVAL" -gt 0 ] 2>/dev/null; then
  while true; do clear; snapshot; echo; echo "${C_DIM}每 ${INTERVAL}s 刷新，Ctrl-C 退出${C_0}"; sleep "$INTERVAL"; done
else
  snapshot
fi

# ── cronjob 配置（每 5 分钟采集一次，CSV append 进日志）─────────────────────────
#
# 1) 建日志目录 + 写一次表头：
#      sudo mkdir -p /var/log/migration
#      FORMAT=csv CSV_HEADER=1 AWS_REGION=eu-south-2 \
#        bash /path/to/sqs-monitor.sh gcs-2-s3-worker > /var/log/migration/sqs-metrics.csv
#
# 2) crontab -e 加一行（每 5 分钟，>> 追加不带表头）：
#      */5 * * * * FORMAT=csv AWS_REGION=eu-south-2 /path/to/sqs-monitor.sh gcs-2-s3-worker >> /var/log/migration/sqs-metrics.csv 2>> /var/log/migration/sqs-metrics.err
#
#    注意：cron 不读 ~/.aws/config 的 region 也行（脚本默认 eu-south-2），但 AWS 凭证
#    需在 cron 用户可见——EC2/跳板机用实例角色最稳（无需配 key）；若用 ~/.aws/credentials
#    则确认运行 cron 的用户 home 下有该文件。
#
# 3) 验证：
#      crontab -l                          # 确认那行在
#      tail -f /var/log/migration/sqs-metrics.csv     # 5 分钟后应每集群多一行
#      cat /var/log/migration/sqs-metrics.err         # 出错看这里(空=正常)
