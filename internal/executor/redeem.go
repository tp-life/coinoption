package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

// ─── 提前赎回（live only）────────────────────────────────────────────
//
// OKX 双币赢支持到期前赎回，但两条硬约束：
//  1. 只能赎回**申购币种**，金额可能低于本金（termRate 为负时就是亏损）；
//  2. 窗口为 [起息时间, 到期前 1 小时]，且 BTC/ETH 币对不支持提前赎回
//     （官方 FAQ）。窗口端点由平台返回 0 时按「不支持」处理。
//
// 因此这里不做任何自动赎回：报价必须由人看过金额后，再显式确认。

// RedeemQuote 申请提前赎回报价。只支持 live（纸面单是本地模拟，没有可赎回的对手方）。
func (e *Executor) RedeemQuote(ordID string) (*okx.DCDRedeemQuote, error) {
	if err := e.requireLive("提前赎回"); err != nil {
		return nil, err
	}
	ordID = strings.TrimSpace(ordID)
	if ordID == "" {
		return nil, fmt.Errorf("ordId 不能为空")
	}
	q, err := e.client.PostDCDRedeemQuote(ordID)
	if err != nil {
		return nil, fmt.Errorf("提前赎回报价失败: %w", err)
	}
	return q, nil
}

// Redeem 确认提前赎回（quoteID 必须来自同一 ordId 的 RedeemQuote）。
//
// 写操作：客户端对非幂等请求不重试，结果不明返回 *okx.AmbiguousError。
// 无论成功与否都清空平台订单缓存，让下一次风控/PnL 重新拉真实状态。
func (e *Executor) Redeem(ordID, quoteID string) (Outcome, error) {
	if err := e.requireLive("提前赎回"); err != nil {
		return Outcome{}, err
	}
	ordID, quoteID = strings.TrimSpace(ordID), strings.TrimSpace(quoteID)
	if ordID == "" || quoteID == "" {
		return Outcome{}, fmt.Errorf("ordId / quoteId 均不能为空")
	}
	res, err := e.client.PostDCDRedeem(ordID, quoteID)
	e.invalidatePlatformCaches()
	e.invalidatePositions()
	if err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Ts: time.Now(), Event: "error", Subject: ordID,
			Reason: "redeem: " + err.Error(), Mode: string(e.mode),
		})
		return Outcome{}, fmt.Errorf("提前赎回失败: %w", err)
	}
	state := ""
	if len(res) > 0 {
		state = res[0].State
	}
	out := Outcome{
		Mode: e.mode, Kind: "redeem", Ref: ordID, OrdID: ordID, QuoteID: quoteID,
		Detail: "提前赎回已受理（state=" + state + "），资金按平台到账时间为准",
	}
	_ = e.st.Audit(store.AuditEvent{
		Ts: time.Now(), Event: "redeem", Subject: ordID,
		Reason: state, Mode: string(e.mode), Detail: out.Detail,
	})
	return out, nil
}

// CheckRedeemable 用平台订单信息做一次本地预检（窗口/币对支持）。
// 返回 nil 只代表「看起来可以赎回」，最终以 redeem-quote 接口为准。
func CheckRedeemable(o okx.DCDOrder, now time.Time) error {
	if o.OrdID == "" {
		return fmt.Errorf("订单不存在")
	}
	start, end := int64(o.RedeemStartTime), int64(o.RedeemEndTime)
	if start == 0 && end == 0 {
		return fmt.Errorf("该产品不支持提前赎回（BTC/ETH 币对或被平台关闭）")
	}
	if start > 0 && now.UnixMilli() < start {
		return fmt.Errorf("未到赎回窗口：%s 开放", time.UnixMilli(start).Local().Format("2006-01-02 15:04"))
	}
	if end > 0 && now.UnixMilli() > end {
		return fmt.Errorf("已过赎回窗口：%s 截止", time.UnixMilli(end).Local().Format("2006-01-02 15:04"))
	}
	if o.ExpTime > 0 && now.UnixMilli() > o.ExpTime-3600_000 {
		return fmt.Errorf("距到期不足 1 小时，无法提前赎回")
	}
	switch strings.ToLower(strings.TrimSpace(o.State)) {
	case "redeemed", "pending_redeem", "settled", "rejected":
		return fmt.Errorf("订单状态 %s 不可赎回", o.State)
	}
	return nil
}

// DCDOrder 查询单笔平台订单（赎回前展示本金/窗口用）。
func (e *Executor) DCDOrder(ordID string) (*okx.DCDOrder, error) {
	if err := e.requireLive("查询双币赢订单"); err != nil {
		return nil, err
	}
	ordID = strings.TrimSpace(ordID)
	if ordID == "" {
		return nil, fmt.Errorf("ordId 不能为空")
	}
	rows, err := e.client.GetDCDOrderHistory(ordID, "", "", "", 10)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].OrdID == ordID {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("未找到订单 %s", ordID)
}

func (e *Executor) requireLive(op string) error {
	if e.mode != ModeLive {
		return fmt.Errorf("%s只支持 live 模式（当前 %s）；纸面单不会被平台赎回", op, e.mode)
	}
	if e.client == nil {
		return fmt.Errorf("%s需要 OKX 客户端", op)
	}
	return nil
}

// invalidatePlatformCaches 让下一次风控/PnL 重新拉平台订单。
func (e *Executor) invalidatePlatformCaches() {
	e.liveMu.Lock()
	e.liveAt = time.Time{}
	e.liveOrders = nil
	e.liveMu.Unlock()
	e.pnlMu.Lock()
	e.pnlAt = time.Time{}
	e.pnlMu.Unlock()
}
