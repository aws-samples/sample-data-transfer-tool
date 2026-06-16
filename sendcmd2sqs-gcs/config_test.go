package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return p
}

// writeTempFile 在临时目录写一个指定名字的文件，返回其绝对路径（用于桶映射 CSV）。
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	return p
}

// 一条最简单的合法 CSV（供 loadConfig 用例复用）。
const oneMapping = "gcsA,s3A,s3://inv/a/manifest.json\n"

// TestLoadConfigBasic 校验正常 KEY=value 文件解析与必填项。
func TestLoadConfigBasic(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	p := writeTemp(t, "# 注释行\nSQS_QUEUE_URL=https://sqs.us-west-2.amazonaws.com/1/q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\n\nGCS_REMOTE=mygcs\n")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.SQSQueueURL != "https://sqs.us-west-2.amazonaws.com/1/q" || cfg.AWSRegion != "us-west-2" || cfg.GCSRemote != "mygcs" {
		t.Errorf("解析结果不符: %+v", cfg)
	}
	if cfg.BucketMap["gcsA"] != "s3A" {
		t.Errorf("BucketMap 派生不符: %+v", cfg.BucketMap)
	}
	if len(cfg.Mappings) != 1 || cfg.Mappings[0].InventoryURI != "s3://inv/a/manifest.json" {
		t.Errorf("Mappings 不符: %+v", cfg.Mappings)
	}
}

// TestLoadConfigBOM 校验 UTF-8 BOM 开头的配置文件首键不被污染（Fix C 回归）。
func TestLoadConfigBOM(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	//  是 UTF-8 BOM；放在第一行 SQS_QUEUE_URL 前。
	p := writeTemp(t, "\uFEFFSQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\n")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("带 BOM 的配置应能正常解析，却报错: %v", err)
	}
	if cfg.SQSQueueURL != "https://q" {
		t.Errorf("BOM 未被剥离，SQSQueueURL=%q", cfg.SQSQueueURL)
	}
}

// TestLoadConfigDefaultGCSRemote 校验 GCS_REMOTE 缺省时回退为 gcs。
func TestLoadConfigDefaultGCSRemote(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	p := writeTemp(t, "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\n")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.GCSRemote != "gcs" {
		t.Errorf("GCS_REMOTE 默认值应为 gcs，实际 %q", cfg.GCSRemote)
	}
}

// TestLoadConfigMissingRequired 校验缺必填项时报错。
func TestLoadConfigMissingRequired(t *testing.T) {
	p := writeTemp(t, "AWS_REGION=us-west-2\n") // 缺 SQS_QUEUE_URL / BUCKET_MAP_FILE
	if _, err := loadConfig(p); err == nil {
		t.Error("缺必填项时应报错，却成功")
	}
}

// TestLoadConfigFileOverridesEnv 校验文件值覆盖环境变量。
func TestLoadConfigFileOverridesEnv(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	t.Setenv("SQS_QUEUE_URL", "https://from-env")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("BUCKET_MAP_FILE", csv)
	p := writeTemp(t, "SQS_QUEUE_URL=https://from-file\n") // 仅覆盖 URL，region/map 由环境兜底
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.SQSQueueURL != "https://from-file" {
		t.Errorf("文件应覆盖环境变量，SQSQueueURL=%q", cfg.SQSQueueURL)
	}
	if cfg.AWSRegion != "eu-west-1" || cfg.BucketMap["gcsA"] != "s3A" {
		t.Errorf("未覆盖项应由环境兜底: region=%q map=%+v", cfg.AWSRegion, cfg.BucketMap)
	}
}

// TestLoadConfigMissingBucketMapFile 校验 BUCKET_MAP_FILE 必填（缺时报错）。
func TestLoadConfigMissingBucketMapFile(t *testing.T) {
	p := writeTemp(t, "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\n")
	if _, err := loadConfig(p); err == nil {
		t.Error("缺 BUCKET_MAP_FILE 时应报错，却成功")
	}
}

// TestLoadConfigEmptyMappingFile 校验映射文件只有注释（0 条有效）时报错。
func TestLoadConfigEmptyMappingFile(t *testing.T) {
	csv := writeTempFile(t, "map.csv", "# 仅注释，无有效条目\n\n")
	p := writeTemp(t, "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\n")
	if _, err := loadConfig(p); err == nil {
		t.Error("空映射文件应报错，却成功")
	}
}

// TestLoadConfigInventoryRegionFallback 校验 INVENTORY_S3_REGION 缺省回退 AWS_REGION、显式设置独立生效。
func TestLoadConfigInventoryRegionFallback(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	p := writeTemp(t, "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\n")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.InventoryS3Region != "us-west-2" {
		t.Errorf("INVENTORY_S3_REGION 缺省应回退 AWS_REGION，实际 %q", cfg.InventoryS3Region)
	}

	p2 := writeTemp(t, "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE="+csv+"\nINVENTORY_S3_REGION=us-east-1\n")
	cfg2, err := loadConfig(p2)
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg2.InventoryS3Region != "us-east-1" {
		t.Errorf("INVENTORY_S3_REGION 显式设置应生效，实际 %q", cfg2.InventoryS3Region)
	}
}

