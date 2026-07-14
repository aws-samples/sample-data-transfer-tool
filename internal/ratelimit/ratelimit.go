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
// 幂等且非阻断：任一读取/设置失败只告警（该项退化为不限速），不返回错误、不影响启动。
func (l *Limiter) ApplyOnce(ctx context.Context) {
	l.applyBw(ctx, l.read(ctx, l.bwlimitParam))   // "off" 或 "745M"
	l.applyTPS(ctx, l.read(ctx, l.tpslimitName))  // "off" 或 "156.25"
}

func (l *Limiter) applyBw(ctx context.Context, bw string) {
	if err := l.rcd.SetBwLimit(ctx, bw); err != nil {
		obslog.Warnf("设 rcd bwlimit=%s 失败（本机不限带宽）: %v", bw, err)
		return
	}
	obslog.Infof("rcd bwlimit 已设为 %s", bw)
}

func (l *Limiter) applyTPS(ctx context.Context, tps string) {
	v := 0.0
	if tps != "off" {
		f, err := strconv.ParseFloat(tps, 64)
		if err != nil {
			obslog.Warnf("SSM tpslimit 非法（本机不限 TPS）: value=%q err=%v", tps, err)
			return
		}
		v = f
	}
	if err := l.rcd.SetTPSLimit(ctx, v); err != nil {
		obslog.Warnf("设 rcd tpslimit=%v 失败（本机不限 TPS）: %v", v, err)
		return
	}
	obslog.Infof("rcd tpslimit 已设为 %s", tps)
}

// read 读单个 SSM 参数；参数名为空、值为空、或读取失败一律视作 "off"（不限速）。
// 启动读一次场景下失败即 off——不保留旧值（无旧值可留），运维保证 SSM 有正确固定上限。
func (l *Limiter) read(ctx context.Context, name string) string {
	if name == "" {
		return "off"
	}
	out, err := l.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		obslog.Warnf("读取 SSM %s 失败，视作 off: %v", name, err)
		return "off"
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "off"
	}
	return *out.Parameter.Value
}
