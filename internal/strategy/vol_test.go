package strategy

import (
	"math"
	"testing"
	"time"
)

func TestBSPutCallParity(t *testing.T) {
	S, K, T, sigma := 100.0, 90.0, 0.5, 0.30
	call := BSCall(S, K, T, sigma)
	put := BSPut(S, K, T, sigma)
	if math.Abs((call-put)-(S-K)) > 1e-9 {
		t.Fatalf("put-call parity: C-P=%.12f, S-K=%.12f", call-put, S-K)
	}
}

func TestBSATMPutKnownValue(t *testing.T) {
	// S=K=100, T=1, sigma=20% → put ≈ 7.9656（r=0）。
	got := BSPut(100, 100, 1, 0.20)
	if math.Abs(got-7.9656) > 1e-3 {
		t.Fatalf("ATM put = %.6f, want ~7.9656", got)
	}
	if got := BSPutFraction(100, 100, 1, 0.20); math.Abs(got-0.079656) > 1e-6 {
		t.Fatalf("ATM put fraction = %.8f, want 0.079656", got)
	}
}

func TestImpliedVolPutRoundTrip(t *testing.T) {
	for _, sigma := range []float64{0.15, 0.40, 0.80} {
		price := BSPut(80_000, 76_000, 10.0/365, sigma)
		got, err := ImpliedVolPut(price, 80_000, 76_000, 10.0/365)
		if err != nil {
			t.Fatalf("IV(%v): %v", sigma, err)
		}
		if math.Abs(got-sigma) > 1e-6 {
			t.Fatalf("IV round trip: got %.9f want %.9f", got, sigma)
		}
	}
}

func TestAbsDeltaPut(t *testing.T) {
	// ATM put |delta| = N(-d1) ≈ 0.4602（sigma=20%, T=1）。
	got := AbsDeltaPut(100, 100, 1, 0.20)
	if math.Abs(got-0.4602) > 1e-3 {
		t.Fatalf("ATM |put delta| = %.6f, want ~0.4602", got)
	}
	// 越 OTM，|delta| 越小。
	otm := AbsDeltaPut(100, 90, 1, 0.20)
	if !(otm < got) {
		t.Fatalf("OTM |delta| %.4f 应小于 ATM %.4f", otm, got)
	}
}

func TestRealizedVol(t *testing.T) {
	// 常数收盘价 → 0 波动。
	flat := make([]float64, 31)
	for i := range flat {
		flat[i] = 100
	}
	if rv, err := RealizedVol(flat, 30); err != nil || rv > 1e-12 {
		t.Fatalf("flat RV = %v, %v; want 0", rv, err)
	}

	// 交替 ±1% 收益：样本标准差 ≈ 0.01005，年化 ≈ 0.192。
	closes := []float64{100}
	for i := 0; i < 30; i++ {
		r := 0.01
		if i%2 == 1 {
			r = -0.01
		}
		closes = append(closes, closes[len(closes)-1]*math.Exp(r))
	}
	rv, err := RealizedVol(closes, 30)
	if err != nil {
		t.Fatalf("RV: %v", err)
	}
	if rv < 0.18 || rv > 0.20 {
		t.Fatalf("alternating RV = %.6f, want 0.18–0.20", rv)
	}

	// 数据不足。
	if _, err := RealizedVol([]float64{1, 2}, 30); err == nil {
		t.Fatal("数据不足应报错")
	}
}

func TestExpiryYears(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	exp := now.Add(10 * 24 * time.Hour)
	if got := ExpiryYears(exp, now); math.Abs(got-10.0/365) > 1e-12 {
		t.Fatalf("ExpiryYears = %.12f, want %.12f", got, 10.0/365)
	}
	if got := ExpiryYears(now.Add(-time.Hour), now); got != 0 {
		t.Fatalf("已到期应返回 0，got %v", got)
	}
}

// EWMA 常数波动序列应收敛到该波动；分位/多窗口组合也要自洽。
func TestEWMAAndEstimateVol(t *testing.T) {
	// ±1% 交替 → 日收益标准差 1%，年化 sqrt(365)% ≈ 19.1%。
	closes := make([]float64, 120)
	closes[0] = 100
	for i := 1; i < len(closes); i++ {
		if i%2 == 1 {
			closes[i] = closes[i-1] * 1.01
		} else {
			closes[i] = closes[i-1] / 1.01
		}
	}
	ewma, err := EWMAVol(closes, 0.94)
	if err != nil {
		t.Fatalf("EWMAVol: %v", err)
	}
	if math.Abs(ewma-0.01*math.Sqrt(365)) > 0.01 {
		t.Fatalf("EWMA = %.4f, want ~%.4f", ewma, 0.01*math.Sqrt(365))
	}
	est := EstimateVolWindow(closes, 30)
	if est.Entry < est.RVW || est.Entry < est.EWMA-1e-9 {
		t.Fatalf("Entry 应取 max(EWMA, RVW): %+v", est)
	}
	if est.Pct < 0 || est.Pct > 100 {
		t.Fatalf("Pct = %v，期望 0~100（120 根样本足够）", est.Pct)
	}
	if est.RV7 <= 0 || est.RV90 <= 0 {
		t.Fatalf("多窗口缺失: %+v", est)
	}
}

func TestEWMAVolTooFew(t *testing.T) {
	if _, err := EWMAVol([]float64{1, 2, 3}, 0.94); err == nil {
		t.Fatal("样本不足应报错")
	}
}

func TestPercentileRank(t *testing.T) {
	sample := []float64{0.1, 0.2, 0.3, 0.4}
	if got := PercentileRank(0.3, sample, 2); math.Abs(got-75) > 1e-9 {
		t.Fatalf("Pct(0.3) = %v, want 75", got)
	}
	if got := PercentileRank(0.05, sample, 2); got != 0 {
		t.Fatalf("Pct(min-1) = %v, want 0", got)
	}
	if got := PercentileRank(1, sample, 2); got != 100 {
		t.Fatalf("Pct(max+1) = %v, want 100", got)
	}
	if got := PercentileRank(0.3, sample, 10); got != -1 {
		t.Fatalf("样本不足应返回 -1，实际 %v", got)
	}
}
