package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

func riskTestProduct(now time.Time, strike, absYield float64) okx.DCDProduct {
	return okx.DCDProduct{
		ProductID: "BTC-USDT-TEST-P", BaseCcy: "BTC", QuoteCcy: "USDT",
		NotionalCcy: "USDT", OptType: "P", Uly: "BTC-USD",
		Strike: okx.FlexReal(strike), AbsYield: okx.FlexReal(absYield),
		MinSize: okx.FlexReal(10), MaxSize: okx.FlexReal(100000), StepSz: okx.FlexReal(1),
		ExpTime: okx.FlexInt(now.Add(22 * 24 * time.Hour).UnixMilli()),
	}
}

func riskTestExecutor(t *testing.T, lim Limits) *Executor {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(ModeSimLocal, nil, NewGuard(lim), st,
		func(string) (float64, error) { return 100000, nil })
}

// v2：组合累计 Delta 超限时，第二笔必须被拒（此前只按单笔折算，永远不生效）。
func TestSubmitDCDDeltaCapRejectsSecond(t *testing.T) {
	lim := DefaultLimits()
	lim.TotalExposureUSDT = 1000
	lim.PerOrderUSDT = 100
	lim.MaxConcurrent = 5
	lim.DCDDeltaExposureUSDT = 50
	lim.StressLossPct = 0 // 本用例只验证 Delta 门槛
	e := riskTestExecutor(t, lim)

	p := riskTestProduct(time.Now(), 97000, 0.02) // 约 3% OTM，delta ≈ 0.35-0.45
	if _, err := e.SubmitDCD(p, 100); err != nil {
		t.Fatalf("第一笔应通过: %v", err)
	}
	_, err := e.SubmitDCD(p, 100)
	if err == nil {
		t.Fatal("第二笔应被累计 Delta 拒绝")
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Field != "delta_exposure" {
		t.Fatalf("期望 delta_exposure，实际 %v", err)
	}
}

// v2：−30% 压力情景浮亏超限时拒绝新单（含已有在持）。
func TestSubmitDCDStressCapRejectsSecond(t *testing.T) {
	lim := DefaultLimits()
	lim.TotalExposureUSDT = 1000
	lim.PerOrderUSDT = 100
	lim.MaxConcurrent = 5
	lim.DCDDeltaExposureUSDT = 0 // 关闭 Delta，只看压力
	lim.StressShockPct = 0.30
	lim.StressLossPct = 0.05 // 50 USDT
	e := riskTestExecutor(t, lim)

	// K=95% spot：单笔 −30% 浮亏 = 100×(1−70/95) ≈ 26.3；两笔 52.6 > 50。
	p := riskTestProduct(time.Now(), 95000, 0.02)
	if _, err := e.SubmitDCD(p, 100); err != nil {
		t.Fatalf("第一笔应通过: %v", err)
	}
	_, err := e.SubmitDCD(p, 100)
	if err == nil {
		t.Fatal("第二笔应被压力情景拒绝")
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Field != "stress_loss" {
		t.Fatalf("期望 stress_loss，实际 %v", err)
	}
}

// Delta 估计：合理输入应落在 (0,1)，非法输入按 0/0.5 处理而不是 NaN。
func TestDCDDeltaSanity(t *testing.T) {
	now := time.Now()
	pos := dcdRiskPos{Strike: 95000, AbsYield: 0.02, ExpTime: now.Add(22 * 24 * time.Hour)}
	if d := dcdDelta(pos, 100000, now); d < 0.05 || d > 1 {
		t.Fatalf("delta = %v，期望 (0.05,1)", d)
	}
	pos.AbsYield = 0
	if d := dcdDelta(pos, 100000, now); d != 0 {
		t.Fatalf("无报价应返回 0，实际 %v", d)
	}
	pos.AbsYield = 0.02
	pos.ExpTime = now.Add(-time.Hour)
	if d := dcdDelta(pos, 100000, now); d != 0 {
		t.Fatalf("已到期应返回 0，实际 %v", d)
	}
}

// 手动下单（Advisory）：单笔/总敞口/并发/日笔数越限只提示，仍然成交并占用额度；
// 自动路径（SubmitDCD）继续硬拒绝。
func TestSubmitDCDAdvisoryBypassesLimits(t *testing.T) {
	lim := DefaultLimits()
	lim.TotalExposureUSDT = 50
	lim.PerOrderUSDT = 50
	lim.MaxConcurrent = 1
	lim.MaxSessionsPerDay = 1
	lim.DCDDeltaExposureUSDT = 0
	lim.StressLossPct = 0
	e := riskTestExecutor(t, lim)
	p := riskTestProduct(time.Now(), 90000, 0.02)

	if _, err := e.SubmitDCD(p, 100); err == nil {
		t.Fatal("自动路径应被单笔上限拒绝")
	}

	out, err := e.SubmitDCDAdvisory(p, 100)
	if err != nil {
		t.Fatalf("手动路径应放行: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("越限应产生提示")
	}
	joined := strings.Join(out.Warnings, "；")
	for _, want := range []string{"单笔", "总敞口"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("提示应包含 %q，实际 %q", want, joined)
		}
	}

	out2, err := e.SubmitDCDAdvisory(p, 100)
	if err != nil {
		t.Fatalf("第二笔手动单也应放行: %v", err)
	}
	joined = strings.Join(out2.Warnings, "；")
	for _, want := range []string{"并发上限", "日笔数上限"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("第二笔提示应包含 %q，实际 %q", want, joined)
		}
	}
	g := e.State()
	if g.OpenCount != 2 || g.ExposureUSDT < 200-1e-9 {
		t.Fatalf("手动放行仍须占用额度: %+v", g)
	}
}

// 手动下单：组合累计 Delta 超限也只提示，不拦单。
func TestSubmitDCDAdvisoryRiskCapsWarn(t *testing.T) {
	lim := DefaultLimits()
	lim.TotalExposureUSDT = 1000
	lim.PerOrderUSDT = 100
	lim.MaxConcurrent = 5
	lim.DCDDeltaExposureUSDT = 50
	lim.StressLossPct = 0
	e := riskTestExecutor(t, lim)
	p := riskTestProduct(time.Now(), 97000, 0.02)

	if _, err := e.SubmitDCD(p, 100); err != nil {
		t.Fatalf("第一笔应通过: %v", err)
	}
	out, err := e.SubmitDCDAdvisory(p, 100)
	if err != nil {
		t.Fatalf("手动放行失败: %v", err)
	}
	if !strings.Contains(strings.Join(out.Warnings, "；"), "delta_exposure") {
		t.Fatalf("应提示累计 Delta 超限，实际 %v", out.Warnings)
	}
}
