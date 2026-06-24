package main

import (
	"encoding/json"
	"regexp"
	"testing"
)

// compileTestRegexes 编译 testDest 里 regex 路由的 re 字段（真实路径由 loadConfig 完成；
// 单测直接构造 Dest，需手动编译以模拟运行时）。
func init() {
	for _, rule := range testDest.BucketMapping {
		for j := range rule.PrefixRoutes {
			if pr := &rule.PrefixRoutes[j]; pr.Regex != "" {
				pr.re = regexp.MustCompile(pr.Regex)
			}
		}
	}
}

// 测试用映射表：普通桶（写法 A）+ 带 prefix 的普通桶 + 前缀路由桶（写法 B）。
var testDest = Dest{
	QueueURL: "https://sqs/q",
	BucketMapping: map[string]BucketRule{
		"eu-abc-dw":             {S3Bucket: "s3euprodabcdw", Prefix: "migrated"},
		"ssmp-gts-euprd-bucket": {S3Bucket: "s3euprodgtseuprdbucket"},
		"special-shared-bucket": {
			PrefixRoutes: []PrefixRoute{
				{Prefix: "hot/", S3Bucket: "s3-hot"},
				{Prefix: "hot/2026/", S3Bucket: "s3-hot-2026"}, // 更长前缀
				{Prefix: "cold/", S3Bucket: "s3-cold"},
			},
			DefaultS3Bucket: "s3-misc",
		},
		"no-default-bucket": {
			PrefixRoutes: []PrefixRoute{{Prefix: "x/", S3Bucket: "s3-x"}},
			// 无 default：未命中前缀 → 未知桶跳过
		},
		// tmseu 风格：字面前缀 + regex 兜底（根文件/日期目录），无 default → 其余 skip。
		"tmseu-style": {
			PrefixRoutes: []PrefixRoute{
				{Prefix: "lods/", S3Bucket: "s3-lods"},
				{Prefix: "label", S3Bucket: "s3-lrms"},          // 无斜杠前缀
				{Prefix: "no_shippingno_", S3Bucket: "s3-lrms"}, // 无斜杠前缀
				{Regex: `^[^/]+$`, S3Bucket: "s3-lods"},         // 根目录文件（无子目录）
				{Regex: `^[0-9]{8}/`, S3Bucket: "s3-lods"},      // {yyyyMMdd}/ 日期目录（恰好 8 位）
			},
			// 无 default_s3_bucket：未命中任何 prefix/regex → skip 不传输
		},
		// mallfile-gcp 风格：strip_prefix 剥掉匹配前缀段 + 兜底
		"mallfile-gcp": {
			PrefixRoutes: []PrefixRoute{
				{Prefix: "gcsprodorms/", S3Bucket: "s3euprodorms", StripPrefix: true},
				{Prefix: "riskgcp/", S3Bucket: "s3euprodrisk1", StripPrefix: true},
			},
			DefaultS3Bucket: "s3euprodfs",
		},
	},
}

func mustMsg(t *testing.T, body string) migrationMessage {
	t.Helper()
	var m migrationMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body 不是合法 JSON: %v", err)
	}
	return m
}

// rawKeys 把 body 解到 map，用于断言「op 字段是否存在」——
// 仅 unmarshal 到 struct 时 {"op":""} 和无 op 都得空串，区分不出会 poison worker 的错。
func rawKeys(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body 不是合法 JSON: %v", err)
	}
	return m
}

// ── 写法 A：整桶映射 ──────────────────────────────────────────────
func TestMapFinalizePlainBucket(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "ssmp-gts-euprd-bucket",
		"objectId": "libs/hive/part-00275.orc"}
	got, err := mapEvent(attrs, []byte(`{"size":"1048576"}`), testDest)
	if err != nil {
		t.Fatal(err)
	}
	if got.skip || got.unknownBkt {
		t.Fatal("应正常映射")
	}
	if got.ObjectSize != 1048576 {
		t.Errorf("size=%d", got.ObjectSize)
	}
	m := mustMsg(t, got.Body)
	if m.Source != "gcs:ssmp-gts-euprd-bucket/libs/hive/part-00275.orc" {
		t.Errorf("source=%q", m.Source)
	}
	if m.Destination != "s3:s3euprodgtseuprdbucket/libs/hive/part-00275.orc" {
		t.Errorf("destination=%q", m.Destination)
	}
}

