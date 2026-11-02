package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

func TestLiveWSUpdatesOptionBid(t *testing.T) {
	const (
		instID = "BTC-USD-261231-80000-C"
		expiry = "1798732800000" // 2026-12-31T08:00:00Z
	)
	var wsConns atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		wsConns.Add(1)
		// 等客户端发出订阅，再推一条买价变化。
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"arg": map[string]string{"channel": "tickers", "instId": instID},
			"data": []map[string]string{{
				"instType": "OPTION", "instId": instID, "last": "0.02",
				"bidPx": "0.02", "askPx": "0.03", "bidSz": "10", "askSz": "10", "ts": "2",
			}},
		})
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("uly"), "_UM") {
			fmt.Fprint(w, `{"code":"0","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"code":"0","data":[{"instId":%q,"uly":"BTC-USD","instFamily":"BTC-USD","settleCcy":"BTC","ctVal":"1","ctValCcy":"BTC","stk":"80000","expTime":%q,"optType":"C"}]}`, instID, expiry)
	})
	mux.HandleFunc("/api/v5/market/tickers", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("uly"), "_UM") {
			fmt.Fprint(w, `{"code":"0","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"code":"0","data":[{"instType":"OPTION","instId":%q,"last":"0.01","bidPx":"0.01","askPx":"0.02","ts":"1"}]}`, instID)
	})
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[{"instId":"BTC-USDT","last":"78000","bidPx":"77999","askPx":"78001","ts":"1"}]}`)
	})
	mux.HandleFunc("/api/v5/finance/sfp/dcd/products", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("optType") != "C" {
			fmt.Fprint(w, `{"code":"0","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"code":"0","data":[{"productId":"BTC-USDT-261231-80000-C","baseCcy":"BTC","quoteCcy":"USDT","notionalCcy":"USDT","optType":"C","uly":"BTC-USD","strike":"80000","absYield":"0.01","annualizedYield":"0.10","minSize":"10","maxSize":"100000","stepSz":"1","expTime":%q,"tradeEndTime":%q}]}`, expiry, expiry)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c := okx.NewClient(okx.WithBaseURL(srv.URL), okx.WithWSURL(wsURL))
	l := NewLive(c, "BTC-USDT", strategy.DefaultParams())
	l.dcdDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer l.Close()
	if err := l.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	snap := l.Snapshot()
	if len(snap.OptionRows) != 1 {
		t.Fatalf("option rows = %d, want 1: %+v", len(snap.OptionRows), snap.OptionRows)
	}
	if snap.OptionRows[0].BidPx != "0.01" {
		t.Fatalf("initial bid = %q, want 0.01", snap.OptionRows[0].BidPx)
	}
	if !snap.DCYAvailable || snap.DcdCount != 1 {
		t.Fatalf("dcy = %v/%d, want true/1; warn=%s", snap.DCYAvailable, snap.DcdCount, snap.Warn)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		snap = l.Snapshot()
		if len(snap.OptionRows) == 1 && snap.OptionRows[0].BidPx == "0.02" {
			if !snap.WSConnected {
				t.Fatalf("bid updated but WSConnected=false")
			}
			if wsConns.Load() < 1 {
				t.Fatalf("ws server never saw a connection")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting WS bid update; last=%+v", l.Snapshot().OptionRows)
}
