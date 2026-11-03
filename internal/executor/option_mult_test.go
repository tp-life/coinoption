package executor

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

// OKX BTC 期权 1 张 = 0.01 BTC（ctVal=1 × ctMult=0.01）。旧实现只乘 ctVal，
// 把 1 张单的敞口算成 77,000 USDT、权利金算成 0.01 BTC —— 默认单笔 2000
// 因此拒掉所有期权单。这里用 mock OKX 端到端校验敞口/权利金。
func TestSubmitOptionRespectsContractMultiplier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/public/instruments"):
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{
				"instId":"BTC-USD-261113-80000-C","instType":"OPTION","uly":"BTC-USD",
				"optType":"C","stk":"80000","expTime":"1794643200000",
				"ctVal":"1","ctValCcy":"BTC","ctMult":"0.01","settleCcy":"BTC"}]}`))
		case strings.Contains(r.URL.Path, "/market/ticker"):
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{
				"instId":"BTC-USD-261113-80000-C","bidPx":"0.01","askPx":"0.02","bidSz":"100"}]}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
		}
	}))
	defer srv.Close()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	c := okx.NewClient(okx.WithBaseURL(srv.URL))
	ex := New(ModeSimLocal, c, NewGuard(DefaultLimits()), st,
		func(string) (float64, error) { return 76000, nil })

	out, err := ex.SubmitOption("BTC-USD-261113-80000-C", 1, 0)
	if err != nil {
		t.Fatalf("SubmitOption: %v", err)
	}
	if math.Abs(out.AmtUSDT-760) > 0.5 { // 0.01 BTC × 76,000
		t.Fatalf("敞口 = %.2f，期望 ≈760（100 倍 bug 时应为 76,000）", out.AmtUSDT)
	}
	if math.Abs(out.Premium-0.0001) > 1e-9 { // 0.01 × bid 0.01
		t.Fatalf("每张权利金 = %.8f，期望 0.0001", out.Premium)
	}

	// 成交前复核：要求 100% APR 显然高于当前报价，应直接放弃（不产生第二单）。
	if _, err := ex.SubmitOptionChecked("BTC-USD-261113-80000-C", 1, 0, 1.0); err == nil {
		t.Fatal("最新 bid 算出的 APR 低于要求时应拒绝")
	}
}

// 申购步长向下取整 + 过截止时间直接拒绝。
func TestSubmitDCDStepAndCutoff(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ex := New(ModeSimLocal, nil, NewGuard(DefaultLimits()), st,
		func(string) (float64, error) { return 76000, nil })

	p := okx.DCDProduct{
		ProductID: "BTC-USDT-261113-80000-P", BaseCcy: "BTC", QuoteCcy: "USDT",
		NotionalCcy: "USDT", OptType: "P", Strike: okx.FlexReal(80000),
		AbsYield: okx.FlexReal(0.005), AnnualizedYield: okx.FlexReal(0.18),
		MinSize: okx.FlexReal(10), MaxSize: okx.FlexReal(100000), StepSz: okx.FlexReal(10),
		ExpTime: okx.FlexInt(time.Now().Add(10 * 24 * time.Hour).UnixMilli()),
	}
	out, err := ex.SubmitDCD(p, 123)
	if err != nil {
		t.Fatalf("SubmitDCD: %v", err)
	}
	if out.Amt != 120 {
		t.Fatalf("步长取整后 amt = %v，期望 120", out.Amt)
	}

	p.TradeEndTime = okx.FlexInt(time.Now().Add(-time.Hour).UnixMilli())
	if _, err := ex.SubmitDCD(p, 120); err == nil {
		t.Fatal("过申购截止应报错")
	}
}
