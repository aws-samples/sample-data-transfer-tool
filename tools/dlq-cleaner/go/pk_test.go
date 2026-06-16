package main

import "testing"

// golden 值由真实 Python migration.status_store.make_pk 生成（2026-06-13）。
// 这是字节级一致性的铁证：Go makePK 必须与生产 Python 逐字节相同，
// 否则算出的 PK 在 DDB 查不到任何 attempt，整个工具失效。
func TestMakePKParityWithPython(t *testing.T) {
	cases := map[string]string{
		"": "126#",
		"gcs:eu-abc-dw/libs/hive/warehouse/dw.db/x/part-00275.orc": "184#gcs:eu-abc-dw/libs/hive/warehouse/dw.db/x/part-00275.orc",
		"s3:migration-dest-784682930398-eu-south-2/small/sm_1.bin": "38#s3:migration-dest-784682930398-eu-south-2/small/sm_1.bin",
		"gcs:bucket/数据/文件 🚀.txt":                                   "216#gcs:bucket/数据/文件 🚀.txt", // 非 ASCII + emoji + 空格
		"s3:b/a//b/leading.bin":                                    "147#s3:b/a//b/leading.bin",  // 连续斜杠
		"gcs:eu-abc-dw/dt=20250724/days=30/p.c000":                 "26#gcs:eu-abc-dw/dt=20250724/days=30/p.c000",
	}
	for src, want := range cases {
		if got := makePK(src); got != want {
			t.Errorf("makePK(%q) = %q, 与 Python golden 不符 want %q", src, got, want)
		}
	}
}
