package emf

import (
	"encoding/json"
	"testing"

	"github.com/aws-samples/sample-data-transfer-tool/internal/worker"
)

func TestBuild_SuccessFileCount(t *testing.T) {
	doc, err := build(worker.EMFEvent{
		QueueType: "small", ErrorClass: "none", InstanceID: "i#0",
		State: "SUCCESS", Op: "copy", Bytes: 1024, Elapsed: 1.5, Speed: 700,
	}, 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	if doc["FileCount"] != 1 {
		t.Errorf("SUCCESS 应 FileCount=1, got %v", doc["FileCount"])
	}
	if doc["AttemptCount"] != 1 {
		t.Errorf("AttemptCount 恒 1, got %v", doc["AttemptCount"])
	}
	if doc["TransferredBytes"] != int64(1024) {
		t.Errorf("bytes 错: %v", doc["TransferredBytes"])
	}
}

func TestBuild_NonSuccessFileCountZero(t *testing.T) {
	doc, _ := build(worker.EMFEvent{State: "RETRYABLE", ErrorClass: "src_rate_limit"}, 1)
	if doc["FileCount"] != 0 {
		t.Errorf("非 SUCCESS 应 FileCount=0, got %v", doc["FileCount"])
	}
}

func TestBuild_OpDefaultsCopy(t *testing.T) {
	doc, _ := build(worker.EMFEvent{State: "SUCCESS"}, 1)
	if doc["Op"] != "copy" {
		t.Errorf("Op 空应默认 copy, got %v", doc["Op"])
	}
}

func TestBuild_TimestampRequired(t *testing.T) {
	doc, _ := build(worker.EMFEvent{State: "SUCCESS"}, 1700000000123)
	aws, ok := doc["_aws"].(map[string]any)
	if !ok || aws["Timestamp"] != int64(1700000000123) {
		t.Error("_aws.Timestamp 必填且需正确（缺失则 CloudWatch 不抽取指标）")
	}
}

func TestEmit_OutputsSingleLineJSON(t *testing.T) {
	var captured string
	err := Emit(worker.EMFEvent{State: "SUCCESS", QueueType: "small", ErrorClass: "none", InstanceID: "i"},
		func(s string) { captured = s })
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(captured), &doc); err != nil {
		t.Fatalf("输出非合法 JSON: %v", err)
	}
	// 验证维度值字段都在
	for _, k := range []string{"QueueType", "ErrorClass", "InstanceId", "State", "Op"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("缺维度字段 %s", k)
		}
	}
}
