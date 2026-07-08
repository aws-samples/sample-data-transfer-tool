package aws

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// InstanceProc is one instance's rclone process view (ps/top drill-down).
// Defined here (not in model) so the aws package has no cycle with model.
type InstanceProc struct {
	InstanceID string
	ProcCount  int      // rclone copyto/copy process count (ps mode); -1 = unknown
	Detail     []string // full rclone command lines (top mode); nil in ps mode
	Err        string   // per-instance SSM error, "" on success
}

// SSMBatchLimit is the hard SendCommand instance cap; we page in ≤50 chunks.
const SSMBatchLimit = 50

// InServiceInstances returns InService instance ids for an ASG.
func (c *Clients) InServiceInstances(ctx context.Context, asg string) ([]string, error) {
	out, err := c.ASG.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asg},
	})
	if err != nil || len(out.AutoScalingGroups) == 0 {
		return nil, err
	}
	var ids []string
	for _, inst := range out.AutoScalingGroups[0].Instances {
		if inst.InstanceId != nil && inst.LifecycleState == "InService" {
			ids = append(ids, *inst.InstanceId)
		}
	}
	return ids, nil
}

// rcloneProcScript counts running rclone copyto/copy processes.
const rcloneProcScript = `printf "rclone_procs=%s\n" "$(pgrep -c "[r]clone (copyto|copy)" || echo 0)"`

// rcloneDetailScript lists full rclone command lines.
const rcloneDetailScript = `pgrep -af "[r]clone (copyto|copy)" || echo "(no rclone running)"`

// RunRclonePS sends the process-count (detail=false) or command-line
// (detail=true) probe to all instances of an ASG, paging in ≤50 batches,
// and returns per-instance results. timeout bounds each batch's polling.
func (c *Clients) RunRclonePS(ctx context.Context, asg string, detail bool, timeout time.Duration) ([]InstanceProc, error) {
	ids, err := c.InServiceInstances(ctx, asg)
	if err != nil {
		return nil, err
	}
	script := rcloneProcScript
	if detail {
		script = rcloneDetailScript
	}
	var results []InstanceProc
	for start := 0; start < len(ids); start += SSMBatchLimit {
		end := start + SSMBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		results = append(results, c.runBatch(ctx, batch, script, detail, timeout)...)
	}
	return results, nil
}

func (c *Clients) runBatch(ctx context.Context, ids []string, script string, detail bool, timeout time.Duration) []InstanceProc {
	res := make([]InstanceProc, 0, len(ids))
	send, err := c.SSM.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: ptr("AWS-RunShellScript"),
		InstanceIds:  ids,
		Parameters:   map[string][]string{"commands": {script}},
	})
	if err != nil || send.Command == nil || send.Command.CommandId == nil {
		for _, id := range ids {
			res = append(res, InstanceProc{InstanceID: id, ProcCount: -1, Err: "send-command failed"})
		}
		return res
	}
	cmdID := *send.Command.CommandId

	// Poll until no invocation is Pending/InProgress/Delayed, or timeout.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		list, lerr := c.SSM.ListCommandInvocations(ctx, &ssm.ListCommandInvocationsInput{CommandId: ptr(cmdID)})
		if lerr != nil {
			break
		}
		pending := 0
		for _, ci := range list.CommandInvocations {
			switch ci.Status {
			case "Pending", "InProgress", "Delayed":
				pending++
			}
		}
		if pending == 0 && len(list.CommandInvocations) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			deadline = time.Now() // fall through to collect what we have
		case <-time.After(2 * time.Second):
		}
	}

	for _, id := range ids {
		inv, ierr := c.SSM.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  ptr(cmdID),
			InstanceId: ptr(id),
		})
		ip := InstanceProc{InstanceID: id, ProcCount: -1}
		if ierr != nil || inv.StandardOutputContent == nil {
			ip.Err = "no output/timeout"
			res = append(res, ip)
			continue
		}
		out := *inv.StandardOutputContent
		if detail {
			ip.Detail = splitNonEmpty(out)
		} else {
			ip.ProcCount = parseProcCount(out)
		}
		res = append(res, ip)
	}
	return res
}

func parseProcCount(out string) int {
	i := strings.Index(out, "rclone_procs=")
	if i < 0 {
		return -1
	}
	rest := out[i+len("rclone_procs="):]
	if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return -1
	}
	return n
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimRight(line, "\r"); t != "" {
			out = append(out, t)
		}
	}
	return out
}
