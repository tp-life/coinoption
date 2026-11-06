package store

import (
	"testing"

	"github.com/tp/coinoption/internal/strategy"
)

func TestDefaultSettingsV1(t *testing.T) {
	d := DefaultSettings()
	if d.StrategyMode != "dcy_only" {
		t.Fatalf("strategyMode = %q", d.StrategyMode)
	}
	if d.MaxAbsDelta != 0.30 || d.MinIvRvSpread != 0.05 || d.DcyMaxPrincipalUSDT != 100 {
		t.Fatalf("v1 defaults wrong: %+v", d)
	}
	if d.MinAbsYieldPct != 0.5 || d.MinAPRPct != 12 {
		t.Fatalf("收益门槛 defaults wrong: %+v", d)
	}
	if d.TenorMinDays != 10 || d.TenorMaxDays != 30 || d.RVWindowDays != 30 {
		t.Fatalf("tenor/RV defaults wrong: %+v", d)
	}
}

func TestSettingsApplyToV1(t *testing.T) {
	st := DefaultSettings()
	st.CapitalUSDT = 1000
	st.MaxAbsDelta = 0.25
	st.MinIvRvSpread = 0.08
	st.DcyMaxPrincipalUSDT = 50
	st.DcyTotalPrincipalPct = 0.20
	st.MinAbsYieldPct = 0.8
	st.MinAPRPct = 15
	st.MaxConcurrent = 2

	p := strategy.DefaultParams()
	st.ApplyTo(&p)
	if p.CapitalUSDT != 1000 || p.MaxAbsDelta != 0.25 || p.MinIvRvSpread != 0.08 {
		t.Fatalf("参数未映射: %+v", p)
	}
	if p.MinAbsYield != 0.008 || p.MinAPR != 0.15 {
		t.Fatalf("收益门槛未映射: %+v", p)
	}
	if p.DcyMaxPrincipalUSDT != 50 || p.DcyTotalPrincipalPct != 0.20 || p.MaxConcurrent != 2 {
		t.Fatalf("sizing 参数未映射: %+v", p)
	}
}

// 旧 settings.json 没有 v1 字段时，LoadSettings 必须回落到默认，而不是 0。
func TestLoadSettingsV1Defaults(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SaveSettings(Settings{CapitalUSDT: 1000, MinOptionEdgePP: 20, MinAbsEdgePct: 0.5, MaxStrikeGapPct: 0.5}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.StrategyMode != "dcy_only" || got.MaxAbsDelta <= 0 || got.DcyMaxPrincipalUSDT <= 0 || got.RVWindowDays <= 0 {
		t.Fatalf("v1 默认未回落: %+v", got)
	}
	if got.MinAbsYieldPct <= 0 || got.MinAPRPct <= 0 {
		t.Fatalf("收益门槛默认未回落: %+v", got)
	}
}

// 风控绝对上限可以独立于资金总额保存；单笔 > 总敞口时夹到总敞口，负值视为未设置。
func TestSettingsRiskOverrides(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SaveSettings(Settings{CapitalUSDT: 1000, TotalExposureUSDT: 6000, PerOrderUSDT: 7000}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CapitalUSDT != 1000 || got.TotalExposureUSDT != 6000 || got.PerOrderUSDT != 6000 {
		t.Fatalf("覆盖值读取/夹取不符: %+v", got)
	}

	if err := s.SaveSettings(Settings{CapitalUSDT: 1000, TotalExposureUSDT: -1, PerOrderUSDT: -2}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = s.LoadSettings()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TotalExposureUSDT != 0 || got.PerOrderUSDT != 0 {
		t.Fatalf("负值应回落为 0: %+v", got)
	}
}

// IV 分位门槛：0 = 关闭，合法值原样映射，越界回落 0。
func TestSettingsIVPercentile(t *testing.T) {
	st := DefaultSettings()
	if st.MinIVPercentilePct != 0 {
		t.Fatalf("默认应为 0（关闭），实际 %v", st.MinIVPercentilePct)
	}
	st.MinIVPercentilePct = 30
	p := strategy.Params{}
	st.ApplyTo(&p)
	if p.MinIVPercentile != 30 {
		t.Fatalf("MinIVPercentile = %v，期望 30", p.MinIVPercentile)
	}

	dir := t.TempDir()
	s, _ := New(dir)
	bad := DefaultSettings()
	bad.MinIVPercentilePct = 150
	if err := s.SaveSettings(bad); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.MinIVPercentilePct != 0 {
		t.Fatalf("越界值应回落 0，实际 %v", got.MinIVPercentilePct)
	}
}
