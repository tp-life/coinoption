package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamTickersReceivesAndCancels(t *testing.T) {
	var gotSub int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var sub struct {
			Args []map[string]string `json:"args"`
		}
		_ = json.Unmarshal(raw, &sub)
		gotSub = len(sub.Args)
		_ = conn.WriteJSON(map[string]any{
			"arg": map[string]string{"channel": "tickers", "instId": "BTC-USDT"},
			"data": []map[string]string{{
				"instType": "SPOT", "instId": "BTC-USDT", "last": "78000",
				"bidPx": "77999", "askPx": "78001", "ts": "1",
			}},
		})
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := NewClient(WithWSURL("ws" + strings.TrimPrefix(srv.URL, "http")))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tick := make(chan OptionTicker, 1)
	go func() {
		_ = c.StreamTickers(ctx, []string{"BTC-USDT", "BTC-USDT", ""}, func(t OptionTicker) {
			select {
			case tick <- t:
			default:
			}
		}, nil)
	}()
	select {
	case t0 := <-tick:
		if t0.InstID != "BTC-USDT" || t0.BidPx != "77999" {
			t.Fatalf("unexpected ticker: %+v", t0)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting ticker")
	}
	if gotSub != 1 {
		t.Fatalf("subscribe args = %d, want 1 (dedupe/empty filter)", gotSub)
	}
	cancel()
}

func TestStreamTickersReconnectsAndResubscribes(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"arg": map[string]string{"channel": "tickers", "instId": "BTC-USDT"},
			"data": []map[string]string{{
				"instType": "SPOT", "instId": "BTC-USDT", "last": "1",
				"bidPx": "1", "askPx": "2", "ts": "1",
			}},
		})
		if n == 1 {
			// 第一轮主动断开，触发退避重连并重放订阅。
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := NewClient(WithWSURL("ws" + strings.TrimPrefix(srv.URL, "http")))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tick := make(chan struct{}, 4)
	go func() {
		_ = c.StreamTickers(ctx, []string{"BTC-USDT"}, func(OptionTicker) {
			tick <- struct{}{}
		}, nil)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-tick:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting reconnect ticker #%d", i+1)
		}
	}
	mu.Lock()
	got := conns
	mu.Unlock()
	if got < 2 {
		t.Fatalf("connections = %d, want >=2", got)
	}
	cancel()
}

// 回归（2026-11 用户反馈）：「一直 WS 重连中」而 REST 正常。
// REST 走 http.Client 默认 Transport 会跟随 HTTPS_PROXY，WS 的 Dialer 此前
// 只是 `&websocket.Dialer{...}`，Proxy 为 nil 直接裸连，于是在只配环境变量
// 代理的环境里永远握手失败。用子进程隔离，绕开 ProxyFromEnvironment 的
// 进程级环境缓存（sync.Once，测试进程内 t.Setenv 可能不生效）。
func TestWSDialerUsesEnvironmentProxy(t *testing.T) {
	if os.Getenv("COINOPTION_WS_PROXY_HELPER") == "1" {
		c := NewClient()
		req, err := http.NewRequest(http.MethodGet, "https://ws.okx.com/ws/v5/public", nil)
		if err != nil {
			fmt.Printf("REQ_ERR=%v\n", err)
			return
		}
		u, err := c.wsDialer().Proxy(req)
		if err != nil {
			fmt.Printf("PROXY_ERR=%v\n", err)
			return
		}
		if u == nil {
			fmt.Println("PROXY=<nil>")
			return
		}
		fmt.Printf("PROXY=%s\n", u)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestWSDialerUsesEnvironmentProxy$")
	cmd.Env = append(os.Environ(),
		"COINOPTION_WS_PROXY_HELPER=1",
		"HTTPS_PROXY=http://127.0.0.1:7890",
		"HTTP_PROXY=", "ALL_PROXY=", "NO_PROXY=", "no_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper 进程失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PROXY=http://127.0.0.1:7890") {
		t.Fatalf("WS dialer 未跟随 HTTPS_PROXY（会一直重连中）: %s", out)
	}
}

// 显式 --proxy 必须优先于环境变量。
func TestWSDialerPrefersExplicitProxy(t *testing.T) {
	c := NewClient(WithProxy("socks5://127.0.0.1:1080"))
	req, err := http.NewRequest(http.MethodGet, "https://ws.okx.com/ws/v5/public", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	u, err := c.wsDialer().Proxy(req)
	if err != nil {
		t.Fatalf("proxy func: %v", err)
	}
	if u == nil || u.String() != "socks5://127.0.0.1:1080" {
		t.Fatalf("显式代理应优先，实际 %v", u)
	}
}

// 订阅被拒（error 事件带 arg）后应自动重试单条订阅，而不是等整条连接重连。
func TestStreamTickersResubscribesRejected(t *testing.T) {
	old := wsResubDelay
	wsResubDelay = 20 * time.Millisecond
	defer func() { wsResubDelay = old }()

	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg struct {
				ID   string              `json:"id"`
				Args []map[string]string `json:"args"`
			}
			if json.Unmarshal(raw, &msg) != nil || len(msg.Args) == 0 {
				continue
			}
			id := msg.Args[0]["instId"]
			mu.Lock()
			got = append(got, id)
			n := len(got)
			mu.Unlock()
			if n == 1 {
				// 第一次拒绝，并带上 arg 让客户端知道是哪一条。
				_ = conn.WriteJSON(map[string]any{
					"event": "error", "code": "60018", "msg": "rejected",
					"arg": map[string]string{"channel": "tickers", "instId": id},
				})
				continue
			}
			_ = conn.WriteJSON(map[string]any{
				"arg": map[string]string{"channel": "tickers", "instId": id},
				"data": []map[string]string{{
					"instType": "OPTION", "instId": id, "last": "1", "bidPx": "1", "askPx": "2", "ts": "1",
				}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(WithWSURL("ws" + strings.TrimPrefix(srv.URL, "http")))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tick := make(chan OptionTicker, 1)
	status := make(chan string, 4)
	go func() {
		_ = c.StreamTickers(ctx, []string{"BTC-USD-261113-75000-P"}, func(t OptionTicker) {
			select {
			case tick <- t:
			default:
			}
		}, func(st WSStatus) {
			if st.Err != "" {
				select {
				case status <- st.Err:
				default:
				}
			}
		})
	}()
	select {
	case s := <-status:
		if !strings.Contains(s, "重试订阅") {
			t.Fatalf("状态应提示重试: %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到订阅错误状态")
	}
	select {
	case t0 := <-tick:
		if t0.InstID != "BTC-USD-261113-75000-P" {
			t.Fatalf("unexpected ticker: %+v", t0)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("重试订阅后仍未收到行情")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 || got[0] != got[1] {
		t.Fatalf("订阅序列 = %v，期望同一条被重试", got)
	}
}
