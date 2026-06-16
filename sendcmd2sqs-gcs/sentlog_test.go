package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestExtractTSV 校验从真实消息体（buildCopyMessage/buildDeleteMessage 产出）提取 op/source/destination。
// 用真实构造函数产消息而非手写 JSON，确保提取逻辑与实际消息格式吻合。
func TestExtractTSV(t *testing.T) {
	// copy 消息
	cp, err := buildCopyMessage("gcs", "my-s3-dw", "my-gcs-dw", "libs/dt=20260531/part-001.c000")
	if err != nil {
		t.Fatalf("buildCopyMessage 报错: %v", err)
	}
	op, src, dst := extractTSV(cp)
	if op != "copy" {
		t.Errorf("copy op 提取错误: %q", op)
	}
	if src != "gcs:my-gcs-dw/libs/dt=20260531/part-001.c000" {
		t.Errorf("copy source 提取错误: %q", src)
	}
	if dst != "s3:my-s3-dw/libs/dt=20260531/part-001.c000" {
		t.Errorf("copy destination 提取错误: %q", dst)
	}

	// delete 消息（source 为空串）
	del, err := buildDeleteMessage("my-s3-dw", "libs/dt=20260508/part-00168.c000")
	if err != nil {
		t.Fatalf("buildDeleteMessage 报错: %v", err)
	}
	op, src, dst = extractTSV(del)
	if op != "delete" {
		t.Errorf("delete op 提取错误: %q", op)
	}
	if src != "" {
		t.Errorf("delete source 应为空串，实际: %q", src)
	}
	if dst != "s3:my-s3-dw/libs/dt=20260508/part-00168.c000" {
		t.Errorf("delete destination 提取错误: %q", dst)
	}
}

// TestExtractTSVSpecialChars 校验 destination 含 = / 等正常路径字符时提取完整（不被提前截断）。
func TestExtractTSVSpecialChars(t *testing.T) {
	cp, err := buildCopyMessage("gcs", "dst", "bkt", "a=b/c d/e.bin")
	if err != nil {
		t.Fatalf("buildCopyMessage 报错: %v", err)
	}
	_, src, dst := extractTSV(cp)
	if src != "gcs:bkt/a=b/c d/e.bin" {
		t.Errorf("source 含特殊字符提取错误: %q", src)
	}
	if dst != "s3:dst/a=b/c d/e.bin" {
		t.Errorf("destination 含特殊字符提取错误: %q", dst)
	}
}

// TestJSONStringFieldMissing 校验找不到字段时返回空串（防御，不 panic）。
func TestJSONStringFieldMissing(t *testing.T) {
	if got := jsonStringField([]byte(`{"op":"copy"}`), patDest); got != "" {
		t.Errorf("缺失字段应返回空串，实际: %q", got)
	}
	if got := jsonStringField([]byte(`not json at all`), patOp); got != "" {
		t.Errorf("非 JSON 应返回空串，实际: %q", got)
	}
	if got := jsonStringField([]byte(``), patOp); got != "" {
		t.Errorf("空输入应返回空串，实际: %q", got)
	}
}

// newTestAsyncLog 直接构造一个写指定文件的 asyncFileLog（不经包级全局，避免污染其他测试）。
func newTestAsyncLog(t *testing.T, path string, writeLine func(*bufio.Writer, []byte)) *asyncFileLog {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	l := &asyncFileLog{
		ch:        make(chan []byte, 16),
		w:         bufio.NewWriter(f),
		f:         f,
		localPath: path,
		kind:      "测试日志",
		writeLine: writeLine,
	}
	l.wg.Add(1)
	go l.loop()
	return l
}

// readLogLines 读回日志文件并按行拆分。
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestSentLoggerEndToEnd 校验发送日志端到端：投递消息 → 后台落盘 → 文件含正确 TSV 行。
func TestSentLoggerEndToEnd(t *testing.T) {
	path := t.TempDir() + "/sent.log"
	l := newTestAsyncLog(t, path, writeSentLine)

	cp, _ := buildCopyMessage("gcs", "s3b", "gb", "p/obj1")
	del, _ := buildDeleteMessage("s3b", "p/obj2")
	l.log(cp)
	l.log(del)
	l.close() // 关 channel + 等排空 + flush + 关文件

	lines := readLogLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d 行: %q", len(lines), lines)
	}
	// 第 1 行：copy
	if lines[0] != "copy\tgcs:gb/p/obj1\ts3:s3b/p/obj1" {
		t.Errorf("copy 行不符: %q", lines[0])
	}
	// 第 2 行：delete（source 空 → 中间字段为空）
	if lines[1] != "delete\t\ts3:s3b/p/obj2" {
		t.Errorf("delete 行不符: %q", lines[1])
	}
}

// TestDirSkipLoggerEndToEnd 校验目录过滤日志端到端：每行 bucket/name 原样落盘，TAB 被转义。
func TestDirSkipLoggerEndToEnd(t *testing.T) {
	path := t.TempDir() + "/skipped.log"
	l := newTestAsyncLog(t, path, writeRawLine)

	l.log([]byte("bkt/libs/hive/dt=20251101/"))
	l.log([]byte("bkt/dir\twith\ttabs/")) // GCS 对象名可含 TAB，应转义为字面 \t
	l.close()

	lines := readLogLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d 行: %q", len(lines), lines)
	}
	if lines[0] != "bkt/libs/hive/dt=20251101/" {
		t.Errorf("第 1 行不符: %q", lines[0])
	}
	if lines[1] != `bkt/dir\twith\ttabs/` {
		t.Errorf("TAB 应被转义为字面 \\t: %q", lines[1])
	}
}

// TestAsyncLogNilSafe 校验 nil 日志器的方法调用安全（未启用时各调用点会这样调）。
func TestAsyncLogNilSafe(t *testing.T) {
	var l *asyncFileLog
	l.log([]byte(`{"op":"copy"}`)) // 不应 panic
	l.close()                      // 不应 panic
}
