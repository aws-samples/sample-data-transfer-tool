package classify

import "testing"

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
		want     string
	}{
		{"success", 0, "anything", ""},
		{"arg_error", 2, "whatever", "arg_error"},
		{"rate_limit_429", 1, "https response error StatusCode: 429, api error TooManyRequests", "src_rate_limit"},
		{"acl_403", 1, "operation error S3: GetObject 403 Forbidden", "src_acl_deny"},
		{"not_found_404", 1, "object 404 notFound", "src_not_found"},
		// 回归：rcd 源不存在返回 "object not found"（带空格），曾误归 uncategorized
		{"rcd_object_not_found", 1, "object not found", "src_not_found"},
		{"src_5xx", 1, "GetObject 503 from source", "src_5xx"},
		{"dst_5xx", 1, "PutObject upload 500 to s3 bucket", "dst_5xx"},
		// 2026-07-22 GCS h2 僵死连接事故的真实错误串——曾落 uncategorized(退避0→重投风暴)。
		{"net_h2_stuck", 1, `operation error S3: HeadObject, https response error StatusCode: 0, request send failed, Head "https://x.storage.googleapis.com/a.gz": http2: timeout awaiting response headers`, "net_transient"},
		{"net_conn_reset", 1, "read: connection reset by peer", "net_transient"},
		// 同族连接死亡 signature——broken pipe/unexpected EOF 不含 "timeout"，漏则误判 FATAL 进 DLQ。
		{"net_broken_pipe", 1, "write tcp 10.0.0.1:443: broken pipe", "net_transient"},
		{"net_unexpected_eof", 1, "read tcp: unexpected EOF", "net_transient"},
		{"net_ctx_deadline", 1, "context deadline exceeded", "net_transient"},
		// 根因优先级:同时含 429 与网络文本时,429 是根因(排在网络类之前)。
		{"rate_limit_over_net", 1, "StatusCode: 429 TooManyRequests; connection reset", "src_rate_limit"},
		{"integrity", 1, "corrupted on transfer: sizes differ", "integrity_hash"},
		{"real_oom", 1, "fatal error: out of memory", "worker_oom"},
		{"sigkill", 1, "process killed by signal 9", "worker_oom"},
		{"uncategorized", 1, "some weird unmatched error", "uncategorized"},
	}
	for _, c := range cases {
		if got := ClassifyError(c.exitCode, c.stderr); got != c.want {
			t.Errorf("%s: ClassifyError(%d, %q) = %q, want %q", c.name, c.exitCode, c.stderr, got, c.want)
		}
	}
}

// 回归测试：worker_oom 误判修复。
// 这是 owner 提供的真实 DDB error_message 的关键尾部——一次 GCS 429 重传放大、
// 最后被 SIGTERM 优雅停机中断的大文件传输。Python 旧逻辑因 `\bsignal\b` 高优先级
// 把它误判成 worker_oom（retry_delay=0 立即重投，掩盖 429 根因）。
// 修复后：① 不再判 worker_oom；② 因含 429 → 判 src_rate_limit（300s 退避，给源端恢复）。
func TestWorkerOOMMisjudgmentFixed(t *testing.T) {
	realStderr := `{"level":"error","msg":"Failed to copy: multi-thread copy: failed to open source: ` +
		`operation error S3: GetObject, https response error StatusCode: 429, api error TooManyRequests: Too Many Requests"}
{"level":"info","msg":"Signal received: terminated","source":"atexit/atexit.go:51"}
{"level":"info","msg":"Exiting...","source":"atexit/atexit.go:53"}`

	got := ClassifyError(1, realStderr)
	if got == "worker_oom" {
		t.Fatalf("回归失败：优雅 SIGTERM 仍被误判为 worker_oom（Python 旧 bug）")
	}
	if got != "src_rate_limit" {
		t.Errorf("含 429 的真实失败应判 src_rate_limit（根因），got %q", got)
	}
	// 且退避必须是 300s（给 GCS 源恢复窗口），而非误判后的 0s。
	if d := RetryDelaySeconds(got); d != 300 {
		t.Errorf("src_rate_limit 退避应 300s，got %ds", d)
	}
}

