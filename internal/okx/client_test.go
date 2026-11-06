package okx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func Test50105Hint(t *testing.T) {
	e := &APIError{Code: "50105", Msg: "Request header OK-ACCESS-PASSPHRASE incorrect."}
	s := e.Error()
	if want := "Passphrase"; !contains(s, want) {
		t.Fatalf("hint missing in %q", s)
	}
}

func Test50106Hint(t *testing.T) {
	e := &APIError{Code: "50106", Msg: "API Key IP restriction"}
	if s := e.Error(); !contains(s, "白名单") {
		t.Fatalf("IP hint missing in %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// wrapKeys 三种响应形态回归（源自 2026-10-27 实测响应头片段）。
func TestWrapShapes(t *testing.T) {
	// 形态1：标准数组
	d1 := json.RawMessage(`[{"instId":"BTC-USDT","last":"79803"}]`)
	spots, err := wrap[SpotTicker](d1, nil)
	if err != nil || len(spots) != 1 || spots[0].Last != "79803" {
		t.Fatalf("array shape: %v %+v", err, spots)
	}

	// 形态2：{"products":[...]}（实测的双币赢响应）
	d2 := json.RawMessage(`{"code":0,"data":{"products":[{"productId":"BTC-USDT-260331-82000-P","annualizedYield":"0.242"}]}}`)
	b := resp{Data: d2}
	_ = b
	prods, err := wrapKeys[DCDProduct](json.RawMessage(`{"products":[{"productId":"BTC-USDT-260331-82000-P","annualizedYield":"0.242"}]}`), nil, "products")
	if err != nil || len(prods) != 1 || prods[0].AnnualizedYield != 0.242 {
		t.Fatalf("products shape: %v %+v", err, prods)
	}

	// flexCode：数字 0 与字符串 "0" 都算成功
	var r1 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":0}`), &r1); err != nil || !r1.Code.OK() {
		t.Fatalf("numeric code: %v %q", err, r1.Code)
	}
	var r2 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":"0"}`), &r2); err != nil || !r2.Code.OK() {
		t.Fatalf("string code: %v", err)
	}

	// flexCode：错误码字符串保留
	var r3 resp
	if err := json.Unmarshal(json.RawMessage(`{"code":50105,"msg":"x"}`), &r3); err != nil || r3.Code.OK() || r3.Code != "50105" {
		t.Fatalf("numeric error code: %v %q", err, r3.Code)
	}
}

// ─── M4 重试/退避硬化 ───

func testClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient(WithBaseURL(url))
	c.retryBase = time.Millisecond
	return c
}

func jsonCode(code, data string) string {
	return `{"code":"` + code + `","msg":"m","data":` + data + `}`
}

