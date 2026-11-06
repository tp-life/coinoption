package executor

import (
	"testing"
	"time"

	"github.com/tp/coinoption/internal/store"
)

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// SettleDual —— C 方向：投入 1 BTC，行权 76000，absYield 0.005（0.5%）。
func TestSettleDualCExercised(t *testing.T) {
	r, err := SettleDual('C', 76000, 0.005, 1, 79000) // 结算 79000 ≥ 行权
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exercised || r.PrincipalCcy != "USDT" {
		t.Fatalf("结算 79000≥76000 应行权交付 USDT: %+v", r)
	}
	// 被行权时收益也要按 strike 折成 USDT：1 BTC × 0.5% = 0.005 BTC → 380 USDT。
	if !nearly(r.PrincipalSz, 76000) || !nearly(r.YieldSz, 380) {
		t.Fatalf("交付应 76000 USDT + 380 USDT: %+v", r)
	}
}

func TestSettleDualCNotExercised(t *testing.T) {
	r, err := SettleDual('C', 76000, 0.005, 2, 74000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exercised || r.PrincipalCcy != "BTC" {
		t.Fatalf("结算<行权应留币: %+v", r)
	}
	if !nearly(r.PrincipalSz, 2) || !nearly(r.YieldSz, 2*0.005) {
		t.Fatalf("应本金 2 BTC + 收益 0.01 BTC: %+v", r)
	}
}

// P 方向：投入 76000 USDT @ 行权 76000；结算低于行权 → 全额换成 BTC。
func TestSettleDualPExercised(t *testing.T) {
	r, err := SettleDual('P', 76000, 0.005, 76000, 70000)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exercised || r.PrincipalCcy != "BTC" {
		t.Fatalf("结算<行权应换 BTC: %+v", r)
	}
	if !nearly(r.PrincipalSz, 1.0) { // 76000/76000 = 1 BTC
		t.Fatalf("应换 1 BTC: %+v", r)
	}
	if !nearly(r.YieldSz, 1.0*0.005) {
		t.Fatalf("收益应为 0.005 BTC: %+v", r)
	}
}

func TestSettleDualPNotExercised(t *testing.T) {
	r, err := SettleDual('P', 76000, 0.005, 5000, 77000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exercised || r.PrincipalCcy != "USDT" {
		t.Fatalf("结算≥行权应留 USDT: %+v", r)
	}
	if !nearly(r.YieldSz, 5000*0.005) {
		t.Fatalf("收益应 25 USDT: %+v", r)
	}
}

func TestSettleDualBadInput(t *testing.T) {
	if _, err := SettleDual('X', 100, 0.01, 1, 1); err == nil {
		t.Fatal("未知方向应报错")
	}
	if _, err := SettleDual('C', 0, 0.01, 1, 1); err == nil {
		t.Fatal("行权价 0 应报错")
	}
	if _, err := SettleDual('P', 100, 0.01, -1, 1); err == nil {
		t.Fatal("负金额应报错")
	}
}

func TestSettleOptionShortLoss(t *testing.T) {
	// 币本位：行权价 80000，结算 83000，2 张 ×1BTC → 每张损失 3000/83000 BTC
	loss, ccy, _ := SettleOptionShortLoss('C', 80000, 83000, 2, 1, "BTC")
	if ccy != "BTC" || !nearly(loss, 6000.0/83000.0) {
		t.Fatalf("C 币本位损失不符: %v %s", loss, ccy)
	}
	// USD 本位（_UM）：损失 6000 USD（除结算价）
	loss, ccy, _ = SettleOptionShortLoss('C', 80000, 83000, 2, 1, "USD")
	if ccy != "USD" || !nearly(loss, 6000) {
		t.Fatalf("UM 损失不符: %v %s", loss, ccy)
	}
	// BTC 期权 1 张 = 0.01 BTC（ctVal×ctMult）：1 张 C 80000→83000 只损失 30 USD 等值
	loss, ccy, _ = SettleOptionShortLoss('C', 80000, 83000, 1, 0.01, "USD")
	if ccy != "USD" || !nearly(loss, 30) {
		t.Fatalf("0.01 BTC/张 的损失不符: %v %s", loss, ccy)
	}
	// 归零
	loss, _, note := SettleOptionShortLoss('P', 80000, 83000, 1, 1, "BTC")
	if loss != 0 || note == "" {
		t.Fatalf("P 归零应 0 损失: %v %s", loss, note)
	}
}

// store 回环 + SettleDue 端到端（用内存执行器 + 临时目录）。
func TestSettleDueEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := osCreateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = st.SaveSim(newSimDCD("sim-dcd-t1", 'C', 76000, 0.005, 1, now.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	err = st.SaveSim(newSimDCD("sim-dcd-t2", 'P', 76000, 0.005, 76000, now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}

	e := &Executor{
		guard: NewGuard(DefaultLimits()),
		st:    st,
		spot:  func(string) (float64, error) { return 79000, nil },
	}
	got, err := e.SettleDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应只结算 1 笔（t2 未到期）: %d", len(got))
	}
	if got[0].ID != "sim-dcd-t1" || got[0].State != "settled" {
		t.Fatalf("结算单不符: %+v", got[0])
	}
	// C 方向结算 79000 ≥ 76000 → 被行权：交付 76000 + 380 USDT，
	// 相对投入 1 BTC（79000）亏 2620 USDT。
	if !nearly(got[0].YieldSz, 380) {
		t.Fatalf("行权收益应 380 USDT: %+v", got[0])
	}
	if !nearly(got[0].PnlUSDT, -2620) {
		t.Fatalf("已实现盈亏应 -2620 USDT: %+v", got[0])
	}
	// 已结算单不重复结算。
	_, err = e.SettleDue(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
}

// 测试辅助：起 store。
func osCreateStore(dir string) (*store.Store, error) { return store.New(dir) }

func newSimDCD(id string, optType rune, strike, absYield, amt float64, exp time.Time) store.SimOrder {
	ccy, usdt := "BTC", amt*79000
	if optType == 'P' {
		ccy, usdt = "USDT", amt
	}
	return store.SimOrder{
		ID: id, Kind: "dcd", Mode: string(ModeSimLocal),
		ProductID: "BTC-USDT-TEST", OptType: optType,
		Strike: strike, AbsYield: absYield,
		Amt: amt, InvestCcy: ccy, AmtUSDT: usdt,
		ExpTime: exp, CreatedAt: time.Now(), State: "open",
	}
}

// 结算必须按每笔订单自己的标的取价，且结算后释放风控敞口、写入 USDT 盈亏。
func TestSettleDuePerUnderlyingAndRelease(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(id, product string, strike, amt float64) store.SimOrder {
		return store.SimOrder{
			ID: id, Kind: "dcd", Mode: string(ModeSimLocal), ProductID: product,
			OptType: 'P', Strike: strike, AbsYield: 0.005,
			Amt: amt, InvestCcy: "USDT", AmtUSDT: amt,
			ExpTime: now.Add(-time.Hour), CreatedAt: now.Add(-48 * time.Hour),
			State: "open",
		}
	}
	if err := st.SaveSim(mk("btc", "BTC-USDT-TEST", 76000, 76000)); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSim(mk("eth", "ETH-USDT-TEST", 3000, 3000)); err != nil {
		t.Fatal(err)
	}
	var asked []string
	e := &Executor{
		guard: NewGuard(DefaultLimits()),
		st:    st,
		spot: func(ccy string) (float64, error) {
			asked = append(asked, ccy)
			switch ccy {
			case "BTC":
				return 79000, nil
			case "ETH":
				return 3100, nil
			}
			return 0, nil
		},
	}
	e.guard.Restore([]float64{76000, 3000}, 0) // 两笔在持敞口
	got, err := e.SettleDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应结算 2 笔，实际 %d", len(got))
	}
	if len(asked) != 2 {
		t.Fatalf("应按标的分两次取价，实际 %v", asked)
	}
	// BTC: 76000×0.005 = 380 USDT；ETH: 3000×0.005 = 15 USDT。
	if !nearly(got[0].PnlUSDT, 380) || !nearly(got[1].PnlUSDT, 15) {
		t.Fatalf("盈亏不符: %+v", got)
	}
	if !nearly(got[0].SettlePx, 79000) || !nearly(got[1].SettlePx, 3100) {
		t.Fatalf("结算价未按标的区分: %+v", got)
	}
	if g := e.guard.Snapshot(); g.OpenCount != 0 || g.ExposureUSDT != 0 {
		t.Fatalf("结算后应释放敞口: %+v", g)
	}
}

func TestNormalizeQuoteYield(t *testing.T) {
	cases := []struct{ quoted, snap, want float64 }{
		{0.017, 0.017, 0.017},   // 已是小数
		{1.7, 0.017, 0.017},     // 百分数形态
		{0.034, 0, 0.034},       // 无锚、已是小数
		{0.0204, 0.017, 0.0204}, // 20% 以内偏差（如 0.0204 vs 0.017）不误判
	}
	for _, c := range cases {
		if got := normalizeQuoteYield(c.quoted, c.snap); !nearly(got, c.want) {
			t.Fatalf("normalizeQuoteYield(%v,%v)=%v，期望 %v", c.quoted, c.snap, got, c.want)
		}
	}
}
