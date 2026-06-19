package status

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
)

type fakeDDB struct {
	lastItem  map[string]ddbtypes.AttributeValue
	lastTable string
}

func (f *fakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.lastItem = in.Item
	f.lastTable = *in.TableName
	return &dynamodb.PutItemOutput{}, nil
}

func sval(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func TestRecordTerminal_SuccessOmitsBody(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	err := s.RecordTerminal(context.Background(), "s3:b/k", "2026-01-01T00:00:00.000",
		model.RunResult{State: model.StateSuccess, Stats: model.TransferStats{Bytes: 100}},
		"i#0", "2026-01-01T00:00:00.000", "") // body 空
	if err != nil {
		t.Fatal(err)
	}
	// PK 必须是 MakePK(source)，与 Python 一致
	if got := sval(f.lastItem["source_hash"]); got != MakePK("s3:b/k") {
		t.Errorf("source_hash = %q, want %q", got, MakePK("s3:b/k"))
	}
	if _, ok := f.lastItem["message_body"]; ok {
		t.Error("SUCCESS 不应写 message_body")
	}
	if _, ok := f.lastItem["error_class"]; ok {
		t.Error("SUCCESS 不应有 error_class")
	}
}

func TestRecordTerminal_FailureIncludesBodyAndError(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	_ = s.RecordTerminal(context.Background(), "s3:b/k", "ts",
		model.RunResult{
			State: model.StateRetryable, ErrorClass: "src_rate_limit",
			ErrorMessage: "429 too many requests",
		},
		"i#0", "ts", `{"source":"s3:b/k","destination":"s3:d/k"}`)
	if _, ok := f.lastItem["message_body"]; !ok {
		t.Error("失败态应写完整 message_body")
	}
	if sval(f.lastItem["error_class"]) != "src_rate_limit" {
		t.Error("应写 error_class")
	}
	if f.lastTable != "status-tbl" {
		t.Errorf("表名错: %s", f.lastTable)
	}
}

func TestRecordTerminal_TruncatesLargeErrorAndBodyUTF8(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	long := strings.Repeat("界", maxTerminalTextBytes)
	_ = s.RecordTerminal(context.Background(), "s3:b/k", "ts",
		model.RunResult{
			State: model.StateFatal, ErrorClass: "fatal",
			ErrorMessage: long,
		},
		"i#0", "ts", long)

	for _, key := range []string{"error_message", "message_body"} {
		got := sval(f.lastItem[key])
		if len(got) > maxTerminalTextBytes {
			t.Errorf("%s 未截断: len=%d", key, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s 截断后不是合法 UTF-8", key)
		}
	}
}

func TestWriteHeartbeat_TTL(t *testing.T) {
	f := &fakeDDB{}
	s := NewStore(f, "status-tbl", "hb-tbl")
	_ = s.WriteHeartbeat(context.Background(), "i#0", "ts", 1000, 64)
	if f.lastTable != "hb-tbl" {
		t.Errorf("心跳应写 heartbeat 表, got %s", f.lastTable)
	}
	ttl := f.lastItem["ttl"].(*ddbtypes.AttributeValueMemberN).Value
	if ttl != "1300" { // 1000 + 300
		t.Errorf("ttl = %s, want 1300 (now+300)", ttl)
	}
}
