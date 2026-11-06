package strategy

import (
	"strings"
	"testing"
)

func TestBuildGuideTargetPriceSection(t *testing.T) {
	g := BuildGuide(DefaultParams())
	if g.Title == "" || g.Intro == "" {
		t.Fatalf("策略说明缺少标题/简介: %+v", g)
	}
	var target *GuideSection
	for i := range g.Sections {
		if g.Sections[i].Key == "target" {
			target = &g.Sections[i]
			break
		}
	}
	if target == nil {
		t.Fatal("策略说明缺少 target（目标价选择）段落")
	}
	joined := strings.Join(target.Lines, "\n")
	for _, want := range []string{"目标价", "OKX", "Delta", "IV", "RV", "score", "空仓"} {
		if !strings.Contains(joined+target.Note, want) {
			t.Errorf("目标价段落缺少 %q:\n%s\n%s", want, joined, target.Note)
		}
	}
	if !strings.Contains(target.Note, "不是选 APR 最高") {
		t.Errorf("目标价结论未说明不追最高 APR: %q", target.Note)
	}
}

func TestBuildGuideUsesParams(t *testing.T) {
	p := DefaultParams()
	p.TenorMinDays = 7
	p.TenorMaxDays = 14
	p.MaxAbsDelta = 0.20
	p.MinIvRvSpread = 0.08
	p.MinAbsYield = 0.008
	p.MinAPR = 0.15
	p.DcyMaxPrincipalUSDT = 50
	p.DcyMaxPrincipalPct = 0.05
	p.DcyTotalPrincipalPct = 0.20
	p.DcyDeltaExposurePct = 0.02
	p.MaxConcurrent = 1
	p.DailyLossPct = 0.01
	g := BuildGuide(p)
	var all []string
	for _, s := range g.Sections {
		all = append(all, s.Lines...)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"7–14", "0.20", "8pp", "0.8%", "44%", "资金口径", "50 USDT", "5%", "20%", "2%", "1 笔", "1%"} {
		if !strings.Contains(joined, want) {
			t.Errorf("策略说明未反映参数 %q:\n%s", want, joined)
		}
	}
}

func TestBuildGuideLegacyMode(t *testing.T) {
	p := DefaultParams()
	p.StrategyMode = "legacy"
	g := BuildGuide(p)
	if !strings.Contains(g.Intro, "legacy") || !strings.Contains(g.Intro, "dcy_only") {
		t.Fatalf("legacy 模式未在简介中提示: %q", g.Intro)
	}
}
