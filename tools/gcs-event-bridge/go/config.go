package main

import (
	"fmt"
	"os"
	"regexp"

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

// Dest AWS SQS 目的地 + GCS桶→S3桶 映射表（一条 pipeline 的所有源桶共用一个队列）。
type Dest struct {
	QueueURL string `yaml:"queue_url"`
	// 源 GCS 桶名 → 映射规则。多桶共用一个 Pub/Sub 订阅时，按消息的 bucketId 查表。
	BucketMapping map[string]BucketRule `yaml:"bucket_mapping"`
}

// BucketRule 单个 GCS 桶的映射规则，两种写法二选一：
//   - 写法 A（整桶映射）：填 s3_bucket（+ 可选 prefix）。
//   - 写法 B（按 key 前缀路由）：填 prefix_routes（+ 可选 default_s3_bucket 兜底）。
type BucketRule struct {
	// 写法 A：整桶映射到这个 S3 桶。
	S3Bucket string `yaml:"s3_bucket"`
	// 写法 A 可选：统一加在目标 key 前的前缀。
	Prefix string `yaml:"prefix"`
	// 写法 B：按对象 key 的前缀头路由到不同 S3 桶（最长前缀优先）。
	PrefixRoutes []PrefixRoute `yaml:"prefix_routes"`
	// 写法 B 兜底：所有 prefix_routes 都不命中时用的 S3 桶（空=当未知桶跳过）。
	DefaultS3Bucket string `yaml:"default_s3_bucket"`
}

// PrefixRoute 写法 B 的一条路由：对象 key 命中 → 投到 S3Bucket。
//   - Prefix（字面前缀，HasPrefix）：key 以 Prefix 开头即命中。
//   - Regex（正则）：Prefix 留空时改用正则匹配 key（如 ^[^/]+$ 根目录文件、
//     ^[0-9]{8}/ 日期目录）。Prefix 与 Regex 二选一，不能同配。
// 匹配顺序见 resolveDest：先字面前缀（最长优先），再 regex（按配置顺序）。
// StripPrefix=true 仅对字面 Prefix 生效：剥掉匹配的 Prefix 段（如 mallfile-gcp 的
// gcsprodorms/a/b → s3euprodorms/a/b）；默认 false 保留完整 key（层级不变）。
type PrefixRoute struct {
	Prefix      string `yaml:"prefix"`
	Regex       string `yaml:"regex"`
	S3Bucket    string `yaml:"s3_bucket"`
	StripPrefix bool   `yaml:"strip_prefix"`

	re *regexp.Regexp // Regex 编译结果（loadConfig 内填充，不入 YAML）
}

// isPrefixRouted 该规则是否走"按 key 前缀路由"（写法 B）。
func (r BucketRule) isPrefixRouted() bool {
	return len(r.PrefixRoutes) > 0
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
	// 填充每条 pipeline 的调优默认值 + 编译 regex 路由（坏正则 fail-fast）。
	for i := range cfg.Pipelines {
		cfg.Pipelines[i].Tuning = cfg.Pipelines[i].Tuning.withDefaults()
		for _, rule := range cfg.Pipelines[i].Dest.BucketMapping {
			for j := range rule.PrefixRoutes {
				pr := &rule.PrefixRoutes[j]
				if pr.Regex == "" {
					continue
				}
				re, err := regexp.Compile(pr.Regex)
				if err != nil {
					return nil, fmt.Errorf("pipeline %q prefix_routes regex %q 编译失败: %w",
						cfg.Pipelines[i].Name, pr.Regex, err)
				}
				pr.re = re
			}
		}
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
		if len(p.Dest.BucketMapping) == 0 {
			return fmt.Errorf("pipeline %q: dest.bucket_mapping 至少配一个源桶", p.Name)
		}
		for gcsBucket, rule := range p.Dest.BucketMapping {
			if err := validateRule(p.Name, gcsBucket, rule); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateRule 校验单条桶映射规则：写法 A 和 B 互斥、各自必填项齐全。
func validateRule(pipeline, gcsBucket string, r BucketRule) error {
	hasA := r.S3Bucket != ""
	hasB := r.isPrefixRouted()
	switch {
	case hasA && hasB:
		return fmt.Errorf("pipeline %q 桶 %q: s3_bucket 与 prefix_routes 不能同时配（写法 A/B 二选一）",
			pipeline, gcsBucket)
	case !hasA && !hasB:
		return fmt.Errorf("pipeline %q 桶 %q: 必须配 s3_bucket（整桶映射）或 prefix_routes（前缀路由）之一",
			pipeline, gcsBucket)
	case hasB:
		for i, pr := range r.PrefixRoutes {
			hasPrefix := pr.Prefix != ""
			hasRegex := pr.Regex != ""
			if hasPrefix && hasRegex {
				return fmt.Errorf("pipeline %q 桶 %q prefix_routes[%d]: prefix 与 regex 不能同配（二选一）", pipeline, gcsBucket, i)
			}
			if !hasPrefix && !hasRegex {
				return fmt.Errorf("pipeline %q 桶 %q prefix_routes[%d]: 必须配 prefix 或 regex", pipeline, gcsBucket, i)
			}
			// strip_prefix 只对字面 prefix 有意义；配在 regex 路由上会被静默忽略 → fail-fast 防误配。
			if hasRegex && pr.StripPrefix {
				return fmt.Errorf("pipeline %q 桶 %q prefix_routes[%d]: strip_prefix 不能用于 regex 路由（regex 保留完整 key）", pipeline, gcsBucket, i)
			}
			if pr.S3Bucket == "" {
				return fmt.Errorf("pipeline %q 桶 %q prefix_routes[%d]: s3_bucket 必填", pipeline, gcsBucket, i)
			}
		}
	}
	return nil
}
