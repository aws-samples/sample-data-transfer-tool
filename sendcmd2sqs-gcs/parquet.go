package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/parquet-go/parquet-go"
)

// shardReader 封装一个本地 Parquet shard 文件，支持按 row group 流式读取 bucket/name。
//
// 用行级 API（RowGroup.Rows + ReadRows）而非 struct 映射：Storage Insights 报告的列
// 可能是 optional/required 混合，行级 API 不受 schema 形态影响，最稳健；且按 row group
// 切分天然支持多 producer 并行解码（每个 producer 认领不同 row group）。
type shardReader struct {
	f         *os.File
	pf        *parquet.File
	bucketIdx int
	nameIdx   int
	sizeIdx   int // size 列叶子索引；-1 表示未启用 size（不读取、回调 size 恒为"未知"）
	// timeDeletedIdx 是 timeDeleted 列叶子索引；-1 表示清单无此列（旧清单兼容，回调 deleted 恒 false）。
	// 该列非 null 表示对象已被删除（GCS Storage Insights 对已删除对象保留记录），必须过滤不迁移。
	timeDeletedIdx int
}

// leafColumnIndex 在扁平 schema 中按列名找叶子列索引。
func leafColumnIndex(pf *parquet.File, name string) (int, bool) {
	for _, c := range pf.Root().Columns() {
		if c.Leaf() && c.Name() == name {
			return c.Index(), true
		}
	}
	return 0, false
}

// openParquetFile 打开本地 Parquet 文件：os.Open → Stat → parquet.OpenFile。
// 任一步失败都会关闭已打开的文件句柄并返回 error。成功时调用方负责 close 返回的 *os.File。
// 由 openShard（inventory）与 openDiffShard（diff，见 incremental.go）共用，避免打开骨架重复。
func openParquetFile(path string) (*os.File, *parquet.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 Parquet 文件失败 %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("stat Parquet 文件失败 %s: %w", path, err)
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("解析 Parquet 失败 %s: %w", path, err)
	}
	return f, pf, nil
}

// openShard 打开本地 Parquet 文件并定位 bucket / name 列索引。
// 列校验隐含于此：缺 bucket/name 列即返回 error（每个 shard 处理前都会经过）。
// needSize 为 true 时额外定位 size 列（缺列报错）；为 false 时完全不碰 size 列，
// sizeIdx=-1，向后兼容不含 size 列的旧清单。
func openShard(path string, needSize bool) (*shardReader, error) {
	f, pf, err := openParquetFile(path)
	if err != nil {
		return nil, err
	}

	bucketIdx, ok := leafColumnIndex(pf, "bucket")
	if !ok {
		f.Close()
		return nil, fmt.Errorf("Inventory 报告缺少必填列 \"bucket\"（请检查 Storage Insights metadata_fields）")
	}
	nameIdx, ok := leafColumnIndex(pf, "name")
	if !ok {
		f.Close()
		return nil, fmt.Errorf("Inventory 报告缺少必填列 \"name\"（请检查 Storage Insights metadata_fields）")
	}

	sizeIdx := -1
	if needSize {
		idx, ok := leafColumnIndex(pf, "size")
		if !ok {
			f.Close()
			return nil, fmt.Errorf("配置了 MIN_SIZE/MAX_SIZE 但 Inventory 报告缺少 \"size\" 列（请在 Storage Insights metadata_fields 中加入 size）")
		}
		sizeIdx = idx
	}

	// timeDeleted 是可选列（清单里配了 metadata_fields 才有）：有则用于过滤已删除对象，
	// 缺则 -1（旧清单兼容，所有行视为未删除），不报错。
	timeDeletedIdx := -1
	if idx, ok := leafColumnIndex(pf, "timeDeleted"); ok {
		timeDeletedIdx = idx
	}

	return &shardReader{f: f, pf: pf, bucketIdx: bucketIdx, nameIdx: nameIdx, sizeIdx: sizeIdx, timeDeletedIdx: timeDeletedIdx}, nil
}

func (sr *shardReader) close() error {
	return sr.f.Close()
}

// numRowGroups 返回 shard 的 row group 数（用于把工作分给多个 producer）。
func (sr *shardReader) numRowGroups() int {
	return len(sr.pf.RowGroups())
}

// valueString 从 parquet.Value 取字符串（STRING 列为 BYTE_ARRAY）。
func valueString(v parquet.Value) string {
	if v.IsNull() {
		return ""
	}
	switch v.Kind() {
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return string(v.ByteArray())
	default:
		return v.String()
	}
}

// valueInt64 从 parquet.Value 取 int64。size 列可能是 INT64/INT32，也可能是字符串
// （GCS Storage Insights 把数字字段编码为字符串，如 size 列为 BYTE_ARRAY "131072"）。
// 返回 (值, ok)：ok=false 表示 null 或无法解析为整数（size 未知）。必须先 IsNull ——
// parquet.Value.Int64() 对 null 读裸 bits 返回 0，否则会把"未知"误当成 size==0。
func valueInt64(v parquet.Value) (int64, bool) {
	if v.IsNull() {
		return 0, false
	}
	switch v.Kind() {
	case parquet.Int64:
		return v.Int64(), true
	case parquet.Int32:
		return int64(v.Int32()), true
	case parquet.ByteArray, parquet.FixedLenByteArray:
		// 字符串形态的整数（如 GCS Insights 的 size="131072"）。
		s := strings.TrimSpace(string(v.ByteArray()))
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false // 非整数字符串：当作未知
		}
		return n, true
	default:
		return 0, false // 其它物理类型：当作未知，交由调用方处理（不 panic）
	}
}

// iterRowGroup 流式遍历指定 row group 的每一行，对每行回调 (bucket, name, size, hasSize, deleted)。
// deleted=true 表示该行 timeDeleted 列非 null（对象已被删除，GCS Storage Insights 对已删除对象
// 保留记录）；清单无 timeDeleted 列时恒为 false。
// 按批读取（ReadRows），不把整个 row group 载入内存。回调返回 error 则中止。
func (sr *shardReader) iterRowGroup(rgIndex int, fn func(bucket, name string, size int64, hasSize, deleted bool) error) error {
	rg := sr.pf.RowGroups()[rgIndex]
	rows := rg.Rows()
	defer rows.Close()

	buf := make([]parquet.Row, 1024)
	for {
		n, readErr := rows.ReadRows(buf)
		for i := 0; i < n; i++ {
			row := buf[i]
			var bucket, name string
			var size int64
			var hasSize, deleted bool
			for _, v := range row {
				switch v.Column() {
				case sr.bucketIdx:
					bucket = valueString(v)
				case sr.nameIdx:
					name = valueString(v)
				case sr.sizeIdx: // sizeIdx==-1 时此 case 永不命中（列索引≥0），关闭时零额外成本
					size, hasSize = valueInt64(v)
				case sr.timeDeletedIdx: // 同上，-1 时永不命中
					deleted = !v.IsNull() // 非 null 即已删除（实测存活对象恒为 null，无 epoch-0 哨兵）
				}
			}
			if err := fn(bucket, name, size, hasSize, deleted); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("读取 Parquet row group %d 失败: %w", rgIndex, readErr)
		}
	}
}