// 优雅停机但无 429（纯 SIGTERM 中断）：不应误判 worker_oom，应落 uncategorized。
func TestGracefulSigtermNotOOM(t *testing.T) {
	stderr := `{"level":"info","msg":"Signal received: terminated"}
{"level":"info","msg":"Exiting..."}`
	if got := ClassifyError(1, stderr); got == "worker_oom" {
		t.Errorf("纯优雅 SIGTERM 不应判 worker_oom，got %q", got)
	}
}

func TestRetryDelaySeconds(t *testing.T) {
	cases := map[string]int{
		"src_rate_limit":  300,
		"src_5xx":         60,
		"dst_5xx":         60,
		"net_transient":   30,
		"worker_shutdown": 60,
		"rcd_stats_reset": 60,
		"src_not_found":   0,
		"worker_oom":      0,
		"":                0,
		"uncategorized":   0,
	}
	for cls, want := range cases {
		if got := RetryDelaySeconds(cls); got != want {
			t.Errorf("RetryDelaySeconds(%q) = %d, want %d", cls, got, want)
		}
	}
}

// RETRYABLE 态退避不变量：绝不 0 秒重投。0s 会把失败中的消息秒级灌回挣扎中的后端
// （7-22 h2 事故重投风暴、rcd_stats_reset 烧 DLQ 同形状）。表内类用表值，未命中给 30s 下限。
func TestRetryableDelayNeverZero(t *testing.T) {
	cases := map[string]int{
		"src_rate_limit":  300, // 表值优先
		"rcd_stats_reset": 60,
		"uncategorized":   30, // 无表项 → 30s 下限
		"src_acl_deny":    30, // 403 quota 被归 acl 时（GCS 403 配额文案）仍有退避
		"":                30,
	}
	for cls, want := range cases {
		if got := RetryableDelaySeconds(cls); got != want {
			t.Errorf("RetryableDelaySeconds(%q) = %d, want %d", cls, got, want)
		}
	}
}

// 词表对称性回归：判态正则（transientError）认得的限流/配额文案，分类正则也必须给出
// 带退避的 class——否则 RETRYABLE + 0s 组合复活。两条实证串来自对抗验证的可触发反例。
func TestRateLimitQuotaClassifiedWithBackoff(t *testing.T) {
	cases := []struct {
		stderr string
		want   string
	}{
		// GCS 真实限流文案带空格，旧表只写了无空格 "ratelimit" 曾漏网
		{"rate limit exceeded for bucket gs://x", "src_rate_limit"},
		// GCS 日配额真实形态：403 伴随 Quota exceeded，旧表先命中 403 归 acl_deny(0s)
		{"googleapi: Error 403: Quota exceeded for quota metric 'egress'", "src_rate_limit"},
		{"user rate limit exceeded", "src_rate_limit"},
	}
	for _, c := range cases {
		got := ClassifyError(1, c.stderr)
		if got != c.want {
			t.Errorf("ClassifyError(%q) = %q, want %q", c.stderr, got, c.want)
		}
		if d := RetryableDelaySeconds(got); d < 30 {
			t.Errorf("%q 的退避 %ds < 30s，RETRYABLE 零退避复活", c.stderr, d)
		}
	}
}

func TestTransientAndTerminalHelpers(t *testing.T) {
	if !IsTransientError("StatusCode: 429 too many requests") {
		t.Error("429 应判瞬时")
	}
	if !IsTransientError("connection reset by peer") {
		t.Error("connection reset 应判瞬时")
	}
	if IsTransientError("403 forbidden permanent") {
		t.Error("403 不应判瞬时")
	}
	if !IsSourceMissing("Source doesn't exist or is a directory") {
		t.Error("source missing 应识别")
	}
	if !IsDeleteNoop("404 not found") {
		t.Error("delete noop 404 应识别")
	}
}
