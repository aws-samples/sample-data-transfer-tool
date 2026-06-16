package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
)

// checkpoint 是 shard 级断点：记录已完成的 shard URI，重启时跳过。
// 文本文件每行一个 URI，追加写。仅当某 shard 全部成功（failed==0）才标记完成。
type checkpoint struct {
	path string
	mu   sync.Mutex
	done map[string]struct{}
}

// defaultCheckpointPath 根据 manifest URI 派生一个稳定的默认断点文件路径（放工作目录）。
// 同一 manifest 重跑得到同一文件名（自动续传）；不同 manifest 互不干扰。
// 未显式传 --checkpoint 时使用，使长跑默认具备断点续传能力。
func defaultCheckpointPath(manifestURI string) string {
	sum := sha256.Sum256([]byte(manifestURI))
	return ".gcs-sqs-ckpt-" + hex.EncodeToString(sum[:6]) + ".txt"
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
	logf("已加载断点文件 %s，已完成 %d 个 shard", path, len(c.done))
	return c
}

// isDone 报告 shard 是否已完成。
func (c *checkpoint) isDone(shardURI string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.done[shardURI]
	return ok
}

// markDone 标记 shard 完成并追加写入断点文件。path 为空则只记内存。
func (c *checkpoint) markDone(shardURI string) {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.done[shardURI]; ok {
		return
	}
	c.done[shardURI] = struct{}{}
	f, err := os.OpenFile(c.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("写入断点文件失败 %s: %v", c.path, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(shardURI + "\n"); err != nil {
		logf("写入断点文件失败 %s: %v", c.path, err)
	}
}