// TestParseBucketMapFile 校验桶映射 CSV 解析的各种情形。
func TestParseBucketMapFile(t *testing.T) {
	// 正常多行 + 注释 + 空行 + 含空格
	good := "# 表头注释\n\ngcsA, s3A , s3://inv/a/m.json\ngcsB,s3B,s3://inv/b/m.json\n"
	ms, err := parseBucketMapFile(writeTempFile(t, "g.csv", good))
	if err != nil {
		t.Fatalf("解析正常 CSV 报错: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d: %+v", len(ms), ms)
	}
	// 保序 + 字段 TrimSpace 正确
	if ms[0].GCSBucket != "gcsA" || ms[0].S3Bucket != "s3A" || ms[0].InventoryURI != "s3://inv/a/m.json" {
		t.Errorf("第 1 条不符: %+v", ms[0])
	}
	if ms[1].GCSBucket != "gcsB" {
		t.Errorf("顺序错误，第 2 条应为 gcsB: %+v", ms[1])
	}

	// 首行带 BOM 不污染首字段
	bom := "\uFEFFgcsA,s3A,s3://inv/a/m.json\n"
	if mb, err := parseBucketMapFile(writeTempFile(t, "bom.csv", bom)); err != nil || mb[0].GCSBucket != "gcsA" {
		t.Errorf("BOM 未剥离: %+v err=%v", mb, err)
	}

	// 列数 != 3 → 报错
	if _, err := parseBucketMapFile(writeTempFile(t, "c2.csv", "gcsA,s3A\n")); err == nil {
		t.Error("2 列应报错")
	}
	// 空字段 → 报错
	if _, err := parseBucketMapFile(writeTempFile(t, "empty.csv", "gcsA,,s3://x/m.json\n")); err == nil {
		t.Error("空字段应报错")
	}
	// inventory 非 s3:// → 报错
	if _, err := parseBucketMapFile(writeTempFile(t, "sch.csv", "gcsA,s3A,gs://x/m.json\n")); err == nil {
		t.Error("inventory 非 s3:// 应报错")
	}
	// 源桶重复 → 报错
	if _, err := parseBucketMapFile(writeTempFile(t, "dup.csv", "gcsA,s3A,s3://x/m.json\ngcsA,s3B,s3://y/m.json\n")); err == nil {
		t.Error("源桶重复应报错")
	}
	// inventory URI 重复（源桶不同）→ 报错（共用断点文件会漏发）
	if _, err := parseBucketMapFile(writeTempFile(t, "dupinv.csv", "gcsA,s3A,s3://x/m.json\ngcsB,s3B,s3://x/m.json\n")); err == nil {
		t.Error("inventory URI 重复应报错")
	}
	// 目标桶被多源映射（源桶/inventory 都不同）→ 允许（合并语义），仅提示，不报错
	if ms, err := parseBucketMapFile(writeTempFile(t, "sametgt.csv", "gcsA,s3X,s3://x/1.json\ngcsB,s3X,s3://x/2.json\n")); err != nil || len(ms) != 2 {
		t.Errorf("不同源桶映射到同一目标桶应允许（仅提示）: err=%v len=%d", err, len(ms))
	}
	// 文件不存在 → 报错
	if _, err := parseBucketMapFile(filepath.Join(t.TempDir(), "nope.csv")); err == nil {
		t.Error("文件不存在应报错")
	}
}

// TestLoadConfigInventoryKeyPrefix 校验清单 key 前缀重写：默认 parquet/→ops/inventory/，可覆盖。
func TestLoadConfigInventoryKeyPrefix(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	base := "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE=" + csv + "\n"

	// 不配 → 默认对
	cfg, err := loadConfig(writeTemp(t, base))
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.InventoryGCSKeyPrefix != "parquet/" || cfg.InventoryS3KeyPrefix != "ops/inventory/" {
		t.Errorf("默认前缀不符: gcs=%q s3=%q", cfg.InventoryGCSKeyPrefix, cfg.InventoryS3KeyPrefix)
	}

	// 显式覆盖
	cfg, err = loadConfig(writeTemp(t, base+"INVENTORY_GCS_KEY_PREFIX=raw/\nINVENTORY_S3_KEY_PREFIX=mirror/inv/\n"))
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.InventoryGCSKeyPrefix != "raw/" || cfg.InventoryS3KeyPrefix != "mirror/inv/" {
		t.Errorf("覆盖前缀不符: gcs=%q s3=%q", cfg.InventoryGCSKeyPrefix, cfg.InventoryS3KeyPrefix)
	}
}

