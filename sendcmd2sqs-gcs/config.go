package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// ============ 运行时配置（部署相关，从 --config 文件 / 环境变量加载）============

// bucketMapping 是一条桶映射：源 GCS 桶 → 目标 S3 桶，外加该桶 inventory 清单的位置。
// 来自 BUCKET_MAP_FILE 指向的 CSV（每行 gcs桶,s3桶,inventoryURI）。
type bucketMapping struct {
	GCSBucket    string // 源 GCS 桶
	S3Bucket     string // 目标 S3 桶
	InventoryURI string // 该桶 inventory 清单的 s3:// URI
}

// Config 持有部署相关的可变配置。这些值随环境/账号变化，故不硬编码在二进制里，
// 而是运行时从 dotenv 风格的配置文件或环境变量加载。
type Config struct {
	SQSQueueURL string // 目标 SQS 队列 URL（必填）
	AWSRegion   string // SQS 所在 AWS region（必填）
	GCSRemote   string // rclone 远端名，须与消费端 rclone.conf 中的 gcs remote 同名（默认 gcs）

	// Mappings 是有序的桶映射列表（来自 BUCKET_MAP_FILE，必填、至少一条）。
	// 主循环按此顺序遍历每个桶、处理其各自的 inventory。
	Mappings []bucketMapping
	// BucketMap 是由 Mappings 派生的 源GCS桶 → 目标S3桶 映射，供 pipeline 查表（O(1)）。
	// destination 由每行的源 GCS 桶经此表派生；未命中的源桶会令该 shard fail-fast。
	BucketMap map[string]string

	// InventoryS3Region 是清单（manifest + shard）所在 AWS S3 桶的 region。
	// 清单桶可能与 SQS 跨区域，故单独配置；为空时回退 AWSRegion。
	InventoryS3Region string

	// 清单 shard 从 GCS 同步到 S3 时的 key 前缀重写（见 inventory.go parseManifest）。
	// manifest 的 reportNames 是 gs://<桶>/<gcs前缀>... 形式；S3 上的副本 key 把 <gcs前缀>
	// 换成了 <s3前缀>。映射 shard 时：剥掉 gs:// 桶名 → 把 InventoryGCSKeyPrefix 换成
	// InventoryS3KeyPrefix → 拼到 manifest 所在 S3 桶。
	// 默认 "parquet/" → "ops/inventory/"（当前部署）。两者相等（含都为空）时不重写，退化为纯换桶。
	InventoryGCSKeyPrefix string
	InventoryS3KeyPrefix  string

	// size 区间过滤（可选，闭区间 [MinSize, MaxSize]，单位字节）。
	// hasSizeFilter=false 时完全不读 parquet 的 size 列（向后兼容）。
	// 哨兵 0 / MaxInt64 是闭区间的自然单位元，使比较无需特判哪侧有界。
	MinSize       int64 // 下界；未配 MIN_SIZE 时为 0
	MaxSize       int64 // 上界；未配 MAX_SIZE 时为 math.MaxInt64
	hasSizeFilter bool  // 是否配置了 MIN_SIZE 或 MAX_SIZE 之一（控制是否读 size 列）

	// 桶日志可选上传 S3。LOG_S3_URI 配置项在启动时预解析为 LogS3Bucket + LogS3Prefix
	// （prefix 可空）；两者皆空表示只写本地不上传。供每桶上传直接用。
	LogS3Bucket string
	LogS3Prefix string
}

// 配置项的 key 名（同时用作配置文件键名和环境变量名）。
const (
	keySQSQueueURL       = "SQS_QUEUE_URL"
	keyAWSRegion         = "AWS_REGION"
	keyGCSRemote         = "GCS_REMOTE"
	keyBucketMapFile     = "BUCKET_MAP_FILE"
	keyInventoryS3Region = "INVENTORY_S3_REGION"
	keyMinSize           = "MIN_SIZE"
	keyMaxSize           = "MAX_SIZE"
	keyLogS3URI          = "LOG_S3_URI"
	keyInvGCSKeyPrefix   = "INVENTORY_GCS_KEY_PREFIX"
	keyInvS3KeyPrefix    = "INVENTORY_S3_KEY_PREFIX"
)

