package rcd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFakeRCD(t *testing.T, handler func(path string, body map[string]any) (int, any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "pw" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		code, resp := handler(r.URL.Path, body)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func clientFor(srv *httptest.Server) *Client {
	return NewWithBaseURL(srv.URL, "u", "pw")
}

func TestCopyFile_SyncSuccess(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path != "/operations/copyfile" {
			t.Errorf("意外 path %s", path)
		}
		if _, ok := body["_async"]; ok {
			t.Error("同步调用不应带 _async")
		}
		if body["srcFs"] != "s3:" || body["srcRemote"] != "b/k" {
			t.Errorf("src 错: %v/%v", body["srcFs"], body["srcRemote"])
		}
		return 200, map[string]any{} // 空对象 = 成功
	})
	defer srv.Close()

	if err := clientFor(srv).CopyFile(context.Background(), "s3:", "b/k", "s3:", "b/k2", "", false); err != nil {
		t.Fatalf("同步 copyfile 应成功: %v", err)
	}
}

// forceRefresh=true 注入 _config{IgnoreTimes:true}（强制重传刷新 metadata）。
func TestCopyFile_ForceRefreshInjectsIgnoreTimes(t *testing.T) {
	srv := newFakeRCD(t, func(_ string, body map[string]any) (int, any) {
		cfg, ok := body["_config"].(map[string]any)
		if !ok {
			t.Fatalf("forceRefresh 应注入 _config，got %v", body["_config"])
		}
		if cfg["IgnoreTimes"] != true {
			t.Errorf("_config.IgnoreTimes 应=true，got %v", cfg["IgnoreTimes"])
		}
		return 200, map[string]any{}
	})
	defer srv.Close()
	if err := clientFor(srv).CopyFile(context.Background(), "s3:", "b/k", "s3:", "b/k2", "", true); err != nil {
		t.Fatal(err)
	}
}

func TestCopyFile_BusinessError(t *testing.T) {
	srv := newFakeRCD(t, func(_ string, _ map[string]any) (int, any) {
		return 500, map[string]any{"error": "StatusCode: 429 too many requests"}
	})
	defer srv.Close()
	err := clientFor(srv).CopyFile(context.Background(), "s3:", "b/k", "s3:", "b/k2", "", false)
	if err == nil {
		t.Fatal("rc 失败应返回 error")
	}
	// 错误文本应回传 rcd 的 error 字段（供四态分类）
	if err.Error() != "StatusCode: 429 too many requests" {
		t.Errorf("应回传 rcd error 文本，got %q", err.Error())
	}
}

func TestDeleteFile_Sync(t *testing.T) {
	called := false
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path == "/operations/deletefile" {
			called = true
			if body["fs"] != "s3:" || body["remote"] != "b/k" {
				t.Errorf("delete 参数错: %v/%v", body["fs"], body["remote"])
			}
		}
		return 200, map[string]any{}
	})
	defer srv.Close()
	if err := clientFor(srv).DeleteFile(context.Background(), "s3:", "b/k"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("deletefile 未被调用")
	}
}

func TestSetBwLimit(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path != "/core/bwlimit" {
			t.Errorf("应调 core/bwlimit，got %s", path)
		}
		if body["rate"] != "100M" {
			t.Errorf("rate 错: %v", body["rate"])
		}
		return 200, map[string]any{"rate": "100Mi"}
	})
	defer srv.Close()
	if err := clientFor(srv).SetBwLimit(context.Background(), "100M"); err != nil {
		t.Fatal(err)
	}
}

func TestSetTPSLimit(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path != "/options/set" {
			t.Errorf("应调 options/set，got %s", path)
		}
		main, ok := body["main"].(map[string]any)
		if !ok {
			t.Fatalf("应带 main 段: %v", body)
		}
		// TPSLimit 必须是数字（JSON 解析为 float64）
		if _, ok := main["TPSLimit"].(float64); !ok {
			t.Errorf("TPSLimit 必须是 float64，got %T", main["TPSLimit"])
		}
		return 200, map[string]any{}
	})
	defer srv.Close()
	if err := clientFor(srv).SetTPSLimit(context.Background(), 156.25); err != nil {
		t.Fatal(err)
	}
}
