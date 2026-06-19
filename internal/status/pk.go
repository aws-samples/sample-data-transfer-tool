// Package status 实现 DynamoDB 状态层：终态记录、心跳、分片主键。
package status

import (
	"crypto/md5" // #nosec G501 -- 非安全用途，仅用于打散分片，必须与 Python hashlib.md5 字节级一致
	"encoding/hex"
	"fmt"
	"math/big"
)

// shardCount 分片数：source 经 md5 取模打散，避免写入集中到单个热分区。
const shardCount = 256

// MakePK 生成稳定分区键 "<分片前缀>#<source>"。
//
// ⚠️ 死穴：必须与 Python status_store.make_pk 字节级一致，否则 migration-cli /
// ddb-inspect 按 source 重建 PK 时查不到 Go worker 写的行。
// Python 实现：shard = int(md5(source).hexdigest(), 16) % 256，前缀 = str(shard)。
// 注意 md5 十六进制摘要是 128-bit 整数，远超 int64，必须用 big.Int 取模
// （不能截断高位——那会得到与 Python 不同的 shard）。golden 值见 pk_test.go。
func MakePK(source string) string {
	sum := md5.Sum([]byte(source)) // #nosec G401 -- 与 Python 对拍，刻意用 md5
	hexDigest := hex.EncodeToString(sum[:])
	// 把完整 128-bit 十六进制摘要当大整数解析后 % 256。
	n := new(big.Int)
	n.SetString(hexDigest, 16)
	shard := new(big.Int).Mod(n, big.NewInt(shardCount))
	return fmt.Sprintf("%s#%s", shard.String(), source)
}
