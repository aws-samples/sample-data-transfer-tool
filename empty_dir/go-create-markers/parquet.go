package main

import (
	"fmt"
	"io"
	"os"

	"github.com/parquet-go/parquet-go"
)

// keyShardReader 封装一个本地 Parquet 文件，按 row group 流式读取目标 key 列。
//
// 改写自 gcs-sqs-go/parquet.go 的 shardReader：那里硬要求 bucket+name 两列（GCS
// Storage Insights 清单），而本工具的 empty_dir_markers.parquet 只有单列。这里支持
// 两种列名以兼容两类输入：
//   - 列 "name"：原始 GCS 目录路径（以 / 结尾），需转换为 <dir>_$folder$（transform=true）。
//   - 列 "key" ：已是最终 S3 key，原样使用（transform=false）。
//
// 用行级 API（RowGroup.Rows + ReadRows）而非 struct 映射：不受 schema optional/required
// 形态影响；且按 row group 切分天然支持多 producer 并行解码（每个 producer 认领不同 RG）。
type keyShardReader struct {
	f         *os.File
	pf        *parquet.File
	colIdx    int
	transform bool // true: 末尾 / -> _$folder$；false: 原样
}

// leafColumnIndex 在扁平 schema 中按列名找叶子列索引。（复制自 gcs-sqs-go/parquet.go）
func leafColumnIndex(pf *parquet.File, name string) (int, bool) {
	for _, c := range pf.Root().Columns() {
		if c.Leaf() && c.Name() == name {
			return c.Index(), true
		}
	}
	return 0, false
}

// openKeyShard 打开本地 Parquet 文件并定位 key 列。优先 "name"（需转换），
// 回退 "key"（原样）。两列皆无则报错。
func openKeyShard(path string) (*keyShardReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 Parquet 文件失败 %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat Parquet 文件失败 %s: %w", path, err)
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("解析 Parquet 失败 %s: %w", path, err)
	}

	if idx, ok := leafColumnIndex(pf, "name"); ok {
		return &keyShardReader{f: f, pf: pf, colIdx: idx, transform: true}, nil
	}
	if idx, ok := leafColumnIndex(pf, "key"); ok {
		return &keyShardReader{f: f, pf: pf, colIdx: idx, transform: false}, nil
	}
	f.Close()
	return nil, fmt.Errorf("Parquet 既无 \"name\" 也无 \"key\" 列: %s", path)
}

func (sr *keyShardReader) close() error {
	return sr.f.Close()
}

// numRowGroups 返回 row group 数（用于把工作分给多个 producer）。
func (sr *keyShardReader) numRowGroups() int {
	return len(sr.pf.RowGroups())
}

// valueString 从 parquet.Value 取字符串（STRING 列为 BYTE_ARRAY）。（复制自 gcs-sqs-go/parquet.go）
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

// iterRowGroup 流式遍历指定 row group 的每一行，对每行回调 (rawValue)。
// rawValue 是源列原值（name 列即 GCS 目录路径）。按批读取（ReadRows），不把整个
// row group 载入内存。回调返回 error 则中止。
func (sr *keyShardReader) iterRowGroup(rgIndex int, fn func(raw string) error) error {
	rg := sr.pf.RowGroups()[rgIndex]
	rows := rg.Rows()
	defer rows.Close()

	buf := make([]parquet.Row, 1024)
	for {
		n, readErr := rows.ReadRows(buf)
		for i := 0; i < n; i++ {
			row := buf[i]
			var raw string
			for _, v := range row {
				if v.Column() == sr.colIdx {
					raw = valueString(v)
				}
			}
			if err := fn(raw); err != nil {
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
