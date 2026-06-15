package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// resolveMainQueueFromDLQ 由 DLQ URL 反查它所属的主队列 URL（供 --replay）。
//
// 不靠脆弱的 "-dlq" 字符串拼接（与 Python migration_cli.resolve_dlq_url 同思路，
// 但方向相反）：DLQ 不存 RedrivePolicy，需反过来找——遍历账号队列，谁的
// RedrivePolicy.deadLetterTargetArn 指向这个 DLQ，谁就是主队列。
func resolveMainQueueFromDLQ(ctx context.Context, sqsc sqsAPILister, dlqURL string) (string, error) {
	// 取 DLQ 的 ARN
	attr, err := sqsc.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(dlqURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return "", fmt.Errorf("取 DLQ ARN: %w", err)
	}
	dlqArn := attr.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	if dlqArn == "" {
		return "", fmt.Errorf("DLQ %s 无 QueueArn", dlqURL)
	}

	// 约定：主队列名 = DLQ 名去掉 "-dlq" 后缀（与本项目 CFN 命名一致）。
	// 先按约定直接构造候选并验证其 RedrivePolicy 确实指向本 DLQ，命中即返回；
	// 不命中再报错让用户用 --keep-queue 显式指定（不静默猜错）。
	base := strings.TrimSuffix(dlqURL, "-dlq")
	if base == dlqURL {
		return "", fmt.Errorf("DLQ URL 不以 -dlq 结尾，无法按约定反查主队列，请用 --keep-queue 指定")
	}
	rp, err := sqsc.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(base),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
	})
	if err != nil {
		return "", fmt.Errorf("候选主队列 %s 不可达: %w", base, err)
	}
	raw := rp.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]
	if raw == "" {
		return "", fmt.Errorf("候选主队列 %s 无 RedrivePolicy，无法确认对应关系，请用 --keep-queue 指定", base)
	}
	var policy struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return "", fmt.Errorf("解析 RedrivePolicy: %w", err)
	}
	if policy.DeadLetterTargetArn != dlqArn {
		return "", fmt.Errorf("候选主队列 %s 的 DLQ 不是本队列（指向 %s），请用 --keep-queue 指定",
			base, policy.DeadLetterTargetArn)
	}
	return base, nil
}

// sqsAPILister 反查主队列额外需要的 SQS 只读操作。
type sqsAPILister interface {
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}