// TestParseSize 校验带单位大小解析。
func TestParseSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"1024", 1024},
		{"1B", 1}, {"1KB", 1024}, {"100KB", 102400}, {"1MB", 1 << 20}, {"100MB", 104857600},
		{"1GB", 1 << 30}, {"1TB", 1 << 40},
		{"1KiB", 1024}, {"1K", 1024}, {"1MiB", 1 << 20}, {"1M", 1 << 20},
		{"1mb", 1 << 20}, {"1Mb", 1 << 20}, {" 1gb", 1 << 30}, {"1GB ", 1 << 30},
		{"1.5GB", 1610612736}, {"0.5KB", 512},
	}
	for _, c := range ok {
		got, err := parseSize(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseSize(%q) = (%d, %v), 期望 (%d, nil)", c.in, got, err, c.want)
		}
	}
	bad := []string{"", "abc", "-5MB", "100XB", "MB", "99999999999999999999GB"}
	for _, in := range bad {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) 应报错，却成功", in)
		}
	}
}

// TestLoadConfigSizeFilter 校验 MIN_SIZE/MAX_SIZE 的加载与校验。
func TestLoadConfigSizeFilter(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	base := "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE=" + csv + "\n"

	// 都不配 → 不启用，[0, MaxInt64]
	cfg, err := loadConfig(writeTemp(t, base))
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.hasSizeFilter || cfg.MinSize != 0 || cfg.MaxSize != math.MaxInt64 {
		t.Errorf("未配过滤时应 [0,MaxInt64] 且不启用: %+v", cfg)
	}

	// 只配 MIN
	cfg, err = loadConfig(writeTemp(t, base+"MIN_SIZE=100KB\n"))
	if err != nil || !cfg.hasSizeFilter || cfg.MinSize != 102400 || cfg.MaxSize != math.MaxInt64 {
		t.Errorf("只配 MIN 不符: min=%d max=%d on=%v err=%v", cfg.MinSize, cfg.MaxSize, cfg.hasSizeFilter, err)
	}

	// 只配 MAX
	cfg, err = loadConfig(writeTemp(t, base+"MAX_SIZE=200KB\n"))
	if err != nil || !cfg.hasSizeFilter || cfg.MinSize != 0 || cfg.MaxSize != 204800 {
		t.Errorf("只配 MAX 不符: min=%d max=%d on=%v err=%v", cfg.MinSize, cfg.MaxSize, cfg.hasSizeFilter, err)
	}

	// 都配（含区间）
	cfg, err = loadConfig(writeTemp(t, base+"MIN_SIZE=100KB\nMAX_SIZE=100MB\n"))
	if err != nil || cfg.MinSize != 102400 || cfg.MaxSize != 104857600 {
		t.Errorf("区间配置不符: min=%d max=%d err=%v", cfg.MinSize, cfg.MaxSize, err)
	}

	// MIN==MAX 合法
	if _, err := loadConfig(writeTemp(t, base+"MIN_SIZE=1MB\nMAX_SIZE=1MB\n")); err != nil {
		t.Errorf("MIN==MAX 应合法: %v", err)
	}
	// MIN>MAX 报错
	if _, err := loadConfig(writeTemp(t, base+"MIN_SIZE=2MB\nMAX_SIZE=1MB\n")); err == nil {
		t.Error("MIN>MAX 应报错")
	}
	// 非法单位报错
	if _, err := loadConfig(writeTemp(t, base+"MIN_SIZE=100XB\n")); err == nil {
		t.Error("非法单位应报错")
	}
}

// TestLoadConfigLogS3URI 校验 LOG_S3_URI 的加载与预解析。
func TestLoadConfigLogS3URI(t *testing.T) {
	csv := writeTempFile(t, "map.csv", oneMapping)
	base := "SQS_QUEUE_URL=https://q\nAWS_REGION=us-west-2\nBUCKET_MAP_FILE=" + csv + "\n"

	// 不配 → bucket/prefix 空
	cfg, err := loadConfig(writeTemp(t, base))
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.LogS3Bucket != "" || cfg.LogS3Prefix != "" {
		t.Errorf("未配 LOG_S3_URI 时 bucket/prefix 应空: %+v", cfg)
	}

	// 配 s3://logb/pre → 预解析正确
	cfg, err = loadConfig(writeTemp(t, base+"LOG_S3_URI=s3://logb/pre/sub\n"))
	if err != nil {
		t.Fatalf("loadConfig 报错: %v", err)
	}
	if cfg.LogS3Bucket != "logb" || cfg.LogS3Prefix != "pre/sub" {
		t.Errorf("LOG_S3_URI 预解析不符: bucket=%q prefix=%q", cfg.LogS3Bucket, cfg.LogS3Prefix)
	}

	// 非法（非 s3://）→ 报错
	if _, err := loadConfig(writeTemp(t, base+"LOG_S3_URI=gs://x/y\n")); err == nil {
		t.Error("非法 LOG_S3_URI 应报错")
	}
}

// TestParseEnvFileQuotesAndComments 校验引号剥离与注释/空行忽略。
func TestParseEnvFileQuotesAndComments(t *testing.T) {
	p := writeTemp(t, "  # 头注释\nKEY1=\"quoted value\"\nKEY2='single'\n\nKEY3=plain\n")
	m, err := parseEnvFile(p)
	if err != nil {
		t.Fatalf("parseEnvFile 报错: %v", err)
	}
	if m["KEY1"] != "quoted value" || m["KEY2"] != "single" || m["KEY3"] != "plain" {
		t.Errorf("解析不符: %+v", m)
	}
}
