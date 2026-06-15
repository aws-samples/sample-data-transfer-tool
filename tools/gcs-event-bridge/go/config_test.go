package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const validCfg = `
region: eu-south-2
pipelines:
  - name: a
    source:
      subscription: projects/proj/subscriptions/sub-a
      sa_secret_arn: arn:aws:secretsmanager:eu-south-2:1:secret:sa-a
    dest:
      queue_url: https://sqs.eu-south-2.amazonaws.com/1/q-a
      dest_bucket: my-bucket
`

func TestLoadValidConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, validCfg))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tn := cfg.Pipelines[0].Tuning
	if defaultSendWorkers != 64 {
		t.Fatalf("默认 send_workers 常量应为 64，got %d", defaultSendWorkers)
	}
	if tn.SendWorkers != 64 {
		t.Errorf("send_workers 默认应为 64，got %d", tn.SendWorkers)
	}
	if tn.NumGoroutines != defaultNumGoroutines || tn.MaxOutstandingMessages != defaultMaxOutstanding {
		t.Errorf("调优默认值未填充: %+v", tn)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		"缺 region":    `pipelines: [{name: a, source: {subscription: s, sa_secret_arn: x}, dest: {queue_url: q, dest_bucket: b}}]`,
		"空 pipelines": `region: eu-south-2`,
		"缺 subscription": `region: r
pipelines: [{name: a, source: {sa_secret_arn: x}, dest: {queue_url: q, dest_bucket: b}}]`,
		"缺 sa_secret_arn": `region: r
pipelines: [{name: a, source: {subscription: s}, dest: {queue_url: q, dest_bucket: b}}]`,
		"缺 dest_bucket": `region: r
pipelines: [{name: a, source: {subscription: s, sa_secret_arn: x}, dest: {queue_url: q}}]`,
	}
	for name, body := range cases {
		if _, err := loadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("%s：应校验失败但通过了", name)
		}
	}
}

func TestDuplicatePipelineName(t *testing.T) {
	dup := `
region: r
pipelines:
  - {name: same, source: {subscription: s1, sa_secret_arn: x1}, dest: {queue_url: q1, dest_bucket: b1}}
  - {name: same, source: {subscription: s2, sa_secret_arn: x2}, dest: {queue_url: q2, dest_bucket: b2}}
`
	if _, err := loadConfig(writeCfg(t, dup)); err == nil {
		t.Error("重复 pipeline name 应报错")
	}
}

func TestProjectFromSubscription(t *testing.T) {
	if got := projectFromSubscription("projects/my-proj/subscriptions/sub"); got != "my-proj" {
		t.Errorf("got %q want my-proj", got)
	}
	if got := subscriptionID("projects/my-proj/subscriptions/sub-x"); got != "sub-x" {
		t.Errorf("got %q want sub-x", got)
	}
}

func TestMultiPipelineConfig(t *testing.T) {
	multi := `
region: eu-south-2
pipelines:
  - {name: a, source: {subscription: projects/p1/subscriptions/sa, sa_secret_arn: arn-a}, dest: {queue_url: qa, dest_bucket: ba}}
  - {name: b, source: {subscription: projects/p2/subscriptions/sb, sa_secret_arn: arn-b}, dest: {queue_url: qb, dest_bucket: bb}, tuning: {send_workers: 128}}
`
	cfg, err := loadConfig(writeCfg(t, multi))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(cfg.Pipelines) != 2 {
		t.Fatalf("应有 2 条 pipeline")
	}
	// 第二条自定义 send_workers=128，第一条走默认 64
	if cfg.Pipelines[0].Tuning.SendWorkers != 64 || cfg.Pipelines[1].Tuning.SendWorkers != 128 {
		t.Errorf("per-pipeline 调优独立: a=%d b=%d want 64/128",
			cfg.Pipelines[0].Tuning.SendWorkers, cfg.Pipelines[1].Tuning.SendWorkers)
	}
}
