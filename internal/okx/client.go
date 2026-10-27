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
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

	baseURL   string
	http      *http.Client
	limiter   *rate.Limiter
	retryBase time.Duration // 重试退避基数（测试可调小）
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
		limiter:   rate.NewLimiter(rate.Limit(10), 10),
		retryBase: 400 * time.Millisecond,
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

// hint 为常见业务错误码补充人类指引。
var hint = map[string]string{
	"50105": "Passphrase 不正确：passphrase 是创建 API Key 时 YOU 自己设置的那串口令（不是 OKX 发的），检查是否漏拷/多空格/引号",
	"50106": "IP 白名单未配置或不匹配：OKX 看到的出口 IP 需加入 API Key 白名单（走代理时应填代理出口 IP），或改为不绑定 IP 的 Key",
	"50110": "IP 不在 API Key 白名单：(上方报文的出口 IP) 加入白名单或换不绑 IP 的 Key",
	"50102": "签名时间戳偏差超过 30s：请校对本机时间/时区",
	"50111": "API Key 为空或未传入",
	"50103": "API Key 不存在或已失效（Header 不匹配）",
	"50113": "API Key 权限不足：双币赢查询需「读取」权限，下单需「交易」权限",
}

func (e *APIError) Error() string {
	if h, has := hint[e.Code]; has {
		return fmt.Sprintf("okx api error: code=%s msg=%s → %s", e.Code, e.Msg, h)
	}
	return fmt.Sprintf("okx api error: code=%s msg=%s", e.Code, e.Msg)
}

// HTTPError HTTP 层错误：429/5xx 可重试，其余 4xx 不可重试。
type HTTPError struct {
	StatusCode int
	Head       string
	RetryAfter time.Duration // 429 响应头 Retry-After（若有）
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Head)
}

// Retryable 判断是否可重试。
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// flexReal / flexInt 兼容两种 JSON 数值形态：
// 金融产品端点数值为字符串（"annualizedYield":"0.242"），交易端点可能为裸数字。
type flexReal float64
type flexInt int64

func (f *flexReal) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexReal(v)
	return nil
}

func (i *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" || s == "" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = flexInt(v)
	return nil
}

// flexCode 兼容 string/number 的错误码（双币赢端点返回数字 0，交易端点返回 "0"）。
type flexCode string

// FlexReal 宽容取值转换（供测试构造/业务换算）。
func FlexReal(v float64) flexReal { return flexReal(v) }

// FlexInt 宽容取值转换（供测试构造/业务换算）。
func FlexInt(v int64) flexInt { return flexInt(v) }

func (c *flexCode) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	*c = flexCode(s)
	return nil
}

// OK 判断业务是否成功（"0"）。
func (c flexCode) OK() bool { return c == "0" }

// OKErr 判断是否成功。
func (c flexCode) OKErr() bool { return c == "0" }

// resp 统一响应外壳（code 兼容数字与字符串）。
type resp struct {
	Code flexCode        `json:"code"`
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
//
// 重试策略（M4 硬化）：
//   - 网络错误 / 429 / 5xx：指数退避（400ms 起，带抖动，429 优先用 Retry-After），最多 2 次
//   - 业务码 50011（限速）：请求未被处理，任何方法均可重试
//   - 其余业务码：GET 的瞬时故障（50001/50026/50013/50004/50005）可重试；
//     POST 一律不重试，避免「下单其实已成功、重试再下一单」的重复成交
func (c *Client) request(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}

		raw, err := c.doOnce(ctx, method, path, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err

		delay, retry := c.retryDelay(method, err, attempt)
		if !retry {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, fmt.Errorf("retry exhausted: %w", lastErr)
}

// retryDelay 返回是否重试与本次退避时长。
func (c *Client) retryDelay(method string, err error, attempt int) (time.Duration, bool) {
	var apiErr *APIError
	if asAPIError(err, &apiErr) {
		if !retryableBusinessCode(method, apiErr.Code) {
			return 0, false
		}
		return c.backoff(attempt, 0), true
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if !httpErr.Retryable() {
			return 0, false
		}
		return c.backoff(attempt, httpErr.RetryAfter), true
	}
	// 传输层错误（超时/连接重置/代理断流）可重试。
	return c.backoff(attempt, 0), true
}

// retryableBusinessCode 判断业务错误码能否安全重试。
func retryableBusinessCode(method, code string) bool {
	if code == "50011" { // Too Many Requests：请求未进入撮合/申购
		return true
	}
	if method != http.MethodGet {
		return false // 写操作不重试，防重复下单
	}
	switch code {
	case "50001", // Service temporarily unavailable
		"50026", // System error
		"50013", // Systems busy
		"50004", // API timeout（GET 无副作用，可重试）
		"50005": // API temporarily unavailable
		return true
	}
	return false
}

// backoff 指数退避 + ±25% 抖动；Retry-After 优先，最长 5s。
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 5*time.Second {
			return 5 * time.Second
		}
		return retryAfter
	}
	base := c.retryBase
	if base <= 0 {
		base = 400 * time.Millisecond
	}
	d := base << (attempt - 1)
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	if half := int64(d) / 4; half > 0 {
		d += time.Duration(rand.Int63n(half))
	}
	return d
}

// parseRetryAfter 解析 Retry-After（秒数或 HTTP 日期）。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
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

	// 4xx/5xx 时 body 往往携带业务错误码（401 → {"code":"50105",...}），
	// 优先按业务错误解析，给出可读性更好的 APIError。
	var pre resp
	if uerr := json.Unmarshal(raw, &pre); uerr == nil && pre.Code != "" && !pre.Code.OK() {
		return nil, &APIError{Code: string(pre.Code), Msg: pre.Msg}
	}

	// 纯 HTTP 层错误：429 / 5xx 可重试（交给 request 循环）
	switch {
	case httpResp.StatusCode == http.StatusTooManyRequests:
		return nil, &HTTPError{
			StatusCode: httpResp.StatusCode,
			RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
		}
	case httpResp.StatusCode >= 500:
		return nil, &HTTPError{StatusCode: httpResp.StatusCode}
	case httpResp.StatusCode != http.StatusOK:
		return nil, &HTTPError{StatusCode: httpResp.StatusCode, Head: truncate(string(raw), 300)}
	}

	var r resp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode body: %w (head=%s)", err, truncate(string(raw), 200))
	}
	if !r.Code.OK() {
		return nil, &APIError{Code: string(r.Code), Msg: r.Msg}
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
