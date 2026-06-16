package main

import (
	"bufio"
	"os"
	"sync"
)

// archiveWriter 把被删除的消息体逐行追加写本地 jsonl（删除前的后悔药）。
// 线程安全：多 goroutine 并发 write，用 mutex 串行化（落盘是 I/O，不是热点）。
// append 模式不覆盖历史；缓冲写 + 退出时 flush。
type archiveWriter struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func newArchiveWriter(path string) (*archiveWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &archiveWriter{f: f, w: bufio.NewWriterSize(f, 64*1024)}, nil
}

// write 追加一条消息体（已是 JSON 字符串）+ 换行，形成 jsonl。
func (a *archiveWriter) write(body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.WriteString(body)
	a.w.WriteByte('\n')
}

func (a *archiveWriter) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.Flush()
	a.f.Close()
}
