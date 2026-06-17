package message

import (
	"testing"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

func TestParse_Copy(t *testing.T) {
	m, err := Parse(`{"source":"gcs:b/k","destination":"s3:d/k"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "gcs:b/k" || m.Destination != "s3:d/k" || m.Op != model.OpCopy {
		t.Errorf("解析错: %+v", m)
	}
}

func TestParse_DeleteOmitsSource(t *testing.T) {
	m, err := Parse(`{"destination":"s3:d/k","op":"delete"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m.Op != model.OpDelete {
		t.Errorf("op 应 delete, got %s", m.Op)
	}
}

func TestParse_Refresh(t *testing.T) {
	m, err := Parse(`{"source":"gcs:b/k","destination":"s3:d/k","op":"refresh"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m.Op != model.OpRefresh {
		t.Errorf("op 应 refresh, got %s", m.Op)
	}
}

func TestParse_Poison(t *testing.T) {
	cases := []string{
		`not json`,
		`{"source":"s3:b/k"}`,                   // 缺 destination
		`{"destination":"s3:d","op":"x"}`,       // 非法 op
		`{"op":"copy","destination":"s3:d"}`,    // copy 缺 source
		`{"op":"refresh","destination":"s3:d"}`, // refresh 也必须有 source
	}
	for _, body := range cases {
		if _, err := Parse(body); err == nil {
			t.Errorf("应判 poison: %s", body)
		}
	}
}

func TestSplitEndpoint_PreservesLiteralKey(t *testing.T) {
	cases := []struct {
		in         string
		wantFs     string
		wantRemote string
	}{
		{"s3:bucket/a/b.bin", "s3:", "bucket/a/b.bin"},
		// 含 // 和前导 / 必须字面保留（这正是走 S3 兼容端点的原因，不归一化）
		{"gcs:bucket/a//b///c.bin", "gcs:", "bucket/a//b///c.bin"},
		{"s3:bucket//leading", "s3:", "bucket//leading"},
		{"s3:前缀/数据.bin", "s3:", "前缀/数据.bin"},
	}
	for _, c := range cases {
		fs, remote, err := SplitEndpoint(c.in, "test")
		if err != nil {
			t.Errorf("SplitEndpoint(%q) 报错: %v", c.in, err)
			continue
		}
		if fs != c.wantFs || remote != c.wantRemote {
			t.Errorf("SplitEndpoint(%q) = (%q,%q), want (%q,%q)", c.in, fs, remote, c.wantFs, c.wantRemote)
		}
	}
}

func TestSplitEndpoint_Invalid(t *testing.T) {
	cases := []string{
		"nocolon",         // 缺前缀
		":path",           // 空前缀
		"with space:path", // 前缀含空格（非法 remote 名）
		"s3\x00:path",     // 空字节
	}
	for _, in := range cases {
		if _, _, err := SplitEndpoint(in, "test"); err == nil {
			t.Errorf("应拒绝非法 endpoint: %q", in)
		}
	}
}

// 回归：Python _validate_endpoint 用 isalnum() 拒绝含 -/_ 的 remote 名（已知坑）。
// Go 实现放宽到 [A-Za-z0-9_-]，确保 s3-src / s3_dst 这类不被误拒。
func TestSplitEndpoint_AllowsHyphenUnderscore(t *testing.T) {
	for _, in := range []string{"s3-src:b/k", "s3_dst:b/k"} {
		if _, _, err := SplitEndpoint(in, "test"); err != nil {
			t.Errorf("含 -/_ 的 remote 名应允许（避开 Python isalnum 坑）: %q → %v", in, err)
		}
	}
}
