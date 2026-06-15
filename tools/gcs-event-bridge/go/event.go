package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// GCS Pub/Sub 通知的 eventType（attribute 值，官方枚举）。
// owner 决策 2026-06-15：只监控 FINALIZE + METADATA_UPDATE，两者都映射为 copy
// （重新把该对象同步到 S3）；DELETE/ARCHIVE/INITIALIZE 暂不处理（跳过）。
const (
	eventFinalize   = "OBJECT_FINALIZE"        // 对象创建/新版本 → copy
	eventMetaUpdate = "OBJECT_METADATA_UPDATE" // 对象元数据更新 → copy（重新同步）
)

// migrationMessage 投递到 SQS 的迁移消息体，形态对齐 worker 契约
// （models.TransferMessage：copy 省略 op；object_size 走 MessageAttribute，不在 body）。
// 当前只产 copy（FINALIZE/METADATA_UPDATE），故不带 op 字段。
type migrationMessage struct {
	Source      string `json:"source"`      // gcs:<bucket>/<key>
	Destination string `json:"destination"` // s3:<destBucket>/<destPrefix>/<key>
}

// mappedMessage 映射结果：SQS body + object_size 属性（仅 copy 有真实大小）。
type mappedMessage struct {
	Body       string
	ObjectSize int64 // copy 来自 payload.size；delete 为 0
	skip       bool  // 非 FINALIZE/DELETE 事件 → 跳过（直接 ack 丢弃）
}

// gcsObjectMetadata 解析 payload（JSON_API_V1 object resource）需要的字段。
// size/storageClass 不在 attributes，只在 payload JSON 里（官方确认），故必须解析 body。
type gcsObjectMetadata struct {
	Size string `json:"size"` // JSON_API_V1 里 size 是字符串
}

// mapEvent 把一条 Pub/Sub 消息（attributes + payload data）映射成迁移消息。
//
// 规则（owner 决策 2026-06-15）：
//   - OBJECT_FINALIZE       → copy：对象创建/新版本，同步到 S3
//   - OBJECT_METADATA_UPDATE → copy：元数据更新，重新同步该对象到 S3
//     （两者映射等价：source=gcs:bucket/key, destination=s3:destBucket/prefix/key,
//     object_size=payload.size）
//   - 其余事件（DELETE/ARCHIVE/INITIALIZE…）→ skip（直接 ack 丢弃，不投 SQS）
//
// 纯函数、无副作用、无 AWS/GCP 依赖，便于单测。
func mapEvent(attrs map[string]string, payload []byte, dest Dest) (mappedMessage, error) {
	eventType := attrs["eventType"]
	bucket := attrs["bucketId"]
	objectKey := attrs["objectId"]

	switch eventType {
	case eventFinalize, eventMetaUpdate:
		if bucket == "" || objectKey == "" {
			return mappedMessage{}, fmt.Errorf("%s 缺 bucketId/objectId", eventType)
		}
		size, err := parseSize(payload)
		if err != nil {
			return mappedMessage{}, fmt.Errorf("解析 payload size: %w", err)
		}
		msg := migrationMessage{
			Source:      fmt.Sprintf("gcs:%s/%s", bucket, objectKey),
			Destination: destPath(dest, objectKey),
		}
		body, _ := json.Marshal(msg)
		return mappedMessage{Body: string(body), ObjectSize: size}, nil

	default:
		return mappedMessage{skip: true}, nil // 非关注事件，跳过
	}
}

// destPath 拼目标 S3 路径：s3:<bucket>/<prefix>/<key>（prefix 可空；规整斜杠）。
func destPath(dest Dest, objectKey string) string {
	prefix := strings.Trim(dest.DestPrefix, "/")
	var path string
	if prefix == "" {
		path = objectKey
	} else {
		path = prefix + "/" + objectKey
	}
	return fmt.Sprintf("s3:%s/%s", dest.DestBucket, path)
}

// parseSize 从 JSON_API_V1 payload 取 size（字符串字段转 int64）。空 payload 返回 0。
func parseSize(payload []byte) (int64, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	var meta gcsObjectMetadata
	if err := json.Unmarshal(payload, &meta); err != nil {
		return 0, err
	}
	if meta.Size == "" {
		return 0, nil
	}
	return strconv.ParseInt(meta.Size, 10, 64)
}