// 清单 key 前缀重写的默认值（当前部署：GCS 上 parquet/ → S3 上 ops/inventory/）。
const (
	defaultInvGCSKeyPrefix = "parquet/"
	defaultInvS3KeyPrefix  = "ops/inventory/"
)

// loadConfig 加载全量迁移模式的部署配置（BUCKET_MAP_FILE 必填）。
func loadConfig(configPath string) (*Config, error) {
	return loadConfigForMode(configPath, false)
}

// loadConfigForMode 加载部署配置：先读环境变量，再用 --config 文件覆盖（文件优先）。
// configPath 为空表示仅用环境变量。返回前校验必填项。
//
// incremental（增量迁移模式）为 true 时：BUCKET_MAP_FILE 不必填、且即便配置了也忽略不解析
// （桶名来自 diff 文件名，见 incremental.go），cfg.Mappings/BucketMap 留空；其余配置项
// （SQS_QUEUE_URL/AWS_REGION 必填，size 过滤、日志上传）两模式共用。
func loadConfigForMode(configPath string, incremental bool) (*Config, error) {
	// 1) 环境变量兜底
	vals := map[string]string{
		keySQSQueueURL:       os.Getenv(keySQSQueueURL),
		keyAWSRegion:         os.Getenv(keyAWSRegion),
		keyGCSRemote:         os.Getenv(keyGCSRemote),
		keyBucketMapFile:     os.Getenv(keyBucketMapFile),
		keyInventoryS3Region: os.Getenv(keyInventoryS3Region),
		keyMinSize:           os.Getenv(keyMinSize),
		keyMaxSize:           os.Getenv(keyMaxSize),
		keyLogS3URI:          os.Getenv(keyLogS3URI),
		keyInvGCSKeyPrefix:   os.Getenv(keyInvGCSKeyPrefix),
		keyInvS3KeyPrefix:    os.Getenv(keyInvS3KeyPrefix),
	}

	// 2) 配置文件覆盖
	if configPath != "" {
		fileVals, err := parseEnvFile(configPath)
		if err != nil {
			return nil, err
		}
		for k, v := range fileVals {
			vals[k] = v
		}
	}

	cfg := &Config{
		SQSQueueURL:       strings.TrimSpace(vals[keySQSQueueURL]),
		AWSRegion:         strings.TrimSpace(vals[keyAWSRegion]),
		GCSRemote:         strings.TrimSpace(vals[keyGCSRemote]),
		InventoryS3Region: strings.TrimSpace(vals[keyInventoryS3Region]),
	}
	if cfg.GCSRemote == "" {
		cfg.GCSRemote = "gcs" // 默认值
	}
	if cfg.InventoryS3Region == "" {
		cfg.InventoryS3Region = cfg.AWSRegion // 清单桶 region 未单独配置时回退 SQS region
	}

	// 清单 key 前缀重写：两项都为空时用默认对（parquet/ → ops/inventory/）；任一非空则按配置取值。
	// （env 未设与显式设为空在此处不可区分——都视为"未配"，故落回默认；要改重写规则就显式配两项。）
	cfg.InventoryGCSKeyPrefix = strings.TrimSpace(vals[keyInvGCSKeyPrefix])
	cfg.InventoryS3KeyPrefix = strings.TrimSpace(vals[keyInvS3KeyPrefix])
	if cfg.InventoryGCSKeyPrefix == "" && cfg.InventoryS3KeyPrefix == "" {
		cfg.InventoryGCSKeyPrefix = defaultInvGCSKeyPrefix
		cfg.InventoryS3KeyPrefix = defaultInvS3KeyPrefix
	}
	bucketMapFile := strings.TrimSpace(vals[keyBucketMapFile])

	// 3) 必填校验。SQS_QUEUE_URL/AWS_REGION 两模式都必填；BUCKET_MAP_FILE 仅全量模式必填。
	var missing []string
	if cfg.SQSQueueURL == "" {
		missing = append(missing, keySQSQueueURL)
	}
	if cfg.AWSRegion == "" {
		missing = append(missing, keyAWSRegion)
	}
	if !incremental && bucketMapFile == "" {
		missing = append(missing, keyBucketMapFile)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少必填配置项 %s（请在 --config 文件或环境变量中设置）", strings.Join(missing, ", "))
	}

	// 4) 解析桶映射 CSV 文件，并派生 gcs桶→s3桶 查表。
	// 增量模式桶名来自 diff 文件名，不依赖 BUCKET_MAP_FILE：即便误配也忽略，Mappings/BucketMap 留空。
	if !incremental {
		mappings, err := parseBucketMapFile(bucketMapFile)
		if err != nil {
			return nil, err
		}
		if len(mappings) == 0 {
			return nil, fmt.Errorf("%s 中没有有效的桶映射条目: %s", keyBucketMapFile, bucketMapFile)
		}
		cfg.Mappings = mappings
		cfg.BucketMap = make(map[string]string, len(mappings))
		for _, m := range mappings {
			cfg.BucketMap[m.GCSBucket] = m.S3Bucket
		}
	}

	// 5) size 区间过滤（可选）。哨兵默认 [0, MaxInt64]；任一配了即启用过滤。
	cfg.MinSize = 0
	cfg.MaxSize = math.MaxInt64
	if minRaw := strings.TrimSpace(vals[keyMinSize]); minRaw != "" {
		v, err := parseSize(minRaw)
		if err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", keyMinSize, err)
		}
		cfg.MinSize = v
		cfg.hasSizeFilter = true
	}
	if maxRaw := strings.TrimSpace(vals[keyMaxSize]); maxRaw != "" {
		v, err := parseSize(maxRaw)
		if err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", keyMaxSize, err)
		}
		cfg.MaxSize = v
		cfg.hasSizeFilter = true
	}
	if cfg.MinSize > cfg.MaxSize {
		return nil, fmt.Errorf("%s(%d 字节) 不能大于 %s(%d 字节)", keyMinSize, cfg.MinSize, keyMaxSize, cfg.MaxSize)
	}

	// 6) 桶日志上传目标（可选）。配了就预解析 bucket/prefix（fail-fast）。
	if logS3URI := strings.TrimSpace(vals[keyLogS3URI]); logS3URI != "" {
		b, p, err := parseS3Prefix(logS3URI)
		if err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", keyLogS3URI, err)
		}
		cfg.LogS3Bucket, cfg.LogS3Prefix = b, p
	}

	return cfg, nil
}