func TestMapPlainBucketWithPrefix(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "eu-abc-dw", "objectId": "dt=2026/p.orc"}
	got, _ := mapEvent(attrs, []byte(`{"size":"10"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3euprodabcdw/migrated/dt=2026/p.orc" {
		t.Errorf("带 prefix destination=%q want s3:s3euprodabcdw/migrated/dt=2026/p.orc", m.Destination)
	}
}

func TestMapMetadataUpdateIsRefresh(t *testing.T) {
	// METADATA_UPDATE = 源端只改 metadata、数据未变 → 必须 op=refresh（rclone copyto
	// --ignore-times 强制重传），否则普通 copy 被 rclone 按 size/mtime skip，metadata 刷不到目标。
	attrs := map[string]string{"eventType": eventMetaUpdate, "bucketId": "ssmp-gts-euprd-bucket", "objectId": "k.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"5"}`), testDest)
	if got.skip || got.unknownBkt {
		t.Fatal("METADATA_UPDATE 应映射为 refresh（非 skip）")
	}
	m := mustMsg(t, got.Body)
	if m.Op != "refresh" {
		t.Errorf("METADATA_UPDATE 的 op 应为 refresh，got %q", m.Op)
	}
	if m.Source != "gcs:ssmp-gts-euprd-bucket/k.bin" {
		t.Errorf("source=%q（refresh 必须带 source）", m.Source)
	}
	if m.Destination != "s3:s3euprodgtseuprdbucket/k.bin" {
		t.Errorf("destination=%q", m.Destination)
	}
	// raw JSON 必须含 op key（worker 据此判 refresh；缺则退化成 copy → metadata 刷不到）
	if _, ok := rawKeys(t, got.Body)["op"]; !ok {
		t.Error("refresh body 的 raw JSON 必须含 op key")
	}
	// refresh 是 copy 路径，size 不能丢（worker 大小路由需要）
	if got.ObjectSize != 5 {
		t.Errorf("refresh 应保留 ObjectSize，got %d", got.ObjectSize)
	}
}

func TestMapFinalizeOmitsOpKey(t *testing.T) {
	// 对照：FINALIZE → copy，body 的 raw JSON 必须【不含】op key（omitempty）。
	// 否则 copy 误带 op="" 会让 worker 的 Op("") 解析异常。
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "ssmp-gts-euprd-bucket", "objectId": "k.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"5"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Op != "" {
		t.Errorf("FINALIZE 的 op 应为空（copy），got %q", m.Op)
	}
	if _, ok := rawKeys(t, got.Body)["op"]; ok {
		t.Error("copy body 的 raw JSON 不应含 op key（omitempty 省略）")
	}
}

// ── 写法 B：前缀路由 ──────────────────────────────────────────────
func TestPrefixRouteLongestMatch(t *testing.T) {
	// hot/2026/x 同时匹配 "hot/" 和 "hot/2026/"，最长优先 → s3-hot-2026
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "special-shared-bucket",
		"objectId": "hot/2026/x.orc"}
	got, _ := mapEvent(attrs, []byte(`{"size":"3"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3-hot-2026/hot/2026/x.orc" {
		t.Errorf("最长前缀应命中 s3-hot-2026，got %q", m.Destination)
	}
}

func TestPrefixRouteShortMatch(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "special-shared-bucket",
		"objectId": "hot/other.orc"} // 只匹配 hot/
	got, _ := mapEvent(attrs, []byte(`{"size":"3"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3-hot/hot/other.orc" {
		t.Errorf("got %q want s3:s3-hot/hot/other.orc（保留完整 key）", m.Destination)
	}
}

func TestPrefixRouteDefault(t *testing.T) {
	// 不命中任何前缀 → 兜底 default_s3_bucket
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "special-shared-bucket",
		"objectId": "warm/y.orc"}
	got, _ := mapEvent(attrs, []byte(`{"size":"3"}`), testDest)
	if got.unknownBkt {
		t.Fatal("有 default 不应判未知")
	}
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3-misc/warm/y.orc" {
		t.Errorf("兜底 destination=%q want s3:s3-misc/warm/y.orc", m.Destination)
	}
}

