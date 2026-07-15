// Package ratelimit 在 worker 启动时从 SSM 读一次固定上限（bwlimit/tpslimit），
// 设给本机 rcd 全局限速端点，之后不再变动。
//
// 设计（2026-07-14 去动态化）：不再有 AIMD 控制器 Lambda 周期改 SSM、也不再周期
// 重读——每台机的 rcd 就是一个固定上限。运维需调整时改 SSM 值 + 滚动实例即可。
// 限速设 rcd **全局**（core/bwlimit + options/set TPSLimit）= per-host 总限速，
// 不用除并发数，也绕开 _config 的 TPSLimit float64 坑。
package ratelimit

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// SSMAPI SSM 客户端最小接口（便于测试注入）。
type SSMAPI interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// RCDLimiter rcd 全局限速接口（rcd.Client 实现）。
type RCDLimiter interface {
	SetBwLimit(ctx context.Context, rate string) error
	SetTPSLimit(ctx context.Context, tps float64) error
}

// Limiter 启动时读一次 SSM 固定上限并设 rcd。读取失败视作 off（不限速），只告警不阻断启动。
type Limiter struct {
	ssm          SSMAPI
	rcd          RCDLimiter
	bwlimitParam string
	tpslimitName string
}

func New(ssmClient SSMAPI, limiter RCDLimiter, bwlimitParam, tpslimitParam string) *Limiter {
	return &Limiter{ssm: ssmClient, rcd: limiter, bwlimitParam: bwlimitParam, tpslimitName: tpslimitParam}
}

// ApplyOnce 从 SSM 读一次 bwlimit + tpslimit，设给 rcd 全局限速。
// ApplyOnce 从 SSM 读一次 bwlimit + tpslimit 设给 rcd 全局。**fail-fast 语义**（硬红线）:
// 任一步失败返回 error,由调用方 fail-fast（worker 不启动）——限速是必须的红线,漏限=打爆
// 源端配额/429,宁可实例起不来也不裸奔无 cap。仅当参数明确为 off/空(运维有意不限)才放行。
//   - 参数名为空 或 SSM 值为 "off"/空:明确不限速,正常返回 nil。
//   - 读 SSM 失败 / 值非法 / SetBwLimit|SetTPSLimit 失败:返回 error → 启动中止。
func (l *Limiter) ApplyOnce(ctx context.Context) error {
	bw, err := l.read(ctx, l.bwlimitParam)
	if err != nil {
		return fmt.Errorf("读取 bwlimit: %w", err)
	}
	if err := l.rcd.SetBwLimit(ctx, bw); err != nil {
		return fmt.Errorf("设 rcd bwlimit=%s: %w", bw, err)
	}
	obslog.Infof("rcd bwlimit 已设为 %s", bw)

	tps, err := l.read(ctx, l.tpslimitName)
	if err != nil {
		return fmt.Errorf("读取 tpslimit: %w", err)
	}
	v := 0.0
	if tps != "off" {
		f, perr := strconv.ParseFloat(tps, 64)
		if perr != nil {
			return fmt.Errorf("tpslimit 值非法 %q: %w", tps, perr)
		}
		v = f
	}
	if err := l.rcd.SetTPSLimit(ctx, v); err != nil {
		return fmt.Errorf("设 rcd tpslimit=%v: %w", v, err)
	}
	obslog.Infof("rcd tpslimit 已设为 %s", tps)
	return nil
}

// read 读单个 SSM 参数。参数名为空 → "off"(运维有意不限,正常)。值为空 → "off"。
// **读取失败返回 error**(硬红线:配了参数名却读不到 SSM,不能假装不限速裸奔)。
func (l *Limiter) read(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "off", nil
	}
	out, err := l.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("SSM GetParameter %s: %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "off", nil
	}
	return *out.Parameter.Value, nil
}
