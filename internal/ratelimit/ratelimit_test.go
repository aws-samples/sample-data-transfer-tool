package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type fakeSSM struct {
	mu     sync.Mutex
	values map[string]string
	errs   map[string]error
}

func (f *fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.Name)
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	v := f.values[name]
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{Value: aws.String(v)}}, nil
}

type fakeLimiter struct {
	bw  []string
	tps []float64
}

func (f *fakeLimiter) SetBwLimit(_ context.Context, rate string) error {
	f.bw = append(f.bw, rate)
	return nil
}

func (f *fakeLimiter) SetTPSLimit(_ context.Context, tps float64) error {
	f.tps = append(f.tps, tps)
	return nil
}

// ApplyOnce 读到有效值时设给 rcd（各调一次）。
func TestApplyOnceSetsValues(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "745M", "/tps": "156.25"}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background())

	if len(limiter.bw) != 1 || limiter.bw[0] != "745M" {
		t.Fatalf("bwlimit 应设为 745M 一次，got %v", limiter.bw)
	}
	if len(limiter.tps) != 1 || limiter.tps[0] != 156.25 {
		t.Fatalf("tpslimit 应设为 156.25 一次，got %v", limiter.tps)
	}
}

// 读取失败 → 该项 off（bwlimit=off / tps=0）。
func TestApplyOnceReadFailSetsOff(t *testing.T) {
	ssmClient := &fakeSSM{
		values: map[string]string{},
		errs:   map[string]error{"/bw": errors.New("ssm down"), "/tps": errors.New("ssm down")},
	}
	limiter := &fakeLimiter{}
	New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background())

	if len(limiter.bw) != 1 || limiter.bw[0] != "off" {
		t.Fatalf("bwlimit 读取失败应设 off，got %v", limiter.bw)
	}
	if len(limiter.tps) != 1 || limiter.tps[0] != 0 {
		t.Fatalf("tpslimit 读取失败应设 0(off)，got %v", limiter.tps)
	}
}

// 空值 → off。
func TestApplyOnceEmptyValueSetsOff(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "", "/tps": ""}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background())

	if limiter.bw[0] != "off" || limiter.tps[0] != 0 {
		t.Fatalf("空值应设 off，got bw=%v tps=%v", limiter.bw, limiter.tps)
	}
}

// tpslimit 非法 → 不调用 rcd 的 SetTPSLimit（该项跳过，退化不限）。
func TestApplyOnceInvalidTPSSkips(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "100M", "/tps": "bad"}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background())

	if len(limiter.bw) != 1 || limiter.bw[0] != "100M" {
		t.Fatalf("bwlimit 应正常设 100M，got %v", limiter.bw)
	}
	if len(limiter.tps) != 0 {
		t.Fatalf("非法 tpslimit 不应调用 rcd，got %v", limiter.tps)
	}
}

// 参数名为空 → off（不读 SSM）。
func TestApplyOnceEmptyParamNameSetsOff(t *testing.T) {
	limiter := &fakeLimiter{}
	New(&fakeSSM{values: map[string]string{}, errs: map[string]error{}}, limiter, "", "").ApplyOnce(context.Background())
	if limiter.bw[0] != "off" || limiter.tps[0] != 0 {
		t.Fatalf("空参数名应设 off，got bw=%v tps=%v", limiter.bw, limiter.tps)
	}
}
