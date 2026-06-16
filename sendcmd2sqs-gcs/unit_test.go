package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseObjectURI(t *testing.T) {
	cases := []struct {
		uri        string
		wantBucket string
		wantObject string
		wantErr    bool
	}{
		{"s3://bkt/path/to/obj.parquet", "bkt", "path/to/obj.parquet", false},
		{"s3://bkt/single", "bkt", "single", false},
		{"s3://inv-bucket/test-inventory/uuid_0.parquet", "inv-bucket", "test-inventory/uuid_0.parquet", false},
		{"gs://bkt/obj", "", "", true}, // 错误 scheme
		{"s3://bkt", "", "", true},     // 缺对象路径
		{"s3:///obj", "", "", true},    // bucket 为空
		{"s3://bkt/", "", "", true},    // 对象为空
	}
	for _, c := range cases {
		b, o, err := parseObjectURI(c.uri)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseObjectURI(%q) 期望报错，实际成功 (%q,%q)", c.uri, b, o)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseObjectURI(%q) 意外报错: %v", c.uri, err)
			continue
		}
		if b != c.wantBucket || o != c.wantObject {
			t.Errorf("parseObjectURI(%q) = (%q,%q), 期望 (%q,%q)", c.uri, b, o, c.wantBucket, c.wantObject)
		}
	}
}

// TestBuildCopyMessageParity 校验 Go 组装的消息体与 Python 语义一致。
//
// 注意：Go encoding/json 输出无空格（{"a":"b"}），Python json.dumps 默认带空格
// （{"a": "b"}）。这是有意的差异——JSON 空格不具语义，消费端 rclone 按 key 取值，
// 两者等价。故断言「解析后字段相等」而非字节相等。关键 parity 点是 rclone_args 必须
// 是 [] 而非 null。
func TestBuildCopyMessageParity(t *testing.T) {
	got, err := buildCopyMessage("gcs", "my-s3-bucket", "gcs-linnjia-test", "archive/2025/obj_00000124.bin")
	if err != nil {
		t.Fatalf("buildCopyMessage 报错: %v", err)
	}

	// 1) rclone_args 必须序列化为 []，绝不能是 null（Go nil slice 的坑）。
	if !containsRcloneArgsEmpty(string(got)) {
		t.Errorf("消息体缺少 \"rclone_args\":[]，实际: %s", got)
	}
	if containsNull(string(got)) {
		t.Errorf("消息体含 null（rclone_args 退化为 nil slice）: %s", got)
	}

	// 2) 解析后字段与 Python 期望逐一相等。
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("Go 消息体非合法 JSON: %v", err)
	}
	want := map[string]string{
		"source":      "gcs:gcs-linnjia-test/archive/2025/obj_00000124.bin",
		"destination": "s3:my-s3-bucket/archive/2025/obj_00000124.bin",
		"op":          "copy",
	}
	for k, v := range want {
		if got, ok := m[k].(string); !ok || got != v {
			t.Errorf("字段 %q = %v, 期望 %q", k, m[k], v)
		}
	}
	args, ok := m["rclone_args"].([]any)
	if !ok {
		t.Errorf("rclone_args 类型错误（应为数组）: %T", m["rclone_args"])
	} else if len(args) != 0 {
		t.Errorf("rclone_args 应为空数组，实际 len=%d", len(args))
	}
}

// TestBuildCopyMessageSpecialChars 校验 object name 含特殊字符时正确转义。
func TestBuildCopyMessageSpecialChars(t *testing.T) {
	got, err := buildCopyMessage("gcs", "dst", "bkt", "dir/a b#c?d.bin")
	if err != nil {
		t.Fatalf("buildCopyMessage 报错: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("含特殊字符时产生非法 JSON: %v", err)
	}
	if m["source"] != "gcs:bkt/dir/a b#c?d.bin" {
		t.Errorf("source 转义错误: %v", m["source"])
	}
}

func TestIgnoreMatcher(t *testing.T) {
	m := &ignoreMatcher{}
	// 空匹配器：不跳过任何东西
	if m.shouldSkip("anything/x.bin") {
		t.Error("空 ignoreMatcher 不应跳过任何对象")
	}

	// 编译几条规则验证匹配语义
	m2 := compileIgnoreForTest(t, []string{"*.tmp", "logs/**", "exact/path.bin"})
	cases := []struct {
		name string
		skip bool
	}{
		{"a.tmp", true},          // *.tmp 命中（同目录）
		{"data/a.tmp", false},    // *.tmp 不跨目录
		{"logs/app/x.bin", true}, // logs/** 跨目录命中
		{"logs/x.bin", true},     // logs/** 命中
		{"exact/path.bin", true}, // 精确命中
		{"other/path.bin", false},
		{"keep/data.bin", false},
	}
	for _, c := range cases {
		if got := m2.shouldSkip(c.name); got != c.skip {
			t.Errorf("shouldSkip(%q) = %v, 期望 %v", c.name, got, c.skip)
		}
	}
}

// 测试辅助
func containsRcloneArgsEmpty(s string) bool {
	return strings.Contains(s, `"rclone_args":[]`)
}
func containsNull(s string) bool {
	return strings.Contains(s, "null")
}

// compileIgnoreForTest 用给定 pattern 构造 ignoreMatcher（复用 ignore.go 的 glob 编译逻辑）。
func compileIgnoreForTest(t *testing.T, patterns []string) *ignoreMatcher {
	t.Helper()
	m := &ignoreMatcher{}
	for _, p := range patterns {
		g, err := globCompile(p)
		if err != nil {
			t.Fatalf("编译 glob %q 失败: %v", p, err)
		}
		m.globs = append(m.globs, g)
	}
	return m
}