// parseSize 解析带单位的大小字符串为字节数。支持纯数字(=字节,可带小数)、
// B、KB/K/KiB、MB/M/MiB、GB/G/GiB、TB/T/TiB（不区分大小写；二进制基数 1KB=1024）。
// 负数/空串/未知单位/无数值/溢出报错。
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("size 字符串为空")
	}
	upper := strings.ToUpper(s)
	// 后缀按长到短匹配，确保 "KIB" 先于 "KB" 先于 "K"/"B"。
	units := []struct {
		suffix string
		mult   float64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	}
	mult := 1.0
	numPart := upper
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			numPart = strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
			break
		}
	}
	if numPart == "" {
		return 0, fmt.Errorf("size %q 缺少数值部分", s)
	}
	num, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q 数值部分非法: %w", s, err)
	}
	if num < 0 {
		return 0, fmt.Errorf("size %q 不能为负", s)
	}
	bytes := num * mult
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("size %q 超出 int64 范围", s)
	}
	return int64(bytes), nil
}

// parseBucketMapFile 解析 BUCKET_MAP_FILE 指向的 CSV：每行 "gcs桶,s3桶,inventoryURI"。
// 首行剥 UTF-8 BOM；# 注释行与空行跳过；每行恰好 3 段（段数 ≠3、任一段为空、inventoryURI
// 非 s3:// 前缀、源桶重复均报错）。返回有序列表（保留文件中的行序）。
func parseBucketMapFile(path string) ([]bucketMapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开桶映射文件失败 %s: %w", path, err)
	}
	defer f.Close()

	var out []bucketMapping
	seen := make(map[string]struct{})     // 已见的源 GCS 桶（去重用）
	seenInv := make(map[string]int)       // 已见的 inventory URI → 首次出现行号
	seenTarget := make(map[string]string) // 已见的目标 S3 桶 → 首个映射到它的源桶（仅提示用）
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		if lineNo == 1 {
			raw = strings.TrimPrefix(raw, "\uFEFF") // 剥 UTF-8 BOM
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("桶映射文件 %s 第 %d 行应为 3 列（gcs桶,s3桶,inventoryURI）: %q", path, lineNo, line)
		}
		gcsBucket := strings.TrimSpace(parts[0])
		s3Bucket := strings.TrimSpace(parts[1])
		inventoryURI := strings.TrimSpace(parts[2])
		if gcsBucket == "" || s3Bucket == "" || inventoryURI == "" {
			return nil, fmt.Errorf("桶映射文件 %s 第 %d 行有空字段: %q", path, lineNo, line)
		}
		if !strings.HasPrefix(inventoryURI, "s3://") {
			return nil, fmt.Errorf("桶映射文件 %s 第 %d 行 inventory 应以 s3:// 开头: %q", path, lineNo, inventoryURI)
		}
		if _, dup := seen[gcsBucket]; dup {
			return nil, fmt.Errorf("桶映射文件 %s 第 %d 行源桶重复: %q", path, lineNo, gcsBucket)
		}
		// inventory URI 复用是隐患：每桶断点按 inventory URI 派生，复用会共用同一断点文件、
		// 导致一个桶标记完成后另一个桶被误跳过。几乎都是复制粘贴忘改，直接报错。
		if firstLine, dup := seenInv[inventoryURI]; dup {
			return nil, fmt.Errorf("桶映射文件 %s 第 %d 行 inventory 与第 %d 行重复: %q", path, lineNo, firstLine, inventoryURI)
		}
		// 目标桶被多个源桶映射是合法的（多源合并到同一 S3 桶），但也常是笔误，给个非致命提示。
		if firstSrc, dup := seenTarget[s3Bucket]; dup {
			logf("提示：目标 S3 桶 %q 同时被源桶 %q 和 %q 映射（如非有意合并请检查 BUCKET_MAP_FILE）", s3Bucket, firstSrc, gcsBucket)
		} else {
			seenTarget[s3Bucket] = gcsBucket
		}
		seen[gcsBucket] = struct{}{}
		seenInv[inventoryURI] = lineNo
		out = append(out, bucketMapping{GCSBucket: gcsBucket, S3Bucket: s3Bucket, InventoryURI: inventoryURI})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取桶映射文件失败 %s: %w", path, err)
	}
	return out, nil
}

