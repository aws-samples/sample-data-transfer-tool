package status

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/obslog"
)

// DDBAPI DynamoDB 客户端最小接口（便于测试注入）。
// PutItem 用于心跳（低频、单表、独立）；BatchWriteItem 用于终态记录（高频、批量、异步 writer）。
type DDBAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
}

// maxTerminalTextBytes 终态错误文本/消息体截断上限（对齐 Python 16KB，防 DDB 单 item 400KB 超限）。
const maxTerminalTextBytes = 16 * 1024

// heartbeatTTLSeconds 心跳存活窗口（对齐 Python 300s）。
const heartbeatTTLSeconds = 300

const (
	// batchMaxItems DynamoDB BatchWriteItem 单次上限（硬限 25）。
	batchMaxItems = 25
	// recordQueueCap 终态记录异步队列容量。满则 drop（best-effort，G1：丢审计记录 ≪ 阻塞消息生命周期）。
	// 4096 × ~1KB ≈ 4MB 上限，足够吸收 DDB 短暂节流下的突发。
	recordQueueCap = 4096
	// batchMaxRetries UnprocessedItems / 整批错误的有界重试次数（退避后仍失败则计 RecordFail）。
	batchMaxRetries = 5
	// batchFlushTimeout 单次 BatchWriteItem 的 ctx 超时（脱离请求 ctx，停机 drain 也能写）。
	batchFlushTimeout = 10 * time.Second
)

// terminalItem 单条终态记录（已构造好的 DDB item + 去重键，供 writer 攒批）。
type terminalItem struct {
	pk   string // source_hash（分区键）
	sk   string // attempt_timestamp（排序键）
	item map[string]ddbtypes.AttributeValue
}

// Store 封装两张表的写入。终态记录经内部异步 writer 批量写（BatchWriteItem），
// 心跳仍走同步 PutItem。writer 生命周期由 StartWriter/StopWriter 管理。
type Store struct {
	ddb            DDBAPI
	statusTable    string
	heartbeatTable string

	recCh        chan terminalItem
	writerStop   chan struct{}
	writerDone   chan struct{}
	startOnce    sync.Once
	stopOnce     sync.Once
	onRecordFail func(int64) // 批写终败 / 队列满时累加（wire 到 Stats.RecordFail）
}

func NewStore(ddb DDBAPI, statusTable, heartbeatTable string) *Store {
	return &Store{ddb: ddb, statusTable: statusTable, heartbeatTable: heartbeatTable}
}

// StartWriter 启动异步批量 writer goroutine。onRecordFail 在批写彻底失败或队列满丢弃时被调用
// （累加到 Stats.RecordFail 供观测）。必须在 RecordTerminal 首次调用前启动；幂等。
func (s *Store) StartWriter(onRecordFail func(int64)) {
	s.startOnce.Do(func() {
		s.recCh = make(chan terminalItem, recordQueueCap)
		s.writerStop = make(chan struct{})
		s.writerDone = make(chan struct{})
		s.onRecordFail = onRecordFail
		go s.runWriter()
	})
}

// StopWriter 停止 writer：排空队列 + 末批 flush，阻塞到 writer 退出。幂等。
// 调用方须保证此前所有 RecordTerminal（worker）已结束，否则残余入队可能丢失。
func (s *Store) StopWriter() {
	if s.writerStop == nil {
		return // 未 StartWriter（如仅测试同步路径）
	}
	s.stopOnce.Do(func() {
		close(s.writerStop)
		<-s.writerDone
	})
}

// RecordTerminal 构造一条终态记录并**非阻塞入队**（O(1)，不等 DDB）→ 不拖延调用方的 SQS
// 生命周期（delete/requeue）。队列满 → 返回 error（调用方按 best-effort 处理，标 RecordFailed）。
// 未 StartWriter 时降级为同步单条写（保留直接单测/非 worker 场景可用）。
func (s *Store) RecordTerminal(
	ctx context.Context,
	source, attemptTS string,
	result model.RunResult,
	instanceID, nowISO, body string,
) error {
	ti := buildTerminalItem(source, attemptTS, result, instanceID, nowISO, body)
	if s.recCh == nil {
		// 无 writer：同步单条写（BatchWriteItem 单元素，走同一去重/重试路径）。
		return flushBatch(ctx, s.ddb, s.statusTable, []terminalItem{ti}, nil)
	}
	select {
	case s.recCh <- ti:
		return nil
	default:
		// 队列满：drop（best-effort），返回 error。调用方（worker）据此标 RecordFailed →
		// tally 累加 RecordFail。此处**不**调 onRecordFail，避免与调用方路径重复计数
		// （onRecordFail 只负责调用方看不到的“异步批写终败”那一类）。
		return errRecordQueueFull
	}
}

var errRecordQueueFull = &recordQueueFullError{}

type recordQueueFullError struct{}

func (*recordQueueFullError) Error() string {
	return "终态记录队列已满，丢弃（best-effort；DDB 写入跟不上，见 RecordFail 计数）"
}

