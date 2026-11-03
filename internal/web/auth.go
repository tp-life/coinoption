package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Web 授权码：整站访问控制。
//
// 设计取舍（个人工具场景）：
//   - 只保存 SHA-256 摘要（settings.json 的 webAuthCodeHash），不落明文；
//   - 登录成功下发随机 HttpOnly + SameSite=Strict 会话 Cookie，默认 12h；
//   - 登录失败按来源 IP 限速（1 分钟 5 次），避免 6 位授权码被在线穷举；
//   - 修改授权码会清空全部会话（包括当前用户），必须重新登录；
//   - 授权中间件包住整个 mux：连行情/决策看板也要求登录，只有 /login
//     与 /api/login 放行，/favicon.ico 返回 204。

const (
	sessionCookieName = "coinoption_session"
	sessionTTL        = 12 * time.Hour
	loginWindow       = time.Minute
	maxLoginFails     = 5
)

// codeAlphabet 去掉易混淆的 I/O/0/1，便于人工抄写。
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// HashCode 授权码 → SHA-256 十六进制摘要（settings.json 只存这个值）。
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// GenerateAuthCode 生成 8 位随机授权码（无易混淆字符）。
func GenerateAuthCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成授权码: %w", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

// authenticator 会话与失败限速状态（进程内；重启后需重新登录）。
type authenticator struct {
	mu       sync.Mutex
	hash     [32]byte
	sessions map[string]time.Time
	fails    map[string][]time.Time
	now      func() time.Time
}

func newAuthenticator() *authenticator {
	return &authenticator{
		sessions: map[string]time.Time{},
		fails:    map[string][]time.Time{},
		now:      func() time.Time { return time.Now() },
	}
}

// setHash 更新授权码摘要并清空全部会话（改码后旧会话立即失效）。
func (a *authenticator) setHash(hashHex string) error {
	b, err := hex.DecodeString(strings.TrimSpace(hashHex))
	if err != nil || len(b) != 32 {
		return fmt.Errorf("授权码摘要需为 64 位十六进制")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	copy(a.hash[:], b)
	a.sessions = map[string]time.Time{}
	return nil
}

// check 常数时间比较；未设置授权码时恒为 false（不会误放行）。
func (a *authenticator) check(code string) bool {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	a.mu.Lock()
	defer a.mu.Unlock()
	return hmac.Equal(sum[:], a.hash[:])
}

func (a *authenticator) newSession() (string, time.Time) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败在正常系统上不会发生；失败则返回空 token，
		// 调用方会拒绝登录而不是发出可预测的会话。
		return "", time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, exp := range a.sessions { // 顺手清理过期会话
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	token := hex.EncodeToString(buf)
	a.sessions[token] = now.Add(sessionTTL)
	return token, now.Add(sessionTTL)
}

func (a *authenticator) validSession(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if a.now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *authenticator) dropSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// allowAttempt 记录失败次数：同一 IP 1 分钟内 5 次失败后拒绝继续尝试。
func (a *authenticator) allowAttempt(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	cut := now.Add(-loginWindow)
	kept := a.fails[ip][:0]
	for _, t := range a.fails[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxLoginFails {
		a.fails[ip] = kept
		return false
	}
	a.fails[ip] = kept
	return true
}

func (a *authenticator) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails[ip] = append(a.fails[ip], a.now())
}

func (a *authenticator) clearFails(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// wrap 整站访问控制：未登录的页面请求 303 到 /login，API/WS 返回 401 JSON。
func (a *authenticator) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		switch r.URL.Path {
		case "/login":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET")
				http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
				return
			}
			a.serveLogin(w, r)
			return
		case "/api/login":
			a.handleLogin(w, r)
			return
		case "/api/logout":
			a.handleLogout(w, r)
			return
		case "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if c, err := r.Cookie(sessionCookieName); err == nil && a.validSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "未授权：请先输入 Web 授权码（/login）",
			})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type 需为 application/json"})
		return
	}
	ip := clientIP(r)
	if !a.allowAttempt(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请 1 分钟后再试"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体需为 {\"code\": \"...\"}"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "授权码不能为空"})
		return
	}
	if !a.check(req.Code) {
		a.fail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "授权码错误"})
		return
	}
	token, exp := a.newSession()
	if token == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "会话创建失败"})
		return
	}
	a.clearFails(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.dropSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *authenticator) serveLogin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginHTML))
}