// parseEnvFile 解析 dotenv 风格配置文件：每行 KEY=value，# 注释与空行忽略，
// value 两端空白与可选的成对引号会被去除。
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开配置文件失败 %s: %w", path, err)
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		if lineNo == 1 {
			// 剥离 UTF-8 BOM（Windows 记事本等保存 UTF-8 时会加），否则首个键名被污染。
			raw = strings.TrimPrefix(raw, "\uFEFF")
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("配置文件 %s 第 %d 行格式错误（应为 KEY=value）: %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// 去除成对的首尾引号（"..." 或 '...'）
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			return nil, fmt.Errorf("配置文件 %s 第 %d 行键名为空: %q", path, lineNo, line)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}
	return out, nil
}

// ============ 编译期常量（事实/不变量，非部署配置，不外置）============

// 过滤文件名（约定）
const IgnoreFile = ".ignore-gcs"

// Inventory 报告必填列（Storage Insights 保证 project/bucket/name 一定存在）。
var RequiredColumns = []string{"bucket", "name"}

// SQS 批量发送配置（AWS 硬限制 + 调优值，极少改动，留在代码中）。
const (
	SQSMaxBatch        = 10 // send_message_batch 单次上限（AWS 硬限制）
	SQSBatchMaxRetries = 5  // 批内失败条目的手动重试次数
	SQSRetryWaitBase   = 1  // 重试退避基数秒：sleep = base * 2^attempt
)

// 并发默认值
const (
	// DefaultSenders 是默认发送 goroutine 数（= 并发在途 SQS 请求数）。
	// SQS 延迟受限（单批往返 ~45ms），吞吐 ≈ senders / 0.045 * 10 条/秒。
	// 300 → 约 8 万条/秒。可用 --senders 扫描 150/300/600 找拐点。
	DefaultSenders = 300
	// DefaultProducers 默认 0，表示运行时取 runtime.NumCPU()。
	DefaultProducers = 0
)
