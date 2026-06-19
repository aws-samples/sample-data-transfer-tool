package main

import (
	"encoding/json"
	"testing"
)

func TestBuildBodyUsesJSONEscaping(t *testing.T) {
	body, err := buildBody("bucket", `src"key`, `dst\key`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body 应为合法 JSON: %v", err)
	}
	if got["source"] != `s3:bucket/src"key` {
		t.Fatalf("source 未正确保留/转义: %q", got["source"])
	}
	if got["destination"] != `s3:bucket/dst\key` {
		t.Fatalf("destination 未正确保留/转义: %q", got["destination"])
	}
}
