package main

import (
	"encoding/json"
	"testing"
)

var testDest = Dest{DestBucket: "my-s3-bucket", DestPrefix: "migrated"}

func TestMapFinalizeToCopy(t *testing.T) {
	attrs := map[string]string{
		"eventType": eventFinalize,
		"bucketId":  "eu-abc-dw",
		"objectId":  "libs/hive/part-00275.orc",
	}
	payload := []byte(`{"size":"1048576","storageClass":"STANDARD"}`)
	got, err := mapEvent(attrs, payload, testDest)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.skip {
		t.Fatal("FINALIZE 不应跳过")
	}
	if got.ObjectSize != 1048576 {
		t.Errorf("size=%d want 1048576（从 payload 取）", got.ObjectSize)
	}
	var m migrationMessage
	json.Unmarshal([]byte(got.Body), &m)
	if m.Source != "gcs:eu-abc-dw/libs/hive/part-00275.orc" {
		t.Errorf("source=%q", m.Source)
	}
	if m.Destination != "s3:my-s3-bucket/migrated/libs/hive/part-00275.orc" {
		t.Errorf("destination=%q", m.Destination)
	}
}

func TestMapMetadataUpdateToCopy(t *testing.T) {
	// METADATA_UPDATE 与 FINALIZE 等价映射为 copy（重新同步该对象）
	attrs := map[string]string{
		"eventType": eventMetaUpdate,
		"bucketId":  "eu-abc-dw",
		"objectId":  "libs/x/meta-changed.orc",
	}
	got, err := mapEvent(attrs, []byte(`{"size":"2048"}`), testDest)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.skip {
		t.Fatal("METADATA_UPDATE 不应跳过")
	}
	if got.ObjectSize != 2048 {
		t.Errorf("size=%d want 2048", got.ObjectSize)
	}
	var m migrationMessage
	json.Unmarshal([]byte(got.Body), &m)
	if m.Source != "gcs:eu-abc-dw/libs/x/meta-changed.orc" {
		t.Errorf("source=%q", m.Source)
	}
	if m.Destination != "s3:my-s3-bucket/migrated/libs/x/meta-changed.orc" {
		t.Errorf("destination=%q", m.Destination)
	}
}

func TestMapOtherEventsSkipped(t *testing.T) {
	// DELETE/ARCHIVE/INITIALIZE/空 一律跳过（只监控 FINALIZE + METADATA_UPDATE）
	for _, et := range []string{"OBJECT_DELETE", "OBJECT_ARCHIVE", "OBJECT_INITIALIZE", ""} {
		attrs := map[string]string{"eventType": et, "bucketId": "b", "objectId": "k"}
		got, err := mapEvent(attrs, nil, testDest)
		if err != nil {
			t.Fatalf("%s: err %v", et, err)
		}
		if !got.skip {
			t.Errorf("%s 应跳过", et)
		}
	}
}

func TestMapNoPrefix(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "b", "objectId": "k.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"10"}`), Dest{DestBucket: "dst"})
	var m migrationMessage
	json.Unmarshal([]byte(got.Body), &m)
	if m.Destination != "s3:dst/k.bin" {
		t.Errorf("无 prefix destination=%q want s3:dst/k.bin", m.Destination)
	}
}

func TestMapFinalizeMissingFields(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "b"} // 缺 objectId
	if _, err := mapEvent(attrs, []byte(`{"size":"1"}`), testDest); err == nil {
		t.Error("缺 objectId 应报错")
	}
}

func TestMapBadPayloadSize(t *testing.T) {
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "b", "objectId": "k"}
	if _, err := mapEvent(attrs, []byte(`{not json`), testDest); err == nil {
		t.Error("坏 payload 应报错（让上游 nack 重投）")
	}
}

func TestParseSizeEmptyPayload(t *testing.T) {
	if n, err := parseSize(nil); err != nil || n != 0 {
		t.Errorf("空 payload: n=%d err=%v want 0/nil", n, err)
	}
}

func TestSpecialCharKeyPreserved(t *testing.T) {
	// GCS key 含空格/中文/连续斜杠等，destination 须字面保留（与现有迁移管道一致）
	attrs := map[string]string{"eventType": eventFinalize, "bucketId": "b", "objectId": "数据/a b//c.bin"}
	got, _ := mapEvent(attrs, []byte(`{"size":"5"}`), Dest{DestBucket: "dst", DestPrefix: "p"})
	var m migrationMessage
	json.Unmarshal([]byte(got.Body), &m)
	if m.Destination != "s3:dst/p/数据/a b//c.bin" {
		t.Errorf("特殊字符 key 未字面保留: %q", m.Destination)
	}
}
