package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// objectStore 是清单读取的最小抽象：按 bucket+object 打开只读流。
// 当前唯一实现是 AWS S3（清单存放于 S3）；保留接口便于将来扩展其他后端与单测打桩。
type objectStore interface {
	// newReader 打开 (bucket, object) 的只读流；调用方负责 Close。
	newReader(ctx context.Context, bucket, object string) (io.ReadCloser, error)
	// close 释放后端资源。
	close() error
}

// manifest 是 GCS Storage Insights 原生 manifest 的 JSON 结构（只取用到的字段）。
// 注意：reportNames 是完整 gs:// URI 列表；recordsProcessed/shardsCount 是 JSON 字符串（非数字）。
type manifest struct {
	ReportNames      []string    `json:"reportNames"`      // 完整 gs:// shard URI 列表
	RecordsProcessed json.Number `json:"recordsProcessed"` // JSON 字符串，如 "706900195"
}

// gcsObjectKey 从 gs://bucket/key 取出 object key（丢弃 GCS 桶名，保留完整 key）。
func gcsObjectKey(gsURI string) (string, error) {
	const scheme = "gs://"
	if !strings.HasPrefix(gsURI, scheme) {
		return "", fmt.Errorf("reportNames 项不是 gs:// URI: %s", gsURI)
	}
	rest := gsURI[len(scheme):]
	idx := strings.IndexByte(rest, '/')
	if idx < 0 || idx == len(rest)-1 {
		return "", fmt.Errorf("gs:// URI 缺少对象路径: %s", gsURI)
	}
	return rest[idx+1:], nil
}

// shardS3KeyFromGCS 把 manifest 里一条 reportNames 的 gs:// URI 映射成 S3 上的 object key。
//  1. 剥掉 gs:// 与 GCS 桶名，得原始 key（gcsObjectKey）；
//  2. 若 key 以 gcsPrefix 开头，把该前缀替换为 s3Prefix（清单从 GCS 同步到 S3 时的目录重写，
//     如 parquet/ → ops/inventory/）；gcsPrefix=="" 或 key 不以其开头则不替换。
//
// 返回的是 object key（不含桶名）；调用方再拼上 manifest 所在的 S3 桶。
func shardS3KeyFromGCS(gsURI, gcsPrefix, s3Prefix string) (string, error) {
	key, err := gcsObjectKey(gsURI)
	if err != nil {
		return "", err
	}
	if gcsPrefix != "" && strings.HasPrefix(key, gcsPrefix) {
		key = s3Prefix + key[len(gcsPrefix):]
	}
	return key, nil
}

// parseObjectURI 解析 s3://bucket/path/to/object 为 (bucket, object)。
// 只在第一个 "/" 切分，object 保留完整路径（含后续的 "/"）。
func parseObjectURI(uri string) (bucket, object string, err error) {
	const scheme = "s3://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", fmt.Errorf("不是合法的 S3 URI（应以 s3:// 开头）: %s", uri)
	}
	rest := uri[len(scheme):]
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "", "", fmt.Errorf("S3 URI 缺少对象路径: %s", uri)
	}
	bucket = rest[:idx]
	object = rest[idx+1:]
	if bucket == "" || object == "" {
		return "", "", fmt.Errorf("S3 URI 的 bucket 或对象名为空: %s", uri)
	}
	return bucket, object, nil
}

// s3InventoryStore 从 AWS S3 读取清单（manifest + shard parquet）。
// 这是真 AWS S3（非 GCS S3 兼容端点），故不设 BaseEndpoint/UsePathStyle/签名兼容中间件。
type s3InventoryStore struct {
	client *s3.Client
}

// newInventoryStore 构建读取 AWS S3 清单的客户端：走 AWS 默认凭证链（同 SQS），
// region 用 INVENTORY_S3_REGION（清单桶可能与 SQS 跨区域）。
func newInventoryStore(ctx context.Context, region string) (*s3InventoryStore, error) {
	logf("清单来源: AWS S3（region %s）", region)
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败（清单读取）: %w", err)
	}
	return &s3InventoryStore{client: s3.NewFromConfig(awsCfg)}, nil
}

func (s *s3InventoryStore) newReader(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(object),
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject 失败 %s/%s: %w", bucket, object, err)
	}
	return out.Body, nil
}

func (s *s3InventoryStore) close() error {
	return nil // s3.Client 无需显式关闭，no-op
}

