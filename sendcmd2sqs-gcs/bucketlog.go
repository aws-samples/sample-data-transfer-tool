package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// logDir 是所有日志（桶日志/审计日志/sent 日志）的本地输出目录。
// 默认运行目录下的 logs/；可由 --log-dir 覆盖（如指向大容量数据盘），main 在所有日志器创建前设置。
var logDir = "logs"

// teeWriter 是 stdLogger 的固定输出目标：始终写 base（stderr），且当 extra 非 nil 时
// 同时写 extra（当前桶的日志文件）。桶处理期间用 setExtra 挂上、setExtra(nil) 摘下。
//
// 并发：log.Logger.Output 在其内部锁中串行调用 Write；set/clear 由主 goroutine 在 log 锁
// 之外调用。Write 全程持 mu，与 setExtra 互斥 —— 故 setExtra(nil) 返回后保证无 in-flight
// Write 仍持有刚摘下的文件，Close 立即安全。日志频率低（桶/shard/重试级），锁内写一行可忽略。
type teeWriter struct {
	mu    sync.Mutex
	base  io.Writer // 恒为 os.Stderr，永不为 nil
	extra *os.File  // 当前桶日志文件；nil 表示只写 stderr
}

func (w *teeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.base.Write(p)
	if w.extra != nil {
		_, _ = w.extra.Write(p) // 文件写失败静默（桶日志是降级特性，不影响主流程）
	}
	return n, err
}

// setExtra 挂上桶日志文件；传 nil 表示摘下（之后只写 stderr）。
func (w *teeWriter) setExtra(f *os.File) {
	w.mu.Lock()
	w.extra = f
	w.mu.Unlock()
}

// bucketLogTee 是全局日志的可切换输出目标；stdLogger 固定写它。
var bucketLogTee = &teeWriter{base: os.Stderr}

// stdLogger 写到 bucketLogTee（始终 stderr，挂了桶文件时同时落文件）。带时间戳。
// 无 per-message 日志（那正是 Python 版的性能 bug）。
var stdLogger = log.New(bucketLogTee, "", 0)

// bucketLogger 表示一个桶的日志会话：本地文件 + 路径（上传 S3 时对 localPath 取 base 作 key）。
type bucketLogger struct {
	file      *os.File
	localPath string // logs/{name}-{ts}.log
}

// createLogFile 在 logDir 下创建一个 "{prefix}-{时间戳}{ext}" 文件（O_APPEND|O_CREATE|O_WRONLY）。
// 返回文件句柄与完整本地路径（S3 key 由调用方对 localPath 取 filepath.Base 派生，无需单独返回文件名）。
// 由 startBucketLog（桶日志，ext=".log"）与异步审计日志（ext=".log.gz"，见 sentlog.go）共用。
func createLogFile(prefix, ext string) (f *os.File, localPath string, err error) {
	if mkErr := os.MkdirAll(logDir, 0o755); mkErr != nil {
		return nil, "", fmt.Errorf("创建日志目录失败 %s: %w", logDir, mkErr)
	}
	fileName := prefix + "-" + time.Now().Format("20060102_150405") + ext
	localPath = filepath.Join(logDir, fileName)
	f, err = os.OpenFile(localPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("创建日志文件失败 %s: %w", localPath, err)
	}
	return f, localPath, nil
}

// startBucketLog 为源桶 srcBucket 创建本地日志文件并挂到全局 tee。
// 时间戳在此刻生成。失败返回 error，调用方据此降级为仅 stderr。
// 桶日志是人读的进度日志（且会上传 S3 供直接查看），保持明文 .log 不压缩。
func startBucketLog(srcBucket string) (*bucketLogger, error) {
	f, localPath, err := createLogFile(sanitizeLogName(srcBucket), ".log")
	if err != nil {
		return nil, err
	}
	bucketLogTee.setExtra(f)
	return &bucketLogger{file: f, localPath: localPath}, nil
}

// stop 摘下 tee 并关闭本地文件（此后 logf 只进 stderr）。
func (b *bucketLogger) stop() {
	if b == nil {
		return
	}
	bucketLogTee.setExtra(nil)
	if err := b.file.Close(); err != nil {
		logf("关闭桶日志文件失败 %s: %v", b.localPath, err)
	}
}

// sanitizeLogName 把源桶名里对文件名/路径不安全的字符替换为 '_'，并去前导点防路径穿越。
// GCS 桶名通常已安全（小写字母数字 -_.），但桶名来自用户 CSV，做防御。
func sanitizeLogName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".") // 去前导点，避免 "."/".." 或隐藏文件
	if out == "" {
		out = "_"
	}
	return out
}

// parseS3Prefix 解析 s3://bucket[/prefix] 为 (bucket, prefix)。
// 与 parseObjectURI 不同：prefix 允许为空（"s3://b" 或 "s3://b/" → prefix=""）；末尾斜杠去除。
func parseS3Prefix(uri string) (bucket, prefix string, err error) {
	const scheme = "s3://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", fmt.Errorf("LOG_S3_URI 不是合法 S3 URI（应以 s3:// 开头）: %s", uri)
	}
	rest := strings.TrimPrefix(uri, scheme)
	bucket, prefix, _ = strings.Cut(rest, "/") // 无 "/" 时 prefix=""
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(prefix, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("LOG_S3_URI 缺少 bucket: %s", uri)
	}
	return bucket, prefix, nil
}

// uploadLogFile 把单个本地日志文件上传到 {LogS3Bucket}/{LogS3Prefix}/{文件名}。
// 用独立 30s 超时 context（不继承主 ctx 的取消），使 Ctrl-C 中断时仍能完成上传。
// 失败仅告警、不中断（保留本地文件）。桶日志与审计/发送日志共用。
func uploadLogFile(store *s3InventoryStore, cfg *Config, localPath string) {
	if localPath == "" {
		return
	}
	key := filepath.Base(localPath)
	if cfg.LogS3Prefix != "" {
		key = cfg.LogS3Prefix + "/" + key
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.uploadFile(ctx, localPath, cfg.LogS3Bucket, key); err != nil {
		logf("上传日志到 S3 失败（已保留本地 %s）: %v", localPath, err)
	} else {
		logf("日志已上传: s3://%s/%s", cfg.LogS3Bucket, key)
	}
}

// uploadBucketLog 把桶日志上传到 S3（复用 uploadLogFile，文件名取 localPath 的 base）。
func uploadBucketLog(store *s3InventoryStore, cfg *Config, b *bucketLogger) {
	if b == nil {
		return
	}
	uploadLogFile(store, cfg, b.localPath)
}

// uploadAuditLogs 是任务收尾的审计/发送日志上传：关闭全部日志器拿到本地路径，逐个上传到 S3。
// 调用方应已判定 cfg.LogS3Bucket != "" && !dryRun。自建一个临时上传 store（newInventoryStore
// 仅是 SDK client 构造、close 为 no-op，开销可忽略），故全量/增量两模式调用方式一致——
// 不必依赖数据面 store 的存在与 defer 关闭顺序。失败仅告警、不中断。
func uploadAuditLogs(ctx context.Context, cfg *Config) {
	paths := closeAuditLogs() // 先收尾拿路径（文件已 flush+gzip 收尾），再上传
	store, err := newInventoryStore(ctx, cfg.InventoryS3Region)
	if err != nil {
		logf("上传审计日志：创建 S3 客户端失败，日志仅留本地: %v", err)
		return
	}
	defer func() {
		if cErr := store.close(); cErr != nil {
			logf("关闭上传 store 失败: %v", cErr)
		}
	}()
	for _, p := range paths {
		uploadLogFile(store, cfg, p)
	}
}