func TestRetryOnRateLimitCodeForPost(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			io.WriteString(w, jsonCode("50011", `[]`))
			return
		}
		io.WriteString(w, jsonCode("0", `[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if _, err := c.request(context.Background(), http.MethodPost, "/trade/order", []byte(`{}`), false); err != nil {
		t.Fatalf("50011 应可重试: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("调用次数 = %d，期望 3", got)
	}
}

func TestNoRetryAmbiguousBusinessCodeOnPost(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		io.WriteString(w, jsonCode("50004", `[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.request(context.Background(), http.MethodPost, "/trade/order", []byte(`{}`), false)
	if err == nil {
		t.Fatal("POST 50004（可能已成单）不应重试成功")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("POST 不应重试，实际调用 %d 次", got)
	}
}

func TestRetryTransientCodeOnGet(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			io.WriteString(w, jsonCode("50013", `[]`))
			return
		}
		io.WriteString(w, jsonCode("0", `[{"instId":"BTC-USDT"}]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	raw, err := c.request(context.Background(), http.MethodGet, "/market/ticker", nil, true)
	if err != nil {
		t.Fatalf("GET 50013 应重试: %v", err)
	}
	if string(raw) != `[{"instId":"BTC-USDT"}]` {
		t.Fatalf("响应不符: %s", raw)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("调用次数 = %d，期望 2", got)
	}
}

func TestRetryExhaustedOnPersistent5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "bad gateway")
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.request(context.Background(), http.MethodGet, "/x", nil, true)
	if err == nil || !contains(err.Error(), "retry exhausted") {
		t.Fatalf("应返回重试耗尽错误: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("调用次数 = %d，期望 3", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Fatalf("秒数解析: %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("空值应为 0: %v", d)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 4*time.Second {
		t.Fatalf("HTTP 日期解析: %v", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Fatalf("非法值应为 0: %v", d)
	}
}

func TestBackoffJitterAndCap(t *testing.T) {
	c := testClient(t, "http://example.invalid")
	for attempt := 1; attempt <= 5; attempt++ {
		d := c.backoff(attempt, 0)
		if d <= 0 || d > 6*time.Second {
			t.Fatalf("attempt %d 退避越界: %v", attempt, d)
		}
	}
	if d := c.backoff(1, 10*time.Second); d != 5*time.Second {
		t.Fatalf("Retry-After 应被截到 5s，实际 %v", d)
	}
}

// ─── 代理配置 ─── 回归（2026-11 用户反馈）：--proxy 1.2.3.4:7890（无 scheme）
// 曾被静默忽略 → 直连 → DCY 白名单报文打出本机 IPv6 出口（50110 误导排查）。

func TestParseProxy(t *testing.T) {
	// 合法 scheme（socks5h 归一为 socks5）
	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"https://proxy.example.com:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"  socks5://user:pass@10.0.0.1:1080/?x=1  ", // 前后空白容忍
	} {
		u, err := ParseProxy(raw)
		if err != nil || u == nil || u.Host == "" {
			t.Fatalf("合法代理 %q 不应报错: %v", raw, err)
		}
		if strings.HasPrefix(strings.TrimSpace(raw), "socks5h://") && u.Scheme != "socks5" {
			t.Fatalf("socks5h 应归一为 socks5，实际 %q", u.Scheme)
		}
	}
	// 非法：裸 host:port / 缺 scheme / 不支持 scheme / 空
	for _, raw := range []string{
		"127.0.0.1:7890",
		"1.2.3.4:7890",
		"http://",
		"ftp://127.0.0.1:21",
		"   ",
		"::bad url::",
	} {
		if _, err := ParseProxy(raw); err == nil {
			t.Fatalf("非法代理 %q 应报错", raw)
		}
	}
}

// EgressIP 通过同一 Transport 探测出口 IP，并识别 IPv4/IPv6。
func TestEgressIP(t *testing.T) {
	old := egressTraceURL
	defer func() { egressTraceURL = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fl=abc\nip=2408:8207:1234:5678::1\nts=1\n")
	}))
	defer srv.Close()
	egressTraceURL = srv.URL

	c := NewClient(WithBaseURL(srv.URL))
	ip, family, err := c.EgressIP(context.Background())
	if err != nil {
		t.Fatalf("EgressIP: %v", err)
	}
	if ip != "2408:8207:1234:5678::1" || family != "IPv6" {
		t.Fatalf("出口解析错误: %q %q", ip, family)
	}
}

// 代理配置非法时 EgressIP 也必须短路报错（不静默直连）。
func TestEgressIPInvalidProxy(t *testing.T) {
	c := NewClient(WithProxy("1.2.3.4:7890"))
	if _, _, err := c.EgressIP(context.Background()); err == nil || !contains(err.Error(), "代理配置错误") {
		t.Fatalf("无效代理应短路报错: %v", err)
	}
}

// 配置非法的代理必须让所有请求短路报错，绝不静默回落直连。
func TestInvalidProxyShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("请求不应发出（无效代理应短路）: %s", r.URL.Path)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithProxy("1.2.3.4:7890"))
	c.retryBase = time.Millisecond
	_, err := c.request(context.Background(), http.MethodGet, "/x", nil, true)
	if err == nil || !contains(err.Error(), "代理配置错误") {
		t.Fatalf("无效代理应短路报错，得到: %v", err)
	}
}

// 回归（2026-11）：双币赢端点限速 1 req/s（User ID 级），但调用方各写各的
// 间隔，CLI --dump/--smoke 漏写就会稳定撞 50011；重试退避 400ms 起步也会在
// 同一个 1 秒窗口里再撞一次。现在由 Client 层按路径统一节流（含重试）。
func TestDCDRequestsArePaced(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		io.WriteString(w, jsonCode("0", `[]`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithDCDGap(80*time.Millisecond))
	c.retryBase = time.Millisecond
	for _, ot := range []string{"C", "P"} {
		if _, err := c.GetDCDProducts("BTC", "USDT", ot); err != nil {
			t.Fatalf("GetDCDProducts(%s): %v", ot, err)
		}
	}
	mu.Lock()
	got := len(at)
	gap := time.Duration(0)
	if got == 2 {
		gap = at[1].Sub(at[0])
	}
	mu.Unlock()
	if got != 2 {
		t.Fatalf("请求数 = %d，期望 2", got)
	}
	if gap < 75*time.Millisecond {
		t.Fatalf("两次 DCD 请求间隔 %v，应 ≥ 节流间隔（会撞 50011）", gap)
	}

	// 非 DCD 路径不应被节流：同客户端连发两次公开行情要很快。
	start := time.Now()
	if _, err := c.GetSpotTicker("BTC-USDT"); err != nil {
		t.Fatalf("spot: %v", err)
	}
	if _, err := c.GetSpotTicker("BTC-USDT"); err != nil {
		t.Fatalf("spot: %v", err)
	}
	if d := time.Since(start); d > 75*time.Millisecond {
		t.Fatalf("公开行情被 DCD 节流误伤，耗时 %v", d)
	}
}

// 运行期替换 API 凭证（Web 设置页）：下一次请求就用新 Key 签名，清空后回到公开请求。
func TestSetAuthRuntime(t *testing.T) {
	var mu sync.Mutex
	var keys, passes, signs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("OK-ACCESS-KEY"))
		passes = append(passes, r.Header.Get("OK-ACCESS-PASSPHRASE"))
		signs = append(signs, r.Header.Get("OK-ACCESS-SIGN"))
		mu.Unlock()
		io.WriteString(w, jsonCode("0", `[]`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	fetch := func() {
		if _, err := c.GetSpotTicker("BTC-USDT"); err != nil {
			t.Fatalf("spot: %v", err)
		}
	}
	fetch() // 无凭证：不带签名头
	c.SetAuth("key-1", "sec-1", "pass-1")
	if !c.HasAuth() {
		t.Fatal("SetAuth 后 HasAuth 应为 true")
	}
	fetch()
	c.SetAuth("key-2", "sec-2", "pass-2")
	fetch()
	c.SetAuth("", "", "")
	if c.HasAuth() {
		t.Fatal("清空凭证后 HasAuth 应为 false")
	}
	fetch()

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 4 {
		t.Fatalf("请求数 = %d，期望 4", len(keys))
	}
	if keys[0] != "" || signs[0] != "" {
		t.Fatalf("无凭证不应签名: key=%q sign=%q", keys[0], signs[0])
	}
	if keys[1] != "key-1" || passes[1] != "pass-1" || signs[1] == "" {
		t.Fatalf("第一次 SetAuth 未生效: key=%q pass=%q sign=%q", keys[1], passes[1], signs[1])
	}
	if keys[2] != "key-2" || passes[2] != "pass-2" {
		t.Fatalf("第二次 SetAuth 未生效: key=%q pass=%q", keys[2], passes[2])
	}
	if keys[3] != "" || signs[3] != "" {
		t.Fatalf("清空后不应再签名: key=%q sign=%q", keys[3], signs[3])
	}
}

// 回归：写请求在传输层失败（服务端可能已受理）时绝不能重发。
// 之前 retryDelay 的默认分支会把超时/连接重置也重试 3 次 → 重复下单。
func TestWriteTransportErrorNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&calls, 1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("no hijacker")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, jsonCode("0", `[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.request(context.Background(), http.MethodPost, "/api/v5/trade/order", []byte(`{}`), false)
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("写请求被重发 %d 次（应为 1 次）", n)
	}
	if !IsAmbiguous(err) {
		t.Fatalf("写请求传输失败应标记为结果未知，得到: %v", err)
	}
}

// 回归：data:[] 不能再被 *q 解引用 panic。
func TestDCDEmptyDataReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, jsonCode("0", `[]`))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.dcdGap = 0
	if _, err := c.PostDCDQuote(DCDQuoteRequest{ProductID: "x"}); err == nil {
		t.Fatal("空 data 应返回错误，不能 panic / 返回零值报价")
	}
	if _, err := c.PostDCDTrade("q1"); err == nil {
		t.Fatal("空 data 应返回错误，不能返回空回执")
	}
}

// 回归：data:{} 不能伪造出零值对象（否则 OrdID="" 会被当成下单成功）。
func TestZeroObjectDecodeRejected(t *testing.T) {
	if _, err := wrapKeys[DCDProduct](json.RawMessage(`{}`), nil, "products"); err == nil {
		t.Fatal("空对象应返回解码错误")
	}
	if _, err := wrapKeys[DCDProduct](json.RawMessage(`null`), nil, "products"); err == nil {
		t.Fatal("null 应返回解码错误")
	}
}
