package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/message"
	"github.com/aws-samples/sample-data-transfer-tool/internal/model"
	"github.com/aws-samples/sample-data-transfer-tool/internal/rcd"
)

// fakeRCDServer 模拟 rcd：copyfile 可配置阻塞时长 + 成功/失败。
type fakeRCDServer struct {
	mu          sync.Mutex
	delay       time.Duration // copyfile 阻塞时长（模拟传输耗时）
	errBody     string        // 非空 = 业务失败，返回该 error
	resetErr    string
	statsErr    string
	gotIgnoreTm bool // 捕获最近 copyfile 是否带 _config.IgnoreTimes
	copyCalled  bool
	groupBytes  int64 // 模拟 group 累计字节（copyfile 成功后 +12345，差值=本次）
	srv         *httptest.Server
}

func newFakeRCDServer(t *testing.T) *fakeRCDServer {
	f := &fakeRCDServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delay, errBody := f.delay, f.errBody
		f.mu.Unlock()
		if r.URL.Path == "/operations/copyfile" {
			var body struct {
				Config map[string]any `json:"_config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.gotIgnoreTm = body.Config["IgnoreTimes"] == true
			f.copyCalled = true
			f.mu.Unlock()
		}
		if r.URL.Path == "/operations/copyfile" || r.URL.Path == "/operations/deletefile" {
			// 模拟传输耗时；ctx 取消则提前返回（HTTP 断开）。
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return // 客户端断开，不写响应（模拟 daemon 传输被中止）
			}
			if errBody != "" {
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": errBody})
				return
			}
			// 成功：模拟本次传输使该 group 字节变为 12345（传前已被 stats-reset 清零）。
			f.mu.Lock()
			f.groupBytes = 12345
			f.mu.Unlock()
		}
		if r.URL.Path == "/core/stats-reset" {
			f.mu.Lock()
			resetErr := f.resetErr
			f.mu.Unlock()
			if resetErr != "" {
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": resetErr})
				return
			}
			// runner 传输前清零该 group（独占借出）。
			f.mu.Lock()
			f.groupBytes = 0
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		if r.URL.Path == "/core/stats" {
			f.mu.Lock()
			statsErr := f.statsErr
			f.mu.Unlock()
			if statsErr != "" {
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": statsErr})
				return
			}
			// 传输后读该 group：已清零→本次传输累计，bytes 即本次字节。
			f.mu.Lock()
			b := f.groupBytes
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"bytes": b, "transfers": 1, "elapsedTime": 0.5, "speed": 24690})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRCDServer) runner(timeout time.Duration) *Runner {
	return NewRunner(rcd.NewWithBaseURL(f.srv.URL, "u", "p", 16), RunnerConfig{Timeout: timeout})
}

var copyMsg = message.TransferMessage{Source: "s3:b/k", Destination: "s3:b/k2", Op: model.OpCopy}

func TestRunCopy_SyncSuccess(t *testing.T) {
	f := newFakeRCDServer(t)
	f.delay = 10 * time.Millisecond
	res := f.runner(5*time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateSuccess {
		t.Errorf("应 SUCCESS，got %s (%s)", res.State, res.ErrorClass)
	}
	// 字节来自 rcd group stats（fake 返回 12345），不依赖 object_size
	if res.Stats.Bytes != 12345 {
		t.Errorf("bytes 应来自 rcd group stats=12345, got %d", res.Stats.Bytes)
	}
}

// 防双写命门：传输超过 Timeout → ctx deadline → HTTP 断开 → UNKNOWN/rclone_timeout。
func TestRunCopy_TimeoutAborts(t *testing.T) {
	f := newFakeRCDServer(t)
	f.delay = 5 * time.Second            // 传输很慢
	r := f.runner(80 * time.Millisecond) // 但超时很短
	t0 := time.Now()
	res := r.RunCopy(context.Background(), copyMsg)
	if time.Since(t0) > 2*time.Second {
		t.Error("超时应快速返回（HTTP 断开），不应等满 5s")
	}
	if res.State != model.StateUnknown || res.ErrorClass != "rclone_timeout" {
		t.Errorf("超时应判 UNKNOWN/rclone_timeout，got %s/%s", res.State, res.ErrorClass)
	}
}

// 优雅停机：父 ctx 取消 → HTTP 断开 → UNKNOWN/worker_shutdown。
func TestRunCopy_CancelAborts(t *testing.T) {
	f := newFakeRCDServer(t)
	f.delay = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res := f.runner(10*time.Second).RunCopy(ctx, copyMsg)
	if res.State != model.StateUnknown || res.ErrorClass != "worker_shutdown" {
		t.Errorf("取消应判 UNKNOWN/worker_shutdown，got %s/%s", res.State, res.ErrorClass)
	}
}

func TestRunCopy_Rate429Retryable(t *testing.T) {
	f := newFakeRCDServer(t)
	f.errBody = "operation error S3: GetObject StatusCode: 429 TooManyRequests"
	res := f.runner(5*time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateRetryable || res.ErrorClass != "src_rate_limit" {
		t.Errorf("429 应 RETRYABLE/src_rate_limit，got %s/%s", res.State, res.ErrorClass)
	}
}

// 语义锁定(G2 分析结论):copy 源不存在时,rcd copyfile RPC 返回非 200 error →
// 判 FATAL/src_not_found(进 DLQ 留底),**不依赖 stats**。这是 rcd 架构对 Python
// "零传输假成功"盲点的天然规避——copyfile 单文件 RPC 源缺失直接报错,不像 CLI copyto
// 会退化成父目录空同步 exit 0 假成功。锁死此语义:防未来有人在成功路径用 transfers==0
// 做 fatal 判定(会误伤"传成功但 stats 抖动"的正常传输,见 StatsReadFailureStillSuccess)。
func TestRunCopy_SourceNotFoundFatalNoStatsDependency(t *testing.T) {
	f := newFakeRCDServer(t)
	f.errBody = "source doesn't exist" // rcd copyfile 源对象不存在
	res := f.runner(5*time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateFatal || res.ErrorClass != "src_not_found" {
		t.Errorf("copy 源不存在应 FATAL/src_not_found（不走假成功），got %s/%s", res.State, res.ErrorClass)
	}
}

// 2026-07-22 GCS h2 僵死连接事故语义锁定：rcd copyfile 返回 "http2: timeout awaiting
// response headers" 应判 RETRYABLE/net_transient（非 uncategorized）→ 30s 退避而非 0s 立即
// 重投，避免把失败消息反复灌回坏连接、打爆 SDK retry token。
func TestRunCopy_H2StuckConnectionNetTransient(t *testing.T) {
	f := newFakeRCDServer(t)
	f.errBody = `operation error S3: HeadObject, https response error StatusCode: 0, request send failed, ` +
		`Head "https://x.storage.googleapis.com/a.gz": http2: timeout awaiting response headers`
	res := f.runner(5 * time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateRetryable || res.ErrorClass != "net_transient" {
		t.Errorf("h2 僵死连接应 RETRYABLE/net_transient，got %s/%s", res.State, res.ErrorClass)
	}
}

// 回归(Codex 交叉审出)：delete 遇网络错误 "no such host" 不得被 IsDeleteNoop 的宽泛 "no such"
// 误判为幂等 SUCCESS——那会删掉消息但删除操作实际没执行(假成功丢数据)。应判 RETRYABLE/net_transient。
func TestRunCopy_DeleteNetworkErrorNotFalseSuccess(t *testing.T) {
	f := newFakeRCDServer(t)
	f.errBody = `Delete "https://x.storage.googleapis.com/a": dial tcp: lookup x: no such host`
	delMsg := message.TransferMessage{Destination: "s3:b/k", Op: model.OpDelete}
	res := f.runner(5 * time.Second).RunCopy(context.Background(), delMsg)
	if res.State == model.StateSuccess {
		t.Fatalf("delete 遇 'no such host' 网络错误被误判假成功(会丢消息)，got %s/%s", res.State, res.ErrorClass)
	}
	if res.State != model.StateRetryable || res.ErrorClass != "net_transient" {
		t.Errorf("delete 网络错误应 RETRYABLE/net_transient，got %s/%s", res.State, res.ErrorClass)
	}
}

func TestRunCopy_DeleteNoopIdempotent(t *testing.T) {
	f := newFakeRCDServer(t)
	f.errBody = "object not found" // 删不存在的对象
	delMsg := message.TransferMessage{Destination: "s3:b/k", Op: model.OpDelete}
	res := f.runner(5*time.Second).RunCopy(context.Background(), delMsg)
	if res.State != model.StateSuccess {
		t.Errorf("delete 不存在对象应幂等 SUCCESS，got %s/%s", res.State, res.ErrorClass)
	}
}

// refresh op：execute 应注入 IgnoreTimes（强制重传刷新 metadata），并成功。
func TestRunCopy_RefreshInjectsIgnoreTimes(t *testing.T) {
	f := newFakeRCDServer(t)
	f.delay = 10 * time.Millisecond
	refreshMsg := message.TransferMessage{Source: "s3:b/k", Destination: "s3:b/k2", Op: model.OpRefresh}
	res := f.runner(5*time.Second).RunCopy(context.Background(), refreshMsg)
	if res.State != model.StateSuccess {
		t.Fatalf("refresh 应 SUCCESS，got %s", res.State)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gotIgnoreTm {
		t.Error("refresh 应注入 _config.IgnoreTimes=true")
	}
}

func TestRunCopy_StatsResetFailureRetryableNoCopy(t *testing.T) {
	f := newFakeRCDServer(t)
	f.resetErr = "stats reset unavailable"
	res := f.runner(5*time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateRetryable || res.ErrorClass != "rcd_stats_reset" {
		t.Fatalf("stats reset 失败应 RETRYABLE/rcd_stats_reset，got %s/%s", res.State, res.ErrorClass)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.copyCalled {
		t.Fatal("stats reset 失败不应启动 copy")
	}
}

func TestRunCopy_StatsReadFailureStillSuccessBytesZero(t *testing.T) {
	f := newFakeRCDServer(t)
	f.statsErr = "stats unavailable"
	res := f.runner(5*time.Second).RunCopy(context.Background(), copyMsg)
	if res.State != model.StateSuccess {
		t.Fatalf("stats read 失败不应阻塞成功，got %s/%s", res.State, res.ErrorClass)
	}
	if res.Stats.Bytes != 0 {
		t.Fatalf("stats read 失败时 bytes 应为 0，got %d", res.Stats.Bytes)
	}
}
