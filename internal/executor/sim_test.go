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
	if !nearly(r.PrincipalSz, 76000) || !nearly(r.YieldSz, 0.005) {
		t.Fatalf("交付应 76000 USDT + 0.005 USDT: %+v", r)
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
	}
	got, err := e.SettleDue(now, 79000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应只结算 1 笔（t2 未到期）: %d", len(got))
	}
	if got[0].ID != "sim-dcd-t1" || got[0].State != "settled" {
		t.Fatalf("结算单不符: %+v", got[0])
	}
	if !nearly(got[0].YieldSz, 0.005) {
		t.Fatalf("收益应 0.005: %+v", got[0])
	}
	// 已结算单不重复结算。
	_, err = e.SettleDue(now.Add(time.Hour), 79000)
	if err != nil {
		t.Fatal(err)
	}
}

// 测试辅助：起 store。
func osCreateStore(dir string) (*store.Store, error) { return store.New(dir) }

func newSimDCD(id string, optType rune, strike, absYield, amt float64, exp time.Time) store.SimOrder {
	return store.SimOrder{
		ID: id, Kind: "dcd", Mode: string(ModeSimLocal),
		ProductID: "BTC-USDT-TEST", OptType: optType,
		Strike: strike, AbsYield: absYield,
		Amt: amt, InvestCcy: "BTC", AmtUSDT: amt * 79000,
		ExpTime: exp, CreatedAt: time.Now(), State: "open",
	}
}
