// Package ratelimit 周期从 SSM 读 bwlimit/tpslimit，调 rcd 全局限速端点生效。
// 对齐 Python worker._ratelimit_loop：worker 只读 SSM，AIMD 控制器 Lambda 写。
//
// 与 Python/旧 go 实现的差异：限速不再 per-call 注入，而是设 rcd **全局**
// （core/bwlimit + options/set TPSLimit）。语义正确——per-host 总限速，不用除并发数；
// 也绕开了 _config 的 TPSLimit float64 坑。改一次 daemon 生效全部在跑+新传输。
package ratelimit

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
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

// Refresher 周期读 SSM → 设 rcd 全局限速。读不到/空 → off（不限速），不阻断传输。
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
	bw := r.read(ctx, r.bwlimitParam)  // "off" 或 "745M"
	tps := r.read(ctx, r.tpslimitName) // "off" 或 "156.25"

	// 带宽：值变了才设。rcd 的 core/bwlimit 接受 "off"/"100M"。
	if bw != r.lastBw {
		if err := r.rcd.SetBwLimit(ctx, bw); err != nil {
			log.Printf("设 rcd bwlimit=%s 失败: %v", bw, err)
		} else {
			r.lastBw = bw
		}
	}
	// TPS：off→0（不限），否则 parse float。
	if tps != r.lastTps {
		v := 0.0
		if tps != "off" {
			if f, err := strconv.ParseFloat(tps, 64); err == nil {
				v = f
			}
		}
		if err := r.rcd.SetTPSLimit(ctx, v); err != nil {
			log.Printf("设 rcd tpslimit=%v 失败: %v", v, err)
		} else {
			r.lastTps = tps
		}
	}
}

// read 读单个 SSM 参数；读不到/空 → "off"（failsafe 不限速，不阻断传输）。
func (r *Refresher) read(ctx context.Context, name string) string {
	if name == "" {
		return "off"
	}
	out, err := r.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil || out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "off"
	}
	return *out.Parameter.Value
}
