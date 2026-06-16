package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// newS3Client 创建 S3 客户端，并调大 HTTP 连接池以支撑高并发 put-worker。
//
// 基于 gcs-sqs-go/sqs.go:newSQSClient 的 transport 调优：aws-sdk-go-v2 默认
// MaxIdleConnsPerHost=10，worker 达数百时必须放大连接池，否则连接复用不足、TLS
// 握手开销吃掉并发收益（等价 Python boto3 的 max_pool_connections 坑）。
// 重试对齐 Python：adaptive 模式 + 最多 10 次（应对 503 SlowDown / 5xx）。
func newS3Client(ctx context.Context, region, profile string, workers int) (*s3.Client, error) {
	maxConns := workers + 64

	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.Proxy = http.ProxyFromEnvironment
		t.MaxIdleConns = 0 // 不限总空闲连接
		t.MaxIdleConnsPerHost = maxConns
		t.MaxConnsPerHost = 0 // 不限每主机连接
		t.IdleConnTimeout = 90 * time.Second
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ExpectContinueTimeout = 1 * time.Second
		t.ForceAttemptHTTP2 = true
		t.WriteBufferSize = 64 * 1024
		t.ReadBufferSize = 64 * 1024
	})

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(httpClient),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(10),
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败: %w", err)
	}
	return s3.NewFromConfig(cfg), nil
}

// putMarker 创建单个 0 字节标记对象。省略 Body 即 0 字节（等价 boto3 put_object 不传 Body）。
// SDK 自带 adaptive 重试已应用；返回的 error 是重试耗尽后的最终错误。
func putMarker(ctx context.Context, c *s3.Client, bucket, key string) error {
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err
}

// headResult 是 headMarker 的核对结果。
type headResult int

const (
	headOK      headResult = iota // 存在且 0 字节
	headBadSize                   // 存在但非 0 字节
	headMissing                   // 不存在（404）
)

// headMarker 核对一个 key：存在性 + 是否 0 字节。
func headMarker(ctx context.Context, c *s3.Client, bucket, key string) (headResult, int64, error) {
	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return headMissing, 0, nil
		}
		return headMissing, 0, err
	}
	var size int64
	if out.ContentLength != nil { // 本 SDK 版本是 *int64，必须 nil check
		size = *out.ContentLength
	}
	if size == 0 {
		return headOK, 0, nil
	}
	return headBadSize, size, nil
}

// isNotFound 判断 error 是否为 S3 404 / NotFound。
func isNotFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

// errorCode 提取简短错误码用于日志（对齐 Python 记 e.response.Error.Code）。
func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return err.Error()
}
