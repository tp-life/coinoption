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
	c := okx.NewClient(okx.WithBaseURL(srv.URL), okx.WithWSURL(wsURL), okx.WithDCDGap(0))
	l := NewLive(c, "BTC-USDT", strategy.DefaultParams())
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

// 回归（2026-11 用户反馈）：DCY 的 C/P 两个方向共用一个刷新周期，此前只要
// 任意方向有数据就整体替换缓存——C 成功、P 失败时 P 方向产品会整批从决策表
// 消失（用户观感：明明有的产品没显示）。现在按方向合并，失败方向沿用上次。
func TestLiveKeepsDCYDirectionsIndependently(t *testing.T) {
	const expiry = "1798732800000" // 2026-12-31T08:00:00Z
	var failC, failP atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","data":[{"instId":"BTC-USD-261231-80000-C","uly":"BTC-USD","settleCcy":"BTC","ctVal":"1","ctValCcy":"BTC","stk":"80000","expTime":%q,"optType":"C"}]}`, expiry)
	})
	mux.HandleFunc("/api/v5/market/tickers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[{"instType":"OPTION","instId":"BTC-USD-261231-80000-C","last":"0.01","bidPx":"0.01","askPx":"0.02","ts":"1"}]}`)
	})
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[{"instId":"BTC-USDT","last":"78000","ts":"1"}]}`)
	})
	mux.HandleFunc("/api/v5/finance/sfp/dcd/products", func(w http.ResponseWriter, r *http.Request) {
		ot := r.URL.Query().Get("optType")
		if (ot == "C" && failC.Load()) || (ot == "P" && failP.Load()) {
			fmt.Fprint(w, `{"code":"50103","msg":"api key invalid","data":[]}`)
			return
		}
		id := "BTC-USDT-261231-80000-" + ot
		fmt.Fprintf(w, `{"code":"0","data":[{"productId":%q,"baseCcy":"BTC","quoteCcy":"USDT","notionalCcy":"USDT","optType":%q,"uly":"BTC-USD","strike":"80000","absYield":"0.01","annualizedYield":"0.10","minSize":"10","maxSize":"100000","stepSz":"1","expTime":%q,"tradeEndTime":%q}]}`,
			id, ot, expiry, expiry)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c := okx.NewClient(okx.WithBaseURL(srv.URL), okx.WithWSURL(wsURL), okx.WithDCDGap(0))
	l := NewLive(c, "BTC-USDT", strategy.DefaultParams())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer l.Close()
	if err := l.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Snapshot 有 500ms 重组节流：测试里把 lastBuild 归零，立即重组而不 sleep。
	snapAfterRefresh := func() *Snapshot {
		l.mu.Lock()
		l.lastBuild = time.Time{}
		l.mu.Unlock()
		return l.Snapshot()
	}

	if snap := l.Snapshot(); snap.DcdCount != 2 {
		t.Fatalf("首轮应拿到 C+P 共 2 个产品，实际 %d: %s", snap.DcdCount, snap.Warn)
	}

	// P 方向失败：C 正常刷新，P 必须沿用上次数据（2 个产品都在）。
	failP.Store(true)
	l.refreshDCD()
	snap := snapAfterRefresh()
	if snap.DcdCount != 2 {
		t.Fatalf("P 失败后仍应保留 P 的上次数据，实际 %d 个产品: %s", snap.DcdCount, snap.Warn)
	}
	if !strings.Contains(snap.Warn, "沿用上次数据") {
		t.Fatalf("应提示部分刷新失败并沿用旧数据，warn=%q", snap.Warn)
	}
	if _, ok := snap.Products["BTC-USDT-261231-80000-P"]; !ok {
		t.Fatalf("P 产品不应消失: %+v", snap.Products)
	}

	// C 也失败：两个方向都沿用，决策表不应闪空。
	failC.Store(true)
	l.refreshDCD()
	snap = snapAfterRefresh()
	if snap.DcdCount != 2 || !snap.DCYAvailable {
		t.Fatalf("全部失败时应沿用两份旧数据，实际 dcd=%d available=%v: %s",
			snap.DcdCount, snap.DCYAvailable, snap.Warn)
	}

	// 恢复后告警清空。
	failC.Store(false)
	failP.Store(false)
	l.refreshDCD()
	snap = snapAfterRefresh()
	if strings.Contains(snap.Warn, "沿用上次数据") {
		t.Fatalf("恢复后不应再有失败告警: %s", snap.Warn)
	}
}
