package okx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if _, err := c.request(context.Background(), http.MethodPost, "/trade/order", []byte(`{}`)); err != nil {
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
	_, err := c.request(context.Background(), http.MethodPost, "/trade/order", []byte(`{}`))
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
	raw, err := c.request(context.Background(), http.MethodGet, "/market/ticker", nil)
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
	_, err := c.request(context.Background(), http.MethodGet, "/x", nil)
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
