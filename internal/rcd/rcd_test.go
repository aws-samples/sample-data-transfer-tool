package rcd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return NewWithBaseURL(srv.URL, "u", "pw", 16)
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
	// 错误文本应回传 rcd 的 error 字段（供四态分类，含 429 token）并带上 path（排障定位）。
	if !strings.Contains(err.Error(), "StatusCode: 429 too many requests") {
		t.Errorf("应回传 rcd error 文本，got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/operations/copyfile") {
		t.Errorf("错误应带 rc path 上下文，got %q", err.Error())
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

func TestGlobalStats_NoGroupShortPayload(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path != "/core/stats" {
			t.Errorf("应调 core/stats，got %s", path)
		}
		// 全局活性探测：不带 group，带 short=true（省略大数组）。
		if _, hasGroup := body["group"]; hasGroup {
			t.Errorf("GlobalStats 不应带 group，got %v", body["group"])
		}
		if body["short"] != true {
			t.Errorf("GlobalStats 应带 short=true，got %v", body["short"])
		}
		return 200, map[string]any{"bytes": 12345, "transfers": 7, "errors": 1}
	})
	defer srv.Close()
	gs, err := clientFor(srv).GlobalStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gs.Bytes != 12345 || gs.Transfers != 7 || gs.Errors != 1 {
		t.Errorf("全局 stats 解析错: %+v", gs)
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

// rcd 的 stats group 只在首次有传输写入 _group 时才创建；传输前 reset 一个尚不存在的
// group，core/stats-reset 返回 `group "<g>" not found`。此时计数本就是 0（零态已达成），
// 应视作成功而非失败，否则每个 group 首用必失败 → 传输永不发起 → group 永远建不起来。
func TestResetStatsGroup_NotFoundTreatedAsZeroState(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		if path != "/core/stats-reset" {
			t.Errorf("应调 core/stats-reset，got %s", path)
		}
		g, _ := body["group"].(string)
		return 500, map[string]any{"error": "group \"" + g + "\" not found"}
	})
	defer srv.Close()
	if err := clientFor(srv).ResetStatsGroup(context.Background(), "go-worker-0"); err != nil {
		t.Fatalf("group 不存在 = 零态，reset 应视作成功，got: %v", err)
	}
}

// 仅吞 group-not-found；其他 reset 错误（网络/认证/参数/endpoint）必须照常传播，
// 否则会让传输在未确认 reset 的状态下继续。
func TestResetStatsGroup_OtherErrorsPropagate(t *testing.T) {
	srv := newFakeRCD(t, func(path string, body map[string]any) (int, any) {
		return 500, map[string]any{"error": "couldn't connect to backend"}
	})
	defer srv.Close()
	if err := clientFor(srv).ResetStatsGroup(context.Background(), "go-worker-0"); err == nil {
		t.Fatal("非 not-found 的 reset 错误必须传播，不能吞掉")
	}
}

func TestOversizedSuccessResponseFailsBeforeUnmarshal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBodyBytes+1)))
	}))
	defer srv.Close()

	_, err := NewWithBaseURL(srv.URL, "u", "pw", 16).StatsByGroup(context.Background(), "g")
	if err == nil {
		t.Fatal("超大响应应返回错误")
	}
	if !strings.Contains(err.Error(), "响应超过") {
		t.Fatalf("错误应说明响应过大，got %v", err)
	}
}