// loginHTML 登录页（无外部依赖，离线也能打开；风格与看板一致）。
const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>coinoption · 授权访问</title>
<style>
:root { --mint:#CFE8D6; --ink:#121210; --paper:#fff; --red:#C03A3A; --shadow:6px 6px 0 var(--ink); }
* { box-sizing:border-box; }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
  background:var(--mint); color:var(--ink); padding:24px;
  font-family:"Hanken Grotesk",-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
  background-image:linear-gradient(rgba(18,18,16,.05) 1px,transparent 1px),linear-gradient(90deg,rgba(18,18,16,.05) 1px,transparent 1px);
  background-size:28px 28px; }
.card { width:min(420px,100%); background:var(--paper); border:3px solid var(--ink); box-shadow:var(--shadow); padding:28px; }
.brand { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:11px; letter-spacing:.22em; opacity:.6; }
h1 { font-size:26px; line-height:1.15; margin:10px 0 6px; letter-spacing:-.02em; }
p { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; line-height:1.75; opacity:.7; margin:0 0 18px; }
label { display:block; font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:11px; letter-spacing:.08em; opacity:.65; margin-bottom:6px; }
input { width:100%; border:3px solid var(--ink); padding:12px; font-family:ui-monospace,SFMono-Regular,Menlo,monospace;
  font-size:20px; letter-spacing:.18em; outline:none; background:var(--paper); }
input:focus { box-shadow:inset 0 0 0 2px var(--ink); }
button { width:100%; margin-top:14px; border:3px solid var(--ink); background:var(--ink); color:var(--paper);
  padding:12px; font-size:13px; font-weight:700; letter-spacing:.08em; cursor:pointer; box-shadow:4px 4px 0 rgba(18,18,16,.25); }
button:active { transform:translate(3px,3px); box-shadow:none; }
button:disabled { opacity:.5; cursor:default; }
.err { margin-top:14px; font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; color:var(--red);
  font-weight:700; min-height:1.4em; }
.hint { margin:16px 0 0; font-size:10px; line-height:1.7; opacity:.5; }
</style>
</head>
<body>
<main class="card">
  <div class="brand">COINOPTION · DCY / OPT</div>
  <h1>需要授权码</h1>
  <p>整站已启用访问控制。请输入启动日志中打印的 Web 授权码。</p>
  <label for="code">AUTH CODE</label>
  <input id="code" type="password" autocomplete="current-password" spellcheck="false" autofocus>
  <button id="go" type="button">进入看板</button>
  <div class="err" id="err"></div>
  <p class="hint">授权码可用启动参数 --web-code / 环境变量 COINOPTION_WEB_CODE 覆盖；登录状态保留 12 小时。</p>
</main>
<script>
(function () {
  var input = document.getElementById("code");
  var btn = document.getElementById("go");
  var err = document.getElementById("err");
  var busy = false;
  function submit() {
    var code = input.value.trim();
    if (!code || busy) return;
    busy = true;
    btn.disabled = true;
    err.textContent = "";
    fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code: code })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; });
    }).then(function (res) {
      if (!res.ok) throw new Error(res.j.error || "登录失败");
      location.replace("/");
    }).catch(function (e) {
      err.textContent = "✗ " + e.message;
      input.select();
    }).then(function () { busy = false; btn.disabled = false; });
  }
  btn.addEventListener("click", submit);
  input.addEventListener("keydown", function (e) { if (e.key === "Enter") submit(); });
})();
</script>
</body>
</html>
`
