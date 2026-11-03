package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// sortSnap 构造排序测试快照：三条决策 + 三条期权链（覆盖币本位 / U 本位、
// 有无买盘、只有最新价的冷门档）。
func sortSnap() *market.Snapshot {
	exp := time.Now().Add(10 * 24 * time.Hour)
	mk := func(pid, inst, settle, bid string, apr, diff float64) strategy.Decision {
		return strategy.Decision{
			Action:     strategy.ActionBuyOption,
			ReasonCode: "test",
			Row: strategy.MatchRow{
				ProductID: pid, Uly: "BTC-USD", OptType: 'P', Strike: 70000,
				InvestCcy: "USDT", DcyAPR: 0.18, OptAPR: apr, Diff: diff,
				Days: 10, ExpTime: exp, OptMatched: true,
				OptInstID: inst, OptBidPx: bid, OptSettleCcy: settle, Spot: 76000,
			},
		}
	}
	return &market.Snapshot{
		At: time.Now(), Uly: "BTC-USDT", OptionUly: "BTC-USD", SpotPrice: 76000,
		DCYAvailable: true,
		Decisions: []strategy.Decision{
			// 币本位：0.0038 BTC × 76000 ≈ 288.8 USDT
			mk("BTC-USDT-261113-70000-P", "BTC-USD-261113-70000-P", "BTC", "0.0038", 0.30, -0.05),
			// U 本位：290 USD（原始数字最大，折算后也最大）
			mk("BTC-USDT-261113-80000-P", "BTC-USD_UM-261113-80000-P", "USD", "290", 0.50, 0.30),
			// 无买盘：算不出 APR / 差%
			mk("BTC-USDT-261113-90000-P", "BTC-USD-261113-90000-P", "BTC", "", 0, 0),
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261113-70000-P", OptType: 'P', Strike: 70000, BidPx: "0.0038",
				LastPx: "0.0035", SettleCcy: "BTC", SpotPx: 76000, OptAPR: 0.30, APRValid: true},
			{InstID: "BTC-USD_UM-261113-80000-P", OptType: 'P', Strike: 80000, BidPx: "290",
				SettleCcy: "USD", SpotPx: 76000, OptAPR: 0.50, APRValid: true},
			{InstID: "BTC-USD_UM-261113-90000-P", OptType: 'P', Strike: 90000, BidPx: "",
				LastPx: "260", SettleCcy: "USD", SpotPx: 76000, APRValid: false},
		},
	}
}

func sortApp(t *testing.T, sortBy string) (App, *fakeExec) {
	t.Helper()
	fe := &fakeExec{}
	if sortBy != "" {
		st := store.DefaultSettings()
		st.SortBy = sortBy
		fe.settings = st
	}
	a := New(fakeFetcher{snap: sortSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(snapMsg{snap: sortSnap()})
	app, ok := m.(App)
	if !ok {
		t.Fatal("Update 返回类型不是 App")
	}
	return app, fe
}

// decisionOrder 「产品/合约」列的显示顺序（决策页该列无样式，可直接比较）。
func decisionOrder(a App) []string {
	out := make([]string, 0, len(a.table.Rows()))
	for _, r := range a.table.Rows() {
		out = append(out, r[1])
	}
	return out
}

// chainOrder 期权链「合约」列顺序。命中有 DCY 的行会追加带样式的 ←DCY
// 标记，因此只比较 instId 前缀。
func chainOrder(t *testing.T, a App, want []string) {
	t.Helper()
	rows := a.table.Rows()
	if len(rows) != len(want) {
		t.Fatalf("期权链行数 = %d，期望 %d", len(rows), len(want))
	}
	for i, r := range rows {
		if !strings.HasPrefix(r[1], want[i]) {
			got := make([]string, len(rows))
			for j, x := range rows {
				got[j] = x[1]
			}
			t.Fatalf("期权链第 %d 行 = %q，期望以 %q 开头（全量 %v）", i, r[1], want[i], got)
		}
	}
}

func TestTUISortDefaultKeepsSnapshotOrder(t *testing.T) {
	app, _ := sortApp(t, "")
	want := []string{
		"BTC-USDT-261113-70000-P → 261113-70000-P",
		"BTC-USDT-261113-80000-P → BTC-USD_UM-261113-80000-P",
		"BTC-USDT-261113-90000-P → 261113-90000-P",
	}
	got := decisionOrder(app)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("默认顺序第 %d 行 = %q，期望 %q（全量 %v）", i, got[i], want[i], got)
		}
	}
}

