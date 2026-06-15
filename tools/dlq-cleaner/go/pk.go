package main

import (
	"crypto/md5" //nolint:gosec // 非安全用途，仅用于分片打散，与 Python status_store.make_pk 对齐
	"fmt"
)

// shardCount 与 Python status_store._SHARD_COUNT 一致。
const shardCount = 256

// makePK 复刻 Python status_store.make_pk：返回 "<md5(source) % 256>#<source>"。
//
// ⚠️ 字节级一致性死穴：Python 用 int(md5_hexdigest, 16) % 256。md5 是 16 字节，
// 对 256 取模等价于「取 md5 原始字节的最后一个字节」(最低 8 bit)。Go 里就是
// sum[15]。两者必须逐字节一致，否则算出的 PK 在 DDB 里查不到任何 attempt。
// 已用真实样本对拍：md5("")=...427e → 0x7e=126 → "126#"（与生产 DDB 实测一致）。
func makePK(source string) string {
	sum := md5.Sum([]byte(source)) //nolint:gosec
	shard := int(sum[15])          // == int(hexdigest,16) % 256
	return fmt.Sprintf("%d#%s", shard, source)
}
