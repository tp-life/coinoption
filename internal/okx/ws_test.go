package okx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
