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
	Body        string
	Bucket      string // 源 GCS 桶名（bucketId），用于按桶分类统计
	ObjectSize  int64  // copy 来自 payload.size
	skip        bool   // 非关注事件 → 跳过（直接 ack 丢弃）
	unknownBkt  bool   // 源桶不在映射表 / 前缀无兜底 → 跳过但单独计数告警
	unknownInfo string // unknownBkt 时的说明（桶名/key），供日志
}

// gcsObjectMetadata 解析 payload（JSON_API_V1 object resource）需要的字段。
// size/storageClass 不在 attributes，只在 payload JSON 里（官方确认），故必须解析 body。
type gcsObjectMetadata struct {
	Size string `json:"size"` // JSON_API_V1 里 size 是字符串
}

// mapEvent 把一条 Pub/Sub 消息（attributes + payload data）映射成迁移消息。
//
// 规则（owner 决策 2026-06-15/16）：
//   - OBJECT_FINALIZE / OBJECT_METADATA_UPDATE → copy，其余事件 skip（ack 丢弃）。
//   - 目标 S3 桶按 dest.BucketMapping[bucketId] 两级路由：
//     · 写法 A（s3_bucket）：整桶映射 → s3:<s3_bucket>/<prefix>/<key>
//     · 写法 B（prefix_routes）：按 key 最长前缀命中路由；不命中走 default_s3_bucket
//   - 源桶不在映射表 / 前缀无命中且无兜底 → unknownBkt（跳过但单独计数告警，不投 SQS）。
//   - 匹配后保留完整 key（不剥前缀），目标层级与源一致。
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
		s3Dest, destKey, ok := resolveDest(dest.BucketMapping, bucket, objectKey)
		if !ok {
			// 未知桶 / 前缀无命中且无兜底：跳过 + 计数告警（不投 SQS，也不报错 nack）。
			return mappedMessage{
				Bucket:      bucket,
				unknownBkt:  true,
				unknownInfo: fmt.Sprintf("bucket=%s key=%s", bucket, objectKey),
			}, nil
		}
		size, err := parseSize(payload)
		if err != nil {
			return mappedMessage{}, fmt.Errorf("解析 payload size: %w", err)
		}
		msg := migrationMessage{
			Source:      fmt.Sprintf("gcs:%s/%s", bucket, objectKey),
			Destination: fmt.Sprintf("s3:%s/%s", s3Dest, destKey),
		}
		body, _ := json.Marshal(msg)
		return mappedMessage{Body: string(body), Bucket: bucket, ObjectSize: size}, nil

	default:
		// 非关注事件跳过：带上 bucket 便于按桶统计 skipped（bucket 可能为空，归到 "(unknown)"）。
		return mappedMessage{Bucket: bucket, skip: true}, nil
	}
}

// resolveDest 按桶映射规则解析目标"S3桶/可选前缀"段 + 目标对象 key。
// 返回 (s3 桶段, 目标 key, 是否解析成功)，destination = s3:<桶段>/<目标key>。
//   - 写法 A：整桶映射，可选 prefix 拼进桶段，目标 key = 原 key（保留完整层级）。
//   - 写法 B 命中路由：默认目标 key = 原 key；该路由 StripPrefix=true 时剥掉匹配的前缀段。
//   - 写法 B 不命中：走 default_s3_bucket（保留完整 key），无 default 则解析失败。
func resolveDest(mapping map[string]BucketRule, gcsBucket, objectKey string) (string, string, bool) {
	rule, ok := mapping[gcsBucket]
	if !ok {
		return "", "", false // 源桶未配映射
	}
	if rule.isPrefixRouted() {
		// 写法 B：最长前缀优先。
		bestLen := -1
		var best PrefixRoute
		for _, pr := range rule.PrefixRoutes {
			if strings.HasPrefix(objectKey, pr.Prefix) && len(pr.Prefix) > bestLen {
				bestLen = len(pr.Prefix)
				best = pr
			}
		}
		if bestLen >= 0 {
			key := objectKey
			if best.StripPrefix {
				key = strings.TrimPrefix(objectKey, best.Prefix)
			}
			return best.S3Bucket, key, true
		}
		if rule.DefaultS3Bucket != "" {
			return rule.DefaultS3Bucket, objectKey, true // 兜底保留完整 key
		}
		return "", "", false // 无命中且无兜底
	}
	// 写法 A：整桶映射，可选 prefix 拼进桶段，保留完整 key。
	prefix := strings.Trim(rule.Prefix, "/")
	if prefix == "" {
		return rule.S3Bucket, objectKey, true
	}
	return rule.S3Bucket + "/" + prefix, objectKey, true
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
