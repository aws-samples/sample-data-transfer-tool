package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
)

// diffRow 模拟 diff 报告的一行（列名与实测 schema 一致：Key/Size/DiffFlag）。
// LastModified/ETag 本工具不读，省略不影响（diffReader 按列名定位，多余/缺失列均无害）。
type diffRow struct {
	Key      string `parquet:"Key"`
	Size     int64  `parquet:"Size"`
	DiffFlag int32  `parquet:"DiffFlag"`
}

// diffRowNoSize 是不含 Size 列的 diff 行（验证 needSize=false 的懒路径）。
type diffRowNoSize struct {
	Key      string `parquet:"Key"`
	DiffFlag int32  `parquet:"DiffFlag"`
}

// writeDiffParquet 写一个单 row group 的 diff parquet 文件，返回其路径。
func writeDiffParquet(t *testing.T, rows []diffRow) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "diff.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 diff parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[diffRow](f)
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写 diff 行失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
	return p
}

// TestParseDiffFileName 校验文件名解析：正常名、缺段、第三段无下划线、带路径前缀。
func TestParseDiffFileName(t *testing.T) {
	// 正常名（带 /tmp/ 前缀，应被 filepath.Base 剥离）
	region, gcs, s3, ts, err := parseDiffFileName("/tmp/eu-south-2__my-gcs-dw__my-s3-dw_2026-06-09T08:24:32Z.parquet")
	if err != nil {
		t.Fatalf("正常文件名不应报错: %v", err)
	}
	if region != "eu-south-2" || gcs != "my-gcs-dw" || s3 != "my-s3-dw" || ts != "2026-06-09T08:24:32Z" {
		t.Errorf("解析结果不符: region=%q gcs=%q s3=%q ts=%q", region, gcs, s3, ts)
	}

	// 不带路径前缀也应正常
	if _, g, _, _, err := parseDiffFileName("r__g__s_ts.parquet"); err != nil || g != "g" {
		t.Errorf("无路径前缀解析失败: g=%q err=%v", g, err)
	}

	// 各种应报错的情形
	bad := []string{
		"only-one-part.parquet",       // 无 __ 分隔
		"r__g.parquet",                // 仅 2 段
		"r__g__s__extra_ts.parquet",   // 4 段
		"r__g__my-s3-dw.parquet", // 第三段无 _ 分隔（s3Bucket_timestamp）
		"__g__s_ts.parquet",           // region 空
		"r____s_ts.parquet",           // gcsBucket 空
		"r__g___ts.parquet",           // s3Bucket 空（第三段以 _ 开头）
	}
	for _, name := range bad {
		if _, _, _, _, err := parseDiffFileName(name); err == nil {
			t.Errorf("文件名 %q 应报错，却成功", name)
		}
	}
}

// TestBuildDeleteMessage 校验 delete 消息体：op=delete、source 空、destination 正确、rclone_args=[]（非 null）。
func TestBuildDeleteMessage(t *testing.T) {
	got, err := buildDeleteMessage("my-s3-dw", "libs/dt=20260508/part-00168.c000")
	if err != nil {
		t.Fatalf("buildDeleteMessage 报错: %v", err)
	}

	// rclone_args 必须是 []，绝不能 null（沿用 copy 的 nil-slice 坑回归）。
	if !containsRcloneArgsEmpty(string(got)) {
		t.Errorf("缺少 \"rclone_args\":[]，实际: %s", got)
	}
	if containsNull(string(got)) {
		t.Errorf("含 null（rclone_args 退化为 nil slice）: %s", got)
	}

	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("delete 消息体非合法 JSON: %v", err)
	}
	if m["op"] != "delete" {
		t.Errorf("op 应为 delete，实际 %v", m["op"])
	}
	if m["source"] != "" {
		t.Errorf("delete 的 source 应为空串，实际 %v", m["source"])
	}
	if m["destination"] != "s3:my-s3-dw/libs/dt=20260508/part-00168.c000" {
		t.Errorf("destination 不符: %v", m["destination"])
	}
}

