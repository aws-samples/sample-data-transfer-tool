package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// decision 一条 DLQ 消息的处置决定。
type decision int

const (
	decideDelete  decision = iota // 源不存在 → 归档 + 删
	decideKeep                    // 其它已知失败 → 转发到保留去向
	decideUnknown                 // DDB 查不到 / uncategorized → 保守保留
)

func (d decision) String() string {
	switch d {
	case decideDelete:
		return "delete"
	case decideKeep:
		return "keep"
	default:
		return "unknown"
	}
}

// ddbAPI 抽象出工具用到的 DynamoDB 操作，便于单元测试注入 fake。
type ddbAPI interface {
	Query(ctx context.Context, in *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// latestErrorClass 查 source 在 transfer-status 表的**最近一次** attempt 的 error_class。
//
// PK = makePK(source)，SK = attempt_timestamp（ISO 字符串，字典序=时间序）。
// ScanIndexForward=false + Limit=1 → 只读最新一行（省 RRU；不投影 message_body
// 等大字段以降低读取字节数）。返回 (error_class, found, err)。
func latestErrorClass(ctx context.Context, ddb ddbAPI, table, source string) (string, bool, error) {
	out, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("source_hash = :pk"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk": &ddbtypes.AttributeValueMemberS{Value: makePK(source)},
		},
		ScanIndexForward:     aws.Bool(false), // SK 降序 → 最新 attempt 在前
		Limit:                aws.Int32(1),
		ConsistentRead:       aws.Bool(false), // 终态记录，最终一致即可（RRU 减半）
		ProjectionExpression: aws.String("error_class, #st"),
		ExpressionAttributeNames: map[string]string{
			"#st": "state", // state 是 DDB 保留字，用别名
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("ddb query: %w", err)
	}
	if len(out.Items) == 0 {
		return "", false, nil
	}
	ec, ok := out.Items[0]["error_class"].(*ddbtypes.AttributeValueMemberS)
	if !ok || ec.Value == "" {
		return "", true, nil // 行存在但无 error_class（SUCCESS 行）
	}
	return ec.Value, true, nil
}

// classify 把 error_class 映射为处置决定。
//   - src_not_found       → delete（源不存在，确定性终态，清理目标）
//   - 其它已知失败 class    → keep（转发到保留去向，可重试/待人工）
//   - 空 / 查不到 / 模糊    → unknown（保守保留，绝不当垃圾删）
//
// deleteClasses 可配置（默认仅 src_not_found），允许将来扩展（如也清 arg_error）。
func classify(errorClass string, found bool, deleteClasses map[string]bool) decision {
	if !found || errorClass == "" || errorClass == "uncategorized" {
		return decideUnknown
	}
	if deleteClasses[errorClass] {
		return decideDelete
	}
	return decideKeep
}

// classCount 统计用：error_class → 条数。
type classCount map[string]int

// sortedByCount 把统计结果按条数降序排列，供报告输出。
func (c classCount) sortedByCount() []struct {
	Class string
	Count int
} {
	out := make([]struct {
		Class string
		Count int
	}, 0, len(c))
	for k, v := range c {
		out = append(out, struct {
			Class string
			Count int
		}{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
