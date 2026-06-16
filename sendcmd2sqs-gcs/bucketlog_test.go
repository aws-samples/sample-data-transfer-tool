package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestTeeWriterSwitch 校验 teeWriter 双输出切换：set 前只 base、set 中 base+extra、clear 后只 base。
func TestTeeWriterSwitch(t *testing.T) {
	var base bytes.Buffer
	tw := &teeWriter{base: &base}

	tw.Write([]byte("a\n")) // 只 base

	f, err := os.CreateTemp(t.TempDir(), "tee-*.log")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	tw.setExtra(f)
	tw.Write([]byte("b\n")) // base + 文件
	tw.setExtra(nil)
	tw.Write([]byte("c\n")) // 只 base
	f.Close()

	if got := base.String(); got != "a\nb\nc\n" {
		t.Errorf("base 内容 = %q, 期望 a\\nb\\nc\\n", got)
	}
	data, _ := os.ReadFile(f.Name())
	if got := string(data); got != "b\n" {
		t.Errorf("extra 文件内容 = %q, 期望 b\\n", got)
	}
}

// TestTeeWriterConcurrent 用 -race 检出并发 logf + 切换 writer 的数据竞争。
// 模拟：多个 sender goroutine 并发写 stdLogger，主 goroutine 反复 set/clear 桶文件。
func TestTeeWriterConcurrent(t *testing.T) {
	var base bytes.Buffer
	tw := &teeWriter{base: &base}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tw.Write([]byte("x\n"))
			}
		}()
	}
	// 主 goroutine 同时反复切换 extra
	for k := 0; k < 100; k++ {
		f, err := os.CreateTemp(t.TempDir(), "c-*.log")
		if err != nil {
			t.Fatalf("建临时文件失败: %v", err)
		}
		tw.setExtra(f)
		tw.setExtra(nil)
		f.Close()
	}
	wg.Wait()
	// 仅验证无 race/panic，且行级完整（不交错）：base 全是 "x\n" 的整数倍长度。
	if base.Len()%2 != 0 {
		t.Errorf("base 长度 %d 非偶数，疑似行交错", base.Len())
	}
}

// TestSanitizeLogName 校验文件名清洗：非法字符替换、去前导点、空→_。
func TestSanitizeLogName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my-bucket", "my-bucket"},
		{"gcs-linnjia-test-1", "gcs-linnjia-test-1"},
		{"a.b_c-1", "a.b_c-1"},
		{"a/b", "a_b"},
		{"../etc", "etc"},      // 非法字符 / 先变 _，再去前导点：".._etc" → 去点 "_etc"？见下断言
		{"a b#c?d", "a_b_c_d"}, // 空格/#/? → _
		{"", "_"},
	}
	for _, c := range cases {
		got := sanitizeLogName(c.in)
		if strings.Contains(got, "/") {
			t.Errorf("sanitizeLogName(%q)=%q 仍含 /", c.in, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeLogName(%q)=%q 仍以 . 开头（路径穿越风险）", c.in, got)
		}
	}
	// 精确断言几个关键项
	if sanitizeLogName("a/b") != "a_b" {
		t.Errorf("a/b 应为 a_b, 实际 %q", sanitizeLogName("a/b"))
	}
	if sanitizeLogName("") != "_" {
		t.Errorf("空串应为 _, 实际 %q", sanitizeLogName(""))
	}
}

// TestParseS3Prefix 校验 LOG_S3_URI 解析（prefix 可空、去首尾斜杠、错误情形）。
func TestParseS3Prefix(t *testing.T) {
	ok := []struct{ in, wantB, wantP string }{
		{"s3://b/p/q", "b", "p/q"},
		{"s3://b", "b", ""},
		{"s3://b/", "b", ""},
		{"s3://b/p/", "b", "p"},
	}
	for _, c := range ok {
		gb, gp, err := parseS3Prefix(c.in)
		if err != nil || gb != c.wantB || gp != c.wantP {
			t.Errorf("parseS3Prefix(%q) = (%q,%q,%v), 期望 (%q,%q,nil)", c.in, gb, gp, err, c.wantB, c.wantP)
		}
	}
	bad := []string{"gs://b/p", "s3://", "s3:///p", "b/p"}
	for _, in := range bad {
		if _, _, err := parseS3Prefix(in); err == nil {
			t.Errorf("parseS3Prefix(%q) 应报错，却成功", in)
		}
	}
}

// TestStartBucketLog 校验桶日志文件生成、含该桶日志、stop 后 logf 不再进文件。
func TestStartBucketLog(t *testing.T) {
	// 隔离到临时目录（logs/ 写在这里）。go.mod 是 1.23，无 t.Chdir，手动切换+恢复。
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd 失败: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir 失败: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	blog, err := startBucketLog("gcs-test-bkt")
	if err != nil {
		t.Fatalf("startBucketLog 失败: %v", err)
	}
	logf("hello in bucket")
	blog.stop()
	logf("after stop should not be in file")

	data, err := os.ReadFile(blog.localPath)
	if err != nil {
		t.Fatalf("读桶日志失败 %s: %v", blog.localPath, err)
	}
	content := string(data)
	if !strings.Contains(content, "hello in bucket") {
		t.Errorf("桶日志应含 'hello in bucket'，实际: %q", content)
	}
	if strings.Contains(content, "after stop") {
		t.Errorf("stop 后的日志不应进文件，实际: %q", content)
	}
	// 文件名形如 logs/gcs-test-bkt-<ts>.log
	if dir := filepath.Dir(blog.localPath); filepath.Base(dir) != logDir {
		t.Errorf("日志应在 %s/ 目录，实际 %s", logDir, blog.localPath)
	}
	if !strings.HasPrefix(filepath.Base(blog.localPath), "gcs-test-bkt-") {
		t.Errorf("文件名应以 源桶名- 开头，实际 %s", blog.localPath)
	}
}
