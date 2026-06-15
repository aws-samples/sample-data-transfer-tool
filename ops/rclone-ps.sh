#!/usr/bin/env bash
# rclone-ps —— 输入一个 ASG 集群名，通过 SSM 到每台 InService 实例执行
# `ps -ef | grep rclone`，汇总当前正在传输的 rclone 命令（看每台在传什么、并发多少）。
#
# 依赖：实例已装 SSM Agent + 挂了带 ssm:SendCommand 的角色（worker 的 AgentRole 满足）；
#       本机/跳板机有 ssm:SendCommand + ssm:GetCommandInvocation + autoscaling:Describe* 权限。
#
# 用法：
#   bash rclone-ps.sh <asg-名或子串>              # 默认 region eu-south-2
#   bash rclone-ps.sh gcs-2-s3-worker-m6in-pool1   # ASG 名带随机后缀，给子串即可模糊匹配
#   AWS_REGION=eu-south-2 bash rclone-ps.sh <名>
#   COUNT=1 bash rclone-ps.sh <名>                 # 只统计每台 rclone 进程数，不打全命令
#
# 说明：SSM 是异步下发——脚本下发后轮询每台的执行结果（默认最多等 60s）。
set -uo pipefail

# cron/非交互 PATH 极简，显式补全（与其它 ops 脚本一致）。
export PATH=/usr/local/bin:/usr/bin:/bin:${PATH:-}

REGION="${AWS_REGION:-eu-south-2}"
NAME="${1:-}"
COUNT="${COUNT:-0}"          # 1 = 只数进程数；0 = 打完整命令
MAX_WAIT="${MAX_WAIT:-60}"   # 轮询 SSM 结果的最长秒数

if [ -t 1 ]; then C_T=$'\033[1;36m'; C_OK=$'\033[32m'; C_DIM=$'\033[2m'; C_ERR=$'\033[31m'; C_0=$'\033[0m'
else C_T=""; C_OK=""; C_DIM=""; C_ERR=""; C_0=""; fi

if [ -z "$NAME" ]; then
  echo "用法: bash rclone-ps.sh <asg-名或子串>   (region=$REGION)" >&2
  echo "  例: bash rclone-ps.sh gcs-2-s3-worker-m6in-pool1" >&2
  exit 2
fi

err() { echo "${C_ERR}$*${C_0}" >&2; }

# ── 1) 解析 ASG 名（支持子串模糊匹配；多个匹配则报错列出让用户精确）──────────────
resolve_asg() {
  local matches
  matches=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --query "AutoScalingGroups[?contains(AutoScalingGroupName, '$NAME')].AutoScalingGroupName" \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$')
  local n; n=$(echo "$matches" | grep -c .)
  if [ "$n" -eq 0 ]; then
    err "没有匹配 '$NAME' 的 ASG（region=$REGION）"; exit 1
  elif [ "$n" -gt 1 ]; then
    err "'$NAME' 匹配到多个 ASG，请给更精确的名字："
    echo "$matches" | sed 's/^/  /' >&2; exit 1
  fi
  echo "$matches"
}

ASG="$(resolve_asg)" || exit 1

