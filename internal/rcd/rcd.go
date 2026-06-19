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
	"strings"
	"time"
)

const maxResponseBodyBytes = 1 << 20

// Client 一个指向本机 rcd 的轻客户端（127.0.0.1:5572 + basic auth）。
type Client struct {
	baseURL string
	user    string
	pass    string
	http    *http.Client
}

// New 构造客户端。addr 形如 "127.0.0.1:5572"。maxConns 为预期峰值并发（≈ worker
// goroutine 数 + 余量），用于设连接池上限——所有 goroutine 都打同一个 host(localhost
// rcd)，默认 Transport 的 MaxIdleConnsPerHost=2 会让 stats 等短调用的连接用完即弃、
// 频繁重建 TCP（高 TPS 下还可能耗尽临时端口），与"连接复用消灭握手开销"的设计相悖。
func New(addr string, user, pass string, maxConns int) *Client {
	return NewWithBaseURL("http://"+addr, user, pass, maxConns)
}

// NewWithBaseURL 用完整 baseURL 构造（如 http://127.0.0.1:5572）。供测试注入 test server。
func NewWithBaseURL(baseURL, user, pass string, maxConns int) *Client {
	if maxConns < 2 {
		maxConns = 2
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	// 全部并发都连同一个 localhost host，per-host 上限必须 = 峰值并发，否则空闲连接
	// 被丢弃后短调用反复重建。MaxIdleConns 同步放大。
	t.MaxIdleConns = maxConns
	t.MaxIdleConnsPerHost = maxConns
	t.MaxConnsPerHost = maxConns
	t.IdleConnTimeout = 90 * time.Second
	return &Client{
		baseURL: baseURL,
		user:    user,
		pass:    pass,
		// 同步传输可能跑数分钟：HTTP client 不设固定 Timeout，由每次调用的 ctx
		// deadline 控制（= RCLONE_TIMEOUT，≤0.7×visibility，防双写）。
		http: &http.Client{Transport: t},
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

// ResetStatsGroup 清零指定 group 的计数（core/stats-reset {group}）。仅清 bytes/transfers
// 等计数与 startedTransfers，**不删除 group 的 StatsInfo 对象**（对象在 runner 的 group 池
// 里长期复用，不重新引入 per-call StatsInfo 泄漏）。传输前 reset、传输后读 bytes，即得本次
// 传输字节，无需累计差值（避免长跑累计值无界增长 / rcd 重启后基准失效等边界）。
func (c *Client) ResetStatsGroup(ctx context.Context, group string) error {
	err := c.post(ctx, "/core/stats-reset", map[string]any{"group": group}, nil)
	if err != nil && group != "" {
		// rcd 的 stats group 仅在首次有传输写入 _group 时才创建。传输前 reset 一个尚未
		// 建立的 group，core/stats-reset 返回 `group "<g>" not found`——此时计数本就是 0，
		// 零态已达成，视作成功。仅精确匹配「该 group 的 not found」，其余错误（网络/认证/
		// 参数/endpoint missing）照常传播，避免在未确认 reset 的状态下继续传输。
		if strings.Contains(err.Error(), `group "`+group+`" not found`) {
			return nil
		}
	}
	return err
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
	body, truncated, readErr := readLimited(resp.Body, maxResponseBodyBytes)
	if readErr != nil {
		return fmt.Errorf("读取 %s 响应: %w", path, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		// rc 业务失败：优先回传 rcd 的 error 文本（供四态分类）。
		var re rcError
		if json.Unmarshal(body, &re) == nil && re.Error != "" {
			return fmt.Errorf("%s", re.Error)
		}
		if truncated {
			return fmt.Errorf("rc %s 返回 %d: %s...(truncated)", path, resp.StatusCode, truncate(body, 512))
		}
		return fmt.Errorf("rc %s 返回 %d: %s", path, resp.StatusCode, truncate(body, 512))
	}
	if out != nil {
		if truncated {
			return fmt.Errorf("rc %s 响应超过 %d bytes", path, maxResponseBodyBytes)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("解析 %s 响应: %w", path, err)
		}
	}
	return nil
}

func readLimited(r io.Reader, max int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > max {
		return body[:max], true, nil
	}
	return body, false, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
