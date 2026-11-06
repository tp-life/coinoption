package market

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// fetchRV 必须只用已收盘日线、按时间升序算 RV，并把失败原因写回 warn。
func TestFetchRV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/market/candles" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// 32 根已收盘日线，收盘价交替 ±1%：RV ≈ 0.01×sqrt(365) ≈ 0.191。
		closes := make([]float64, 32)
		closes[0] = 100
		for i := 1; i < len(closes); i++ {
			if i%2 == 1 {
				closes[i] = closes[i-1] * 1.01
			} else {
				closes[i] = closes[i-1] / 1.01
			}
		}
		fmt.Fprint(w, `{"code":"0","data":[`)
		for i := len(closes) - 1; i >= 0; i-- {
			if i != len(closes)-1 {
				fmt.Fprint(w, ",")
			}
			close := closes[i]
			fmt.Fprintf(w, `["%d","%.8f","%.8f","%.8f","%.8f","0","0","0","1"]`,
				1700000000000+int64(i)*86400000, close, close, close, close)
		}
		fmt.Fprint(w, `]}`)
	}))
	defer srv.Close()

	c := okx.NewClient(okx.WithBaseURL(srv.URL))
	est, warn := fetchVol(c, "BTC-USDT", 30)
	if warn != "" {
		t.Fatalf("unexpected warn: %s", warn)
	}
	if est.RV30 < 0.18 || est.RV30 > 0.20 {
		t.Fatalf("RV30 = %.6f, want 0.18–0.20", est.RV30)
	}
	if est.Entry < est.RV30 {
		t.Fatalf("Entry = %.6f 应 ≥ RV30 %.6f", est.Entry, est.RV30)
	}
	if est.EWMA <= 0 || est.RV7 <= 0 {
		t.Fatalf("EWMA/RV7 缺失: %+v", est)
	}
}

func TestFetchRVMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[]}`)
	}))
	defer srv.Close()
	c := okx.NewClient(okx.WithBaseURL(srv.URL))
	est, warn := fetchVol(c, "BTC-USDT", 30)
	if est.Entry != 0 || warn == "" {
		t.Fatalf("est/warn = %+v/%q, want 0 + warn", est, warn)
	}
}

// 端到端：行情快照 → RV → BuildDCY → dcy_ok。确保 candles / 期权 / DCY
// 数据真的能串起来，而不是只在 strategy 单测里成立。
func TestFetchWithDCYV1Passes(t *testing.T) {
	now := time.Now()
	exp := now.Add(11 * 24 * time.Hour)
	expMS := exp.UnixMilli()
	expStr := strconv.FormatInt(expMS, 10)
	const instID = "BTC-USD_UM-261114-75000-P"

	candles := make([]float64, 32)
	candles[0] = 100
	for i := 1; i < len(candles); i++ {
		if i%2 == 1 {
			candles[i] = candles[i-1] * 1.01
		} else {
			candles[i] = candles[i-1] / 1.01
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/market/candles", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[`)
		for i := len(candles) - 1; i >= 0; i-- {
			if i != len(candles)-1 {
				fmt.Fprint(w, ",")
			}
			c := candles[i]
			fmt.Fprintf(w, `["%d","%.8f","%.8f","%.8f","%.8f","0","0","0","1"]`,
				1700000000000+int64(i)*86400000, c, c, c, c)
		}
		fmt.Fprint(w, `]}`)
	})
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","data":[{"instId":%q,"uly":"BTC-USD_UM","instFamily":"BTC-USD_UM","settleCcy":"USDT","ctVal":"1","ctValCcy":"BTC","ctMult":"1","stk":"75000","expTime":%q,"optType":"P"}]}`, instID, expStr)
	})
	mux.HandleFunc("/api/v5/market/tickers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","data":[{"instType":"OPTION","instId":%q,"last":"1140","bidPx":"1134.98","askPx":"1160","bidSz":"10","askSz":"10","bidVol":"0.60","ts":"1"}]}`, instID)
	})
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","data":[{"instId":"BTC-USDT","last":"80000","ts":"1"}]}`)
	})
	mux.HandleFunc("/api/v5/finance/sfp/dcd/products", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("optType") != "P" {
			fmt.Fprint(w, `{"code":"0","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"code":"0","data":[{"productId":"BTC-USDT-261114-75000-P","baseCcy":"BTC","quoteCcy":"USDT","notionalCcy":"USDT","optType":"P","uly":"BTC-USD","strike":"75000","absYield":"0.01592958","annualizedYield":"0.58","minSize":"10","maxSize":"100000","stepSz":"1","expTime":%q}]}`, expStr)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := okx.NewClient(okx.WithBaseURL(srv.URL), okx.WithDCDGap(0))
	p := strategy.DefaultParams()
	p.CapitalUSDT = 1000

	snap, err := FetchWith(c, "BTC-USDT", p)
	if err != nil {
		t.Fatalf("FetchWith: %v", err)
	}
	if snap.RV30 < 0.18 || snap.RV30 > 0.20 {
		t.Fatalf("RV30 = %.6f, want 0.18–0.20", snap.RV30)
	}
	if len(snap.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1: %+v", len(snap.Decisions), snap.Decisions)
	}
	d := snap.Decisions[0]
	if d.Action != strategy.ActionSubscribeDual || d.ReasonCode != strategy.ReasonDCYOK {
		t.Fatalf("action/reason = %s/%s, want subscribe_dual/dcy_ok (warn=%s)", d.Action, d.ReasonCode, snap.Warn)
	}
	if d.SuggestedAmt != 100 {
		t.Fatalf("suggested = %.2f, want 100", d.SuggestedAmt)
	}
	if d.Delta <= 0 || d.IV <= 0 || d.Score <= 0 {
		t.Fatalf("diagnostics missing: delta=%.3f iv=%.3f score=%.4f", d.Delta, d.IV, d.Score)
	}
}

// opt-summary → instId |delta|；单链失败不影响另一条链。
func TestFetchGreeks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/public/opt-summary" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.URL.Query().Get("uly") {
		case "BTC-USD":
			fmt.Fprint(w, `{"code":"0","data":[
				{"instId":"BTC-USD-261113-75000-P","delta":"-0.31"},
				{"instId":"BTC-USD-261113-80000-P","delta":"0"}]}`)
		default:
			fmt.Fprint(w, `{"code":"51000","msg":"no such chain","data":[]}`)
		}
	}))
	defer srv.Close()
	c := okx.NewClient(okx.WithBaseURL(srv.URL))
	got, warn := fetchGreeks(c, "BTC-USD", "BTC-USD_UM")
	if warn != "" {
		t.Fatalf("单链成功不应告警: %s", warn)
	}
	if math.Abs(got["BTC-USD-261113-75000-P"]-0.31) > 1e-9 {
		t.Fatalf("delta 应为 0.31（取绝对值），实际 %v", got["BTC-USD-261113-75000-P"])
	}
	if _, ok := got["BTC-USD-261113-80000-P"]; ok {
		t.Fatal("delta=0 不应写入（视为缺失）")
	}
}

// 期权链 PUT 买盘 IV 中位数：只取期限窗口内的 PUT，百分数口径归一化。
func TestMedianPutIV(t *testing.T) {
	now := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	exp := now.Add(15 * 24 * time.Hour).UnixMilli()
	far := now.Add(45 * 24 * time.Hour).UnixMilli()
	ms := strconv.FormatInt(exp, 10)
	fs := strconv.FormatInt(far, 10)
	tks := []okx.OptionTicker{
		{InstID: "A", OptType: "P", ExpTime: ms, BidIV: "50"},
		{InstID: "B", OptType: "P", ExpTime: ms, BidIV: "60"},
		{InstID: "C", OptType: "P", ExpTime: ms, BidIV: "70"},
		{InstID: "D", OptType: "C", ExpTime: ms, BidIV: "99"},
		{InstID: "E", OptType: "P", ExpTime: fs, BidIV: "99"},
	}
	got := medianPutIV(tks, now, 10, 30)
	if math.Abs(got-0.60) > 1e-9 {
		t.Fatalf("median IV = %v，期望 0.60", got)
	}
	if got := medianPutIV(nil, now, 10, 30); got != 0 {
		t.Fatalf("空链应返回 0，实际 %v", got)
	}
}
