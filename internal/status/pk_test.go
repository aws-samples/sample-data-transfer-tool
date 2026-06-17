package status

import "testing"

// golden 值由 Python 真实输出生成（PYTHONPATH=src python3 -c
// "from migration.status_store import make_pk; ..."），逐字节锁定。
// 任何改动导致这些值变化 = 与 Python 分片不一致 = inspect 查不到行 → 必须红。
func TestMakePK_GoldenParityWithPython(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{
			"s3:migration-dest-784682930398-eu-south-2/stress-large/x.orc",
			"249#s3:migration-dest-784682930398-eu-south-2/stress-large/x.orc",
		},
		{
			"gcs:eu-abc-dw/libs/hive/warehouse/dmp.db/t/dt=20260603/000054_0",
			"13#gcs:eu-abc-dw/libs/hive/warehouse/dmp.db/t/dt=20260603/000054_0",
		},
		// 含 // 和多重斜杠：字面保留（不归一化）
		{"s3:bucket/a//b///c.bin", "179#s3:bucket/a//b///c.bin"},
		// 非 ASCII（中文 UTF-8 字节）
		{"s3:bucket/前缀/数据.bin", "9#s3:bucket/前缀/数据.bin"},
		// 前导双斜杠
		{"gcs:b/leading//double", "235#gcs:b/leading//double"},
		// 空串
		{"", "126#"},
		// 含空格
		{"s3:b/key with spaces.txt", "109#s3:b/key with spaces.txt"},
	}
	for _, c := range cases {
		if got := MakePK(c.source); got != c.want {
			t.Errorf("MakePK(%q)\n  got  %q\n  want %q (Python golden)", c.source, got, c.want)
		}
	}
}

// 长 key（500 个 x）单独一例，避免上面表格太宽。
func TestMakePK_LongKey(t *testing.T) {
	src := "s3:b/"
	for i := 0; i < 500; i++ {
		src += "x"
	}
	want := "83#" + src
	if got := MakePK(src); got != want {
		t.Errorf("long key shard mismatch:\n  got  %q\n  want %q", got[:10], want[:10])
	}
}
