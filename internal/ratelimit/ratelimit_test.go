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
	bw     []string
	tps    []float64
	bwErr  error // 令 SetBwLimit 返回错误
	tpsErr error // 令 SetTPSLimit 返回错误
}

func (f *fakeLimiter) SetBwLimit(_ context.Context, rate string) error {
	f.bw = append(f.bw, rate)
	return f.bwErr
}

func (f *fakeLimiter) SetTPSLimit(_ context.Context, tps float64) error {
	f.tps = append(f.tps, tps)
	return f.tpsErr
}

// 有效上限 → 设给 rcd,ApplyOnce 返回 nil。
func TestApplyOnceSetsValues(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "745M", "/tps": "156.25"}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	if err := New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background()); err != nil {
		t.Fatalf("有效值不应报错: %v", err)
	}
	if len(limiter.bw) != 1 || limiter.bw[0] != "745M" {
		t.Fatalf("bwlimit 应设 745M,got %v", limiter.bw)
	}
	if len(limiter.tps) != 1 || limiter.tps[0] != 156.25 {
		t.Fatalf("tpslimit 应设 156.25,got %v", limiter.tps)
	}
}

// off/空值 → 明确不限速,正常返回 nil,SetBwLimit 收到 "off"、SetTPSLimit 收到 0。
func TestApplyOnceOffIsOK(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "off", "/tps": ""}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	if err := New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background()); err != nil {
		t.Fatalf("off/空值应正常放行: %v", err)
	}
	if limiter.bw[0] != "off" || limiter.tps[0] != 0 {
		t.Fatalf("off 应设 off/0,got bw=%v tps=%v", limiter.bw, limiter.tps)
	}
}

// 参数名为空 → 运维有意不限,放行。
func TestApplyOnceEmptyParamNameOK(t *testing.T) {
	limiter := &fakeLimiter{}
	if err := New(&fakeSSM{values: map[string]string{}, errs: map[string]error{}}, limiter, "", "").ApplyOnce(context.Background()); err != nil {
		t.Fatalf("空参数名应放行: %v", err)
	}
	if limiter.bw[0] != "off" || limiter.tps[0] != 0 {
		t.Fatalf("空参数名应设 off/0,got bw=%v tps=%v", limiter.bw, limiter.tps)
	}
}

// 硬红线:读 SSM 失败 → fail-fast 返回 error(不裸奔当不限速)。
func TestApplyOnceReadFailReturnsError(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{}, errs: map[string]error{"/bw": errors.New("ssm down")}}
	if err := New(ssmClient, &fakeLimiter{}, "/bw", "/tps").ApplyOnce(context.Background()); err == nil {
		t.Fatal("读 SSM 失败应返回 error(fail-fast),而非放行")
	}
}

// 硬红线:SetBwLimit 失败 → fail-fast。
func TestApplyOnceSetBwFailReturnsError(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "745M", "/tps": "off"}, errs: map[string]error{}}
	limiter := &fakeLimiter{bwErr: errors.New("rcd unreachable")}
	if err := New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background()); err == nil {
		t.Fatal("SetBwLimit 失败应返回 error(fail-fast),不能裸奔无 cap")
	}
}

// 硬红线:tpslimit 值非法 → fail-fast。
func TestApplyOnceInvalidTPSReturnsError(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "off", "/tps": "notanumber"}, errs: map[string]error{}}
	if err := New(ssmClient, &fakeLimiter{}, "/bw", "/tps").ApplyOnce(context.Background()); err == nil {
		t.Fatal("非法 tpslimit 应返回 error(fail-fast)")
	}
}

// 硬红线:SetTPSLimit 失败 → fail-fast。
func TestApplyOnceSetTPSFailReturnsError(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "off", "/tps": "100"}, errs: map[string]error{}}
	limiter := &fakeLimiter{tpsErr: errors.New("rcd unreachable")}
	if err := New(ssmClient, limiter, "/bw", "/tps").ApplyOnce(context.Background()); err == nil {
		t.Fatal("SetTPSLimit 失败应返回 error(fail-fast)")
	}
}