func TestTUISortByPriceNormalizesSettleCurrency(t *testing.T) {
	app, _ := sortApp(t, "")

	app.sorting = sortSpec{field: sortPrice, desc: true}
	app = app.rebuildTable()
	got := decisionOrder(app)
	// 0.0038 BTC × 76000 = 288.8 USDT 略小于 290 USD，因此 U 本位排第一；
	// 无买盘行无论升降序都沉底。
	if got[0] != "BTC-USDT-261113-80000-P → BTC-USD_UM-261113-80000-P" {
		t.Fatalf("价格降序首行 = %q（期望 80000 那条 U 本位）", got[0])
	}
	if got[2] != "BTC-USDT-261113-90000-P → 261113-90000-P" {
		t.Fatalf("无买盘行应沉底，实际末行 = %q", got[2])
	}

	app.sorting = sortSpec{field: sortPrice}
	app = app.rebuildTable()
	got = decisionOrder(app)
	if got[0] != "BTC-USDT-261113-70000-P → 261113-70000-P" {
		t.Fatalf("价格升序首行 = %q（期望 288.8 USDT 的币本位那条）", got[0])
	}
	if got[2] != "BTC-USDT-261113-90000-P → 261113-90000-P" {
		t.Fatalf("升序时无买盘行也应沉底，实际末行 = %q", got[2])
	}
}

func TestTUISortByAPRAndDiff(t *testing.T) {
	app, _ := sortApp(t, "")

	app.sorting = sortSpec{field: sortAPR, desc: true}
	app = app.rebuildTable()
	got := decisionOrder(app)
	if got[0] != "BTC-USDT-261113-80000-P → BTC-USD_UM-261113-80000-P" ||
		got[1] != "BTC-USDT-261113-70000-P → 261113-70000-P" {
		t.Fatalf("APR 降序前两行 = %v（期望 50%% → 30%%）", got[:2])
	}

	app.sorting = sortSpec{field: sortAPR}
	app = app.rebuildTable()
	got = decisionOrder(app)
	if got[0] != "BTC-USDT-261113-70000-P → 261113-70000-P" {
		t.Fatalf("APR 升序首行 = %q（期望 30%%）", got[0])
	}

	app.sorting = sortSpec{field: sortDiff, desc: true}
	app = app.rebuildTable()
	got = decisionOrder(app)
	if got[0] != "BTC-USDT-261113-80000-P → BTC-USD_UM-261113-80000-P" {
		t.Fatalf("差%% 降序首行 = %q（期望 +30%%）", got[0])
	}
	if got[1] != "BTC-USDT-261113-70000-P → 261113-70000-P" {
		t.Fatalf("差%% 降序次行 = %q（期望 -5%%，仍排在无买盘行之前）", got[1])
	}
}

func TestTUISortChainPriceFallsBackToLast(t *testing.T) {
	app, _ := sortApp(t, "")
	m, _ := app.Update(key('2'))
	app = m.(App)
	app.sorting = sortSpec{field: sortPrice, desc: true}
	app = app.rebuildTable()

	// 290 USD > 0.0038 BTC(≈288.8) > 仅最新价 260（无买盘兜底）
	chainOrder(t, app, []string{
		"BTC-USD_UM-261113-80000-P",
		"BTC-USD-261113-70000-P",
		"BTC-USD_UM-261113-90000-P",
	})
}

func TestTUISortChainDiffNeedsDCY(t *testing.T) {
	app, _ := sortApp(t, "")
	m, _ := app.Update(key('2'))
	app = m.(App)
	app.sorting = sortSpec{field: sortDiff, desc: true}
	app = app.rebuildTable()

	// 有 DCY 对应的两行（+32% / +12%）在前，无 DCY 对应的行沉底。
	chainOrder(t, app, []string{
		"BTC-USD_UM-261113-80000-P",
		"BTC-USD-261113-70000-P",
		"BTC-USD_UM-261113-90000-P",
	})
}

func TestTUISortPickerAppliesAndPersists(t *testing.T) {
	app, fe := sortApp(t, "")
	m, _ := app.Update(key('f'))
	app = m.(App)
	if app.picker != pickerSort {
		t.Fatalf("按 f 后 picker = %v，期望排序弹窗", app.picker)
	}
	// 预选中「默认顺序」（index 0）；↓ 一次 → 价格 高→低。
	m, _ = app.Update(down())
	app = m.(App)
	m, _ = app.Update(enter())
	app = m.(App)

	if app.picker != pickerNone {
		t.Fatal("Enter 后排序弹窗应关闭")
	}
	if app.sorting != (sortSpec{field: sortPrice, desc: true}) {
		t.Fatalf("排序状态 = %+v，期望 价格↓", app.sorting)
	}
	if fe.settings.SortBy != "price:desc" {
		t.Fatalf("settings.SortBy = %q，期望 price:desc", fe.settings.SortBy)
	}
	rows := app.table.Rows()
	if len(rows) == 0 || len(rows[0]) != len(app.table.Columns()) {
		t.Fatal("排序后列数与行单元格数不一致")
	}
}

