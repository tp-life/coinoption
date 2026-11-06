package executor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

func newRedeemExec(t *testing.T, mux *http.ServeMux) *Executor {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := okx.NewClient(okx.WithBaseURL(srv.URL))
	return New(ModeLive, c, NewGuard(DefaultLimits()), st, nil)
}

// redeem-quote → redeem 全链路：请求体字段、回执、缓存失效。
func TestRedeemFlow(t *testing.T) {
	var gotQuoteBody, gotRedeemBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/finance/sfp/dcd/redeem-quote", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotQuoteBody = string(b)
		io.WriteString(w, `{"code":"0","data":{"ordId":"ord-1","quoteId":"q-9","redeemCcy":"USDT","redeemSz":"995.25","termRate":"-0.0047","validUntil":"1700000000000"}}`)
	})
	mux.HandleFunc("/api/v5/finance/sfp/dcd/redeem", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotRedeemBody = string(b)
		io.WriteString(w, `{"code":"0","data":{"ordId":"ord-1","state":"pending_redeem_booking"}}`)
	})
	ex := newRedeemExec(t, mux)

	q, err := ex.RedeemQuote("ord-1")
	if err != nil {
		t.Fatalf("RedeemQuote: %v", err)
	}
	if q.QuoteID != "q-9" || q.RedeemCcy != "USDT" || float64(q.RedeemSz) != 995.25 {
		t.Fatalf("quote = %+v", q)
	}
	if !strings.Contains(gotQuoteBody, `"ordId":"ord-1"`) {
		t.Fatalf("quote body = %s", gotQuoteBody)
	}

	out, err := ex.Redeem("ord-1", "q-9")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if out.Kind != "redeem" || out.Ref != "ord-1" || out.QuoteID != "q-9" {
		t.Fatalf("outcome = %+v", out)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotRedeemBody), &body); err != nil {
		t.Fatalf("redeem body 不是 JSON: %s", gotRedeemBody)
	}
	if body["ordId"] != "ord-1" || body["quoteId"] != "q-9" {
		t.Fatalf("redeem body = %v", body)
	}
	// 成功后应清空平台缓存，下一次风控会重新拉订单。
	if !ex.liveAt.IsZero() {
		t.Fatal("redeem 成功后应失效 live 缓存")
	}
}

func TestRedeemRejectsSimAndEmpty(t *testing.T) {
	ex := newRedeemExec(t, http.NewServeMux())
	ex.mode = ModeSimLocal
	if _, err := ex.RedeemQuote("ord-1"); err == nil || !strings.Contains(err.Error(), "live") {
		t.Fatalf("sim 模式应拒绝: %v", err)
	}
	ex.mode = ModeLive
	if _, err := ex.Redeem("", "q"); err == nil {
		t.Fatal("空 ordId 应拒绝")
	}
	if _, err := ex.Redeem("ord", ""); err == nil {
		t.Fatal("空 quoteId 应拒绝")
	}
}

// 本地预检：窗口缺失/未开始/已结束/临近到期/终态都不可赎回。
func TestCheckRedeemable(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	base := okx.DCDOrder{
		OrdID: "ord", State: "live", ExpTime: now.Add(5 * 24 * time.Hour).UnixMilli(),
		RedeemStartTime: okx.FlexInt(now.Add(-24 * time.Hour).UnixMilli()),
		RedeemEndTime:   okx.FlexInt(now.Add(48 * time.Hour).UnixMilli()),
	}
	if err := CheckRedeemable(base, now); err != nil {
		t.Fatalf("窗口内应可赎回: %v", err)
	}

	noWin := base
	noWin.RedeemStartTime, noWin.RedeemEndTime = 0, 0
	if err := CheckRedeemable(noWin, now); err == nil {
		t.Fatal("无赎回窗口（BTC/ETH）应拒绝")
	}

	early := base
	early.RedeemStartTime = okx.FlexInt(now.Add(day).UnixMilli())
	if err := CheckRedeemable(early, now); err == nil {
		t.Fatal("未到窗口应拒绝")
	}

	late := base
	late.RedeemEndTime = okx.FlexInt(now.Add(-time.Hour).UnixMilli())
	if err := CheckRedeemable(late, now); err == nil {
		t.Fatal("已过窗口应拒绝")
	}

	near := base
	near.ExpTime = now.Add(30 * time.Minute).UnixMilli()
	if err := CheckRedeemable(near, now); err == nil {
		t.Fatal("距到期 <1h 应拒绝")
	}

	for _, state := range []string{"redeemed", "settled", "rejected", "pending_redeem"} {
		s := base
		s.State = state
		if err := CheckRedeemable(s, now); err == nil {
			t.Fatalf("状态 %s 应拒绝", state)
		}
	}
}

// DCDOrder 按 ordId 过滤历史订单。
func TestDCDOrderLookup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/finance/sfp/dcd/order-history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ordId"); got != "ord-2" && got != "nope" {
			t.Errorf("ordId 查询参数 = %q", got)
		}
		io.WriteString(w, `{"code":"0","data":{"orders":[
			{"ordId":"ord-1","state":"live","notionalSz":"100"},
			{"ordId":"ord-2","state":"live","notionalSz":"200","redeemStartTime":"1700000000000","redeemEndTime":"1700086400000"}]}}`)
	})
	ex := newRedeemExec(t, mux)
	o, err := ex.DCDOrder("ord-2")
	if err != nil {
		t.Fatalf("DCDOrder: %v", err)
	}
	if o.OrdID != "ord-2" || o.NotionalSz != "200" || int64(o.RedeemStartTime) == 0 {
		t.Fatalf("order = %+v", o)
	}
	if _, err := ex.DCDOrder("nope"); err == nil {
		t.Fatal("不存在的订单应报错")
	}
}
