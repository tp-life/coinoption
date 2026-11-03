package ui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/tp/coinoption/internal/strategy"
)

// sortField 表格排序键。默认顺序由策略层/行情层给出（决策：动作分组 →
// 到期 → 行权价；期权链：到期 → 行权价），这里只是在其之上叠一层「按值排序」，
// 并列时仍回落到默认顺序，避免同一合约在每秒刷新时因为 map 顺序漂移。
type sortField int

const (
	sortDefault sortField = iota // 策略默认顺序
	sortPrice                    // 价格（买价；无买盘时用最新价兜底）
	sortAPR                      // 期权 APR
	sortDiff                     // 差%（期权 APR − DCY APR）
)

var sortFieldNames = map[sortField]string{
	sortDefault: "默认",
	sortPrice:   "价格",
	sortAPR:     "APR",
	sortDiff:    "差%",
}

// sortSpec 当前排序键 + 方向。字符串形式（"price:desc"）同时用于
// settings.json 记忆与排序弹窗的选项值。
type sortSpec struct {
	field sortField
	desc  bool
}

func (s sortSpec) active() bool { return s.field != sortDefault }

// label 状态短标签：默认 / 价格↓ / APR↑ / 差%↓。
func (s sortSpec) label() string {
	if !s.active() {
		return sortFieldNames[sortDefault]
	}
	if s.desc {
		return sortFieldNames[s.field] + "↓"
	}
	return sortFieldNames[s.field] + "↑"
}

// String 配置文件存储形式：默认顺序为 ""，其余如 "price:desc"。
func (s sortSpec) String() string {
	if !s.active() {
		return ""
	}
	dir := "desc"
	if !s.desc {
		dir = "asc"
	}
	switch s.field {
	case sortPrice:
		return "price:" + dir
	case sortAPR:
		return "apr:" + dir
	case sortDiff:
		return "diff:" + dir
	}
	return ""
}

// parseSortSpec 解析 settings.json / 排序弹窗的选项值；非法值回落默认顺序。
func parseSortSpec(v string) sortSpec {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(v)), ":")
	if len(parts) != 2 {
		return sortSpec{}
	}
	var f sortField
	switch parts[0] {
	case "price":
		f = sortPrice
	case "apr":
		f = sortAPR
	case "diff":
		f = sortDiff
	default:
		return sortSpec{}
	}
	switch parts[1] {
	case "desc":
		return sortSpec{field: f, desc: true}
	case "asc":
		return sortSpec{field: f, desc: false}
	}
	return sortSpec{}
}

// refValue 一行排序键（ok=false 表示该行没有这个值，统一沉底）。
type refValue struct {
	ref rowRef
	val float64
	ok  bool
}

// sortRefs 按当前排序键重排行引用。无值的行（无买盘 → 算不出 APR/差%）
// 无论升降序都排在最后；并列行保持默认顺序（稳定排序 + 原始次序）。
func (a App) sortRefs(refs []rowRef) []rowRef {
	if !a.sorting.active() || len(refs) < 2 || a.snap == nil {
		return refs
	}
	// 期权链的「差%」需要回查 DCY APR：先建一次 map，避免比较器里反复遍历。
	var dcy map[string]float64
	if a.sorting.field == sortDiff {
		dcy = make(map[string]float64, len(a.snap.Decisions))
		for _, d := range a.snap.Decisions {
			if d.Row.OptInstID == "" {
				continue
			}
			if _, ok := dcy[d.Row.OptInstID]; !ok {
				dcy[d.Row.OptInstID] = d.Row.DcyAPR
			}
		}
	}
	vals := make([]refValue, 0, len(refs))
	for _, ref := range refs {
		v, ok := a.sortValue(ref, dcy)
		vals = append(vals, refValue{ref: ref, val: v, ok: ok})
	}
	desc := a.sorting.desc
	sort.SliceStable(vals, func(i, j int) bool {
		x, y := vals[i], vals[j]
		if x.ok != y.ok {
			return x.ok // 有值在前，无值沉底（升序也不把 — 顶到最前）
		}
		if !x.ok || x.val == y.val {
			return false // 并列：保留默认顺序
		}
		if desc {
			return x.val > y.val
		}
		return x.val < y.val
	})
	out := make([]rowRef, len(vals))
	for i := range vals {
		out[i] = vals[i].ref
	}
	return out
}

// sortValue 取一行在当前排序键下的数值（ok=false = 该行没有可比值）。
func (a App) sortValue(ref rowRef, dcy map[string]float64) (float64, bool) {
	switch ref.kind {
	case rowOption:
		if ref.idx < 0 || ref.idx >= len(a.snap.OptionRows) {
			return 0, false
		}
		or := a.snap.OptionRows[ref.idx]
		switch a.sorting.field {
		case sortPrice:
			if v, ok := premiumUSDT(or.BidPx, or.SettleCcy, or.InstID, or.SpotPx); ok {
				return v, true
			}
			// 无买盘的冷门档位用最新价兜底（同为折算后口径）
			return premiumUSDT(or.LastPx, or.SettleCcy, or.InstID, or.SpotPx)
		case sortAPR:
			if !or.APRValid {
				return 0, false
			}
			return or.OptAPR, true
		case sortDiff:
			if !or.APRValid {
				return 0, false
			}
			base, ok := dcy[or.InstID]
			if !ok {
				return 0, false // 无对应 DCY 产品，没有差值可比
			}
			return or.OptAPR - base, true
		}
	default:
		if ref.idx < 0 || ref.idx >= len(a.snap.Decisions) {
			return 0, false
		}
		r := a.snap.Decisions[ref.idx].Row
		hasBid := strings.TrimSpace(r.OptBidPx) != ""
		switch a.sorting.field {
		case sortPrice:
			// 决策表只有期权买一价（没有最新价列）。
			return premiumUSDT(r.OptBidPx, r.OptSettleCcy, r.OptInstID, r.Spot)
		case sortAPR:
			if !hasBid {
				return 0, false // 无买盘算不出卖方 APR
			}
			return r.OptAPR, true
		case sortDiff:
			if !hasBid {
				return 0, false
			}
			return r.Diff, true
		}
	}
	return 0, false
}

// priceValue 把权利金原始串转成可比较数值；空串/非法值 → ok=false。
func priceValue(v string) (float64, bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// premiumUSDT 把权利金折算成 USDT 口径，保证币本位（BTC 计价，如 0.0038）
// 与 U 本位（USD/USDT 计价，如 290）能在同一把尺子上比较——否则按原始数字
// 排序会把「同一经济价值的两个链」拆到表格两端。现价缺失时退回原始值。
func premiumUSDT(raw, settleCcy, instID string, spot float64) (float64, bool) {
	f, ok := priceValue(raw)
	if !ok {
		return 0, false
	}
	if strategy.SettleKind(settleCcy, instID) == 'B' && spot > 0 {
		return f * spot, true
	}
	return f, true
}
