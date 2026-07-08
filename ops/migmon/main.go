// migmon — migration cluster monitoring & ops TUI (Go/bubbletea, AWS SDK v2).
//
// No args (TTY)    → interactive overview TUI (60s auto-refresh, key drill-down).
// No args (no TTY) → prints one overview snapshot (or CSV if -csv).
// Subcommands:
//   migmon sqs [-csv] [-header]   overview snapshot / CSV time-series (cron)
//   migmon ps   <cluster-substr>  rclone process count per instance (SSM)
//   migmon top  <cluster-substr>  rclone command lines per instance (SSM)
//   migmon refresh <cluster-substr>   rolling instance refresh (typed confirm)
//
// Env / flags: -region, -prefix, -project-tag, -parallel, -ssm-timeout,
//   -min-healthy, -warmup. Env vars AWS_REGION/PREFIX/PROJECT_TAG are honored.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	maws "migmon/internal/aws"
	"migmon/internal/model"
	"migmon/internal/render"
	"migmon/internal/tui"
)

func main() {
	region := flag.String("region", envOr("AWS_REGION", "eu-south-2"), "AWS region")
	prefix := flag.String("prefix", envOr("PREFIX", "gcs-2-s3-worker"), "queue/cluster name prefix")
	projectTag := flag.String("project-tag", envOr("PROJECT_TAG", "gcs-s3-migration"), "ASG tag:Project value")
	parallel := flag.Int("parallel", 8, "concurrent clusters collected")
	ssmTimeout := flag.Duration("ssm-timeout", 60*time.Second, "per-batch SSM poll timeout")
	minHealthy := flag.Int("min-healthy", 50, "instance-refresh MinHealthyPercentage")
	warmup := flag.Int("warmup", 180, "instance-refresh InstanceWarmup seconds")
	csv := flag.Bool("csv", envBool("FORMAT_CSV"), "sqs: output CSV instead of table")
	header := flag.Bool("header", false, "csv: include header line")
	flag.Parse()

	ctx := context.Background()
	clients, err := maws.New(ctx, *region)
	if err != nil {
		fatal("AWS 配置失败: %v", err)
	}
	col := &model.Collector{Clients: clients, Prefix: *prefix, ProjectTag: *projectTag, Parallel: *parallel}

	args := flag.Args()
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "sqs":
		runSnapshot(ctx, col, *region, *prefix, *csv, *header)
	case "ps", "top":
		if len(args) < 2 {
			fatal("用法: migmon %s <集群子串>", cmd)
		}
		runPS(ctx, clients, *region, *prefix, *projectTag, args[1], cmd == "top", *ssmTimeout)
	case "refresh":
		if len(args) < 2 {
			fatal("用法: migmon refresh <集群子串>")
		}
		runRefresh(ctx, clients, *region, *prefix, *projectTag, args[1], int32(*minHealthy), int32(*warmup))
	case "":
		// TTY → TUI; non-TTY → snapshot (or CSV).
		if !*csv && isTTY() {
			runTUI(col, clients, *region, *prefix, int32(*minHealthy), int32(*warmup), *ssmTimeout)
		} else {
			runSnapshot(ctx, col, *region, *prefix, *csv, *header)
		}
	default:
		fatal("未知命令: %s (sqs|ps|top|refresh 或无参进 TUI)", cmd)
	}
}

func runTUI(col *model.Collector, clients *maws.Clients, region, prefix string, minHealthy, warmup int32, ssmTimeout time.Duration) {
	m := tui.New(col, clients, region, prefix, minHealthy, warmup, ssmTimeout)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fatal("TUI 运行失败: %v", err)
	}
}

