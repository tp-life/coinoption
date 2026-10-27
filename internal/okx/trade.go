package okx

import (
	"fmt"
	"net/url"
)

// ─── 交易：期权下单（/api/v5/trade/order）── 限速 60次/2s ───

const pathTradeOrder = "/api/v5/trade/order"

// OptionOrderReq 期权卖单请求（v5 docs: 期权免保 cross；net 模式 posSide=net）。
type OptionOrderReq struct {
	InstID     string `json:"instId"`
	TdMode     string `json:"tdMode"`               // cross（M3 定稿）| isolated
	Side       string `json:"side"`                 // sell（开仓卖方）
	PosSide    string `json:"posSide,omitempty"`    // net / short（买卖模式）
	OrdType    string `json:"ordType"`              // limit（市价保护由限价=bid承担）
	Sz         string `json:"sz"`                   // 张数
	Px         string `json:"px"`                   // 限价（币本位=BTC；_UM=USD）
	ReduceOnly bool   `json:"reduceOnly,omitempty"` // 平仓用
}

// TradeOrderResp 下单回执 data[0]（sCode 非零 = 该单业务失败）。
type TradeOrderResp struct {
	OrdID string `json:"ordId"`
	SCode string `json:"sCode"`
	SMsg  string `json:"sMsg"`
}

// PostOptionOrder 提交期权订单，返回平台 ordId。
func (c *Client) PostOptionOrder(req OptionOrderReq) (string, error) {
	r, err := c.postDecode[TradeOrderResp](pathTradeOrder, req)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", errNoData
	}
	if r.SCode != "" && r.SCode != "0" {
		return "", fmt.Errorf("下单被拒 sCode=%s sMsg=%s", r.SCode, r.SMsg)
	}
	return r.OrdID, nil
}

// ─── 账户：余额 / 期权持仓 ───

// BalanceEntry /account/balance data[] 条目节选（账户模式 NON_FIXED → 单条）。
type BalanceEntry struct {
	Details []BalanceDetail `json:"details"`
}

// BalanceDetail 单币余额（数值全部为字符串 → flexReal）。
type BalanceDetail struct {
	Ccy       string   `json:"ccy"`
	AvailBal  flexReal `json:"availBal"` // 现货可用
	AvailEq   flexReal `json:"availEq"`  // 保证金账户等值
	FrozenBal flexReal `json:"frozenBal"`
}

// GetBalance 账户余额（可指定币种过滤，分批拉全账户后本地筛——账户步接口数据形态稳定）。
func (c *Client) GetBalance(ccy string) ([]BalanceDetail, error) {
	q := url.Values{}
	if ccy != "" {
		q.Set("ccy", ccy)
	}
	raw, err := c.get(ctx(), pathAccountBalance, q)
	entries, wErr := wrap[BalanceEntry](raw, err)
	if wErr != nil {
		return nil, wErr
	}
	if err != nil {
		return nil, err
	}
	var out []BalanceDetail
	for _, e := range entries {
		for _, d := range e.Details {
			if ccy == "" || d.Ccy == ccy {
				out = append(out, d)
			}
		}
	}
	return out, nil
}

// OptPosition /account/positions（instType=OPTION）节选。
type OptPosition struct {
	InstID   string   `json:"instId"`
	InstType string   `json:"instType"`
	MgnMode  string   `json:"mgnMode"`
	PosSide  string   `json:"posSide"`
	Pos      flexReal `json:"pos"` // 正=多 负=空（卖方为负）
	AvailPos flexReal `json:"availPos"`
	AvgPx    flexReal `json:"avgPx"`
	Ccy      string   `json:"ccy"`
}

const (
	pathAccountBalance  = "/api/v5/account/balance"
	pathAccountPosition = "/api/v5/account/positions"
)

// GetOptionPositions 卖出期权在手仓位。
func (c *Client) GetOptionPositions() ([]OptPosition, error) {
	q := url.Values{"instType": {"OPTION"}}
	return wrap[OptPosition](c.get(ctx(), pathAccountPosition, q))
}

var errNoData = fmt.Errorf("服务端返回空 data")