# ── 2) 取该 ASG 的 InService 实例 ────────────────────────────────────────────
INSTANCE_IDS=$(aws autoscaling describe-auto-scaling-groups --region "$REGION" \
  --auto-scaling-group-names "$ASG" \
  --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" \
  --output text 2>/dev/null)

if [ -z "$INSTANCE_IDS" ]; then
  err "ASG '$ASG' 无 InService 实例"; exit 1
fi
N_INST=$(echo "$INSTANCE_IDS" | wc -w | tr -d ' ')

echo "${C_T}ASG${C_0}     : $ASG"
echo "${C_T}Region${C_0}  : $REGION"
echo "${C_T}实例数${C_0}  : $N_INST  ($(echo "$INSTANCE_IDS" | tr '\t' ' '))"
echo "${C_DIM}下发 SSM 命令并轮询结果（最多等 ${MAX_WAIT}s）…${C_0}"

# ── 3) SSM 下发 ps 采集命令 ──────────────────────────────────────────────────
# grep '[r]clone' 的方括号技巧让 grep 自身进程不被匹配到（经典写法，省一个 grep -v grep）。
# 只保留真正的 rclone 传输进程（命令行含 copyto/deletefile）。
# 末尾 `; exit 0`：grep 无匹配时退出码非 0，会被 SSM RunShellScript（默认 set -e）
# 判成 Failed——但"没有 rclone 在跑"是完全正常的情况。强制 exit 0，结果靠 stdout 判。
if [ "$COUNT" = "1" ]; then
  REMOTE_CMD='ps -eo args | grep "[r]clone" | grep -Ec "copyto|deletefile"; exit 0'
else
  REMOTE_CMD='ps -eo pid,etime,args | grep "[r]clone" | grep -E "copyto|deletefile" || echo "(无正在运行的 rclone 传输进程)"; exit 0'
fi

# --parameters 的 shorthand 在命令含引号/管道/括号时会破坏内嵌 JSON（实测踩坑）。
# 用 python3 安全编码成 --cli-input-json（项目惯例：手拼 JSON 一律走 json.dumps）。
PARAM_JSON=$(python3 -c '
import json, sys
print(json.dumps({
    "InstanceIds": sys.argv[1].split(),
    "DocumentName": "AWS-RunShellScript",
    "Comment": "rclone-ps: list rclone transfer commands",
    "Parameters": {"commands": [sys.argv[2]]},
}))' "$INSTANCE_IDS" "$REMOTE_CMD")

CMD_ID=$(aws ssm send-command --region "$REGION" \
  --cli-input-json "$PARAM_JSON" \
  --query "Command.CommandId" --output text 2>&1)

if [ -z "$CMD_ID" ] || [ "$CMD_ID" = "None" ] || echo "$CMD_ID" | grep -qiE "error|exception"; then
  err "SSM send-command 失败：$CMD_ID"
  err "（检查实例是否在线/已装 SSM Agent/角色含 ssm:SendCommand）"
  exit 1
fi

# ── 4) 轮询每台的执行结果 ────────────────────────────────────────────────────
wait_done() {  # <instance-id> → 打印该实例的 stdout（或错误状态）
  local id="$1" status out waited=0
  while :; do
    status=$(aws ssm get-command-invocation --region "$REGION" \
      --command-id "$CMD_ID" --instance-id "$id" \
      --query "Status" --output text 2>/dev/null)
    case "$status" in
      Success|Failed|Cancelled|TimedOut)
        break ;;
      ""|None)
        # invocation 还没注册，稍等
        ;;
    esac
    waited=$((waited + 2))
    [ "$waited" -ge "$MAX_WAIT" ] && { echo "__TIMEOUT__"; return; }
    sleep 2
  done
  if [ "$status" != "Success" ]; then
    echo "__STATUS__:$status"
    return
  fi
  aws ssm get-command-invocation --region "$REGION" \
    --command-id "$CMD_ID" --instance-id "$id" \
    --query "StandardOutputContent" --output text 2>/dev/null
}

echo
TOTAL_PROC=0
for id in $INSTANCE_IDS; do
  out="$(wait_done "$id")"
  echo "${C_T}═══ $id ═══${C_0}"
  case "$out" in
    __TIMEOUT__)
      err "  SSM 结果超时（>${MAX_WAIT}s），跳过" ;;
    __STATUS__:*)
      err "  SSM 执行未成功：${out#__STATUS__:}" ;;
    *)
      if [ "$COUNT" = "1" ]; then
        proc_n="$(echo "$out" | tr -d ' \n')"
        proc_n="${proc_n//[!0-9]/}"   # 只留数字，防异常输出污染累加
        echo "  rclone 传输进程数: ${C_OK}${proc_n:-0}${C_0}"
        [ -n "$proc_n" ] && TOTAL_PROC=$((TOTAL_PROC + proc_n))
      else
        echo "$out" | sed 's/^/  /'
      fi ;;
  esac
  echo
done

if [ "$COUNT" = "1" ]; then
  echo "${C_T}全集群 rclone 传输进程总数: ${C_OK}${TOTAL_PROC}${C_0}"
fi
