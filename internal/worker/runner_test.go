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
	gotIgnoreTm bool          // 捕获最近 copyfile 是否带 _config.IgnoreTimes
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
		}
		if r.URL.Path == "/core/stats" {
			// 模拟 group stats：固定返回 12345 字节（验证 runner 用 rcd 真实字节）。
			_ = json.NewEncoder(w).Encode(map[string]any{"bytes": 12345, "transfers": 1, "elapsedTime": 0.5, "speed": 24690})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRCDServer) runner(timeout time.Duration) *Runner {
	return NewRunner(rcd.NewWithBaseURL(f.srv.URL, "u", "p"), RunnerConfig{Timeout: timeout})
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
