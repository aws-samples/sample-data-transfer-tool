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

// rotate 重开失败的重试路径不得重复移位归档：上次 reopen 失败(w.f==nil)后，后续每条
// 日志都会因 size>maxBytes 重入 rotate——若无守卫，每次都跑 rename 阶梯，keep 轮内把
// 全部历史归档冲毁（毁在最需要历史日志的故障时刻）。守卫后仅重试 OpenFile。
func TestRotateRetryDoesNotShiftArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.log")
	w, err := newRotWriter(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 制造归档 .1 并写入标记内容
	if err := os.WriteFile(path+".1", []byte("archive-1-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 模拟"上次重开失败"的退化态：f=nil 且 size 超阈值
	w.mu.Lock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.size = 999
	w.mu.Unlock()
	// 连续写多条：每条都触发 rotate 重试路径。无守卫时 .1 会被移位/覆盖丢失。
	for i := 0; i < 5; i++ {
		_, _ = w.Write([]byte("retry line\n"))
	}
	got, err := os.ReadFile(path + ".1")
	if err != nil || string(got) != "archive-1-content" {
		t.Errorf("重试路径不得移位归档：.1 应原样保留，got err=%v content=%q", err, got)
	}
	// 且退化态应已自愈（OpenFile 成功、恢复写入）
	if _, err := os.Stat(path); err != nil {
		t.Errorf("重试后应重开主日志文件: %v", err)
	}
}

// InfoOpsf：INFO 级低频聚合行落 ops 文件（进度行/停机汇总的观测断链修复）；Infof 仍不落。
func TestInfoOpsfWritesToOpsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.log")
	if err := Setup(path, 1<<20, 2); err != nil {
		t.Fatal(err)
	}
	Infof("plain info %d", 1)
	InfoOpsf("进度: total=%d", 42)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "进度: total=42") {
		t.Errorf("InfoOpsf 应落 ops 文件，got %q", s)
	}
	if strings.Contains(s, "plain info") {
		t.Errorf("Infof 不应落 ops 文件，got %q", s)
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
