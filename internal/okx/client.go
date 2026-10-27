// Package okx 封装 OKX v5 REST API：签名、限速、重试与响应解析。
// 本层不含业务语义（DCY/期权决策逻辑在 strategy 层）。
package okx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const (
	// DefaultBaseURL 实盘 REST 基地址。
	DefaultBaseURL = "https://www.okx.com"

	// DemoHeader 模拟盘标识头（value=1 即模拟盘）。
	DemoHeader = "x-simulated-trading"

	// FlagLive / FlagDemo OKX 官方 flag 语义。
	FlagLive = "0"
	FlagDemo = "1"
)

// Client OKX REST 客户端。
type Client struct {
	apiKey     string
	secret     string
	passphrase string
	flag       string // FlagLive | FlagDemo

	baseURL string
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient 创建客户端。未提供 WithAuth 时为纯公开数据（无需密钥）。
func NewClient(opts ...Option) *Client {
	c := &Client{
		flag:    FlagLive,
		baseURL: DefaultBaseURL,
		http: &http.Client{
			// OKX 建议交易类超时留足；公开行情 5s 足够
			Timeout: 5 * time.Second,
		},
		// 公开行情速率上限 20 req/2s，统一取 10 rps、burst 10 留余量
		limiter: rate.NewLimiter(rate.Limit(10), 10),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Option 客户端构造选项。
type Option func(*Client)

// WithAuth 配置 API Key（私有接口必需）。
func WithAuth(key, secret, passphrase string) Option {
	return func(c *Client) {
		c.apiKey, c.secret, c.passphrase = key, secret, passphrase
	}
}

// WithDemo 切到模拟盘（REST 同端点 + 头部标识）。
func WithDemo() Option {
	return func(c *Client) { c.flag = FlagDemo }
}

// WithFlag 显式指定 flag（"0"实盘 / "1"模拟盘）。
func WithFlag(flag string) Option {
	return func(c *Client) { c.flag = flag }
}

// WithBaseURL 覆盖基地址（测试用）。
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithTimeout 覆盖 HTTP 超时。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithProxy 配置 HTTP/SOCKS5 代理（如 http://127.0.0.1:7890）。
func WithProxy(proxyURL string) Option {
	return func(c *Client) {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return // 无效代理则忽略，回落直连
		}
		c.http.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	}
}

// APIError OKX 返回的业务错误（code != "0"）。
type APIError struct {
	Code string
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("okx api error: code=%s msg=%s", e.Code, e.Msg)
}

// HTTPError HTTP 层错误：429/5xx 可重试，其余 4xx 不可重试。
type HTTPError struct {
	StatusCode int
	Head       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Head)
}

// Retryable 判断是否可重试。
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// resp 统一响应外壳。
type resp struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// get 发起限速 + 重试的 GET，data 字段返回原始 JSON。
// path 为-api 路径（不含域名）；query 为空则省略。
func (c *Client) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	if len(query) > 0 {
		// OKX 签名要求参数按原样拼接；这里保持键序稳定（按输入顺序），
		// 便于 timestamp+method+path 的签名可复现。
		path = path + "?" + query.Encode()
	}
	return c.request(ctx, http.MethodGet, path, nil)
}

// doJSON 发起签名 POST（JSON body）。data 字段返回原始 JSON。
func (c *Client) doJSON(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	bb, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return c.request(ctx, method, path, bb)
}

// request 单次请求核心：签名、限速、重试、解析。
func (c *Client) request(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ { // 最多重试 2 次
		if attempt > 0 {
			// 429 / 5xx 退避后重试
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}

		raw, err := c.doOnce(ctx, method, path, body)
		if err != nil {
			lastErr = err
			var apiErr *APIError
			if asAPIError(err, &apiErr) {
				// 业务错误码不重试（签名错误、参数错误等重试无意义）
				return nil, err
			}
			var httpErr *HTTPError
			if errors.As(err, &httpErr) && !httpErr.Retryable() {
				return nil, err // 4XX（除 429）不重试
			}
			continue // 网络/5xx/429 才重试
		}
		return raw, nil
	}
	return nil, fmt.Errorf("retry exhausted: %w", lastErr)
}

func asAPIError(err error, target **APIError) bool {
	if e, ok := err.(*APIError); ok {
		*target = e
		return true
	}
	return false
}

// doOnce 不含重试的单次执行。
func (c *Client) doOnce(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// OKX 统一要求这两个头
	req.Header.Set("Content-Type", "application/json")
	if body != nil {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	// 已配置密钥则签名（公开接口签名头可省略但带上无害）
	if c.apiKey != "" {
		ts := isoTimestamp(time.Now())
		sign := sign(c.secret, ts, method, path, string(body))
		req.Header.Set("OK-ACCESS-KEY", c.apiKey)
		req.Header.Set("OK-ACCESS-SIGN", sign)
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)
	}
	if c.flag == FlagDemo {
		req.Header.Set(DemoHeader, "1")
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
	if err != nil {
		return nil, err
	}

	// HTTP 层错误：429 / 5xx 可重试（交给 request 循环）
	switch {
	case httpResp.StatusCode == http.StatusTooManyRequests:
		return nil, &HTTPError{StatusCode: httpResp.StatusCode}
	case httpResp.StatusCode >= 500:
		return nil, &HTTPError{StatusCode: httpResp.StatusCode}
	case httpResp.StatusCode != http.StatusOK:
		return nil, &HTTPError{StatusCode: httpResp.StatusCode, Head: truncate(string(raw), 300)}
	}

	var r resp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode body: %w (head=%s)", err, truncate(string(raw), 200))
	}
	if r.Code != "0" {
		return nil, &APIError{Code: r.Code, Msg: r.Msg}
	}
	return r.Data, nil
}

// isoTimestamp OKX 签名时间戳格式：2020-12-08T09:08:57.715Z
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// sign HMAC-SHA256(timestamp + method + requestPath + body) → base64
func sign(secret, ts, method, path, body string) string {
	payload := ts + method + path + body
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ctx M0 阶段统一使用可被外部中断的上游 context，这里为缺省后台 ctx。
func ctx() context.Context { return context.Background() }

// RawGet 直通原始 GET（/smoke 与字段核对用，业务层不使用）。
func (c *Client) RawGet(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return c.get(ctx, path, q)
}