func TestTUISortRestoredFromSettings(t *testing.T) {
	app, _ := sortApp(t, "diff:asc")
	if app.sorting != (sortSpec{field: sortDiff}) {
		t.Fatalf("启动时排序状态 = %+v，期望 差%%↑", app.sorting)
	}
	if got := decisionOrder(app); got[0] != "BTC-USDT-261113-70000-P → 261113-70000-P" {
		t.Fatalf("差%% 升序首行 = %q（期望 -5%% 那条）", got[0])
	}
}

func TestParseSortSpec(t *testing.T) {
	cases := []struct {
		in   string
		want sortSpec
	}{
		{"", sortSpec{}},
		{"price:desc", sortSpec{field: sortPrice, desc: true}},
		{"price:asc", sortSpec{field: sortPrice}},
		{"APR:DESC", sortSpec{field: sortAPR, desc: true}},
		{"diff:asc", sortSpec{field: sortDiff}},
		{"diff", sortSpec{}},
		{"volume:desc", sortSpec{}},
		{"price:sideways", sortSpec{}},
	}
	for _, c := range cases {
		if got := parseSortSpec(c.in); got != c.want {
			t.Fatalf("parseSortSpec(%q) = %+v，期望 %+v", c.in, got, c.want)
		}
	}
	if got := (sortSpec{field: sortDiff, desc: true}).String(); got != "diff:desc" {
		t.Fatalf("String() = %q", got)
	}
	if got := (sortSpec{}).String(); got != "" {
		t.Fatalf("默认排序 String() = %q，期望空串", got)
	}
}

// 实时按价格排序时行会上下移动：光标必须跟着「同一产品」走，不能只按行号保留，
// 否则按 o/d 会对着另一条产品下单。
func TestTUISortKeepsCursorOnSameRow(t *testing.T) {
	app, _ := sortApp(t, "price:desc")
	if got := decisionOrder(app)[0]; !strings.HasPrefix(got, "BTC-USDT-261113-80000-P") {
		t.Fatalf("价格降序首行 = %q（期望 80000 那条）", got)
	}
	if app.table.Cursor() != 0 {
		t.Fatalf("初始光标 = %d，期望 0", app.table.Cursor())
	}

	// 下一帧：70000 买价涨到 0.005 BTC（≈380 USDT），应顶到第一行。
	snap := sortSnap()
	snap.Decisions[0].Row.OptBidPx = "0.005"
	snap.Decisions[0].Row.OptAPR = 0.80
	snap.Decisions[0].Row.Diff = 0.62
	m, _ := app.Update(snapMsg{snap: snap})
	app = m.(App)

	if got := decisionOrder(app)[0]; !strings.HasPrefix(got, "BTC-USDT-261113-70000-P") {
		t.Fatalf("刷新后首行 = %q（期望 70000 涨价后顶上来）", got)
	}
	if app.table.Cursor() != 1 {
		t.Fatalf("刷新后光标 = %d，期望仍在 80000 那行（index 1）", app.table.Cursor())
	}
	ref, ok := app.currentRef()
	if !ok {
		t.Fatal("光标应指向有效行")
	}
	if pid := app.snap.Decisions[ref.idx].Row.ProductID; pid != "BTC-USDT-261113-80000-P" {
		t.Fatalf("刷新后选中产品 = %q，期望仍为 BTC-USDT-261113-80000-P", pid)
	}
}

// 组合条新增「排序」chip 后仍不能超宽换行（换行会把表格顶下去）。
func TestTUIFilterBarFitsWidth(t *testing.T) {
	app, _ := sortApp(t, "price:desc")
	for _, w := range []int{140, 120, 100, 90, 80, 60} {
		m, _ := app.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		app = m.(App)
		if got := lipgloss.Width(app.filterBar()); got > w {
			t.Fatalf("终端宽 %d 时组合条宽 %d（超宽会换行）", w, got)
		}
	}
	if got := app.filterBar(); !strings.Contains(got, "排序") {
		t.Fatalf("组合条应显示排序状态，实际 %q", got)
	}
}