// uploadFile 把本地文件流式上传到 (bucket, key)，复用清单读取的 S3 client（同凭证同 region）。
// 用于桶日志上传。显式传 ContentLength，避免 SDK 对 *os.File 缓冲/重读。
func (s *s3InventoryStore) uploadFile(ctx context.Context, localPath, bucket, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败 %s: %w", localPath, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat 本地文件失败 %s: %w", localPath, err)
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(fi.Size()),
		ContentType:   aws.String("text/plain; charset=utf-8"),
	})
	if err != nil {
		return fmt.Errorf("S3 PutObject 失败 %s/%s: %w", bucket, key, err)
	}
	return nil
}

// parseManifest 下载并解析 manifest，返回所有 shard 的完整 s3:// 路径与 records_processed。
//
// manifest 的 reportNames 是 gs:// URI 列表。映射到 S3：剥掉 gs:// 桶名得 object key，
// 按 gcsPrefix→s3Prefix 重写 key 前缀（清单从 GCS 同步到 S3 时的目录重命名），桶换成 manifest
// 所在的 S3 桶（shard 与 manifest 同桶）。前缀重写规则来自 Config.InventoryGCSKeyPrefix/S3KeyPrefix。
func parseManifest(ctx context.Context, store objectStore, manifestURI, gcsPrefix, s3Prefix string) (shardURIs []string, recordsProcessed int64, err error) {
	bucketName, manifestObject, err := parseObjectURI(manifestURI)
	if err != nil {
		return nil, 0, err
	}

	logf("下载 manifest: %s", manifestURI)
	rc, err := store.newReader(ctx, bucketName, manifestObject)
	if err != nil {
		return nil, 0, fmt.Errorf("打开 manifest 失败 %s: %w", manifestURI, err)
	}
	data, err := io.ReadAll(rc)
	closeErr := rc.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("读取 manifest 失败 %s: %w", manifestURI, err)
	}
	if closeErr != nil {
		return nil, 0, fmt.Errorf("关闭 manifest reader 失败 %s: %w", manifestURI, closeErr)
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, 0, fmt.Errorf("解析 manifest JSON 失败 %s: %w", manifestURI, err)
	}
	if len(m.ReportNames) == 0 {
		return nil, 0, fmt.Errorf("manifest 缺少 reportNames 字段: %s", manifestURI)
	}

	// recordsProcessed 是 JSON 字符串：空串当 0，非法报错。
	var records int64
	if s := m.RecordsProcessed.String(); s != "" {
		records, err = m.RecordsProcessed.Int64()
		if err != nil {
			return nil, 0, fmt.Errorf("manifest recordsProcessed 非法 %q: %w", s, err)
		}
	}

	// reportNames 是完整 gs:// URI。映射到 S3：key 前缀重写 + 桶换成 manifest 所在的 S3 桶。
	shardURIs = make([]string, 0, len(m.ReportNames))
	for i, gsURI := range m.ReportNames {
		key, kErr := shardS3KeyFromGCS(gsURI, gcsPrefix, s3Prefix)
		if kErr != nil {
			return nil, 0, fmt.Errorf("manifest reportNames[%d]: %w", i, kErr)
		}
		shardURIs = append(shardURIs, fmt.Sprintf("s3://%s/%s", bucketName, key))
	}

	logf("manifest 解析完成: recordsProcessed=%d, shard 数=%d", records, len(shardURIs))
	return shardURIs, records, nil
}

// downloadShard 将单个 shard 流式下载到 destDir 下的本地临时文件，返回本地路径。
// 用 io.Copy 流式写盘，不把整个 shard 读进内存（shard 可达上百 MB）。
func downloadShard(ctx context.Context, store objectStore, shardURI, destDir string) (string, error) {
	bucketName, objectName, err := parseObjectURI(shardURI)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("创建临时目录失败 %s: %w", destDir, err)
	}
	localPath := path.Join(destDir, path.Base(objectName))

	rc, err := store.newReader(ctx, bucketName, objectName)
	if err != nil {
		return "", fmt.Errorf("打开 shard 失败 %s: %w", shardURI, err)
	}
	defer rc.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("创建本地文件失败 %s: %w", localPath, err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("下载 shard 失败 %s: %w", shardURI, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("关闭本地文件失败 %s: %w", localPath, err)
	}
	return localPath, nil
}
