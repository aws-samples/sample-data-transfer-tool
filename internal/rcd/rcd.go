// Package rcd 封装与 rclone 守护进程（rclone rcd）的 HTTP rc 交互。
//
// 核心契约（替代 Python subprocess+killpg 模型）：
//   - CopyFile/DeleteFile: 同步调用 operations/copyfile|deletefile（不带 _async），
//     HTTP 请求阻塞到传输完成才返回。worker goroutine 直接拿四态，无需轮询。
//   - 防双写：HTTP 请求携带 ctx（带 RCLONE_TIMEOUT deadline）。ctx 取消（超时/停机）
//     → HTTP 断开 → rcd 的传输 context 随之取消 → daemon 中止传输。实测确认：断开后
//     core/stats 字节冻结（killpg 的等价替身，无需 job/stop）。
//   - SetBwLimit/SetTPSLimit: 运行时设 rcd 全局限速（core/bwlimit + options/set），
//     一次生效全部传输，语义正确（per-host 总限速，不用除并发数）。
//
// 连接复用：实测 rcd 在 rc 调用间复用同一条到 S3 的 TLS 连接（30 次拷贝仅 1 条连接），
// 这是消灭小文件每文件 fork+握手开销的根据。
package rcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client 一个指向本机 rcd 的轻客户端（127.0.0.1:5572 + basic auth）。
type Client struct {
	baseURL string
	user    string
	pass    string
	http    *http.Client
}

// New 构造客户端。addr 形如 "127.0.0.1:5572"。
func New(addr, user, pass string) *Client {
	return NewWithBaseURL("http://"+addr, user, pass)
}

// NewWithBaseURL 用完整 baseURL 构造（如 http://127.0.0.1:5572）。供测试注入 test server。
func NewWithBaseURL(baseURL, user, pass string) *Client {
	return &Client{
		baseURL: baseURL,
		user:    user,
		pass:    pass,
		// 同步传输可能跑数分钟：HTTP client 不设固定 Timeout，由每次调用的 ctx
		// deadline 控制（= RCLONE_TIMEOUT，≤0.7×visibility，防双写）。
		http: &http.Client{},
	}
}

// CopyFile 同步拷贝单文件，阻塞到完成。group 非空时按该 stats group 归集本次传输
// 统计（供传完用 GroupStats 查真实字节，取代不可靠的 SQS object_size）。
// forceRefresh=true 注入 _config{IgnoreTimes:true}：忽略 size/mtime 检查强制重传，
// 使源端 metadata-only 变更也能刷新到目标（数据未变时 rclone 默认 skip）。代价=重传数据。
func (c *Client) CopyFile(ctx context.Context, srcFs, srcRemote, dstFs, dstRemote, group string, forceRefresh bool) error {
	payload := map[string]any{
		"srcFs":     srcFs,
		"srcRemote": srcRemote,
		"dstFs":     dstFs,
		"dstRemote": dstRemote,
	}
	if group != "" {
		payload["_group"] = group
	}
	if forceRefresh {
		payload["_config"] = map[string]any{"IgnoreTimes": true}
	}
	return c.post(ctx, "/operations/copyfile", payload, nil)
}

// DeleteFile 同步删除目标端单对象，阻塞到完成。
func (c *Client) DeleteFile(ctx context.Context, fs, remote string) error {
	return c.post(ctx, "/operations/deletefile", map[string]any{"fs": fs, "remote": remote}, nil)
}

// GroupStats 查询某 stats group 的真实传输统计（rcd 实测值，非消息体推断）。
type GroupStats struct {
	Bytes       int64   `json:"bytes"`
	Transfers   int64   `json:"transfers"`
	ElapsedTime float64 `json:"elapsedTime"`
	Speed       float64 `json:"speed"`
	Errors      int64   `json:"errors"`
}

// StatsByGroup 查 core/stats 指定 group 的统计。
func (c *Client) StatsByGroup(ctx context.Context, group string) (GroupStats, error) {
	var s GroupStats
	err := c.post(ctx, "/core/stats", map[string]any{"group": group}, &s)
	return s, err
}

// DeleteStatsGroup 删除某 group 的累积统计（用完即清，防 rcd 内存随 group 数增长）。
func (c *Client) DeleteStatsGroup(ctx context.Context, group string) {
	_ = c.post(ctx, "/core/stats-delete", map[string]any{"group": group}, nil)
}

// SetBwLimit 运行时设 rcd 全局带宽限速。rate 形如 "100M"，"off" 解除。
func (c *Client) SetBwLimit(ctx context.Context, rate string) error {
	return c.post(ctx, "/core/bwlimit", map[string]any{"rate": rate}, nil)
}

// SetTPSLimit 运行时设 rcd 全局 TPS 限速（options/set；core 无 tpslimit 端点）。
// tps<=0 表示不限（设 0）。rcd 的 TPSLimit 是 float64。
func (c *Client) SetTPSLimit(ctx context.Context, tps float64) error {
	return c.post(ctx, "/options/set", map[string]any{"main": map[string]any{"TPSLimit": tps}}, nil)
}

// Noop 就绪探测：rc/noop。
func (c *Client) Noop(ctx context.Context) error {
	return c.post(ctx, "/rc/noop", map[string]any{}, nil)
}

// rcError rc 调用失败时 rcd 返回的 JSON（含 error 字段）。
type rcError struct {
	Error string `json:"error"`
}

// post 发一个 rc 调用。out 非 nil 时解析 200 响应；非 200 时尽力解析 error 字段。
func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码 %s 请求: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.http.Do(req)
	if err != nil {
		// ctx 取消/超时也走这里：HTTP 断开 → rcd 传输随之中止（防双写）。
		return fmt.Errorf("调用 %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// rc 业务失败：优先回传 rcd 的 error 文本（供四态分类）。
		var re rcError
		if json.Unmarshal(body, &re) == nil && re.Error != "" {
			return fmt.Errorf("%s", re.Error)
		}
		return fmt.Errorf("rc %s 返回 %d: %s", path, resp.StatusCode, truncate(body, 512))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("解析 %s 响应: %w", path, err)
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
