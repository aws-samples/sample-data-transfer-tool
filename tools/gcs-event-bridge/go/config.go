package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// 默认调优值（见架构评估：send_workers=64 把单机 QPS 从 ~1万 提到 ~2.8万；
// num_goroutines=32 / max_outstanding=50000 让 Pub/Sub 拉取端不成为瓶颈）。
const (
	defaultNumGoroutines  = 32
	defaultMaxOutstanding = 50000
	defaultSendWorkers    = 64
)

// Config 顶层配置：region + 多条独立 pipeline。
type Config struct {
	Region    string     `yaml:"region"`
	Pipelines []Pipeline `yaml:"pipelines"`
}

// Pipeline 一条独立的 GCS事件源 → SQS目的地 管道（多源即多条，互相故障隔离）。
type Pipeline struct {
	Name   string `yaml:"name"`
	Source Source `yaml:"source"`
	Dest   Dest   `yaml:"dest"`
	Tuning Tuning `yaml:"tuning"`
}

// Source GCP Pub/Sub 订阅 + SA 凭证来源（SA JSON key 存 Secrets Manager，配置只放 ARN）。
type Source struct {
	// 完整订阅名：projects/<project>/subscriptions/<sub>
	Subscription string `yaml:"subscription"`
	// Secrets Manager 中 GCP SA JSON key 的 ARN（绝不把 key 内联进配置）。
	SASecretARN string `yaml:"sa_secret_arn"`
	// 可选：直接指定 GCP project id（缺省从 subscription 名解析）。
	ProjectID string `yaml:"project_id"`
}

// Dest AWS SQS 目的地 + 目标对象映射规则（1 源 → 1 队列固定）。
type Dest struct {
	QueueURL   string `yaml:"queue_url"`
	DestBucket string `yaml:"dest_bucket"` // 映射 destination 的目标 S3 桶
	DestPrefix string `yaml:"dest_prefix"` // 可选目标前缀
}

// Tuning 每条 pipeline 独立调优（缺省走 default*）。
type Tuning struct {
	NumGoroutines          int `yaml:"num_goroutines"`
	MaxOutstandingMessages int `yaml:"max_outstanding_messages"`
	SendWorkers            int `yaml:"send_workers"`
}

// withDefaults 返回填充了默认值的副本（不可变：不改原值）。
func (t Tuning) withDefaults() Tuning {
	if t.NumGoroutines <= 0 {
		t.NumGoroutines = defaultNumGoroutines
	}
	if t.MaxOutstandingMessages <= 0 {
		t.MaxOutstandingMessages = defaultMaxOutstanding
	}
	if t.SendWorkers <= 0 {
		t.SendWorkers = defaultSendWorkers
	}
	return t
}

// loadConfig 从 YAML 文件读配置并校验。
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读配置文件: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// 填充每条 pipeline 的调优默认值。
	for i := range cfg.Pipelines {
		cfg.Pipelines[i].Tuning = cfg.Pipelines[i].Tuning.withDefaults()
	}
	return &cfg, nil
}

// validate 校验配置完整性，fail-fast（启动时就拒绝坏配置，不带病运行）。
func (c *Config) validate() error {
	if c.Region == "" {
		return fmt.Errorf("region 必填")
	}
	if len(c.Pipelines) == 0 {
		return fmt.Errorf("至少配置一条 pipeline")
	}
	seen := map[string]bool{}
	for i, p := range c.Pipelines {
		if p.Name == "" {
			return fmt.Errorf("pipelines[%d]: name 必填", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("pipeline name 重复: %q", p.Name)
		}
		seen[p.Name] = true
		if p.Source.Subscription == "" {
			return fmt.Errorf("pipeline %q: source.subscription 必填", p.Name)
		}
		if p.Source.SASecretARN == "" {
			return fmt.Errorf("pipeline %q: source.sa_secret_arn 必填", p.Name)
		}
		if p.Dest.QueueURL == "" {
			return fmt.Errorf("pipeline %q: dest.queue_url 必填", p.Name)
		}
		if p.Dest.DestBucket == "" {
			return fmt.Errorf("pipeline %q: dest.dest_bucket 必填", p.Name)
		}
	}
	return nil
}