// buildTerminalItem 构造终态记录 DDB item（纯函数，便于单测截断/省 body/错误字段）。
func buildTerminalItem(
	source, attemptTS string,
	result model.RunResult,
	instanceID, nowISO, body string,
) terminalItem {
	// 时间语义：attempt_timestamp/started_at = 传输开始（SK，也是 worker 取到消息时刻）；
	// updated_at = 终态写入时刻（≈传输完成）。elapsed_seconds 是 runner wall-clock 实测
	// 传输耗时；transferred_bytes/speed_bps 来自 runner 读取的 rcd group stats。
	pk := MakePK(source)
	item := map[string]ddbtypes.AttributeValue{
		"source_hash":       &ddbtypes.AttributeValueMemberS{Value: pk},
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
	return terminalItem{pk: pk, sk: attemptTS, item: item}
}

// runWriter 异步批量 writer 主循环：收到一条即贪婪凑满 batchMaxItems 再 flush（无遗留延迟，
// 免 ticker）；stopCh 关闭后排空剩余队列 + 末批 flush 再退出。
func (s *Store) runWriter() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.writerStop:
			// 停机 drain：worker 已停，队列不再新增，逐批排空写完。
			for {
				buf := drainUpTo(s.recCh, batchMaxItems)
				if len(buf) == 0 {
					return
				}
				s.flush(buf)
			}
		case first := <-s.recCh:
			buf := append(make([]terminalItem, 0, batchMaxItems), first)
			buf = append(buf, drainUpTo(s.recCh, batchMaxItems-1)...)
			s.flush(buf)
		}
	}
}

// drainUpTo 非阻塞从 ch 拉最多 n 条（队列空即返回，不阻塞）。
func drainUpTo(ch <-chan terminalItem, n int) []terminalItem {
	buf := make([]terminalItem, 0, n)
	for len(buf) < n {
		select {
		case it := <-ch:
			buf = append(buf, it)
		default:
			return buf
		}
	}
	return buf
}

// flush 写一批终态记录，失败则累加 RecordFail。
func (s *Store) flush(items []terminalItem) {
	ctx, cancel := context.WithTimeout(context.Background(), batchFlushTimeout)
	defer cancel()
	_ = flushBatch(ctx, s.ddb, s.statusTable, items, s.onRecordFail)
}

// flushBatch 用 BatchWriteItem 写一批（≤25），处理 UnprocessedItems + 整批错误的有界重试。
// 批内按 (PK,SK) 去重（BatchWriteItem 同批同键会整批 ValidationException；同键保留最后一条，
// 与旧同步 PutItem 的“后写覆盖”语义等价）。彻底失败的条数经 onRecordFail 上报。返回最后一次错误。
func flushBatch(ctx context.Context, ddb DDBAPI, table string, items []terminalItem, onFail func(int64)) error {
	if len(items) == 0 {
		return nil
	}
	reqs := dedupeToWriteRequests(items)
	var lastErr error
	backoff := 50 * time.Millisecond
	for attempt := 0; attempt < batchMaxRetries; attempt++ {
		out, err := ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]ddbtypes.WriteRequest{table: reqs},
		})
		if err != nil {
			// 整批错误（节流耗尽/网络/校验）：退避后整批重试。
			lastErr = err
			if !sleepBackoff(ctx, &backoff) {
				break
			}
			continue
		}
		reqs = out.UnprocessedItems[table]
		if len(reqs) == 0 {
			return nil // 全部写入
		}
		// 仍有 UnprocessedItems：保留一个"未完成"错误信号，避免退避耗尽后误报 nil（成功）。
		// 同步 fallback 路径靠此返回值让调用方标 RecordFailed；异步路径靠下方 onFail 计数。
		lastErr = errUnprocessedRemaining
		if !sleepBackoff(ctx, &backoff) {
			break
		}
	}
	// 重试耗尽仍有未写入：best-effort 放弃，计 RecordFail + 记日志。
	if n := len(reqs); n > 0 {
		if onFail != nil {
			onFail(int64(n))
		}
		obslog.Errorf("终态记录批写失败(重试 %d 次仍剩 %d 条未写入，best-effort 放弃): lastErr=%v", batchMaxRetries, n, lastErr)
	}
	return lastErr
}

// errUnprocessedRemaining：重试后仍有 UnprocessedItems（无整批 error 但未全部写入）。
// 使 flushBatch 不会在“剩余未写入”时误返回 nil（同步 fallback 据此判失败）。
var errUnprocessedRemaining = &unprocessedRemainingError{}

type unprocessedRemainingError struct{}

func (*unprocessedRemainingError) Error() string {
	return "BatchWriteItem 重试后仍有 UnprocessedItems 未写入"
}

// dedupeToWriteRequests 按 (pk,sk) 去重（保留最后一条）并转为 PutRequest 列表。
func dedupeToWriteRequests(items []terminalItem) []ddbtypes.WriteRequest {
	byKey := make(map[string]map[string]ddbtypes.AttributeValue, len(items))
	order := make([]string, 0, len(items))
	for _, it := range items {
		k := it.pk + "\x00" + it.sk
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = it.item // 同键后写覆盖（与旧同步 PutItem 语义一致）
	}
	reqs := make([]ddbtypes.WriteRequest, 0, len(order))
	for _, k := range order {
		reqs = append(reqs, ddbtypes.WriteRequest{
			PutRequest: &ddbtypes.PutRequest{Item: byKey[k]},
		})
	}
	return reqs
}

// sleepBackoff 退避 *d 并翻倍（上限 1s）；ctx 取消则返回 false（停止重试）。
func sleepBackoff(ctx context.Context, d *time.Duration) bool {
	t := time.NewTimer(*d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	}
	if *d < time.Second {
		*d *= 2
	}
	return true
}

// WriteHeartbeat 写/覆盖心跳；ttl = now + 300（DDB 自动过期 + 存活判定）。低频、独立、同步。
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
