// Package ratelimit 周期从 SSM 读 bwlimit/tpslimit，调 rcd 全局限速端点生效。
// 对齐 Python worker._ratelimit_loop：worker 只读 SSM，AIMD 控制器 Lambda 写。
//
// 与 Python/旧 go 实现的差异：限速不再 per-call 注入，而是设 rcd **全局**
// （core/bwlimit + options/set TPSLimit）。语义正确——per-host 总限速，不用除并发数；
// 也绕开了 _config 的 TPSLimit float64 坑。改一次 daemon 生效全部在跑+新传输。
package ratelimit

import (
	"context"
	"strconv"
	"time"

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

// Refresher 周期读 SSM → 设 rcd 全局限速。读取失败保留旧值；初始无旧值才 off。
type Refresher struct {
	ssm          SSMAPI
	rcd          RCDLimiter
	bwlimitParam string
	tpslimitName string
	lastBw       string // 去重：值没变不重复调 rcd
	lastTps      string
}

func New(ssmClient SSMAPI, limiter RCDLimiter, bwlimitParam, tpslimitParam string) *Refresher {
	return &Refresher{ssm: ssmClient, rcd: limiter, bwlimitParam: bwlimitParam, tpslimitName: tpslimitParam}
}

// Run 阻塞循环，每 interval 刷新一次，直到 ctx 取消。先立即刷一次。
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	r.refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh(ctx)
		}
	}
}

func (r *Refresher) refresh(ctx context.Context) {
	bw, bwOK := r.read(ctx, r.bwlimitParam)   // "off" 或 "745M"
	tps, tpsOK := r.read(ctx, r.tpslimitName) // "off" 或 "156.25"

	if bwOK {
		r.applyBw(ctx, bw)
	} else if r.lastBw == "" {
		r.applyBw(ctx, "off")
	} else {
		obslog.Warnf("读取 bwlimit SSM 失败，保留旧值 %s", r.lastBw)
	}

	if tpsOK {
		r.applyTPS(ctx, tps)
	} else if r.lastTps == "" {
		r.applyTPS(ctx, "off")
	} else {
		obslog.Warnf("读取 tpslimit SSM 失败，保留旧值 %s", r.lastTps)
	}
}

func (r *Refresher) applyBw(ctx context.Context, bw string) {
	if bw == r.lastBw {
		return
	}
	if err := r.rcd.SetBwLimit(ctx, bw); err != nil {
		obslog.Warnf("设 rcd bwlimit=%s 失败: %v", bw, err)
		return
	}
	r.lastBw = bw
}

func (r *Refresher) applyTPS(ctx context.Context, tps string) {
	if tps == r.lastTps {
		return
	}
	v := 0.0
	if tps != "off" {
		f, err := strconv.ParseFloat(tps, 64)
		if err != nil {
			obslog.Warnf("SSM tpslimit 非法，保留旧值 %s: value=%q err=%v", r.lastTps, tps, err)
			return
		}
		v = f
	}
	if err := r.rcd.SetTPSLimit(ctx, v); err != nil {
		obslog.Warnf("设 rcd tpslimit=%v 失败: %v", v, err)
		return
	}
	r.lastTps = tps
}

// read 读单个 SSM 参数；空值视作 off，读取失败返回 ok=false 由调用方保留旧值。
func (r *Refresher) read(ctx context.Context, name string) (value string, ok bool) {
	if name == "" {
		return "off", true
	}
	out, err := r.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		return "", false
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "off", true
	}
	return *out.Parameter.Value, true
}
