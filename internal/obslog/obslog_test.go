package obslog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WARNING+ 落 ops 文件，INFO 不落（对齐 Python：worker-ops 只收 WARNING+）。
func TestOpsFileOnlyWarnAndError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker-ops.log")
	if err := Setup(path, 10*1024*1024, 3); err != nil {
		t.Fatal(err)
	}
	Infof("这条 INFO 不该进文件")
	Warnf("warn-marker-%d", 1)
	Errorf("error-marker-%d", 2)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "不该进文件") {
		t.Error("INFO 不应落 ops 文件")
	}
	if !strings.Contains(s, "warn-marker-1") || !strings.Contains(s, "WARNING") {
		t.Error("WARNING 应落 ops 文件并带级别")
	}
	if !strings.Contains(s, "error-marker-2") || !strings.Contains(s, "ERROR") {
		t.Error("ERROR 应落 ops 文件并带级别")
	}
}

// 轮转：超过 maxBytes 后滚动到 .1。
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.log")
	// maxBytes 很小，强制轮转
	if err := Setup(path, 200, 2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		Errorf("填充行 padding padding padding %d", i)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("应产生轮转文件 .1: %v", err)
	}
}

// 空 path：Setup 不报错，降级仅控制台（不阻断启动）。
func TestSetupEmptyPathNoError(t *testing.T) {
	std.opsFile = nil // 复位
	if err := Setup("", 0, 0); err != nil {
		t.Errorf("空 path 应静默成功: %v", err)
	}
}

func TestSetupPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secure", "ops.log")
	if err := Setup(path, 10*1024*1024, 1); err != nil {
		t.Fatal(err)
	}
	dinfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dinfo.Mode().Perm(); got != 0o750 {
		t.Errorf("日志目录权限=%o, want 750", got)
	}
	if _, err := std.opsFile.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	finfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := finfo.Mode().Perm(); got != 0o600 {
		t.Errorf("日志文件权限=%o, want 600", got)
	}
}
