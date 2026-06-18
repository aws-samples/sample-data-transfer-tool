// Package message 解析 SQS 消息体为传输契约，并把 rclone endpoint 拆成 rc 所需的
// fs + remote 两段。对齐 Python models.TransferMessage / rclone_runner._validate_endpoint。
package message

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

// TransferMessage 一条 SQS 工作项：一个对象 = 一条消息。
//
// 注：rcd 模式不支持 per-message rclone 参数。所有 rclone flag 在 rclone-rcd 守护进程
// 启动时一次性给全（systemd unit 的 ExecStart），限速经 ratelimit 层设 rcd 全局。
// 消息体只承载 source/destination/op 三个字段（旧 Python 的 rclone_args 已废弃）。
type TransferMessage struct {
	Source      string
	Destination string
	Op          model.Op
}

// rawBody SQS body 的 JSON 形态（op 缺省 copy）。
type rawBody struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Op          string `json:"op"`
}

// Parse 解析消息体。非法（坏 JSON / 缺 destination / 非法 op）→ error，调用方按 poison 处理。
func Parse(body string) (TransferMessage, error) {
	var rb rawBody
	if err := json.Unmarshal([]byte(body), &rb); err != nil {
		return TransferMessage{}, fmt.Errorf("消息体非合法 JSON: %w", err)
	}
	if rb.Destination == "" {
		return TransferMessage{}, fmt.Errorf("消息缺 destination 字段")
	}
	op := model.OpCopy
	if rb.Op != "" {
		switch model.Op(rb.Op) {
		case model.OpCopy, model.OpDelete, model.OpRefresh:
			op = model.Op(rb.Op)
		default:
			return TransferMessage{}, fmt.Errorf("非法 op: %q", rb.Op)
		}
	}
	// copy/refresh 必须有 source；delete 可省（只需 destination）。
	if (op == model.OpCopy || op == model.OpRefresh) && rb.Source == "" {
		return TransferMessage{}, fmt.Errorf("%s 消息缺 source 字段", op)
	}
	return TransferMessage{
		Source:      rb.Source,
		Destination: rb.Destination,
		Op:          op,
	}, nil
}

// SplitEndpoint 把 "remote:path" 拆成 ("remote:", "path")，供 rc 的 srcFs/srcRemote。
//
// 校验对齐 Python _validate_endpoint：不含空字节、有合法 backend 前缀。
// ⚠️ Python 用 prefix.isalnum() 会拒绝含 -/_ 的 remote 名（已知坑）；当前 remote
// 是 gcs/s3 纯字母数字所以安全。这里放宽到 [A-Za-z0-9_-] 以免重蹈覆辙，但仍要求
// 前缀非空且 path 段保留字面（含 // 和前导 /，不归一化——这正是走 S3 兼容端点的原因）。
func SplitEndpoint(value, label string) (fs, remote string, err error) {
	if strings.ContainsRune(value, '\x00') {
		return "", "", fmt.Errorf("%s 含空字节", label)
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf("%s 缺 backend 前缀（remote:path）: %q", label, value)
	}
	prefix := value[:colon]
	if !isRemoteName(prefix) {
		return "", "", fmt.Errorf("%s 的 backend 前缀非法: %q", label, value)
	}
	// fs 段保留冒号（rc 的 Fs 形如 "s3:"），remote 段是冒号后的全部路径（字面保留）。
	return value[:colon+1], value[colon+1:], nil
}

func isRemoteName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
