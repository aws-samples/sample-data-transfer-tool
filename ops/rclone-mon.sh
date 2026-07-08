#!/usr/bin/env bash
# rclone-mon.sh — 自包含的传输集群监控 CLI(发现集群 + SSM 群发 + 汇总,内建 50 台分批分页)。
#
# 为什么单文件:SSM send-command 的 --instance-ids 单次最多 50 台,集群做大后必须分批。
# 把"发现集群 / 选择 / 群发 / 汇总"收进一个脚本,分批循环只写一处,不再拆 watch+ps 两文件。
#
# 用法:
#   ./rclone-mon.sh                    # 交互:方向键↑↓选集群(Enter 选中/a 全部/q 取消)→ 看进程数
#   ./rclone-mon.sh -d                 # 交互:选完看每台在传哪些文件(完整命令行)
#   ./rclone-mon.sh small-pool1        # 直选(ASG 名子串匹配),看进程数
#   ./rclone-mon.sh -d small-pool1     # 直选 + 详情
#   ./rclone-mon.sh -a                 # 不选,扫描 prefix 下所有集群的进程数汇总
#
# 环境变量:
#   AWS_REGION   默认 eu-south-2
#   PREFIX       集群 ASG 名前缀,默认 gcs-2-s3-worker
#   SSM_BATCH    每批实例数,默认 50(SSM 硬上限;调小可降并发压力)
#   TIMEOUT      单批 SSM 结果轮询秒数上限,默认 60
#
# 依赖权限:autoscaling:Describe*, ec2:DescribeInstances,
#          ssm:SendCommand / ssm:GetCommandInvocation / ssm:ListCommandInvocations。
set -uo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:${PATH:-}

REGION="${AWS_REGION:-eu-south-2}"
PREFIX="${PREFIX:-gcs-2-s3-worker}"
SSM_BATCH="${SSM_BATCH:-50}"
TIMEOUT="${TIMEOUT:-60}"

# ── 参数解析:-d 详情 / -a 全部 / 位置参数=集群子串 ──────────────────────────
DETAIL=0
ALL=0
FILTER=""
for arg in "$@"; do
  case "$arg" in
    -d) DETAIL=1 ;;
    -a) ALL=1 ;;
    -*) echo "未知选项: $arg" >&2; exit 2 ;;
    *)  FILTER="$arg" ;;
  esac
done

# 远端在每台机器上执行的探测命令:
#   DETAIL=0 → 只数 rclone copyto 进程数(pgrep -c,[r] 技巧避免匹配到自己)
#   DETAIL=1 → 列出每个 rclone 的完整命令行(看在传哪些文件)
if [ "$DETAIL" -eq 1 ]; then
  REMOTE_CMD='pgrep -af "[r]clone copyto" || echo "(no rclone running)"'
else
  REMOTE_CMD='printf "rclone_procs=%s\n" "$(pgrep -c "[r]clone copyto" || echo 0)"'
fi

# ── 方向键菜单 ───────────────────────────────────────────────────────────────
# arrow_menu <array_name> <prefix>：↑/↓(k/j)移动,Enter 选中,a=全部,q/Esc 取消。
# 结果写全局:MENU_SEL(选中下标,0-based)、MENU_ALL(1=选了全部)。返回 1=取消。
# 全部渲染/读键走 /dev/tty & stderr,不污染 stdout(stdout 只留最终数据)。
MENU_SEL=0
MENU_ALL=0
arrow_menu() {
  local __arr="$1" prefix="$2"
  local n; eval "n=\${#$__arr[@]}"
  local cur=0 key
  MENU_ALL=0

  # 光标隐藏,退出时恢复。
  printf '\e[?25l' >&2
  trap 'printf "\e[?25h" >&2' RETURN

  _render() {
    local i short item
    printf '\r\e[2m↑/↓ 或 k/j 移动 · Enter 选中 · a 全部 · q 取消\e[0m\n' >&2
    for ((i = 0; i < n; i++)); do
      eval "item=\${$__arr[$i]}"
      short="${item#${prefix}-}"; short="${short%-AgentASG-*}"
      if [ "$i" -eq "$cur" ]; then
        printf '\e[7m  ▶ %s  \e[0m\n' "$short" >&2   # 反色高亮当前项
      else
        printf '    %s\n' "$short" >&2
      fi
    done
  }

  _render
  while true; do
    IFS= read -rsn1 key </dev/tty || return 1
    case "$key" in
      $'\e')  # 可能是方向键转义序列 \e[A / \e[B
        read -rsn2 -t 0.001 key2 </dev/tty
        case "$key2" in
          '[A') ((cur > 0)) && cur=$((cur - 1)) ;;      # ↑
          '[B') ((cur < n - 1)) && cur=$((cur + 1)) ;;  # ↓
          '')   return 1 ;;                              # 单独 Esc = 取消
        esac ;;
      k|K) ((cur > 0)) && cur=$((cur - 1)) ;;
      j|J) ((cur < n - 1)) && cur=$((cur + 1)) ;;
      a|A) MENU_ALL=1; printf '\e[?25h' >&2; return 0 ;;
      q|Q) return 1 ;;
      '')  MENU_SEL="$cur"; printf '\e[?25h' >&2; return 0 ;;  # Enter
    esac
    # 重绘:光标上移到菜单起点(标题 1 行 + n 项),清屏到底再画。
    printf '\e[%dA\e[J' "$((n + 1))" >&2
    _render
  done
}

