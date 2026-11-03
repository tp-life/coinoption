package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAuthServer(t *testing.T, code string, opts ...ServerOption) (*httptest.Server, *http.Client) {
	t.Helper()
	all := append([]ServerOption{WithAuthCode(code)}, opts...)
	ts := httptest.NewServer(New(&fakeFetcher{snap: testSnap()}, "BTC-USDT", "", all...).Handler)
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return ts, &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func doLogin(t *testing.T, c *http.Client, base, code string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/login", strings.NewReader(`{"code":"`+code+`"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func doReq(t *testing.T, c *http.Client, method, url, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// 整站授权：页面 303 到登录页、API 401；登录后放行，退出后重新拦截。
func TestAuthBlocksWholeSite(t *testing.T) {
	ts, c := newAuthServer(t, "letmein")

	code, _ := doReq(t, c, http.MethodGet, ts.URL+"/", "")
	if code != http.StatusSeeOther {
		t.Fatalf("未登录 GET / = %d，期望 303", code)
	}
	code, body := doReq(t, c, http.MethodGet, ts.URL+"/login", "")
	if code != http.StatusOK || !strings.Contains(string(body), "授权码") {
		t.Fatalf("登录页异常 %d: %.120s", code, body)
	}
	if code, _ := doReq(t, c, http.MethodGet, ts.URL+"/api/decisions", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录 API = %d，期望 401", code)
	}
	if code, _ := doReq(t, c, http.MethodGet, ts.URL+"/api/settings", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录设置 API = %d，期望 401", code)
	}

	if code, _ := doLogin(t, c, ts.URL, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错误授权码 = %d，期望 401", code)
	}
	if code, body := doLogin(t, c, ts.URL, "letmein"); code != http.StatusOK {
		t.Fatalf("正确授权码 = %d: %s", code, body)
	}
	if code, _ := doReq(t, c, http.MethodGet, ts.URL+"/", ""); code != http.StatusOK {
		t.Fatalf("登录后 GET / = %d，期望 200", code)
	}
	if code, body := doReq(t, c, http.MethodGet, ts.URL+"/api/decisions", ""); code != http.StatusOK {
		t.Fatalf("登录后 API = %d: %s", code, body)
	}

	if code, _ := doReq(t, c, http.MethodPost, ts.URL+"/api/logout", ""); code != http.StatusOK {
		t.Fatalf("退出登录 = %d", code)
	}
	if code, _ := doReq(t, c, http.MethodGet, ts.URL+"/", ""); code != http.StatusSeeOther {
		t.Fatalf("退出后 GET / = %d，期望 303", code)
	}
}

// 同一 IP 连续失败 5 次后，即使输入正确也先被限速拒绝。
func TestAuthLoginRateLimit(t *testing.T) {
	ts, c := newAuthServer(t, "letmein")
	for i := 0; i < maxLoginFails; i++ {
		if code, _ := doLogin(t, c, ts.URL, "bad"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 = %d，期望 401", i+1, code)
		}
	}
	if code, _ := doLogin(t, c, ts.URL, "letmein"); code != http.StatusTooManyRequests {
		t.Fatalf("超限后 = %d，期望 429", code)
	}
}

// 修改授权码：摘要落盘、旧会话立即失效、旧码不再可用、新码可登录。
func TestAuthCodeChangeInvalidatesSessions(t *testing.T) {
	fs := &fakeSettingsStore{}
	ts, c := newAuthServer(t, "oldcode", WithSettingsStore(fs))
	if code, _ := doLogin(t, c, ts.URL, "oldcode"); code != http.StatusOK {
		t.Fatalf("旧码登录失败 %d", code)
	}
	code, body := doReq(t, c, http.MethodPost, ts.URL+"/api/settings", `{"newAuthCode":"newcode"}`)
	if code != http.StatusOK {
		t.Fatalf("修改授权码 = %d: %s", code, body)
	}
	var got settingsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Relogin {
		t.Fatal("修改授权码后应要求重新登录（relogin=true）")
	}
	saved, _ := fs.snapshot()
	if saved.WebAuthCodeHash != HashCode("newcode") {
		t.Fatalf("settings.json 里的授权码摘要不符: %q", saved.WebAuthCodeHash)
	}
	if code, _ := doReq(t, c, http.MethodGet, ts.URL+"/", ""); code != http.StatusSeeOther {
		t.Fatalf("改码后旧会话仍可用: %d", code)
	}
	if code, _ := doLogin(t, c, ts.URL, "oldcode"); code != http.StatusUnauthorized {
		t.Fatalf("旧授权码仍可登录: %d", code)
	}
	if code, _ := doLogin(t, c, ts.URL, "newcode"); code != http.StatusOK {
		t.Fatalf("新授权码登录失败: %d", code)
	}
}

// 摘要写坏（如手改 settings.json）不能误放行：LoadSettings 会回落为空，
// 由 main 重新生成；这里直接验证 authenticator 拒绝非法摘要。
func TestAuthRejectsMalformedHash(t *testing.T) {
	a := newAuthenticator()
	if err := a.setHash("not-a-hash"); err == nil {
		t.Fatal("非法摘要应报错")
	}
	if a.check("") {
		t.Fatal("未设置有效摘要时不得放行空授权码")
	}
}
