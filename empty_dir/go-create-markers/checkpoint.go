package main

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// checkpoint 是 row-group 级断点：记录已完成的 RG key，重启时跳过。
// 文本文件每行一个 key（形如 "empty_dir_markers.parquet#rg=12"），追加写。
// 仅当某 RG 全部成功（failed==0 且未被 --limit 截断）才标记完成。
//
// 复制自 gcs-sqs-go/checkpoint.go，去掉了按 manifest URI 派生路径的 sha256 逻辑
// （本工具按 --checkpoint 直接给文件名）。mutex 保护，多 goroutine 安全。
type checkpoint struct {
	path string
	mu   sync.Mutex
	done map[string]struct{}
}

// newCheckpoint 加载断点文件（不存在则从空开始）。path 为空表示禁用断点。
func newCheckpoint(path string) *checkpoint {
	c := &checkpoint{path: path, done: make(map[string]struct{})}
	if path == "" {
		return c
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("读取断点文件失败 %s: %v，从头开始", path, err)
		}
		return c
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			c.done[line] = struct{}{}
		}
	}
	logf("已加载断点文件 %s，已完成 %d 个 row group", path, len(c.done))
	return c
}

// isDone 报告该 RG 是否已完成。
func (c *checkpoint) isDone(rgKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.done[rgKey]
	return ok
}

// count 返回已完成 RG 数。
func (c *checkpoint) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.done)
}

// markDone 标记 RG 完成并追加写入断点文件。path 为空则只记内存。
func (c *checkpoint) markDone(rgKey string) {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.done[rgKey]; ok {
		return
	}
	c.done[rgKey] = struct{}{}
	f, err := os.OpenFile(c.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("写入断点文件失败 %s: %v", c.path, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(rgKey + "\n"); err != nil {
		logf("写入断点文件失败 %s: %v", c.path, err)
	}
}
