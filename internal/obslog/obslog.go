// Package obslog 提供分级运维日志，对齐 Python logging_setup 的双流契约：
//   - WARNING+ 写 worker-ops 文件（轮转）→ CW agent 采集 → /migration/{stack}/worker-ops
//   - EMF 不经此包，仍由调用方 println 到 stdout → /migration/{stack}/worker
//
// 与 Python 一致的关键点：失败/poison 打 ERROR（含 rclone 命令+stderr），SUCCESS 不打；
// INFO 仅控制台（journald），不进 ops 文件（高频噪音不灌 CW）。
package obslog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level 日志级别。仅 WARNING/ERROR 落 ops 文件（对齐 Python RotatingFileHandler 的 WARNING+）。
type Level int

const (
	INFO Level = iota
	WARNING
	ERROR
)

func (l Level) String() string {
	switch l {
	case WARNING:
		return "WARNING"
	case ERROR:
		return "ERROR"
	default:
		return "INFO"
	}
}

// logger 全局单例：INFO→stderr(journald)；WARNING+→stderr + ops 文件。
type logger struct {
	mu      sync.Mutex
	console *log.Logger // → stderr，所有级别
	opsFile *rotWriter  // → worker-ops 文件，仅 WARNING+
}

var std = &logger{console: log.New(os.Stderr, "", log.LstdFlags)}

// Setup 打开 ops 日志文件（WARNING+ 落盘供 CW 采集）。path 为空则仅控制台。
// maxBytes/keep 控制轮转（对齐 Python 50MB×5）。
func Setup(path string, maxBytes int64, keep int) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		// 目录建不了：降级仅控制台（不阻断启动，对齐 Python 容错）。
		std.console.Printf("WARNING obslog: 无法建日志目录 %s: %v（降级仅控制台）", filepath.Dir(path), err)
		return nil
	}
	rw, err := newRotWriter(path, maxBytes, keep)
	if err != nil {
		std.console.Printf("WARNING obslog: 无法打开 ops 日志 %s: %v（降级仅控制台）", path, err)
		return nil
	}
	std.mu.Lock()
	std.opsFile = rw
	std.mu.Unlock()
	return nil
}

func emit(level Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	std.console.Printf("%s %s", level, msg)
	if level >= WARNING {
		std.mu.Lock()
		f := std.opsFile
		std.mu.Unlock()
		if f != nil {
			line := fmt.Sprintf("%s %s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000"), level, msg)
			_, _ = f.Write([]byte(line))
		}
	}
}

// Infof 仅控制台（journald），不进 ops 文件/CW。
func Infof(format string, args ...any) { emit(INFO, format, args...) }

// Warnf WARNING：控制台 + ops 文件 → CW。
func Warnf(format string, args ...any) { emit(WARNING, format, args...) }

// Errorf ERROR：控制台 + ops 文件 → CW（失败/poison 用）。
func Errorf(format string, args ...any) { emit(ERROR, format, args...) }

// rotWriter 简单按大小轮转的 writer（worker-ops.log → .1 → .2 ...）。
type rotWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func newRotWriter(path string, maxBytes int64, keep int) (*rotWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	var sz int64
	if st != nil {
		sz = st.Size()
	}
	return &rotWriter{path: path, maxBytes: maxBytes, keep: keep, f: f, size: sz}, nil
}

func (w *rotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件，path→path.1→path.2... 滚动，重开 path。
func (w *rotWriter) rotate() {
	_ = w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if w.keep >= 1 {
		_ = os.Rename(w.path, w.path+".1")
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// 重开失败：退回 stderr，避免丢日志崩溃。
		w.f = nil
		std.console.Printf("ERROR obslog: 轮转后重开 %s 失败: %v", w.path, err)
		return
	}
	w.f = f
	w.size = 0
}

var _ io.Writer = (*rotWriter)(nil)
