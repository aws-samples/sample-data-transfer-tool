// Package app 组装运行时配置与 worker 主循环。
package app

import (
	"fmt"
	"os"
	"strconv"
)

// Config 运行时配置，从环境变量解析（对齐 Python config.Settings 注入约定）。
type Config struct {
	Region          string
	QueueURL        string
	StatusTable     string
	HeartbeatTable  string
	InstanceID      string
	RCDAddr         string
	RCDUser         string
	RCDPass         string
	BwlimitParam    string
	TpslimitParam   string
	Workers         int
	Receivers       int
	RcloneTransfers int
	RcloneTimeout   int    // 秒，HTTP 传输 deadline
	VisibilityTO    int    // 秒，SQS VisibilityTimeout
	OpsLogPath      string // worker-ops 分级日志路径（WARNING+ 落盘供 CW 采集）
}

// timeoutVisibilitySafeRatio rclone timeout ≤ 0.7×visibility（防双写，对齐 Python）。
const timeoutVisibilityNumerator = 7

// FromEnv 解析环境变量，校验 timeout 不变量，缺必填项 fail-fast。
func FromEnv(getenv func(string) string) (Config, error) {
	req := func(k string) (string, error) {
		v := getenv(k)
		if v == "" {
			return "", fmt.Errorf("必填环境变量缺失: %s", k)
		}
		return v, nil
	}
	region, err := req("AWS_REGION")
	if err != nil {
		return Config{}, err
	}
	queueURL, err := req("QUEUE_URL")
	if err != nil {
		return Config{}, err
	}

	positiveIntOr := func(k string, def int) (int, error) {
		if v := getenv(k); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n <= 0 {
				return 0, fmt.Errorf("%s 必须为正整数，当前=%q", k, v)
			}
			return n, nil
		}
		return def, nil
	}
	optionalPositiveInt := func(k string) (int, error) {
		if v := getenv(k); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n <= 0 {
				return 0, fmt.Errorf("%s 必须为正整数，当前=%q", k, v)
			}
			return n, nil
		}
		return 0, nil
	}

	workers, err := positiveIntOr("WORKER_GOROUTINES", 16)
	if err != nil {
		return Config{}, err
	}
	receivers, err := positiveIntOr("RECEIVER_GOROUTINES", 2)
	if err != nil {
		return Config{}, err
	}
	rcloneTimeout, err := positiveIntOr("RCLONE_TIMEOUT_SECONDS", 30240)
	if err != nil {
		return Config{}, err
	}
	visibilityTO, err := positiveIntOr("QUEUE_VISIBILITY_TIMEOUT", 43200)
	if err != nil {
		return Config{}, err
	}
	rcloneTransfers, err := optionalPositiveInt("RCLONE_TRANSFERS")
	if err != nil {
		return Config{}, err
	}

	c := Config{
		Region:          region,
		QueueURL:        queueURL,
		StatusTable:     orDefault(getenv("DYNAMODB_TABLE"), "transfer-message-status-"+region),
		HeartbeatTable:  orDefault(getenv("HEARTBEAT_TABLE"), "worker-heartbeat-"+region),
		InstanceID:      getenv("WORKER_INSTANCE_ID"), // 空则运行时探 IMDS
		RCDAddr:         orDefault(getenv("RCD_ADDR"), "127.0.0.1:5572"),
		RCDUser:         getenv("RCD_USER"),
		RCDPass:         getenv("RCD_PASS"),
		BwlimitParam:    orDefault(getenv("RATELIMIT_BWLIMIT_PARAM"), "/migration/ratelimit/bwlimit"),
		TpslimitParam:   orDefault(getenv("RATELIMIT_TPSLIMIT_PARAM"), "/migration/ratelimit/tpslimit"),
		Workers:         workers,
		Receivers:       receivers,
		RcloneTransfers: rcloneTransfers,
		RcloneTimeout:   rcloneTimeout, // 0.7×43200
		VisibilityTO:    visibilityTO,
		OpsLogPath:      orDefault(getenv("OPS_LOG_PATH"), "/var/log/migration/worker-ops-0.log"),
	}

	if err := c.validateTimeoutInvariant(); err != nil {
		return Config{}, err
	}
	if err := c.validateConcurrencyInvariant(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validateTimeoutInvariant 校验 rclone timeout ≤ 0.7×visibility（防双写，整数运算避免浮点误判）。
func (c Config) validateTimeoutInvariant() error {
	if c.VisibilityTO <= 0 {
		return fmt.Errorf("QUEUE_VISIBILITY_TIMEOUT 必须为正数，当前=%d", c.VisibilityTO)
	}
	safeCeiling := c.VisibilityTO * timeoutVisibilityNumerator / 10
	if c.RcloneTimeout > safeCeiling {
		return fmt.Errorf(
			"RCLONE_TIMEOUT_SECONDS(%d) 相对 VisibilityTimeout(%d) 无安全裕量，"+
				"要求 ≤ 0.7×=%d，否则传输超时与 SQS 重投竞态导致双写",
			c.RcloneTimeout, c.VisibilityTO, safeCeiling)
	}
	return nil
}

// validateConcurrencyInvariant 校验提交并发与 rcd 执行并发对齐。
func (c Config) validateConcurrencyInvariant() error {
	if c.RcloneTransfers > 0 && c.RcloneTransfers != c.Workers {
		return fmt.Errorf(
			"RCLONE_TRANSFERS(%d) 必须等于 WORKER_GOROUTINES(%d)，否则 rcd 内排队会破坏超时/visibility 语义",
			c.RcloneTransfers, c.Workers)
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Getenv os.Getenv 包装（main 用）。
func Getenv(k string) string { return os.Getenv(k) }