func TestPrefixRouteStripPrefix(t *testing.T) {
	// mallfile-gcp: 命中 gcsprodorms/ 且 strip_prefix → 剥掉前缀段
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "mallfile-gcp",
		"objectId": "gcsprodorms/2026/order-001.json"}
	got, _ := mapEvent(attrs, []byte(`{"size":"7"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Source != "gcs:mallfile-gcp/gcsprodorms/2026/order-001.json" {
		t.Errorf("source 应保留原 GCS 完整 key: %q", m.Source)
	}
	if m.Destination != "s3:s3euprodorms/2026/order-001.json" {
		t.Errorf("strip_prefix 后 destination=%q want s3:s3euprodorms/2026/order-001.json（剥掉 gcsprodorms/）", m.Destination)
	}
}

func TestPrefixRouteStripToDefault(t *testing.T) {
	// mallfile-gcp: 不命中任何前缀 → 兜底 s3euprodfs，保留完整 key（不剥）
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "mallfile-gcp",
		"objectId": "unknownprefix/x.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"7"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3euprodfs/unknownprefix/x.bin" {
		t.Errorf("兜底 destination=%q want s3:s3euprodfs/unknownprefix/x.bin（保留完整 key）", m.Destination)
	}
}

func TestPrefixRouteNoMatchNoDefault(t *testing.T) {
	// 无 default 且不命中 → 未知桶跳过
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "no-default-bucket",
		"objectId": "zzz/y.orc"}
	got, _ := mapEvent(attrs, nil, testDest)
	if !got.unknownBkt {
		t.Fatalf("无 default 不命中应判 unknownBkt，got %+v", got)
	}
}

// ── 写法 B + regex 兜底（tmseu 风格）─────────────────────────────
// 路由优先级：字面 prefix（最长优先）> regex（按序）> 无 default 则 skip。
func TestRegexRouteFallback(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		wantDest string // 空串 = 期望 skip(unknownBkt)
	}{
		// 字面前缀命中（优先于 regex）
		{"命中 lods/ 前缀", "lods/a/b.dat", "s3:s3-lods/lods/a/b.dat"},
		{"命中 label 无斜杠前缀", "label_001.png", "s3:s3-lrms/label_001.png"},
		{"命中 no_shippingno_ 前缀", "no_shippingno_x", "s3:s3-lrms/no_shippingno_x"},
		// regex 兜底：根目录文件（无 /）
		{"根目录普通文件 → lods", "report.csv", "s3:s3-lods/report.csv"},
		{"根目录无扩展名 → lods", "abc", "s3:s3-lods/abc"},
		// 边界：1label_001.png 以 '1' 开头,不命中 label 前缀,但无 / → 根文件兜底
		{"1label_001 根文件兜底(非 label)", "1label_001.png", "s3:s3-lods/1label_001.png"},
		// regex 兜底：{yyyyMMdd}/ 恰好 8 位日期目录
		{"日期目录 20260608/ → lods", "20260608/x.dat", "s3:s3-lods/20260608/x.dat"},
		{"日期目录 20260624/sub/y → lods", "20260624/sub/y", "s3:s3-lods/20260624/sub/y"},
		// 带时分秒的不管：14 位不匹配 ^[0-9]{8}/,且含 / 非根文件 → skip
		{"带时分秒 20260608120000/ → skip", "20260608120000/x", ""},
		// 未知前缀(非根文件、非日期目录、未命中字面前缀)→ skip
		{"未知前缀 weird/x → skip", "weird/x.bin", ""},
		{"7 位数字目录(非 8 位)→ skip", "2026060/x", ""},
		{"9 位数字目录(非 8 位)→ skip", "202606081/x", ""},
	}
	for _, c := range cases {
		attrs := map[string]string{"eventType": eventFinalize, "bucketId": "tmseu-style", "objectId": c.key}
		got, err := mapEvent(attrs, []byte(`{"size":"1"}`), testDest)
		if err != nil {
			t.Errorf("%s: 意外错误 %v", c.name, err)
			continue
		}
		if c.wantDest == "" {
			if !got.unknownBkt {
				t.Errorf("%s: key=%q 应 skip(unknownBkt),got body=%s", c.name, c.key, got.Body)
			}
			continue
		}
		if got.unknownBkt || got.skip {
			t.Errorf("%s: key=%q 应命中 %s,却被跳过", c.name, c.key, c.wantDest)
			continue
		}
		m := mustMsg(t, got.Body)
		if m.Destination != c.wantDest {
			t.Errorf("%s: key=%q destination=%q want %q", c.name, c.key, m.Destination, c.wantDest)
		}
	}
}

// ── 未知桶 / 跳过 / 边界 ──────────────────────────────────────────
func TestUnknownBucketSkipped(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "never-configured-bucket",
		"objectId": "k"}
	got, err := mapEvent(attrs, nil, testDest)
	if err != nil {
		t.Fatal(err)
	}
	if !got.unknownBkt {
		t.Fatal("未配映射的桶应判 unknownBkt（跳过+告警）")
	}
	if got.unknownInfo == "" {
		t.Error("unknownInfo 应含桶名/key 供日志")
	}
}

func TestOtherEventsSkipped(t *testing.T) {
	for _, et := range []string{"OBJECT_DELETE", "OBJECT_ARCHIVE", "OBJECT_INITIALIZE", ""} {
		got, _ := mapEvent(map[string]string{"eventType": et, "bucketId": "ssmp-gts-euprd-bucket", "objectId": "k"}, nil, testDest)
		if !got.skip {
			t.Errorf("%s 应跳过", et)
		}
	}
}

func TestFinalizeMissingFields(t *testing.T) {
	got, err := mapEvent(map[string]string{"eventType": eventFinalize, "bucketId": "ssmp-gts-euprd-bucket"}, []byte(`{"size":"1"}`), testDest)
	if err == nil && !got.skip {
		t.Error("缺 objectId 应报错")
	}
}

func TestBadPayloadSize(t *testing.T) {
	if _, err := mapEvent(map[string]string{"eventType": eventFinalize, "bucketId": "ssmp-gts-euprd-bucket", "objectId": "k"}, []byte(`{bad`), testDest); err == nil {
		t.Error("坏 payload 应报错")
	}
}

func TestSpecialCharKeyPreserved(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "ssmp-gts-euprd-bucket", "objectId": "数据/a b//c.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"5"}`), testDest)
	m := mustMsg(t, got.Body)
	if m.Destination != "s3:s3euprodgtseuprdbucket/数据/a b//c.bin" {
		t.Errorf("特殊字符 key 未字面保留: %q", m.Destination)
	}
}

func TestParseSizeEmpty(t *testing.T) {
	if n, err := parseSize(nil); err != nil || n != 0 {
		t.Errorf("空 payload n=%d err=%v", n, err)
	}
}
