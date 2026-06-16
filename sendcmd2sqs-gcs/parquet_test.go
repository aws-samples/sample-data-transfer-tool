package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/parquet-go/parquet-go"
)

// invRow 模拟 Storage Insights inventory 的一行（含本工具用到的列）。
type invRow struct {
	Bucket string `parquet:"bucket"`
	Name   string `parquet:"name"`
	Size   int64  `parquet:"size"`
}

// invRowNoSize 是不含 size 列的清单行（验证"缺 size 列"与"懒路径不读 size"）。
type invRowNoSize struct {
	Bucket string `parquet:"bucket"`
	Name   string `parquet:"name"`
}

// writeMultiRowGroupParquet 生成一个含 numRG 个 row group、每组 rowsPerRG 行的 Parquet 文件。
// 每写满一组就 Flush() 切 row group 边界。每行带固定 size=1024。
func writeMultiRowGroupParquet(t *testing.T, numRG, rowsPerRG int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shard.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRow](f)
	for rg := 0; rg < numRG; rg++ {
		rows := make([]invRow, rowsPerRG)
		for i := range rows {
			rows[i] = invRow{Bucket: "gcs-linnjia-test", Name: fmt.Sprintf("rg%d/obj_%06d.bin", rg, i), Size: 1024}
		}
		if _, err := w.Write(rows); err != nil {
			t.Fatalf("写 row group %d 失败: %v", rg, err)
		}
		if err := w.Flush(); err != nil { // Flush 切 row group 边界
			t.Fatalf("flush row group %d 失败: %v", rg, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
	return p
}

// writeSizedParquet 写一个单 row group 文件，行的 size 由 sizes 给出（一行一个）。
func writeSizedParquet(t *testing.T, sizes []int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sized.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRow](f)
	rows := make([]invRow, len(sizes))
	for i, sz := range sizes {
		rows[i] = invRow{Bucket: "gcs-linnjia-test", Name: fmt.Sprintf("obj_%06d.bin", i), Size: sz}
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
	return p
}

// writeNoSizeParquet 写一个不含 size 列的 parquet（验证缺列报错 / 懒路径）。
func writeNoSizeParquet(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nosize.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRowNoSize](f)
	if _, err := w.Write([]invRowNoSize{{Bucket: "gcs-linnjia-test", Name: "a.bin"}}); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
	return p
}

// TestShardReaderMultiRowGroup 校验多 row group 能被正确识别与读取。
func TestShardReaderMultiRowGroup(t *testing.T) {
	const numRG, rowsPerRG = 4, 500
	p := writeMultiRowGroupParquet(t, numRG, rowsPerRG)
	sr, err := openShard(p, false) // 懒路径：不读 size 列
	if err != nil {
		t.Fatalf("openShard 失败: %v", err)
	}
	defer sr.close()

	if got := sr.numRowGroups(); got != numRG {
		t.Fatalf("row group 数 = %d, 期望 %d（fixture 未按预期切分）", got, numRG)
	}

	total := 0
	for rg := 0; rg < numRG; rg++ {
		if err := sr.iterRowGroup(rg, func(bucket, name string, _ int64, hasSize, _ bool) error {
			if bucket != "gcs-linnjia-test" || name == "" {
				t.Errorf("行内容异常: bucket=%q name=%q", bucket, name)
			}
			if hasSize {
				t.Errorf("needSize=false 时 hasSize 应为 false")
			}
			total++
			return nil
		}); err != nil {
			t.Fatalf("iterRowGroup(%d) 失败: %v", rg, err)
		}
	}
	if total != numRG*rowsPerRG {
		t.Errorf("读到行数 = %d, 期望 %d", total, numRG*rowsPerRG)
	}
}

// TestShardReaderConcurrentRowGroups 校验多 producer 并发对同一 *shardReader 读不同 row group
// 的并发安全性（对应审查 Finding #6：e2e 只测过单 row group，此路径从未被覆盖）。
// 用 `go test -race` 运行才能真正检出数据竞争。
func TestShardReaderConcurrentRowGroups(t *testing.T) {
	const numRG, rowsPerRG = 8, 1000
	p := writeMultiRowGroupParquet(t, numRG, rowsPerRG)
	sr, err := openShard(p, false)
	if err != nil {
		t.Fatalf("openShard 失败: %v", err)
	}
	defer sr.close()
	if sr.numRowGroups() != numRG {
		t.Fatalf("row group 数 = %d, 期望 %d", sr.numRowGroups(), numRG)
	}

	var wg sync.WaitGroup
	var totalRows int64
	var mu sync.Mutex
	errCh := make(chan error, numRG)

	// 每个 goroutine 认领一个 row group 并发读（模拟 processShard 的 producer）。
	for rg := 0; rg < numRG; rg++ {
		wg.Add(1)
		go func(rgIdx int) {
			defer wg.Done()
			local := 0
			if err := sr.iterRowGroup(rgIdx, func(bucket, name string, _ int64, _, _ bool) error {
				local++
				return nil
			}); err != nil {
				errCh <- fmt.Errorf("row group %d: %w", rgIdx, err)
				return
			}
			mu.Lock()
			totalRows += int64(local)
			mu.Unlock()
		}(rg)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
	if totalRows != int64(numRG*rowsPerRG) {
		t.Errorf("并发读到行数 = %d, 期望 %d", totalRows, numRG*rowsPerRG)
	}
}

// TestOpenShardNeedSize 校验：needSize=true 读出 size 且 hasSize=true；缺 size 列时报错；
// needSize=false 对缺列文件也能正常打开（懒路径向后兼容）。
func TestOpenShardNeedSize(t *testing.T) {
	// 有 size 列 + needSize=true → 读出实际值
	p := writeSizedParquet(t, []int64{100, 200, 300})
	sr, err := openShard(p, true)
	if err != nil {
		t.Fatalf("openShard(needSize=true) 失败: %v", err)
	}
	var got []int64
	if err := sr.iterRowGroup(0, func(_, _ string, size int64, hasSize, _ bool) error {
		if !hasSize {
			t.Error("有 size 列时 hasSize 应为 true")
		}
		got = append(got, size)
		return nil
	}); err != nil {
		t.Fatalf("iterRowGroup 失败: %v", err)
	}
	sr.close()
	if len(got) != 3 || got[0] != 100 || got[1] != 200 || got[2] != 300 {
		t.Errorf("size 读取不符: %v", got)
	}

	// 缺 size 列 + needSize=true → 报错
	np := writeNoSizeParquet(t)
	if _, err := openShard(np, true); err == nil {
		t.Error("缺 size 列且 needSize=true 应报错")
	}
	// 缺 size 列 + needSize=false → 成功（懒路径）
	sr2, err := openShard(np, false)
	if err != nil {
		t.Errorf("缺 size 列但 needSize=false 应成功: %v", err)
	} else {
		sr2.close()
	}
}

// TestProcessShardSkipsDirMarkers 校验全量模式过滤 GCS 目录占位对象（name 以 / 结尾）：
// 不组装消息、不占 --limit 额度、独立计数 dirsFiltered；size==0 但不结尾 / 的真实空文件不受影响。
// dry-run + nil client（runSenders 的 dry-run 分支不触碰 client）。
func TestProcessShardSkipsDirMarkers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dirmix.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRow](f)
	rows := []invRow{
		{Bucket: "gcsA", Name: "data/a.bin", Size: 100},
		{Bucket: "gcsA", Name: "libs/hive/dt=20251101/", Size: 0}, // 目录占位 → 过滤
		{Bucket: "gcsA", Name: "empty.bin", Size: 0},              // 真实空文件 → 必须迁移
		{Bucket: "gcsA", Name: "warehouse/db/", Size: 0},          // 目录占位 → 过滤
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	w.Close()
	f.Close()

	cfg := &Config{
		GCSRemote: "gcs",
		BucketMap: map[string]string{"gcsA": "s3A"},
	}
	res, pErr := processShard(
		context.Background(), cfg, nil, p, &ignoreMatcher{}, 1, 1, -1, true,
	)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	sent, failed, skipped := res.sent, res.failed, res.skipped
	sizeFiltered, dirsFiltered, produced := res.sizeFiltered, res.dirsFiltered, res.produced
	if dirsFiltered != 2 {
		t.Errorf("dirsFiltered 应为 2（两个结尾/的目录占位），实际 %d", dirsFiltered)
	}
	if produced != 2 || sent != 2 {
		t.Errorf("应组装/发送 2 条（a.bin + 空文件 empty.bin），实际 produced=%d sent=%d", produced, sent)
	}
	if skipped != 0 || sizeFiltered != 0 || failed != 0 {
		t.Errorf("skipped/sizeFiltered/failed 应全 0，实际 %d/%d/%d", skipped, sizeFiltered, failed)
	}
}

// TestProcessShardLimited 回归：--limit 截断 shard 时 res.limited 必须为 true（否则 checkpoint
// 会误标该 shard 完成、重跑跳过其剩余对象，--limit 1 时跳过整个 shard）。limit 充足时为 false。
func TestProcessShardLimited(t *testing.T) {
	p := filepath.Join(t.TempDir(), "limit.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRow](f)
	rows := make([]invRow, 5) // 5 个可迁移对象
	for i := range rows {
		rows[i] = invRow{Bucket: "gcsA", Name: fmt.Sprintf("obj_%d.bin", i), Size: 100}
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	w.Close()
	f.Close()

	cfg := &Config{GCSRemote: "gcs", BucketMap: map[string]string{"gcsA": "s3A"}}

	// limit=1：5 个对象只组装 1 个 → 必被截断 → limited=true
	res, pErr := processShard(context.Background(), cfg, nil, p, &ignoreMatcher{}, 1, 1, 1, true)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	if !res.limited {
		t.Errorf("limit=1 截断 5 个对象的 shard，limited 应为 true（否则会误标 checkpoint 漏发），实际 false")
	}
	if res.produced != 1 {
		t.Errorf("limit=1 应只组装 1 条，实际 %d", res.produced)
	}

	// limit=10 ≥ 5：完整处理完 → limited=false（可安全标记 checkpoint）
	res2, pErr := processShard(context.Background(), cfg, nil, p, &ignoreMatcher{}, 1, 1, 10, true)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	if res2.limited {
		t.Errorf("limit=10 ≥ 5 个对象，shard 完整处理完，limited 应为 false，实际 true")
	}
	if res2.produced != 5 {
		t.Errorf("limit 充足应组装全部 5 条，实际 %d", res2.produced)
	}

	// limit 恰好等于对象数（=5）：完整处理完、最后一条正好用尽额度，不应误判截断
	res3, pErr := processShard(context.Background(), cfg, nil, p, &ignoreMatcher{}, 1, 1, 5, true)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	if res3.limited {
		t.Errorf("limit=5 恰好等于对象数，完整处理完，limited 应为 false（边界），实际 true")
	}
	if res3.produced != 5 {
		t.Errorf("limit=5 应组装全部 5 条，实际 %d", res3.produced)
	}
}

// invRowWithDeleted 模拟含 timeDeleted 列的清单行（指针：nil=存活/null，非 nil=已删除）。
// 物理类型用 INT64 而非真实清单的 timestamp[us]——deleted 判定只看 null/非 null（v.IsNull()），
// 与物理类型无关，INT64 即可覆盖判定逻辑。
type invRowWithDeleted struct {
	Bucket      string `parquet:"bucket"`
	Name        string `parquet:"name"`
	Size        int64  `parquet:"size"`
	TimeDeleted *int64 `parquet:"timeDeleted,optional"`
}

// TestProcessShardSkipsDeleted 校验已删除对象（timeDeleted 非 null）被过滤：
// 不组装、独立计数 deletedFiltered；deleted 判定优先于目录占位（已删除的目录占位计入 deleted）。
func TestProcessShardSkipsDeleted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "deleted.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	ts := int64(1780000000000000) // 任意非零时间戳（微秒）
	w := parquet.NewGenericWriter[invRowWithDeleted](f)
	rows := []invRowWithDeleted{
		{Bucket: "gcsA", Name: "live/a.bin", Size: 100},                   // 存活 → 组装
		{Bucket: "gcsA", Name: "gone/b.bin", Size: 200, TimeDeleted: &ts}, // 已删除 → deletedFiltered
		{Bucket: "gcsA", Name: "gone/dir/", Size: 0, TimeDeleted: &ts},    // 已删除的目录占位 → deletedFiltered（deleted 优先）
		{Bucket: "gcsA", Name: "live/dir/", Size: 0},                      // 存活的目录占位 → dirsFiltered
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("写行失败: %v", err)
	}
	w.Close()
	f.Close()

	cfg := &Config{GCSRemote: "gcs", BucketMap: map[string]string{"gcsA": "s3A"}}
	res, pErr := processShard(context.Background(), cfg, nil, p, &ignoreMatcher{}, 1, 1, -1, true)
	if pErr != nil {
		t.Fatalf("processShard 报错: %v", pErr)
	}
	if res.deletedFiltered != 2 {
		t.Errorf("deletedFiltered 应为 2（含已删除的目录占位），实际 %d", res.deletedFiltered)
	}
	if res.dirsFiltered != 1 {
		t.Errorf("dirsFiltered 应为 1（仅存活的目录占位），实际 %d", res.dirsFiltered)
	}
	if res.produced != 1 || res.sent != 1 {
		t.Errorf("应组装 1 条（live/a.bin），实际 produced=%d sent=%d", res.produced, res.sent)
	}

	// 无 timeDeleted 列的旧清单：openShard 应正常打开且 deleted 恒 false（向后兼容）。
	old := writeSizedParquet(t, []int64{1})
	sr, err := openShard(old, false)
	if err != nil {
		t.Fatalf("旧清单（无 timeDeleted 列）应能打开: %v", err)
	}
	defer sr.close()
	if sr.timeDeletedIdx != -1 {
		t.Errorf("无 timeDeleted 列时 timeDeletedIdx 应为 -1，实际 %d", sr.timeDeletedIdx)
	}
	if err := sr.iterRowGroup(0, func(_, _ string, _ int64, _, deleted bool) error {
		if deleted {
			t.Error("无 timeDeleted 列时 deleted 应恒为 false")
		}
		return nil
	}); err != nil {
		t.Fatalf("iterRowGroup 失败: %v", err)
	}
}

// sizePass 复刻 pipeline.go 的过滤判定，便于在解析层断言区间语义（含等、未知放行）。
func sizePass(size int64, hasSize bool, min, max int64) bool {
	if !hasSize {
		return true // 未知放行
	}
	return size >= min && size <= max
}

// TestSizeFilterBoundary 校验闭区间边界含等、未知 size 放行。
func TestSizeFilterBoundary(t *testing.T) {
	const lo, hi = int64(100), int64(200)
	cases := []struct {
		size int64
		want bool
	}{
		{99, false}, {100, true}, {150, true}, {200, true}, {201, false},
	}
	for _, c := range cases {
		if got := sizePass(c.size, true, lo, hi); got != c.want {
			t.Errorf("[%d,%d] size=%d => %v, 期望 %v", lo, hi, c.size, got, c.want)
		}
	}
	// 未知 size 放行
	if !sizePass(0, false, lo, hi) {
		t.Error("未知 size 应放行")
	}
}

// invRowStrSize 模拟 GCS Storage Insights 把 size 编码为字符串的清单行（BYTE_ARRAY）。
type invRowStrSize struct {
	Bucket string `parquet:"bucket"`
	Name   string `parquet:"name"`
	Size   string `parquet:"size"`
}

// TestValueInt64StringSize 回归：size 列为字符串时 valueInt64 仍能解析（否则 size 过滤会失效，
// 导致超过 MAX_SIZE 的对象因 hasSize=false 被放行）。
func TestValueInt64StringSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "strsize.parquet")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("创建 parquet 失败: %v", err)
	}
	w := parquet.NewGenericWriter[invRowStrSize](f)
	if _, err := w.Write([]invRowStrSize{
		{Bucket: "b", Name: "a.bin", Size: "131072"},
		{Bucket: "b", Name: "b.bin", Size: ""},  // 空字符串 → 未知
		{Bucket: "b", Name: "c.bin", Size: "x"}, // 非数字 → 未知
	}); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	w.Close()
	f.Close()

	sr, err := openShard(p, true)
	if err != nil {
		t.Fatalf("openShard 失败: %v", err)
	}
	defer sr.close()

	got := map[string]struct {
		size    int64
		hasSize bool
	}{}
	if err := sr.iterRowGroup(0, func(_, name string, size int64, hasSize, _ bool) error {
		got[name] = struct {
			size    int64
			hasSize bool
		}{size, hasSize}
		return nil
	}); err != nil {
		t.Fatalf("iterRowGroup 失败: %v", err)
	}
	if !got["a.bin"].hasSize || got["a.bin"].size != 131072 {
		t.Errorf("字符串 size \"131072\" 应解析为 (131072,true)，实际 %+v", got["a.bin"])
	}
	if got["b.bin"].hasSize {
		t.Errorf("空字符串 size 应为未知(hasSize=false)，实际 %+v", got["b.bin"])
	}
	if got["c.bin"].hasSize {
		t.Errorf("非数字 size 应为未知(hasSize=false)，实际 %+v", got["c.bin"])
	}
}
