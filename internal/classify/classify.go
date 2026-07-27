// Package classify 把 rclone 结果/错误文本映射为四态 + 低基数 error_class。
// 移植自 Python src/migration/error_classifier.py，并修正其 worker_oom 误判。
package classify

import "regexp"

// error_class 模式表（按顺序匹配，先具体后宽泛）。IGNORECASE。
//
// 🔧 与 Python 的关键差异（修正 worker_oom 误判，见下）：
// Python 把 `\bsignal\b` 放在第 0 位最高优先级，导致 rclone 优雅停机日志
// "Signal received: terminated"（SIGTERM drain，根本不是 OOM）被误判成 worker_oom，
// 进而 retry_delay=0 立即重投、把真正的 429 根因掩盖、退避失效放大限流风暴。
// 这里：① worker_oom 正则收窄为真正的 OOM/SIGKILL（排除优雅 "signal received"）；
// ② 把 429/5xx 等"实际传输错误"排在信号之前——同时含 429 和 SIGTERM 时，根因是 429。
var errorPatterns = []struct {
	re    *regexp.Regexp
	label string // 空串表示用 5xx resolver 再分流
}{
	// 限流 429/配额 优先（真实传输错误，根因价值最高）。
	// rate\s*limit 带空格变体 + quota exceeded：GCS 日配额真实文案是 "403 ... Quota exceeded"，
	// 必须排在 403/acl 之前——配额是限流问题（要 300s 退避等窗口恢复），不是权限问题（0s 烧 DLQ）。
	// 词表须与下方 transientError 保持对称：判态认得的限流文案，这里也要给出带退避的 class，
	// 否则落 uncategorized/acl_deny → RETRYABLE + 0s 秒级回灌限流后端（对抗审查实证反例）。
	{regexp.MustCompile(`(?i)\b429\b|too many requests|rate\s*limit|quota.{0,40}exceeded|exceeded.{0,40}quota|egress bandwidth`), "src_rate_limit"},
	// 完整性校验
	{regexp.MustCompile(`(?i)hash (mismatch|differ)|corrupted on transfer`), "integrity_hash"},
	// 403 / 权限
	{regexp.MustCompile(`(?i)\b403\b|permissiondenied|forbidden`), "src_acl_deny"},
	// 404 / 不存在
	// not\s*found 同时匹配 "notFound"(GCS XML) 和 "object not found"(rcd 实测返回，
	// 部署验证 2026-06-16：源不存在曾被误归 uncategorized，因 "notfound" 不匹配带空格的 "not found")。
	{regexp.MustCompile(`(?i)\b404\b|not\s*found|does not exist|doesn'?t exist`), "src_not_found"},
	// 5xx（resolver 再按源/目标分流）
	{regexp.MustCompile(`(?i)\b5\d\d\b`), ""},
	// 网络/连接层瞬时失败（http2 僵死连接、连接重置、请求发送失败、DNS/TLS 握手超时）。
	// 独立成类而非落 uncategorized：uncategorized 退避=0 → 零延迟立即重投，把失败中的消息
	// 反复灌回同一批坏连接，打爆 AWS SDK retry token（2026-07-22 GCS h2 僵死连接事故实证：
	// RETRYABLE 风暴 + "retry quota exceeded"）。排在 429/5xx 之后：带 5xx/429 的错误按其根因归类。
	{regexp.MustCompile(`(?i)awaiting response headers|request send failed|connection reset|connection refused|broken pipe|unexpected eof|no such host|network is unreachable|i/o timeout|tls handshake timeout|deadline exceeded|dial tcp`), "net_transient"},
	// 真正的 OOM / 被强杀（SIGKILL/信号9/out of memory）——排除优雅 "signal received: terminated"
	{regexp.MustCompile(`(?i)\bout of memory\b|\boom-?kill`), "worker_oom"},
	{regexp.MustCompile(`(?i)\bsigkill\b|\bkilled\b|signal 9\b`), "worker_oom"},
}

// dstMarker 标记目标侧（S3/上传）关键词；缺省视为源侧（GCS/下载）。
var dstMarker = regexp.MustCompile(`(?i)\b(s3|putobject|upload|bucket|slowdown|dest)\b`)

// transientError 瞬时（可重试）错误：429/配额/5xx/网络中断/timeout。
var transientError = regexp.MustCompile(
	`(?i)\b429\b|too many requests|rate\s*limit|ratelimit|` +
		`exceeded.{0,40}quota|quota.{0,40}exceeded|egress bandwidth|slow\s*down|` +
		`\b50[0234]\b|service unavailable|` +
		`connection reset|connection refused|timeout|temporarily`)