# ── 发现集群 ─────────────────────────────────────────────────────────────────
echo "发现集群(prefix=$PREFIX, region=$REGION)…" >&2
# 用 while read 而非 mapfile:兼容 bash 3.2(macOS)与 4+(Amazon Linux)。
ASGS=()
while IFS= read -r line; do [ -n "$line" ] && ASGS+=("$line"); done < <(
  aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --query "AutoScalingGroups[?contains(AutoScalingGroupName,'$PREFIX')].AutoScalingGroupName" \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$' | sort)

[ "${#ASGS[@]}" -eq 0 ] && { echo "没发现集群(检查权限/PREFIX/REGION)。" >&2; exit 1; }

# ── 选定要查的集群集合 CHOSEN[] ──────────────────────────────────────────────
CHOSEN=()
if [ "$ALL" -eq 1 ]; then
  CHOSEN=("${ASGS[@]}")
elif [ -n "$FILTER" ]; then
  for asg in "${ASGS[@]}"; do
    [[ "$asg" == *"$FILTER"* ]] && CHOSEN+=("$asg")
  done
  [ "${#CHOSEN[@]}" -eq 0 ] && { echo "没有匹配 '$FILTER' 的集群。" >&2; exit 1; }
else
  # 方向键交互菜单:↑/↓(或 k/j)移动,Enter 选中,a=全部,q/Esc 取消。
  # 无 TTY(管道/非交互)时回退到数字输入,保证脚本仍可脚本化调用。
  if [ -t 0 ] && [ -t 2 ]; then
    arrow_menu ASGS "$PREFIX" || { echo "已取消。" >&2; exit 0; }
    if [ "$MENU_ALL" -eq 1 ]; then
      CHOSEN=("${ASGS[@]}")
    else
      CHOSEN=("${ASGS[$MENU_SEL]}")
    fi
  else
    echo >&2
    i=1
    for asg in "${ASGS[@]}"; do
      short="${asg#${PREFIX}-}"; short="${short%-AgentASG-*}"
      printf "  %2d) %s\n" "$i" "$short" >&2
      i=$((i + 1))
    done
    echo >&2
    read -rp "选择集群编号(回车取消,a=全部): " SEL
    [ -z "$SEL" ] && { echo "已取消。" >&2; exit 0; }
    if [ "$SEL" = "a" ]; then
      CHOSEN=("${ASGS[@]}")
    elif [[ "$SEL" =~ ^[0-9]+$ ]] && [ "$SEL" -ge 1 ] && [ "$SEL" -le "${#ASGS[@]}" ]; then
      CHOSEN=("${ASGS[$((SEL - 1))]}")
    else
      echo "无效编号: $SEL" >&2; exit 1
    fi
  fi
fi

# ── 取某 ASG 的在跑实例 id 列表(仅 InService + running) ─────────────────────
asg_instances() {
  local asg="$1"
  aws autoscaling describe-auto-scaling-groups --region "$REGION" \
    --auto-scaling-group-names "$asg" \
    --query "AutoScalingGroups[0].Instances[?LifecycleState=='InService'].InstanceId" \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$'
}

# 读命令输出到数组(bash 3.2 兼容替代 mapfile)。用法: read_into ARR < <(cmd)
read_into() {
  local __name="$1" __line
  eval "$__name=()"
  while IFS= read -r __line; do
    [ -n "$__line" ] && eval "$__name+=(\"\$__line\")"
  done
}

# ── 对一批(≤50)实例发 SSM 并回收每台输出 ────────────────────────────────────
# 打印形如 "<instance_id>\t<单行输出>";详情模式下多行原样透传。
run_batch() {
  local -a ids=("$@")
  [ "${#ids[@]}" -eq 0 ] && return 0
  # 用 JSON 文件传 parameters,彻底避免 shell 引号/特殊字符转义地雷(REMOTE_CMD 含引号)。
  local params_json
  params_json=$(mktemp)
  # printf %s 后经 python json.dumps 生成合法 JSON 字符串,再包成 {"commands":[...]}。
  python3 - "$REMOTE_CMD" >"$params_json" <<'PY'
import json, sys
print(json.dumps({"commands": [sys.argv[1]]}))
PY
  local cmd_id
  cmd_id=$(aws ssm send-command --region "$REGION" \
    --document-name "AWS-RunShellScript" \
    --instance-ids "${ids[@]}" \
    --parameters "file://$params_json" \
    --query 'Command.CommandId' --output text 2>/dev/null)
  rm -f "$params_json"
  if [ -z "$cmd_id" ] || [ "$cmd_id" = "None" ]; then
    echo "  [批发送失败] ${ids[*]}" >&2
    return 1
  fi

  # 轮询直到本批全部终态(Success/Failed/TimedOut…)或超时。
  local waited=0
  while [ "$waited" -lt "$TIMEOUT" ]; do
    local pending
    pending=$(aws ssm list-command-invocations --region "$REGION" \
      --command-id "$cmd_id" \
      --query "length(CommandInvocations[?Status=='Pending' || Status=='InProgress' || Status=='Delayed'])" \
      --output text 2>/dev/null)
    [ "${pending:-0}" = "0" ] && break
    sleep 3; waited=$((waited + 3))
  done

  # 回收每台输出。
  local iid out
  for iid in "${ids[@]}"; do
    out=$(aws ssm get-command-invocation --region "$REGION" \
      --command-id "$cmd_id" --instance-id "$iid" \
      --query 'StandardOutputContent' --output text 2>/dev/null)
    if [ "$DETAIL" -eq 1 ]; then
      echo "─── $iid ───"
      printf '%s\n' "${out:-(无输出/超时)}"
    else
      # 只数进程数:从 rclone_procs=N 提取
      local n="${out#*rclone_procs=}"; n="${n%%$'\n'*}"
      [[ "$n" =~ ^[0-9]+$ ]] || n="?"
      printf '%s\t%s\n' "$iid" "$n"
    fi
  done
}

# ── 主循环:逐集群 → 实例分批(50)→ 群发 → 汇总 ──────────────────────────────
for asg in "${CHOSEN[@]}"; do
  short="${asg#${PREFIX}-}"; short="${short%-AgentASG-*}"
  read_into IIDS < <(asg_instances "$asg")
  echo
  echo "===== 集群 $short  (实例 ${#IIDS[@]} 台, region=$REGION) ====="
  if [ "${#IIDS[@]}" -eq 0 ]; then
    echo "  (无 InService 实例)"; continue
  fi

  total_procs=0
  batch_no=0
  # 按 SSM_BATCH 切片,分批群发(核心:超 50 台自动分页)。
  for ((start = 0; start < ${#IIDS[@]}; start += SSM_BATCH)); do
    batch_no=$((batch_no + 1))
    batch=("${IIDS[@]:start:SSM_BATCH}")
    echo "  --- 批 $batch_no: ${#batch[@]} 台 ---"
    if [ "$DETAIL" -eq 1 ]; then
      run_batch "${batch[@]}"
    else
      # 数进程模式:边打印边累加总数
      while IFS=$'\t' read -r iid n; do
        printf "    %-21s rclone=%s\n" "$iid" "$n"
        [[ "$n" =~ ^[0-9]+$ ]] && total_procs=$((total_procs + n))
      done < <(run_batch "${batch[@]}")
    fi
  done

  [ "$DETAIL" -eq 0 ] && echo "  >>> 集群 $short 合计 rclone 进程数: $total_procs (across ${#IIDS[@]} 台, $batch_no 批)"
done
