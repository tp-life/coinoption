package strategy

import (
	"testing"
	"time"
)

// 普通档 0.03% 费率：10 天年化费率为 1.095pp；低 APR 时触发
// “手续费不超过 7% 权利金”的上限，净收益为 93% gross。
func TestNetAPROfFee(t *testing.T) {
	net := netAPROfFee(0.20, 10) // 7% × 20% = 1.4% > 1.095% → 费率生效
	assertNear(t, "20% APR 扣 3bps", net, 0.20-0.0003*365/10, 1e-12)
	net = netAPROfFee(0.01, 10) // 7% × 1% = 0.07% < 1.095%
	assertNear(t, "低 APR 走 7% 权利金上限", net, 0.01-0.0007, 1e-12)
	if got := netAPROfFee(0.5, 30); got <= 0 || got >= 0.5 {
		t.Fatalf("30 天 50%% APR 净收益应略低于 gross: %v", got)
	}
}

// ShortOptionAPR 返回的是净年化（25 天、gross≈29.1%）。
func TestShortOptionAPRIsNet(t *testing.T) {
	now := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	q := OptQuote{
		InstID: "BTC-USD-260327-54500-P", OptType: 'P', Strike: 54500,
		Expiry: now.Add(25 * 24 * time.Hour), BidPx: "0.02",
		CtVal: 0.01, SettleCcy: "BTC",
	}
	apr, days, err := ShortOptionAPR(q, goldenSpot, now)
	if err != nil {
		t.Fatal(err)
	}
	gross := (0.02 * 0.01 * goldenSpot) / (54500 * 0.01) * 365 / days
	assertNear(t, "net APR", apr, netAPROfFee(gross, days), 1e-12)
	if apr >= gross {
		t.Fatalf("净 APR 应低于 gross: net=%v gross=%v", apr, gross)
	}
}
