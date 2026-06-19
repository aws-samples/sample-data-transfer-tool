package status

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

// DDBAPI DynamoDB 客户端最小接口（便于测试注入）。
type DDBAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

// maxTerminalTextBytes 终态错误文本/消息体截断上限（对齐 Python 16KB，防 DDB 单 item 400KB 超限）。
const maxTerminalTextBytes = 16 * 1024

// heartbeatTTLSeconds 心跳存活窗口（对齐 Python 300s）。
const heartbeatTTLSeconds = 300

// Store 封装两张表的写入。
type Store struct {
	ddb            DDBAPI
	statusTable    string
	heartbeatTable string
}

func NewStore(ddb DDBAPI, statusTable, heartbeatTable string) *Store {
	return &Store{ddb: ddb, statusTable: statusTable, heartbeatTable: heartbeatTable}
}

// RecordTerminal 写一条终态记录（每次 attempt 一行，SK=attemptTS 不覆盖历史）。
// body 非空时写 message_body（失败消息留痕，便于定位/replay）；SUCCESS 传空串不写。
func (s *Store) RecordTerminal(
	ctx context.Context,
	source, attemptTS string,
	result model.RunResult,
	instanceID, nowISO, body string,
) error {
	// 时间语义：attempt_timestamp/started_at = 传输开始（SK，也是 worker 取到消息时刻）；
	// updated_at = 终态写入时刻（≈传输完成）。elapsed_seconds 是 runner wall-clock 实测
	// 传输耗时；transferred_bytes/speed_bps 来自 runner 读取的 rcd group stats。
	item := map[string]ddbtypes.AttributeValue{
		"source_hash":       &ddbtypes.AttributeValueMemberS{Value: MakePK(source)},
		"attempt_timestamp": &ddbtypes.AttributeValueMemberS{Value: attemptTS},
		"started_at":        &ddbtypes.AttributeValueMemberS{Value: attemptTS},
		"source":            &ddbtypes.AttributeValueMemberS{Value: source},
		"state":             &ddbtypes.AttributeValueMemberS{Value: string(result.State)},
		"transferred_bytes": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(result.Stats.Bytes, 10)},
		"elapsed_seconds":   &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(result.Stats.ElapsedSeconds, 'f', 3, 64)},
		"speed_bps":         &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(result.Stats.Speed, 'f', 0, 64)},
		"instance_id":       &ddbtypes.AttributeValueMemberS{Value: instanceID},
		"rclone_command":    &ddbtypes.AttributeValueMemberS{Value: result.CmdStr},
		"updated_at":        &ddbtypes.AttributeValueMemberS{Value: nowISO},
	}
	if result.ErrorClass != "" {
		item["error_class"] = &ddbtypes.AttributeValueMemberS{Value: result.ErrorClass}
	}
	if result.ErrorMessage != "" {
		item["error_message"] = &ddbtypes.AttributeValueMemberS{Value: truncateUTF8Bytes(result.ErrorMessage, maxTerminalTextBytes)}
	}
	if body != "" {
		item["message_body"] = &ddbtypes.AttributeValueMemberS{Value: truncateUTF8Bytes(body, maxTerminalTextBytes)}
	}
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.statusTable),
		Item:      item,
	})
	return err
}

// WriteHeartbeat 写/覆盖心跳；ttl = now + 300（DDB 自动过期 + 存活判定）。
func (s *Store) WriteHeartbeat(ctx context.Context, instanceID, nowISO string, nowEpoch int64, activeThreads int) error {
	_, err := s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.heartbeatTable),
		Item: map[string]ddbtypes.AttributeValue{
			"instance_id":    &ddbtypes.AttributeValueMemberS{Value: instanceID},
			"last_heartbeat": &ddbtypes.AttributeValueMemberS{Value: nowISO},
			"ttl":            &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(nowEpoch+heartbeatTTLSeconds, 10)},
			"active_threads": &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(activeThreads)},
		},
	})
	return err
}

// NowISO 当前 UTC 时间戳，对齐 Python now_iso 形态（毫秒，无时区后缀）。
func NowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000") }

func truncateUTF8Bytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(min(len(s), max))
	for _, r := range s {
		// range over string 已把非法字节解码为 RuneError(U+FFFD)，RuneLen 恒 ≥ 1。
		n := utf8.RuneLen(r)
		if b.Len()+n > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