// TestOpenDiffShard 校验列定位：正常读出 key/flag/size；缺 Key/DiffFlag 列报错；needSize=false 对无 Size 列文件可开。
func TestOpenDiffShard(t *testing.T) {
	// 正常文件 + needSize=true → 读出 key/flag/size
	p := writeDiffParquet(t, []diffRow{
		{Key: "a/obj1", Size: 100, DiffFlag: 1},
		{Key: "b/obj2", Size: 200, DiffFlag: 2},
	})
	dr, err := openDiffShard(p, true)
	if err != nil {
		t.Fatalf("openDiffShard(needSize=true) 失败: %v", err)
	}
	type row struct {
		flag    int64
		size    int64
		hasSize bool
	}
	got := map[string]row{}
	if err := dr.iterDiffRowGroup(0, func(key string, diffFlag, size int64, hasSize bool) error {
		got[key] = row{diffFlag, size, hasSize}
		return nil
	}); err != nil {
		t.Fatalf("iterDiffRowGroup 失败: %v", err)
	}
	dr.close()
	if got["a/obj1"].flag != 1 || got["a/obj1"].size != 100 || !got["a/obj1"].hasSize {
		t.Errorf("a/obj1 读取不符: %+v", got["a/obj1"])
	}
	if got["b/obj2"].flag != 2 || got["b/obj2"].size != 200 {
		t.Errorf("b/obj2 读取不符: %+v", got["b/obj2"])
	}

	// 缺 Size 列 + needSize=true → 报错
	np := writeDiffParquetNoSize(t, []diffRowNoSize{{Key: "x", DiffFlag: 1}})
	if _, err := openDiffShard(np, true); err == nil {
		t.Error("缺 Size 列且 needSize=true 应报错")
	}
	// 缺 Size 列 + needSize=false → 成功（懒路径）
	if dr2, err := openDiffShard(np, false); err != nil {
		t.Errorf("缺 Size 列但 needSize=false 应成功: %v", err)
	} else {
		dr2.close()
	}

	// 缺 Key 列 → 报错
	kp := writeKeylessParquet(t)
	if _, err := openDiffShard(kp, false); err == nil {
		t.Error("缺 Key 列应报错")
	}
}

// writeDiffParquetNoSize 写不含 Size 列的 diff 文件。
func writeDiffParquetNoSize(t *testing.T, rows []diffRowNoSize) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nosize.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[diffRowNoSize](f)
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	w.Close()
	f.Close()
	return p
}

