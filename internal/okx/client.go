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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	credMu     sync.RWMutex // 保护 apiKey/secret/passphrase（Web 设置页可在运行期更新）
	apiKey     string
	secret     string
	passphrase string
	flag       string // FlagLive | FlagDemo

	baseURL   string
	http      *http.Client // 读请求（5s）
	httpWrite *http.Client // 写请求（15s：下单需要更长的响应窗口）
	proxyErr  error        // 代理配置非法（请求一律短路报错，不静默直连）
	proxyURL  *url.URL     // WS Dialer 复用同一代理链路
	wsURL     string       // 覆盖 WebSocket 公共行情地址（测试/特殊网络）
	limiter   *rate.Limiter
	retryBase time.Duration // 重试退避基数（测试可调小）

	// dcdGap/dcdLast 给双币赢（sfp/dcd）端点做独立节流：官方限速是
	// 「1 request per second，User ID 级」，与公开行情的 10rps 限速器无关。
	// 放在客户端而不是调用方，是为了让重试也遵守 1s 窗口——此前重试退避
	// 从 400ms 起步，会在同一个 1 秒窗口里再撞一次 50011。
	dcdMu   sync.Mutex
	dcdLast time.Time
	dcdGap  time.Duration
}

// dcdMinGap 双币赢端点的默认最小请求间隔（官方 1 req/s，留 100ms 余量）。
const dcdMinGap = 1100 * time.Millisecond

