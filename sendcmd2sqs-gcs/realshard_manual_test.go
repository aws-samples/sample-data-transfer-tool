package main

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealInventoryShardDirFilter 用真实 GCS Storage Insights 清单 shard 验证目录占位过滤
// （手动验证用，默认跳过）：
//
//	REAL_SHARD=/path/to/shard.parquet go test -run TestRealInventoryShardDirFilter -v
func TestRealInventoryShardDirFilter(t *testing.T) {
	path := os.Getenv("REAL_SHARD")
	if path == "" {
		t.Skip("未设置 REAL_SHARD 环境变量，跳过真实清单验证。" +
			"注意：下方绝对数断言（171650/9884/818432）只对特定基准文件成立——" +
			"166ac753-..._2026-06-05T00:24_114.parquet（桶 my-gcs-dw，已用 pyarrow 独立核验）；" +
			"换其他 shard 跑时绝对数断言会失败（仅一致性断言仍有效）。")
	}

	// 挂一个写临时文件的 dirSkipLogger（大 channel 容量保证零丢弃，验证日志逐条完整）。
	logPath := t.TempDir() + "/skipped-dirs.log"
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("建日志文件失败: %v", err)
	}
	l := &asyncFileLog{
		ch:        make(chan []byte, 1<<16),
		w:         bufio.NewWriterSize(f, 4<<20),
		f:         f,
		localPath: logPath,
		kind:      "目录过滤日志",
		writeLine: writeRawLine,
	}
	l.wg.Add(1)
	go l.loop()
	dirSkipLogger = l
	defer func() { dirSkipLogger = nil }()

	cfg := &Config{
		GCSRemote: "gcs",
		BucketMap: map[string]string{"my-gcs-dw": "my-s3-dw"},
	}

	start := time.Now()
	res, pErr := processShard(
		context.Background(), cfg, nil, path, &ignoreMatcher{}, 4, 4, -1, true, // dry-run
	)
	elapsed := time.Since(start)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	sent, failed, skipped := res.sent, res.failed, res.skipped
	sizeFiltered, dirsFiltered, produced := res.sizeFiltered, res.dirsFiltered, res.produced
	deletedFiltered := res.deletedFiltered

	dirSkipLogger.close() // 排空+flush
	dirSkipLogger = nil

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	logLines := 0
	if len(data) > 0 {
		logLines = strings.Count(string(data), "\n")
	}

	t.Logf("=== 真实清单 %s 处理结果（耗时 %v）===", path, elapsed)
	t.Logf("组装消息(produced) = %d", produced)
	t.Logf("dry-run 发送(sent) = %d", sent)
	t.Logf("已删除对象过滤(deletedFiltered) = %d", deletedFiltered)
	t.Logf("目录占位过滤(dirsFiltered) = %d", dirsFiltered)
	t.Logf("目录过滤日志行数 = %d（丢弃 %d）", logLines, l.dropped.Load())
	t.Logf("skipped=%d sizeFiltered=%d failed=%d", skipped, sizeFiltered, failed)

	// 与 pyarrow 预先核验的基准对照（该文件实测：总 999966 行；timeDeleted 非 null 171650 条；
	// 存活行中结尾/ 9884 条——deleted 过滤在前，已删除的目录占位计入 deleted 而非 dirs）。
	if deletedFiltered != 171650 {
		t.Errorf("deletedFiltered 应为 171650（pyarrow 基准），实际 %d", deletedFiltered)
	}
	if dirsFiltered != 9884 {
		t.Errorf("dirsFiltered 应为 9884（存活的目录占位，pyarrow 基准），实际 %d", dirsFiltered)
	}
	if produced != 818432 {
		t.Errorf("produced 应为 818432（999966-171650-9884），实际 %d", produced)
	}
	if int64(logLines)+l.dropped.Load() != dirsFiltered {
		t.Errorf("日志行数(%d)+丢弃(%d) 应等于 dirsFiltered(%d)", logLines, l.dropped.Load(), dirsFiltered)
	}
	// 抽查日志首行格式应为 bucket/name 且结尾 /
	firstLine, _, _ := strings.Cut(string(data), "\n")
	if !strings.HasPrefix(firstLine, "my-gcs-dw/") || !strings.HasSuffix(firstLine, "/") {
		t.Errorf("日志行格式应为 my-gcs-dw/<name>/，实际首行: %q", firstLine)
	}
}