// sourceMissing 源对象不存在（确定性终态，重试无用）。要求 "source" 紧邻 "doesn't exist"。
var sourceMissing = regexp.MustCompile(`(?i)source\s+(doesn'?t|does\s+not)\s+exist`)

// deleteNoop delete 操作目标已不存在（幂等成功）。
var deleteNoop = regexp.MustCompile(`(?i)\b404\b|not\s*found|does not exist|no such`)

// retryDelayByClass 失败快速重投的分级退避（秒）。
// 限流给源端恢复窗口；5xx 短退避；其余 0 秒立即重投快速烧进 DLQ。
var retryDelayByClass = map[string]int{
	"src_rate_limit": 300,
	"src_5xx":        60,
	"dst_5xx":        60,
	"net_transient":  30,
	// worker_shutdown 必须带退避：watchdog 杀 worker 时在途消息若 0 秒重投，重启后的
	// worker 立刻重新咬住同一批消息——若卡死源在 rcd 侧未清除，就是"重启-再卡死"死循环
	// （2026-07-23 缓冲池死锁事故实证）。60s 给 rcd 侧自愈（连带重启/池释放）留窗口；
	// 代价仅是正常滚动重启的在途消息多等 1 分钟。
	"worker_shutdown": 60,
	// rcd_stats_reset：stats-reset 前置失败（copy 从未执行）多因 rcd 短暂不可达/僵死。
	// 0s 重投会 5s/次烧 receive count，watchdog 判死重启 rcd 前（最坏 ~180s）就把从未
	// 传输的好消息烧进 DLQ（maxReceiveCount=3 × ~15s）。60s 对齐 worker_shutdown，
	// 熬过 rcd 自愈窗口（30s 不够：30×3=90s 仍可能落在判死窗口内）。
	"rcd_stats_reset": 60,
}

// retryableFloorSeconds RETRYABLE 态的退避下限。RETRYABLE 语义=“带退避重试”，
// 0s 重投是它的语义反面：把失败中的消息秒级灌回挣扎中的后端（限流风暴放大/烧 DLQ）。
// 判态词表（transientError 宽正则）与分类词表（errorPatterns）难以永久对称，
// 词表缝隙里漏出的 RETRYABLE 类由此下限兜底，杜绝 RETRYABLE+0s 组合复活。
const retryableFloorSeconds = 30

// RetryDelaySeconds 按 error_class 返回重投 VisibilityTimeout 秒数（无表项 → 0，
// 供 FATAL/UNKNOWN 等“快速烧 DLQ / 立即重投”路径使用）。
func RetryDelaySeconds(errorClass string) int {
	if d, ok := retryDelayByClass[errorClass]; ok {
		return d
	}
	return 0
}

// RetryableDelaySeconds RETRYABLE 态专用：表值优先，无表项给 retryableFloorSeconds
// 下限——RETRYABLE 绝不 0 秒重投（不变量）。
func RetryableDelaySeconds(errorClass string) int {
	if d, ok := retryDelayByClass[errorClass]; ok {
		return d
	}
	return retryableFloorSeconds
}

// ClassifyError 把错误文本映射为低基数 error_class（空串=无错误/未命中）。
// exitCode==0 → 无错误；否则按模式表顺序匹配，5xx 再按源/目标分流。
func ClassifyError(exitCode int, stderr string) string {
	if exitCode == 0 {
		return ""
	}
	if exitCode == 2 {
		return "arg_error"
	}
	for _, p := range errorPatterns {
		if p.re.MatchString(stderr) {
			if p.label == "" {
				return classify5xx(stderr)
			}
			return p.label
		}
	}
	return "uncategorized"
}

func classify5xx(stderr string) string {
	if dstMarker.MatchString(stderr) {
		return "dst_5xx"
	}
	return "src_5xx"
}

// IsTransientError 是否瞬时可重试错误（供 UNKNOWN → RETRYABLE 升级）。
func IsTransientError(stderr string) bool { return transientError.MatchString(stderr) }

// IsSourceMissing 是否"源对象不存在"（确定性终态，UNKNOWN → FATAL 升级）。
func IsSourceMissing(stderr string) bool { return sourceMissing.MatchString(stderr) }

// IsDeleteNoop delete 路径目标已不存在（幂等成功）。
func IsDeleteNoop(stderr string) bool { return deleteNoop.MatchString(stderr) }