// writeKeylessParquet 写一个不含 Key 列的 parquet（只有 DiffFlag），验证缺必填列报错。
func writeKeylessParquet(t *testing.T) string {
	t.Helper()
	type onlyFlag struct {
		DiffFlag int32 `parquet:"DiffFlag"`
	}
	p := filepath.Join(t.TempDir(), "keyless.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[onlyFlag](f)
	if _, err := w.Write([]onlyFlag{{DiffFlag: 1}}); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	w.Close()
	f.Close()
	return p
}

// TestProcessDiffShardDispatch 校验 DiffFlag 分流：1/3→copy，2→delete，未知值→skipped。
// 用 dry-run + nil client（runSenders 的 dry-run 分支不触碰 client），断言返回的统计计数。
func TestProcessDiffShardDispatch(t *testing.T) {
	p := writeDiffParquet(t, []diffRow{
		{Key: "plus1", Size: 10, DiffFlag: 1},            // copy
		{Key: "plus2", Size: 20, DiffFlag: 1},            // copy
		{Key: "minus1", Size: 30, DiffFlag: 2},           // delete
		{Key: "astrisk1", Size: 40, DiffFlag: 3},         // copy
		{Key: "unknown1", Size: 50, DiffFlag: 9},         // 未知 → skipped
		{Key: "", Size: 60, DiffFlag: 1},                 // 空 Key → skipped
		{Key: "libs/dt=20251101/", Size: 0, DiffFlag: 1}, // 目录占位 → dirsFiltered
	})

	cfg := &Config{GCSRemote: "gcs"} // 无 size 过滤
	ig := &ignoreMatcher{}           // 不过滤

	res, err := processDiffShard(
		context.Background(), cfg, nil, p, "my-gcs-dw", "my-s3-dw",
		ig, 1, 1, -1, true, // producers=1, senders=1, limit 不限, dryRun=true
	)
	if err != nil {
		t.Fatalf("processDiffShard 报错: %v", err)
	}

	// flag 1/1/3 → copy=3；flag 2 → delete=1；unknown(9)+空Key → skipped=2；结尾/ → dirsFiltered=1。
	if res.copies != 3 {
		t.Errorf("copies 应为 3（两个 flag=1 + 一个 flag=3），实际 %d", res.copies)
	}
	if res.deletes != 1 {
		t.Errorf("deletes 应为 1（flag=2），实际 %d", res.deletes)
	}
	if res.skipped != 2 {
		t.Errorf("skipped 应为 2（未知 flag + 空 Key），实际 %d", res.skipped)
	}
	if res.dirsFiltered != 1 {
		t.Errorf("dirsFiltered 应为 1（结尾/的目录占位），实际 %d", res.dirsFiltered)
	}
	// produced = copy + delete = 4（占 limit 额度的消息）；dry-run 下 sent 也应为 4。
	if res.produced != 4 {
		t.Errorf("produced 应为 4（copy+delete），实际 %d", res.produced)
	}
	if res.sent != 4 {
		t.Errorf("dry-run 下 sent 应等于组装数 4，实际 %d", res.sent)
	}
	if res.failed != 0 {
		t.Errorf("failed 应为 0，实际 %d", res.failed)
	}
}

// TestProcessDiffShardMessageWiring 校验组装的 copy/delete 消息 source/destination 正确
// （直接验消息构造，避免 dry-run stdout 捕获的脆弱性；与 unit_test.go 的 parity 风格一致）。
// 重点防 buildCopyMessage 参数顺序写反（targetBucket=s3Bucket, bucket=gcsBucket）。
func TestProcessDiffShardMessageWiring(t *testing.T) {
	// copy：source=gcs:{gcsBucket}/{key}，destination=s3:{s3Bucket}/{key}
	cp, err := buildCopyMessage("gcs", "my-s3-dw", "my-gcs-dw", "libs/dt=20260531/part-001.c000")
	if err != nil {
		t.Fatalf("buildCopyMessage 报错: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(cp, &m); err != nil {
		t.Fatalf("copy 消息非合法 JSON: %v", err)
	}
	if m["source"] != "gcs:my-gcs-dw/libs/dt=20260531/part-001.c000" {
		t.Errorf("copy source 不符（参数顺序可能写反）: %v", m["source"])
	}
	if m["destination"] != "s3:my-s3-dw/libs/dt=20260531/part-001.c000" {
		t.Errorf("copy destination 不符: %v", m["destination"])
	}
	if m["op"] != "copy" {
		t.Errorf("op 应为 copy，实际 %v", m["op"])
	}
}

// TestProcessDiffShardEmpty 校验空文件（0 行）返回全 0、无错误。
func TestProcessDiffShardEmpty(t *testing.T) {
	p := writeDiffParquet(t, []diffRow{})
	cfg := &Config{GCSRemote: "gcs"}
	res, err := processDiffShard(
		context.Background(), cfg, nil, p, "g", "s", &ignoreMatcher{}, 1, 1, -1, true,
	)
	if err != nil {
		t.Fatalf("空文件不应报错: %v", err)
	}
	if res.copies != 0 || res.deletes != 0 || res.produced != 0 || res.sent != 0 {
		t.Errorf("空文件统计应全 0: %+v", res)
	}
}