func runSnapshot(ctx context.Context, col *model.Collector, region, prefix string, csv, header bool) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rows, err := col.Collect(cctx)
	if err != nil {
		fatal("采集失败: %v", err)
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if csv {
		if len(rows) == 0 {
			fmt.Fprintf(os.Stderr, "%s,NO_QUEUE_MATCH(%s),,,,,,,,\n", ts, prefix)
			os.Exit(1)
		}
		fmt.Print(render.CSV(rows, ts, header))
		return
	}
	if len(rows) == 0 {
		fatal("没找到前缀为 %s 的队列", prefix)
	}
	fmt.Print(render.Snapshot(rows, prefix, region, time.Now().Format("15:04:05")))
}

func runPS(ctx context.Context, clients *maws.Clients, region, prefix, projectTag, filter string, detail bool, timeout time.Duration) {
	asg, short, err := clients.ResolveASG(ctx, prefix, projectTag, filter)
	if err != nil {
		fatal("解析集群失败: %v", err)
	}
	if asg == "" {
		fatal("没有匹配 '%s' 的集群", filter)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	res, err := clients.RunRclonePS(cctx, asg, detail, timeout)
	if err != nil {
		fatal("SSM 群发失败: %v", err)
	}
	fmt.Printf("===== 集群 %s  (实例 %d 台, region=%s) =====\n", short, len(res), region)
	total := 0
	for _, ip := range res {
		if detail {
			fmt.Printf("─── %s ───\n", ip.InstanceID)
			if ip.Err != "" {
				fmt.Printf("  %s\n", ip.Err)
				continue
			}
			if len(ip.Detail) == 0 {
				fmt.Println("  (no rclone running)")
			}
			for _, l := range ip.Detail {
				fmt.Println(l)
			}
		} else {
			cnt := "?"
			if ip.ProcCount >= 0 {
				cnt = fmt.Sprintf("%d", ip.ProcCount)
				total += ip.ProcCount
			}
			suffix := ""
			if ip.Err != "" {
				suffix = "  " + ip.Err
			}
			fmt.Printf("  %-21s rclone=%s%s\n", ip.InstanceID, cnt, suffix)
		}
	}
	if !detail {
		fmt.Printf(">>> 合计 rclone 进程: %d (across %d 台)\n", total, len(res))
	}
}

func runRefresh(ctx context.Context, clients *maws.Clients, region, prefix, projectTag, filter string, minHealthy, warmup int32) {
	asg, short, err := clients.ResolveASG(ctx, prefix, projectTag, filter)
	if err != nil {
		fatal("解析集群失败: %v", err)
	}
	if asg == "" {
		fatal("没有匹配 '%s' 的集群", filter)
	}
	n := clients.InstanceCount(ctx, asg)
	fmt.Println()
	fmt.Println("⚠  即将滚动更新(instance-refresh):")
	fmt.Printf("     集群 : %s\n     ASG  : %s\n     实例 : %d 台将逐步替换(MinHealthy=%d%%, Warmup=%ds)\n",
		short, asg, n, minHealthy, warmup)
	fmt.Println("     后果 : 每台重启并 git clone 拉最新代码,传输中的对象会中断重投(SQS 重投,不丢)。")
	fmt.Printf("确认请键入集群短名【%s】(其他任意键取消): ", short)

	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	if strings.TrimSpace(sc.Text()) != short {
		fmt.Println("已取消。")
		return
	}
	id, err := clients.StartRefresh(ctx, asg, minHealthy, warmup)
	if err != nil {
		fatal("发起失败: %v", err)
	}
	if id == "" {
		fatal("发起失败(权限?已有进行中的 refresh?)")
	}
	fmt.Printf("✓ 已发起滚动更新  InstanceRefreshId=%s\n", id)
	fmt.Printf("  查进度: aws autoscaling describe-instance-refreshes --region %s --auto-scaling-group-name %s --query 'InstanceRefreshes[0].[Status,PercentageComplete]' --output text\n", region, asg)
}

// ---- helpers ----
func isTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envBool(k string) bool {
	v := strings.ToLower(os.Getenv(k))
	return v == "1" || v == "true" || v == "csv"
}
func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