// NewClient 创建客户端。未提供 WithAuth 时为纯公开数据（无需密钥）。
func NewClient(opts ...Option) *Client {
	c := &Client{
		flag:    FlagLive,
		baseURL: DefaultBaseURL,
		http: &http.Client{
			// 公开行情 5s 足够
			Timeout: 5 * time.Second,
		},
		// 交易类超时留足：5s 在拥堵/代理抖动时容易把「已受理」误判成失败，
		// 再叠加自动重试就会重复下单。写请求单独用 15s。
		httpWrite: &http.Client{Timeout: 15 * time.Second},
		// 公开行情速率上限 20 req/2s，统一取 10 rps、burst 10 留余量
		limiter:   rate.NewLimiter(rate.Limit(10), 10),
		retryBase: 400 * time.Millisecond,
		dcdGap:    dcdMinGap,
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
	return func(c *Client) { c.SetAuth(key, secret, passphrase) }
}

// SetAuth 运行期替换 API 凭证（Web 设置页保存后立即生效）。
// 传空串会关闭签名（公开接口仍可用，私有接口返回未授权）。
// 注意：切换账号前应确认没有在持订单，风控敞口不会随账号切换重新计算。
func (c *Client) SetAuth(key, secret, passphrase string) {
	c.credMu.Lock()
	c.apiKey, c.secret, c.passphrase = key, secret, passphrase
	c.credMu.Unlock()
}

// authSnapshot 一次取齐三项，避免签名时读到半更新状态。
func (c *Client) authSnapshot() (key, secret, passphrase string) {
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	return c.apiKey, c.secret, c.passphrase
}

// HasAuth 当前是否配置了完整凭证（TUI/诊断展示用）。
func (c *Client) HasAuth() bool {
	key, secret, passphrase := c.authSnapshot()
	return key != "" && secret != "" && passphrase != ""
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

// WithDCDGap 覆盖双币赢端点（sfp/dcd/*）的最小请求间隔（默认 1.1s）。
// 测试或批量核对时可设为 0 关闭节流；生产不要低于 1s（官方限速 1 req/s）。
func WithDCDGap(d time.Duration) Option {
	return func(c *Client) { c.dcdGap = d }
}

// WithProxy 配置 HTTP/SOCKS5 代理（如 http://127.0.0.1:7890 / socks5://127.0.0.1:1080）。
// 配置非法时不静默回落直连：私有接口一旦直连，OKX 会看到本机真实出口
// （NAT/公网下常是 IPv6），50110 白名单报文极具迷惑性。代理配置错误一律
// 短路报错（见 ParseProxy）。
func WithProxy(proxyURL string) Option {
	return func(c *Client) {
		u, err := ParseProxy(proxyURL)
		if err != nil {
			c.proxyErr = err
			return
		}
		c.proxyURL = u
		c.http.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	}
}

// WithWSURL 覆盖 WebSocket 公共行情地址（测试或特殊网络环境）。
// 生产默认：实盘 wss://ws.okx.com/ws/v5/public；模拟盘 wspap.okx.com。
func WithWSURL(raw string) Option {
	return func(c *Client) {
		c.wsURL = strings.TrimSpace(raw)
	}
}

// ParseProxy 校验 HTTP/SOCKS5 代理配置并转为 *url.URL。
// 要求显式 scheme（http/https/socks5/socks5h）+ host:port；`host:port` 这类
// 裸地址一律报错——历史上静默回落直连曾让 DCY 白名单报文打出本机 IPv6 出口。
// socks5h 会归一为 Go 支持的 socks5（远端 DNS 语义）。
func ParseProxy(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("代理地址为空")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("无法解析 %q: %w", s, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		if u.Host == "" {
			return nil, fmt.Errorf("%q 缺少主机:端口（示例 http://127.0.0.1:7890）", s)
		}
		return u, nil
	case "socks5h":
		if u.Host == "" {
			return nil, fmt.Errorf("%q 缺少主机:端口（示例 socks5h://127.0.0.1:1080）", s)
		}
		u.Scheme = "socks5"
		return u, nil
	case "":
		return nil, fmt.Errorf("%q 缺少 scheme 前缀（应为 http:// 或 socks5://）", s)
	default:
		return nil, fmt.Errorf("%q 的 scheme %q 不支持（支持 http/https/socks5/socks5h）", s, u.Scheme)
	}
}

// egressTraceURL 出口 IP 探测端点（返回 ip=... 文本行；测试可覆盖）。
var egressTraceURL = "https://www.cloudflare.com/cdn-cgi/trace"

// EgressIP 通过当前 Transport（含代理）查询外部服务看到的出口 IP 与 IP 家族。
// 用途：核对代理是否生效、出口是 IPv4 还是 IPv6——OKX 的 API Key 白名单
// 校验的就是这个 IP（代理出口是 IPv6 时，白名单填 IPv4 必然 50110）。
func (c *Client) EgressIP(ctx context.Context) (ip, family string, err error) {
	if c.proxyErr != nil {
		return "", "", fmt.Errorf("代理配置错误: %w", c.proxyErr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, egressTraceURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("查询出口 IP 失败（代理链路不通？）: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		ip = strings.TrimPrefix(line, "ip=")
		parsed := net.ParseIP(ip)
		switch {
		case parsed == nil:
			family = "未知"
		case parsed.To4() != nil:
			family = "IPv4"
		default:
			family = "IPv6"
		}
		return ip, family, nil
	}
	return "", "", fmt.Errorf("出口 IP 探测响应缺少 ip= 字段（head=%.120s）", body)
}

// APIError OKX 返回的业务错误（code != "0"）。
type APIError struct {
	Code string
	Msg  string
}

// hint 为常见业务错误码补充人类指引。
var hint = map[string]string{
	"50105": "Passphrase 不正确：passphrase 是创建 API Key 时 YOU 自己设置的那串口令（不是 OKX 发的），检查是否漏拷/多空格/引号",
	"50106": "IP 白名单未配置或不匹配：OKX 校验的是「出口 IP」（代理生效时=代理出口）。若报文里的 IP 是 IPv6，而白名单只填了 IPv4：说明代理链路走了 IPv6 出口（或代理未生效）——在代理侧强制 IPv4 出口（如 Clash ipv6: false），或把该 IPv6 加进白名单",
	"50110": "IP 不在 API Key 白名单：(上方报文的出口 IP) 加入白名单或换不绑 IP 的 Key；用 --egress 核对代理出口 IP，报文为 IPv6 时按 50106 处理",
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

// AmbiguousError 写请求「结果未知」：请求可能已经被交易所受理，但客户端
// 没拿到可信回执（超时 / 连接重置 / 5xx / 响应解码失败）。
//
// 调用方**不得**自动重试，也不应立即释放风控敞口——必须先按订单号/持仓
// 对账，确认未成交后再释放。这是防重复下单的最后一道语义保护。
type AmbiguousError struct {
	Op  string // 动作，如 "option_order" / "dcd_trade"
	Err error
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s 结果未知（可能已成交，禁止自动重试，请人工对账）: %v", e.Op, e.Err)
}

func (e *AmbiguousError) Unwrap() error { return e.Err }

// IsAmbiguous 判断错误是否为「写请求结果未知」。
func IsAmbiguous(err error) bool {
	var ae *AmbiguousError
	return errors.As(err, &ae)
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
	return c.request(ctx, http.MethodGet, path, nil, true)
}

// doJSON 发起签名 POST（JSON body）。data 字段返回原始 JSON。
//
// idempotent=false 表示写操作：只有「确定请求未被处理」的错误才允许重试，
// 其余一律返回 *AmbiguousError；调用方不得再自动重试。
func (c *Client) doJSON(ctx context.Context, method, path string, body any, idempotent bool) (json.RawMessage, error) {
	bb, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return c.request(ctx, method, path, bb, idempotent)
}

// request 单次请求核心：签名、限速、重试、解析。
//
// 重试策略：
//   - 读请求（idempotent=true）：网络错误 / 429 / 5xx 指数退避（400ms 起，
//     带抖动，429 优先用 Retry-After），最多 2 次；GET 的瞬时业务码可重试。
//   - 写请求（idempotent=false）：只有 50011（限速，官方语义=请求未处理）
//     重试；超时/连接重置/5xx/解码失败一律不重试，包成 *AmbiguousError
//     交给调用方对账（宁可少做一笔，也不能重复下单）。
//
// 说明：此前实现把「POST 一律不重试」写在注释里，但传输层错误会落到默认
// 分支被重试 3 次——交易所已受理 + 客户端超时 = 重复下单。
func (c *Client) request(ctx context.Context, method, path string, body []byte, idempotent bool) (json.RawMessage, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.waitDCDGap(ctx, path); err != nil {
			return nil, err
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}

		raw, err := c.doOnce(ctx, method, path, body, idempotent)
		if err == nil {
			return raw, nil
		}
		lastErr = err

		delay, retry := c.retryDelay(method, err, attempt, idempotent)
		if !retry {
			if !idempotent && ambiguousWriteFailure(err) {
				return nil, &AmbiguousError{Op: method + " " + path, Err: err}
			}
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
	if !idempotent {
		return nil, &AmbiguousError{Op: method + " " + path, Err: fmt.Errorf("重试次数用尽: %w", lastErr)}
	}
	return nil, fmt.Errorf("retry exhausted: %w", lastErr)
}

// ambiguousWriteFailure 判断写请求失败是否可能「已在服务端生效」。
// 业务错误码（含 50011）由交易所明确拒绝 → 安全；纯 HTTP 4xx 也视为拒绝；
// 5xx/429、传输层错误（超时/断开）与响应解码失败都可能是「已受理但没读到」。
func ambiguousWriteFailure(err error) bool {
	var apiErr *APIError
	if asAPIError(err, &apiErr) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500
	}
	return true
}

// isDCDPath 判断是否双币赢（金融产品）端点，这些端点按 User ID 限速 1 req/s。
func isDCDPath(path string) bool {
	return strings.HasPrefix(path, "/api/v5/finance/sfp/dcd/")
}

// waitDCDGap 为双币赢请求预留一个 1s 窗口：同一次调用（含重试）与并发调用
// 之间都至少间隔 dcdGap。dcdGap<=0 时不做任何等待（测试用）。
func (c *Client) waitDCDGap(ctx context.Context, path string) error {
	if c.dcdGap <= 0 || !isDCDPath(path) {
		return nil
	}
	for {
		c.dcdMu.Lock()
		now := time.Now()
		if c.dcdLast.IsZero() || !now.Before(c.dcdLast.Add(c.dcdGap)) {
			c.dcdLast = now
			c.dcdMu.Unlock()
			return nil
		}
		wait := c.dcdLast.Add(c.dcdGap).Sub(now)
		c.dcdMu.Unlock()
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

// sleepCtx 可被 ctx 取消的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryDelay 返回是否重试与本次退避时长。
//
// idempotent=false（写请求）时只允许 50011：OKX 明确表示该请求未被处理。
// 其余错误一律不重试，由上层按「结果未知」对账。
func (c *Client) retryDelay(method string, err error, attempt int, idempotent bool) (time.Duration, bool) {
	var apiErr *APIError
	if asAPIError(err, &apiErr) {
		// 50011 对读写都安全（请求未进入撮合/申购）。
		if apiErr.Code == "50011" {
			return c.backoff(attempt, 0), true
		}
		// 幂等 POST（如报价）按读请求的瞬时错误处理；写请求仍只看 50011。
		effMethod := method
		if idempotent {
			effMethod = http.MethodGet
		}
		if !retryableBusinessCode(effMethod, apiErr.Code) {
			return 0, false
		}
		return c.backoff(attempt, 0), true
	}

	if !idempotent {
		return 0, false
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if !httpErr.Retryable() {
			return 0, false
		}
		return c.backoff(attempt, httpErr.RetryAfter), true
	}
	// 读请求的传输层错误（超时/连接重置/代理断流）可重试。
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

// doOnce 不含重试的单次执行。idempotent=false 使用写请求 client（更长超时）。
func (c *Client) doOnce(ctx context.Context, method, path string, body []byte, idempotent bool) (json.RawMessage, error) {
	// 代理配置非法 → 请求短路报错（绝不静默直连打私有接口）
	if c.proxyErr != nil {
		return nil, fmt.Errorf("代理配置错误: %w", c.proxyErr)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// OKX 统一要求这两个头
	req.Header.Set("Content-Type", "application/json")
	if body != nil {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	// 已配置密钥则签名（公开接口签名头可省略但带上无害）。
	// 凭证可在运行期被 Web 设置页替换，统一从锁保护的快照读取。
	apiKey, secret, passphrase := c.authSnapshot()
	if apiKey != "" && secret != "" && passphrase != "" {
		ts := isoTimestamp(time.Now())
		sign := sign(secret, ts, method, path, string(body))
		req.Header.Set("OK-ACCESS-KEY", apiKey)
		req.Header.Set("OK-ACCESS-SIGN", sign)
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", passphrase)
	}
	if c.flag == FlagDemo {
		req.Header.Set(DemoHeader, "1")
	}

	hc := c.http
	if !idempotent && c.httpWrite != nil {
		hc = c.httpWrite
	}
	httpResp, err := hc.Do(req)
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
