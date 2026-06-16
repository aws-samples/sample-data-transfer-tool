package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

// fakeStore 是内存版 objectStore，按 (bucket, object) 返回预置内容，用于离线测 parseManifest。
type fakeStore struct {
	objects map[string]string // "bucket/object" -> 内容
}

func (s *fakeStore) newReader(_ context.Context, bucket, object string) (io.ReadCloser, error) {
	body, ok := s.objects[bucket+"/"+object]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (s *fakeStore) close() error { return nil }

// TestGCSObjectKey 校验从 gs:// URI 剥桶取 object key。
func TestGCSObjectKey(t *testing.T) {
	ok := []struct{ in, want string }{
		{"gs://b/p/x_0.parquet", "p/x_0.parquet"},
		{"gs://my-gcs-inventory/parquet/my-gcs-dw/166ac753_0.parquet", "parquet/my-gcs-dw/166ac753_0.parquet"},
		{"gs://b/single", "single"},
	}
	for _, c := range ok {
		got, err := gcsObjectKey(c.in)
		if err != nil || got != c.want {
			t.Errorf("gcsObjectKey(%q) = (%q,%v), 期望 (%q,nil)", c.in, got, err, c.want)
		}
	}
	bad := []string{"gs://b/", "gs://b", "s3://b/x", "b/x", ""}
	for _, in := range bad {
		if _, err := gcsObjectKey(in); err == nil {
			t.Errorf("gcsObjectKey(%q) 应报错，却成功", in)
		}
	}
}

// TestParseManifestNonGCSReportName 校验 reportNames 含非 gs:// 项时报错。
func TestParseManifestNonGCSReportName(t *testing.T) {
	store := &fakeStore{objects: map[string]string{
		"inv/m.json": `{"recordsProcessed":"10","shardsCount":"2","reportNames":["gs://gcsb/p/a.parquet","s3://x/b.parquet"]}`,
	}}
	if _, _, err := parseManifest(context.Background(), store, "s3://inv/m.json", "", ""); err == nil {
		t.Error("reportNames 含非 gs:// 项应报错，却成功")
	}
}

// TestShardS3KeyFromGCS 校验 gs:// → S3 object key 的前缀重写（本次新增）。
func TestShardS3KeyFromGCS(t *testing.T) {
	cases := []struct {
		gsURI, gcsPrefix, s3Prefix, want string
	}{
		// 真实场景：parquet/ → ops/inventory/（剥桶 + 换前缀）
		{"gs://my-gcs-inventory/parquet/my-gcs-ailab/48449a57_152.parquet", "parquet/", "ops/inventory/",
			"ops/inventory/my-gcs-ailab/48449a57_152.parquet"},
		// 不以 gcsPrefix 开头 → 不替换
		{"gs://b/other/x.parquet", "parquet/", "ops/inventory/", "other/x.parquet"},
		// gcsPrefix 为空 → 不替换（纯剥桶，向后兼容旧部署）
		{"gs://b/parquet/x.parquet", "", "", "parquet/x.parquet"},
		// s3Prefix 为空、gcsPrefix 非空 → 等于删掉前缀
		{"gs://b/parquet/eu/x.parquet", "parquet/", "", "eu/x.parquet"},
	}
	for _, c := range cases {
		got, err := shardS3KeyFromGCS(c.gsURI, c.gcsPrefix, c.s3Prefix)
		if err != nil || got != c.want {
			t.Errorf("shardS3KeyFromGCS(%q,%q,%q) = (%q,%v), 期望 (%q,nil)", c.gsURI, c.gcsPrefix, c.s3Prefix, got, err, c.want)
		}
	}
	// 非 gs:// 仍报错
	if _, err := shardS3KeyFromGCS("s3://b/x", "parquet/", "ops/inventory/"); err == nil {
		t.Error("非 gs:// URI 应报错")
	}
}

// TestParseManifestPrefixRewrite 端到端校验：reportNames 的 parquet/ 前缀被重写为 ops/inventory/，
// 桶换成 manifest 所在 S3 桶（复刻真实部署 gs://my-gcs-inventory/parquet/... → s3://my-s3-inventory/ops/inventory/...）。
func TestParseManifestPrefixRewrite(t *testing.T) {
	store := &fakeStore{objects: map[string]string{
		"my-s3-inventory/ops/inventory/m.json": `{"recordsProcessed":"199","reportNames":[` +
			`"gs://my-gcs-inventory/parquet/my-gcs-ailab/48449a57_152.parquet",` +
			`"gs://my-gcs-inventory/parquet/my-gcs-ailab/48449a57_84.parquet"]}`,
	}}
	shardURIs, records, err := parseManifest(context.Background(), store,
		"s3://my-s3-inventory/ops/inventory/m.json", "parquet/", "ops/inventory/")
	if err != nil {
		t.Fatalf("parseManifest 报错: %v", err)
	}
	if records != 199 {
		t.Errorf("recordsProcessed 应为 199，实际 %d", records)
	}
	want := []string{
		"s3://my-s3-inventory/ops/inventory/my-gcs-ailab/48449a57_152.parquet",
		"s3://my-s3-inventory/ops/inventory/my-gcs-ailab/48449a57_84.parquet",
	}
	if len(shardURIs) != 2 || shardURIs[0] != want[0] || shardURIs[1] != want[1] {
		t.Errorf("shard URI 前缀重写不符:\n  实际 %+v\n  期望 %+v", shardURIs, want)
	}
}

// TestParseManifestOK 校验 GCS 原生格式解析：reportNames(gs://) 映射到 manifest 所在 S3 桶。
func TestParseManifestOK(t *testing.T) {
	store := &fakeStore{objects: map[string]string{
		"s3invbucket/parquet/my-gcs-dw/m.json": `{"recordsProcessed":"42","shardsCount":"1","reportNames":["gs://gcsb/parquet/my-gcs-dw/x_0.parquet"]}`,
	}}
	shardURIs, records, err := parseManifest(context.Background(), store, "s3://s3invbucket/parquet/my-gcs-dw/m.json", "", "")
	if err != nil {
		t.Fatalf("parseManifest 报错: %v", err)
	}
	if records != 42 {
		t.Errorf("recordsProcessed 应为 42，实际 %d", records)
	}
	// 桶换成 manifest 所在 S3 桶(s3invbucket)，key 来自 gs:// 的 object key。
	if len(shardURIs) != 1 || shardURIs[0] != "s3://s3invbucket/parquet/my-gcs-dw/x_0.parquet" {
		t.Errorf("shard URI 映射不符: %+v", shardURIs)
	}
}

// TestParseManifestEmptyRecords 校验 recordsProcessed 为空字符串时当 0、不报错。
func TestParseManifestEmptyRecords(t *testing.T) {
	store := &fakeStore{objects: map[string]string{
		"inv/m.json": `{"reportNames":["gs://gcsb/a.parquet"]}`,
	}}
	_, records, err := parseManifest(context.Background(), store, "s3://inv/m.json", "", "")
	if err != nil {
		t.Fatalf("缺 recordsProcessed 应成功(当 0)，却报错: %v", err)
	}
	if records != 0 {
		t.Errorf("空 recordsProcessed 应为 0，实际 %d", records)
	}
}
