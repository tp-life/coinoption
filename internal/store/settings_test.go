package store

import (
	"strings"
	"testing"
)

// 新增设置项（资金总额 / 差值比例 / 授权码摘要）必须能往返，
// 手改出的非法值要有安全回落，不能让风控或授权被写坏。
func TestSettingsCapitalDiffAndAuthHash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	def := DefaultSettings()
	if def.CapitalUSDT != 5000 || def.MinDiffPct != 0 {
		t.Fatalf("默认资金/差值不符: %+v", def)
	}

	v := def
	v.CapitalUSDT = 12000
	v.MinDiffPct = 2.5
	v.WebAuthCodeHash = strings.Repeat("ab", 32)
	if err := s.SaveSettings(v); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.CapitalUSDT != 12000 || got.MinDiffPct != 2.5 || got.WebAuthCodeHash != v.WebAuthCodeHash {
		t.Fatalf("往返不符: %+v", got)
	}

	// 非法值：资金 ≤0 回落 5000、负差值归零、坏哈希视为未启用。
	v.CapitalUSDT = 0
	v.MinDiffPct = -3
	v.WebAuthCodeHash = "zzzz"
	if err := s.SaveSettings(v); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err = s.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.CapitalUSDT != 5000 || got.MinDiffPct != 0 || got.WebAuthCodeHash != "" {
		t.Fatalf("非法值未安全回落: %+v", got)
	}
}

// 排序记忆必须在 settings.json 里往返，并且手改出的非法值要回落默认顺序
// （否则重启后表格会停在一个不存在的排序键上）。
func TestSettingsSortByRoundTripAndValidation(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := DefaultSettings()
	v.SortBy = "diff:asc"
	v.SideFilter = "P"
	if err := s.SaveSettings(v); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.SortBy != "diff:asc" {
		t.Fatalf("SortBy = %q，期望 diff:asc", got.SortBy)
	}

	v.SortBy = "volume:desc"
	if err := s.SaveSettings(v); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err = s.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.SortBy != "" {
		t.Fatalf("非法排序键应回落默认顺序，实际 %q", got.SortBy)
	}
}
