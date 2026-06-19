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

func TestRefreshInitialReadFailSetsOff(t *testing.T) {
	ssmClient := &fakeSSM{
		values: map[string]string{},
		errs: map[string]error{
			"/bw":  errors.New("ssm down"),
			"/tps": errors.New("ssm down"),
		},
	}
	limiter := &fakeLimiter{}
	r := New(ssmClient, limiter, "/bw", "/tps")
	r.refresh(context.Background())
	if len(limiter.bw) != 1 || limiter.bw[0] != "off" {
		t.Fatalf("初始 bwlimit 读取失败应设 off，got %v", limiter.bw)
	}
	if len(limiter.tps) != 1 || limiter.tps[0] != 0 {
		t.Fatalf("初始 tpslimit 读取失败应设 0，got %v", limiter.tps)
	}
}

func TestRefreshReadFailPreservesLastValue(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "100M", "/tps": "10"}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	r := New(ssmClient, limiter, "/bw", "/tps")
	r.refresh(context.Background())

	ssmClient.mu.Lock()
	ssmClient.errs["/bw"] = errors.New("ssm down")
	ssmClient.errs["/tps"] = errors.New("ssm down")
	ssmClient.mu.Unlock()
	r.refresh(context.Background())

	if len(limiter.bw) != 1 || limiter.bw[0] != "100M" {
		t.Fatalf("bwlimit 读取失败应保留旧值且不重复设置，got %v", limiter.bw)
	}
	if len(limiter.tps) != 1 || limiter.tps[0] != 10 {
		t.Fatalf("tpslimit 读取失败应保留旧值且不重复设置，got %v", limiter.tps)
	}
}

func TestRefreshInvalidTPSDoesNotOverwriteLastValue(t *testing.T) {
	ssmClient := &fakeSSM{values: map[string]string{"/bw": "100M", "/tps": "10"}, errs: map[string]error{}}
	limiter := &fakeLimiter{}
	r := New(ssmClient, limiter, "/bw", "/tps")
	r.refresh(context.Background())

	ssmClient.mu.Lock()
	ssmClient.values["/tps"] = "bad"
	ssmClient.mu.Unlock()
	r.refresh(context.Background())

	if r.lastTps != "10" {
		t.Fatalf("非法 tpslimit 不应覆盖 lastTps，got %q", r.lastTps)
	}
	if len(limiter.tps) != 1 {
		t.Fatalf("非法 tpslimit 不应调用 rcd，got %v", limiter.tps)
	}
}
